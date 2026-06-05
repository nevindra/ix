use std::sync::Arc;

pub struct AppState {
    pub config: ix_core::config::DaemonConfig,
    pub browser: Arc<dyn ix_browser::BrowserBackend>,
    pub kernels: Arc<ix_code::KernelManager>,
    pub shell_sessions: Arc<ix_shell::SessionManager>,
    pub egress: Option<Arc<ix_egress::EgressFilter>>,
    pub start_time: std::time::Instant,
}
