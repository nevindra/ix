use std::sync::atomic::{AtomicU32, Ordering};
use std::time::Duration;

use bytes::Bytes;
use serde::Deserialize;
use tokio::process::{Child, Command};
use tracing::{debug, info, warn};
use zeromq::{DealerSocket, Socket, SocketRecv, SocketSend, SubSocket, ZmqMessage};

use ix_core::{Error, Result};

use crate::protocol::JupyterMessage;

/// The JSON connection file written by ipykernel on startup
#[derive(Debug, Deserialize)]
pub struct ConnectionInfo {
    pub transport: String,
    pub ip: String,
    pub shell_port: u16,
    pub iopub_port: u16,
    #[allow(dead_code)]
    pub stdin_port: u16,
    #[allow(dead_code)]
    pub control_port: u16,
    #[allow(dead_code)]
    pub hb_port: u16,
    pub key: String,
    #[allow(dead_code)]
    pub signature_scheme: String,
}

impl ConnectionInfo {
    pub fn endpoint(&self, port: u16) -> String {
        format!("{}://{}:{}", self.transport, self.ip, port)
    }
}

pub struct Kernel {
    pub language: String,
    pub process: Child,
    pub connection: ConnectionInfo,
    pub shell_socket: DealerSocket,
    pub iopub_socket: SubSocket,
    pub session_id: String,
    pub execution_count: AtomicU32,
}

impl Kernel {
    /// Spawn a kernel for the given language and connect ZMQ sockets.
    pub async fn start(language: &str) -> Result<Self> {
        let id = uuid::Uuid::new_v4();
        let conn_file = format!("/tmp/ix-kernel-{language}-{id}.json");
        let session_id = id.to_string();

        // Remove stale connection file if present
        let _ = tokio::fs::remove_file(&conn_file).await;

        let child = spawn_kernel(language, &conn_file)?;
        debug!(language, conn_file, "kernel process spawned");

        // Wait for kernel to write its connection file (up to 15s)
        let conn_info = wait_for_connection_file(&conn_file, Duration::from_secs(15)).await?;

        // Connect shell (DEALER) socket
        let shell_ep = conn_info.endpoint(conn_info.shell_port);
        let iopub_ep = conn_info.endpoint(conn_info.iopub_port);

        let mut shell_socket = DealerSocket::new();
        shell_socket
            .connect(&shell_ep)
            .await
            .map_err(|e| Error::Internal(format!("shell socket connect failed: {e}")))?;

        // Connect iopub (SUB) socket and subscribe to all topics
        let mut iopub_socket = SubSocket::new();
        iopub_socket
            .connect(&iopub_ep)
            .await
            .map_err(|e| Error::Internal(format!("iopub socket connect failed: {e}")))?;
        iopub_socket
            .subscribe("")
            .await
            .map_err(|e| Error::Internal(format!("iopub subscribe failed: {e}")))?;

        debug!(shell = %shell_ep, iopub = %iopub_ep, "connected ZMQ sockets");

        let mut kernel = Self {
            language: language.to_string(),
            process: child,
            connection: conn_info,
            shell_socket,
            iopub_socket,
            session_id,
            execution_count: AtomicU32::new(0),
        };

        // Send kernel_info_request and wait for reply to confirm readiness
        kernel.wait_ready(Duration::from_secs(30)).await?;
        info!(language, "kernel is ready");

        Ok(kernel)
    }

    /// Send kernel_info_request and wait for kernel_info_reply (signals ready)
    async fn wait_ready(&mut self, timeout: Duration) -> Result<()> {
        let msg = JupyterMessage::new(
            "kernel_info_request",
            &self.session_id,
            serde_json::json!({}),
        );
        let zmq_msg = frames_to_zmq(msg.serialize(&self.connection.key))?;

        self.shell_socket
            .send(zmq_msg)
            .await
            .map_err(|e| Error::Internal(format!("failed to send kernel_info_request: {e}")))?;

        tokio::time::timeout(timeout, async {
            loop {
                let recv = self.shell_socket.recv().await
                    .map_err(|e| Error::Internal(format!("recv error: {e}")))?;
                let frames: Vec<Bytes> = recv.into_vec();
                let reply = JupyterMessage::deserialize(frames, &self.connection.key)?;
                if reply.header.msg_type == "kernel_info_reply" {
                    return Ok(());
                }
            }
        })
        .await
        .map_err(|_| Error::Timeout("kernel did not become ready within 30s".into()))?
    }

    /// Increment and return the next execution count
    pub fn next_execution_count(&self) -> u32 {
        self.execution_count.fetch_add(1, Ordering::SeqCst) + 1
    }

    /// Shut down the kernel gracefully
    pub async fn shutdown(&mut self) {
        let msg = JupyterMessage::new(
            "shutdown_request",
            &self.session_id,
            serde_json::json!({"restart": false}),
        );
        if let Ok(zmq_msg) = frames_to_zmq(msg.serialize(&self.connection.key)) {
            let _ = self.shell_socket.send(zmq_msg).await;
        }
        if let Err(e) = self.process.kill().await {
            warn!(error = %e, "failed to kill kernel process");
        }
    }
}

/// Convert serialized frames to a `ZmqMessage`
pub fn frames_to_zmq(frames: Vec<Bytes>) -> Result<ZmqMessage> {
    ZmqMessage::try_from(frames)
        .map_err(|e| Error::Internal(format!("failed to build ZMQ message: {e}")))
}

/// Spawn the appropriate kernel process for the given language
fn spawn_kernel(language: &str, conn_file: &str) -> Result<Child> {
    let mut cmd = match language.to_lowercase().as_str() {
        "python" | "python3" | "py" => {
            let mut c = Command::new("python3");
            c.args(["-m", "ipykernel_launcher", "-f", conn_file]);
            c
        }
        "javascript" | "js" | "node" => {
            let mut c = Command::new("ijskernel");
            c.args(["--spec-path", conn_file]);
            c
        }
        "bash" | "sh" | "shell" => {
            // Try bash_kernel first; if not available, caller falls back to process exec
            let mut c = Command::new("python3");
            c.args(["-m", "bash_kernel", "-f", conn_file]);
            c
        }
        other => {
            return Err(Error::BadRequest(format!("unsupported language: {other}")));
        }
    };

    cmd.stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());

    cmd.spawn().map_err(|e| {
        Error::Internal(format!(
            "failed to spawn kernel for language '{language}': {e}"
        ))
    })
}

/// Poll until the connection file exists and can be parsed, or timeout
pub(crate) async fn wait_for_connection_file_pub(path: &str, timeout: Duration) -> Result<ConnectionInfo> {
    wait_for_connection_file(path, timeout).await
}

/// Exposed for testing: spawn kernel command for language, return Err if unsupported
pub(crate) fn language_command(language: &str) -> Result<(String, Vec<String>)> {
    match language.to_lowercase().as_str() {
        "python" | "python3" | "py" => Ok(("python3".into(), vec!["-m".into(), "ipykernel_launcher".into()])),
        "javascript" | "js" | "node" => Ok(("ijskernel".into(), vec!["--spec-path".into()])),
        "bash" | "sh" | "shell" => Ok(("python3".into(), vec!["-m".into(), "bash_kernel".into()])),
        other => Err(ix_core::Error::BadRequest(format!("unsupported language: {other}"))),
    }
}

/// Poll until the connection file exists and can be parsed, or timeout
async fn wait_for_connection_file(path: &str, timeout: Duration) -> Result<ConnectionInfo> {
    let start = std::time::Instant::now();
    loop {
        if start.elapsed() > timeout {
            return Err(Error::Timeout(format!(
                "kernel connection file not created within {}s: {}",
                timeout.as_secs(),
                path
            )));
        }

        match tokio::fs::read_to_string(path).await {
            Ok(contents) if !contents.trim().is_empty() => {
                match serde_json::from_str::<ConnectionInfo>(&contents) {
                    Ok(info) => return Ok(info),
                    Err(_) => {
                        // File may be partially written — keep waiting
                    }
                }
            }
            _ => {}
        }

        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE_CONNECTION_JSON: &str = r#"{
        "transport": "tcp",
        "ip": "127.0.0.1",
        "shell_port": 54321,
        "iopub_port": 54322,
        "stdin_port": 54323,
        "control_port": 54324,
        "hb_port": 54325,
        "key": "abc123-secret-key",
        "signature_scheme": "hmac-sha256",
        "kernel_name": "python3"
    }"#;

    // -----------------------------------------------------------------------
    // ConnectionInfo deserialization
    // -----------------------------------------------------------------------

    #[test]
    fn connection_info_deserializes_from_json() {
        let info: ConnectionInfo = serde_json::from_str(SAMPLE_CONNECTION_JSON).unwrap();
        assert_eq!(info.transport, "tcp");
        assert_eq!(info.ip, "127.0.0.1");
        assert_eq!(info.shell_port, 54321);
        assert_eq!(info.iopub_port, 54322);
        assert_eq!(info.key, "abc123-secret-key");
        assert_eq!(info.signature_scheme, "hmac-sha256");
    }

    #[test]
    fn connection_info_endpoint_formats_correctly() {
        let info: ConnectionInfo = serde_json::from_str(SAMPLE_CONNECTION_JSON).unwrap();
        assert_eq!(info.endpoint(54321), "tcp://127.0.0.1:54321");
        assert_eq!(info.endpoint(54322), "tcp://127.0.0.1:54322");
    }

    #[test]
    fn connection_info_endpoint_uses_transport_and_ip() {
        let json = r#"{
            "transport": "ipc",
            "ip": "0.0.0.0",
            "shell_port": 1,
            "iopub_port": 2,
            "stdin_port": 3,
            "control_port": 4,
            "hb_port": 5,
            "key": "k",
            "signature_scheme": "hmac-sha256"
        }"#;
        let info: ConnectionInfo = serde_json::from_str(json).unwrap();
        assert_eq!(info.endpoint(9999), "ipc://0.0.0.0:9999");
    }

    #[test]
    fn connection_info_missing_required_field_fails() {
        let json = r#"{"transport": "tcp", "ip": "127.0.0.1"}"#;
        let result = serde_json::from_str::<ConnectionInfo>(json);
        assert!(result.is_err());
    }

    // -----------------------------------------------------------------------
    // Language-to-command mapping
    // -----------------------------------------------------------------------

    #[test]
    fn python_maps_to_python3_ipykernel() {
        let (cmd, args) = language_command("python").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"-m".to_string()));
        assert!(args.contains(&"ipykernel_launcher".to_string()));
    }

    #[test]
    fn python3_alias_maps_to_python3_ipykernel() {
        let (cmd, args) = language_command("python3").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"ipykernel_launcher".to_string()));
    }

    #[test]
    fn py_alias_maps_to_python3_ipykernel() {
        let (cmd, _) = language_command("py").unwrap();
        assert_eq!(cmd, "python3");
    }

    #[test]
    fn javascript_maps_to_ijskernel() {
        let (cmd, _) = language_command("javascript").unwrap();
        assert_eq!(cmd, "ijskernel");
    }

    #[test]
    fn js_alias_maps_to_ijskernel() {
        let (cmd, _) = language_command("js").unwrap();
        assert_eq!(cmd, "ijskernel");
    }

    #[test]
    fn node_alias_maps_to_ijskernel() {
        let (cmd, _) = language_command("node").unwrap();
        assert_eq!(cmd, "ijskernel");
    }

    #[test]
    fn bash_maps_to_python3_bash_kernel() {
        let (cmd, args) = language_command("bash").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"bash_kernel".to_string()));
    }

    #[test]
    fn sh_alias_maps_to_bash_kernel() {
        let (cmd, args) = language_command("sh").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"bash_kernel".to_string()));
    }

    #[test]
    fn shell_alias_maps_to_bash_kernel() {
        let (cmd, _) = language_command("shell").unwrap();
        assert_eq!(cmd, "python3");
    }

    #[test]
    fn unsupported_language_returns_bad_request_error() {
        let result = language_command("ruby");
        assert!(result.is_err());
        let err = result.unwrap_err().to_string();
        assert!(err.contains("unsupported") || err.contains("ruby"));
    }

    #[test]
    fn case_insensitive_python_uppercase() {
        let result = language_command("PYTHON");
        assert!(result.is_ok());
    }

    #[test]
    fn case_insensitive_bash_mixed_case() {
        let result = language_command("Bash");
        assert!(result.is_ok());
    }
}
