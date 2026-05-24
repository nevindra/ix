use std::time::Instant;

use ix_core::sse::SseSender;
use ix_core::types::ShellRequest;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;
use tracing::{debug, error, warn};

use crate::signal::kill_process_group;

pub async fn execute_shell(req: ShellRequest, sender: SseSender) {
    let start = Instant::now();

    // Build the command: bash -l -c "<command>"
    let mut cmd = Command::new("bash");
    cmd.args(["-l", "-c", &req.command]);

    // Set working directory if provided
    if let Some(ref cwd) = req.cwd {
        cmd.current_dir(cwd);
    }

    // Pipe stdout and stderr for streaming
    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    // Create a new process group so we can signal the whole group on timeout.
    // SAFETY: setpgid(0, 0) is async-signal-safe per POSIX.
    unsafe {
        cmd.pre_exec(|| {
            nix::unistd::setpgid(nix::unistd::Pid::from_raw(0), nix::unistd::Pid::from_raw(0))
                .map_err(|e| std::io::Error::from_raw_os_error(e as i32))?;
            Ok(())
        });
    }

    // Spawn the child process
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            error!(error = %e, "failed to spawn shell command");
            sender
                .send_error(&format!("failed to spawn: {e}"), Some(-1))
                .await;
            return;
        }
    };

    // Get the process group ID (same as PID since we called setpgid(0,0))
    let pid = match child.id() {
        Some(p) => p as i32,
        None => {
            // Child already exited before we could read its PID
            let elapsed_ms = start.elapsed().as_millis() as u64;
            let exit_code = child
                .wait()
                .await
                .map(|s| s.code().unwrap_or(-1))
                .unwrap_or(-1);
            sender.send_complete(exit_code, elapsed_ms).await;
            return;
        }
    };

    debug!(pid, command = %req.command, "spawned shell process");

    // Take stdout/stderr handles
    let stdout = child.stdout.take().expect("stdout was piped");
    let stderr = child.stderr.take().expect("stderr was piped");

    let mut stdout_lines = BufReader::new(stdout).lines();
    let mut stderr_lines = BufReader::new(stderr).lines();

    // Optional deadline as a boxed future so it can be reused across loop iterations.
    let timeout_secs = req.timeout;
    let timed_out = stream_with_timeout(
        pid,
        timeout_secs,
        &sender,
        &mut stdout_lines,
        &mut stderr_lines,
    )
    .await;

    if timed_out {
        kill_process_group(pid).await;
        let elapsed_ms = start.elapsed().as_millis() as u64;
        sender
            .send_error(
                &format!(
                    "command timed out after {}s",
                    timeout_secs.unwrap_or(0)
                ),
                Some(-1),
            )
            .await;
        sender.send_complete(-1, elapsed_ms).await;
        return;
    }

    // Wait for the process to fully exit and collect the exit code.
    let exit_code = match child.wait().await {
        Ok(status) => status.code().unwrap_or(-1),
        Err(e) => {
            error!(error = %e, "failed to wait for child process");
            -1
        }
    };

    let elapsed_ms = start.elapsed().as_millis() as u64;
    debug!(pid, exit_code, elapsed_ms, "shell command finished");
    sender.send_complete(exit_code, elapsed_ms).await;
}

/// Stream stdout/stderr line by line, with an optional timeout.
/// Returns `true` if the timeout fired, `false` if the pipes were drained normally.
async fn stream_with_timeout(
    pid: i32,
    timeout_secs: Option<u64>,
    sender: &SseSender,
    stdout_lines: &mut tokio::io::Lines<BufReader<tokio::process::ChildStdout>>,
    stderr_lines: &mut tokio::io::Lines<BufReader<tokio::process::ChildStderr>>,
) -> bool {
    // Build a deadline Instant so we can check it without re-pinning a future.
    let deadline = timeout_secs.map(|s| tokio::time::Instant::now() + std::time::Duration::from_secs(s));

    loop {
        // How long until the deadline fires (None = no timeout).
        let sleep = deadline.map(|d| {
            let now = tokio::time::Instant::now();
            if d > now { d - now } else { std::time::Duration::ZERO }
        });

        // We need select! with a conditional third arm. We do this by racing
        // a short-circuited future: if sleep is None, use `pending()`.
        let timed_out = tokio::select! {
            // Timeout arm (only active when `sleep` is Some and non-zero)
            _ = async {
                match sleep {
                    Some(d) => tokio::time::sleep(d).await,
                    None => std::future::pending::<()>().await,
                }
            } => {
                warn!(pid, "shell command timed out");
                true
            }

            line = stdout_lines.next_line() => {
                match line {
                    Ok(Some(l)) => { sender.send_stdout(&l).await; false }
                    Ok(None) => {
                        // stdout EOF — drain stderr then signal done
                        while let Ok(Some(l)) = stderr_lines.next_line().await {
                            sender.send_stderr(&l).await;
                        }
                        return false;
                    }
                    Err(e) => {
                        warn!(error = %e, "error reading stdout");
                        return false;
                    }
                }
            }

            line = stderr_lines.next_line() => {
                match line {
                    Ok(Some(l)) => { sender.send_stderr(&l).await; false }
                    Ok(None) => {
                        // stderr EOF — drain stdout then signal done
                        while let Ok(Some(l)) = stdout_lines.next_line().await {
                            sender.send_stdout(&l).await;
                        }
                        return false;
                    }
                    Err(e) => {
                        warn!(error = %e, "error reading stderr");
                        return false;
                    }
                }
            }
        };

        if timed_out {
            return true;
        }
    }
}
