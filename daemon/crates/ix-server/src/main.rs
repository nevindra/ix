use std::sync::Arc;

use tokio::signal;
use tracing::{error, info};
use tracing_subscriber::EnvFilter;

use ix_server::router;
use ix_server::state::AppState;

#[cfg(target_os = "linux")]
mod vsock_listener {
    use std::io;
    use tokio_vsock::{VsockAddr, VsockListener, VsockStream};

    pub struct AxumVsockListener {
        inner: VsockListener,
    }

    impl AxumVsockListener {
        pub fn bind(port: u32) -> io::Result<Self> {
            let addr = VsockAddr::new(libc::VMADDR_CID_ANY, port);
            Ok(Self {
                inner: VsockListener::bind(addr)?,
            })
        }
    }

    impl axum::serve::Listener for AxumVsockListener {
        type Io = VsockStream;
        type Addr = VsockAddr;

        fn accept(
            &mut self,
        ) -> impl std::future::Future<Output = (Self::Io, Self::Addr)> + Send {
            async {
                loop {
                    match self.inner.accept().await {
                        Ok(conn) => return conn,
                        Err(e) => {
                            tracing::error!(error = %e, "vsock accept error, retrying");
                            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
                        }
                    }
                }
            }
        }

        fn local_addr(&self) -> io::Result<Self::Addr> {
            self.inner.local_addr()
        }
    }
}

#[tokio::main]
async fn main() {
    // 1. Init tracing
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info")),
        )
        .init();

    // 2. Load config from env
    let config = ix_core::config::DaemonConfig::from_env();
    let addr = config.addr.clone();
    let socket = config.socket.clone();
    let vsock_port = config.vsock_port;
    let vsock_ready_port = config.vsock_ready_port;
    info!(addr = %addr, workspace = %config.workspace, "starting ixd");

    // 3. Create shared state
    let browser = Arc::new(ix_browser::PinchtabBackend::new().await);
    let browser_trait: Arc<dyn ix_browser::BrowserBackend> = browser.clone();

    let kernels = Arc::new(ix_code::KernelManager::new());

    let egress = if config.egress.enabled {
        match ix_egress::EgressFilter::start(config.egress.clone()).await {
            Ok(filter) => {
                info!("egress filter started");
                Some(Arc::new(filter))
            }
            Err(e) => {
                error!(error = %e, "failed to start egress filter, continuing without it");
                None
            }
        }
    } else {
        None
    };

    let state = Arc::new(AppState {
        config,
        browser: browser_trait,
        kernels: kernels.clone(),
        egress: egress.clone(),
        start_time: std::time::Instant::now(),
    });

    // 4. Build router
    let app = router::build_router(state);

    // 5. Bind listener: vsock (IX_VSOCK_PORT), Unix socket (IX_SOCKET), or TCP
    #[cfg(target_os = "linux")]
    if let Some(port) = vsock_port {
        let listener = vsock_listener::AxumVsockListener::bind(port)
            .unwrap_or_else(|e| panic!("failed to bind vsock port {port}: {e}"));
        info!(port, "ixd listening on vsock");

        if let Some(ready_port) = vsock_ready_port {
            let ready_addr = tokio_vsock::VsockAddr::new(libc::VMADDR_CID_HOST, ready_port);
            match tokio_vsock::VsockStream::connect(ready_addr).await {
                Ok(mut stream) => {
                    use tokio::io::AsyncWriteExt;
                    let _ = stream.write_all(b"READY\n").await;
                    drop(stream);
                    info!(ready_port, "sent READY signal");
                }
                Err(e) => error!(error = %e, ready_port, "failed to send READY signal"),
            }
        }

        axum::serve(listener, app)
            .with_graceful_shutdown(shutdown_signal())
            .await
            .unwrap_or_else(|e| error!(error = %e, "server error"));
    } else if let Some(ref socket_path) = socket {
        let _ = std::fs::remove_file(socket_path);
        let listener = tokio::net::UnixListener::bind(socket_path)
            .unwrap_or_else(|e| panic!("failed to bind to {socket_path}: {e}"));
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(socket_path, std::fs::Permissions::from_mode(0o666));
        info!(path = %socket_path, "ixd listening on unix socket");

        axum::serve(listener, app)
            .with_graceful_shutdown(shutdown_signal())
            .await
            .unwrap_or_else(|e| error!(error = %e, "server error"));
    } else {
        let listener = tokio::net::TcpListener::bind(&addr)
            .await
            .unwrap_or_else(|e| panic!("failed to bind to {addr}: {e}"));
        info!(addr = %addr, "ixd listening");

        axum::serve(listener, app)
            .with_graceful_shutdown(shutdown_signal())
            .await
            .unwrap_or_else(|e| error!(error = %e, "server error"));
    }

    #[cfg(not(target_os = "linux"))]
    if let Some(ref socket_path) = socket {
        let _ = std::fs::remove_file(socket_path);
        let listener = tokio::net::UnixListener::bind(socket_path)
            .unwrap_or_else(|e| panic!("failed to bind to {socket_path}: {e}"));
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(socket_path, std::fs::Permissions::from_mode(0o666));
        info!(path = %socket_path, "ixd listening on unix socket");

        axum::serve(listener, app)
            .with_graceful_shutdown(shutdown_signal())
            .await
            .unwrap_or_else(|e| error!(error = %e, "server error"));
    } else {
        let listener = tokio::net::TcpListener::bind(&addr)
            .await
            .unwrap_or_else(|e| panic!("failed to bind to {addr}: {e}"));
        info!(addr = %addr, "ixd listening");

        axum::serve(listener, app)
            .with_graceful_shutdown(shutdown_signal())
            .await
            .unwrap_or_else(|e| error!(error = %e, "server error"));
    }

    // 7. Cleanup on shutdown
    info!("shutting down...");
    kernels.shutdown().await;
    if let Some(egress) = egress {
        if let Ok(filter) = Arc::try_unwrap(egress) {
            filter.shutdown().await;
        }
    }
    browser.shutdown().await;
    info!("ixd stopped");
}

async fn shutdown_signal() {
    let ctrl_c = async {
        signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        signal::unix::signal(signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => info!("received Ctrl+C"),
        _ = terminate => info!("received SIGTERM"),
    }
}
