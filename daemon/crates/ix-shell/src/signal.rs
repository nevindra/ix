use nix::sys::signal::{Signal, killpg};
use nix::unistd::Pid;
use std::time::Duration;
use tracing::{debug, warn};

/// Send SIGTERM to the process group, wait 5 seconds, then SIGKILL if still running.
pub async fn kill_process_group(pgid: i32) {
    let pid = Pid::from_raw(pgid);

    debug!(%pgid, "sending SIGTERM to process group");
    if let Err(e) = killpg(pid, Signal::SIGTERM) {
        warn!(%pgid, error = %e, "failed to send SIGTERM to process group");
        return;
    }

    tokio::time::sleep(Duration::from_secs(5)).await;

    debug!(%pgid, "sending SIGKILL to process group");
    if let Err(e) = killpg(pid, Signal::SIGKILL) {
        // ESRCH means the process group no longer exists — that's fine.
        if e != nix::errno::Errno::ESRCH {
            warn!(%pgid, error = %e, "failed to send SIGKILL to process group");
        }
    }
}
