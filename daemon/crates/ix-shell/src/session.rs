//! Persistent bash sessions. One long-lived `bash -l` per session_id: the
//! login-shell profile cost (~18 ms) is paid once, then each command is a
//! stdin write + sentinel-delimited read (<1 ms in-guest).
//!
//! Protocol per command:
//!   <cwd line, when requested>
//!   <user command lines>
//!   printf '__IX_DONE_<nonce>__ %d\n' "$?"
//!   printf '__IX_DONE_<nonce>__\n' >&2
//!
//! The nonce is unique per command, so command output containing the literal
//! sentinel of a *previous* command cannot terminate the read early. Output
//! produced without a trailing newline lands on the sentinel line; the reader
//! splits it back apart. Known limitation (documented): a command that itself
//! consumes stdin (bare `cat`, heredoc typos) swallows the sentinel printf
//! lines and runs until the timeout kills the session — same failure class as
//! any REPL. One-shot exec (no session_id) remains available for isolation.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStderr, ChildStdin, ChildStdout, Command};
use tokio::sync::Mutex;
use tracing::{info, warn};

use ix_core::sse::SseSender;
use ix_core::types::ShellRequest;

use crate::signal::kill_process_group;

/// Idle sessions are killed by the eviction loop after this long.
const SESSION_IDLE_TTL: Duration = Duration::from_secs(600);
/// Leak guard: one VM serves one chat, so this should never be reached.
const MAX_SESSIONS: usize = 16;
/// Commands in a session get a default timeout: an unbalanced quote/heredoc
/// makes bash swallow the sentinel and hang forever otherwise.
const DEFAULT_TIMEOUT_SECS: u64 = 300;

static NONCE: AtomicU64 = AtomicU64::new(0);

struct Session {
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
    stderr: BufReader<ChildStderr>,
    child: Child,
    pgid: i32,
    last_used: Instant,
}

impl Session {
    async fn spawn() -> std::io::Result<Self> {
        let mut cmd = Command::new("bash");
        cmd.arg("-l"); // login env, paid once per session
        cmd.stdin(std::process::Stdio::piped());
        cmd.stdout(std::process::Stdio::piped());
        cmd.stderr(std::process::Stdio::piped());
        // Own process group so timeout kills children too (same as exec.rs).
        // SAFETY: setpgid(0, 0) is async-signal-safe per POSIX.
        unsafe {
            cmd.pre_exec(|| {
                nix::unistd::setpgid(
                    nix::unistd::Pid::from_raw(0),
                    nix::unistd::Pid::from_raw(0),
                )
                .map_err(|e| std::io::Error::from_raw_os_error(e as i32))?;
                Ok(())
            });
        }
        let mut child = cmd.spawn()?;
        let pgid = child.id().unwrap_or_default() as i32;
        let stdin = child.stdin.take().expect("stdin piped");
        let stdout = BufReader::new(child.stdout.take().expect("stdout piped"));
        let stderr = BufReader::new(child.stderr.take().expect("stderr piped"));
        Ok(Self {
            stdin,
            stdout,
            stderr,
            child,
            pgid,
            last_used: Instant::now(),
        })
    }

    fn alive(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }

    async fn kill(&mut self) {
        kill_process_group(self.pgid).await;
        // Reap deterministically so the child does not linger as a zombie until
        // tokio's deferred orphan reaping (mirrors exec.rs, which waits after
        // kill_process_group). start_kill() is redundant once we wait().
        let _ = self.child.wait().await;
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.child.start_kill();
    }
}

/// Single-quote a string for safe interpolation into a bash line.
fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

pub struct SessionManager {
    /// session_id -> Some(session) when idle, None while a command is running
    /// (a concurrent command for the same id gets "session busy").
    sessions: Mutex<HashMap<String, Option<Session>>>,
}

impl SessionManager {
    /// Construct shared and start the idle-eviction loop.
    pub fn new_shared() -> Arc<Self> {
        let mgr = Arc::new(Self {
            sessions: Mutex::new(HashMap::new()),
        });
        let weak = Arc::downgrade(&mgr);
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(60));
            loop {
                tick.tick().await;
                let Some(mgr) = weak.upgrade() else { return };
                mgr.evict_idle().await;
            }
        });
        mgr
    }

    async fn evict_idle(&self) {
        let mut sessions = self.sessions.lock().await;
        let now = Instant::now();
        let expired: Vec<String> = sessions
            .iter()
            .filter_map(|(id, slot)| match slot {
                Some(s) if now.duration_since(s.last_used) > SESSION_IDLE_TTL => {
                    Some(id.clone())
                }
                _ => None, // busy (None) or fresh sessions stay
            })
            .collect();
        for id in expired {
            if let Some(Some(mut s)) = sessions.remove(&id) {
                info!(session = %id, "evicting idle shell session");
                s.kill().await;
            }
        }
    }

    /// Execute `req.command` in the persistent session `req.session_id`,
    /// streaming output via `sender`. Caller guarantees session_id is Some.
    pub async fn execute(&self, req: ShellRequest, sender: SseSender) {
        let start = Instant::now();
        let sid = req
            .session_id
            .clone()
            .expect("route dispatches here only with session_id");

        // Claim the session (or spawn one). Every branch that yields a claimed
        // Session must leave the busy marker `sessions[sid] = None` in the map
        // BEFORE releasing the lock, so a concurrent execute() for the same sid
        // sees "busy" and cannot double-spawn (TOCTOU). We never hold a claimed
        // Session and an empty/absent map slot across the lock boundary.
        let mut session = {
            let mut sessions = self.sessions.lock().await;
            match sessions.get_mut(&sid) {
                Some(slot @ Some(_)) => {
                    let mut s = slot.take().expect("matched Some");
                    // `take()` already left None in the slot (the busy marker).
                    if s.alive() {
                        s
                    } else {
                        info!(session = %sid, "session process died; respawning");
                        match Session::spawn().await {
                            Ok(ns) => ns, // slot is still None — busy marker holds
                            Err(e) => {
                                sessions.remove(&sid);
                                sender
                                    .send_error(&format!("spawn session: {e}"), Some(-1))
                                    .await;
                                return;
                            }
                        }
                    }
                }
                Some(None) => {
                    sender
                        .send_error("session busy: a command is already running", Some(-1))
                        .await;
                    return;
                }
                None => {
                    if sessions.len() >= MAX_SESSIONS {
                        // Evict the oldest idle session to stay bounded.
                        let oldest = sessions
                            .iter()
                            .filter_map(|(id, slot)| {
                                slot.as_ref().map(|s| (id.clone(), s.last_used))
                            })
                            .min_by_key(|(_, t)| *t)
                            .map(|(id, _)| id);
                        // No let-chain here: the Docker builder pins rust:1.87
                        // and let-chains need 1.88+.
                        #[allow(clippy::collapsible_if)]
                        if let Some(oldest) = oldest {
                            if let Some(mut s) = sessions.remove(&oldest).flatten() {
                                warn!(evicted = %oldest, "session cap reached");
                                s.kill().await;
                            }
                        }
                    }
                    match Session::spawn().await {
                        Ok(s) => {
                            // Brand-new id: install the busy marker before
                            // releasing the lock.
                            sessions.insert(sid.clone(), None);
                            s
                        }
                        Err(e) => {
                            sender
                                .send_error(&format!("spawn session: {e}"), Some(-1))
                                .await;
                            return;
                        }
                    }
                }
            }
        };

        let outcome = run_command(&mut session, &req, &sender).await;
        let elapsed_ms = start.elapsed().as_millis() as u64;

        let mut sessions = self.sessions.lock().await;
        match outcome {
            Ok(exit_code) => {
                session.last_used = Instant::now();
                sessions.insert(sid, Some(session));
                sender.send_complete(exit_code, elapsed_ms).await;
            }
            Err(msg) => {
                // Timeout or IO failure: the session state is unknowable —
                // kill it; the next command transparently respawns.
                session.kill().await;
                sessions.remove(&sid);
                sender.send_error(&msg, Some(-1)).await;
                sender.send_complete(-1, elapsed_ms).await;
            }
        }
    }

    /// Kill all sessions (daemon shutdown).
    pub async fn shutdown(&self) {
        let mut sessions = self.sessions.lock().await;
        for (_, slot) in sessions.drain() {
            if let Some(mut s) = slot {
                s.kill().await;
            }
        }
    }
}

/// Write the command + sentinels, stream output until both sentinels.
/// Ok(exit_code) on completion; Err(message) on timeout/IO error (caller
/// kills the session).
async fn run_command(
    session: &mut Session,
    req: &ShellRequest,
    sender: &SseSender,
) -> std::result::Result<i32, String> {
    let nonce = NONCE.fetch_add(1, Ordering::Relaxed);
    let sentinel = format!("__IX_DONE_{}_{}__", session.pgid, nonce);

    let mut block = String::new();
    if let Some(cwd) = &req.cwd {
        // cd persists for the session lifetime — documented session semantics.
        // Best-effort: a failed `cd` writes its error to stderr (streamed to the
        // caller) but does NOT abort the command. `$?` reflects the user command,
        // not the cd, so the command runs in the previous directory — surfaced to
        // the caller only via the cd's stderr line.
        block.push_str(&format!("cd {}\n", shell_quote(cwd)));
    }
    block.push_str(&req.command);
    if !req.command.ends_with('\n') {
        block.push('\n');
    }
    block.push_str(&format!("printf '{sentinel} %d\\n' \"$?\"\n"));
    block.push_str(&format!("printf '{sentinel}\\n' >&2\n"));

    session
        .stdin
        .write_all(block.as_bytes())
        .await
        .map_err(|e| format!("write session stdin: {e}"))?;
    session
        .stdin
        .flush()
        .await
        .map_err(|e| format!("flush session stdin: {e}"))?;

    let timeout = Duration::from_secs(req.timeout.unwrap_or(DEFAULT_TIMEOUT_SECS));
    let stdout = &mut session.stdout;
    let stderr = &mut session.stderr;

    let stdout_read = async {
        let mut line = String::new();
        loop {
            line.clear();
            let n = stdout
                .read_line(&mut line)
                .await
                .map_err(|e| format!("read session stdout: {e}"))?;
            if n == 0 {
                return Err("session closed stdout".to_string());
            }
            let trimmed = line.trim_end_matches('\n');
            if let Some(idx) = trimmed.find(&sentinel) {
                // Output without trailing newline lands on the sentinel line.
                if idx > 0 {
                    sender.send_stdout(&trimmed[..idx]).await;
                }
                let code = trimmed[idx + sentinel.len()..]
                    .trim()
                    .parse::<i32>()
                    .unwrap_or(-1);
                return Ok(code);
            }
            sender.send_stdout(trimmed).await;
        }
    };

    let stderr_read = async {
        let mut line = String::new();
        loop {
            line.clear();
            let n = stderr
                .read_line(&mut line)
                .await
                .map_err(|e| format!("read session stderr: {e}"))?;
            if n == 0 {
                return Err("session closed stderr".to_string());
            }
            let trimmed = line.trim_end_matches('\n');
            if let Some(idx) = trimmed.find(&sentinel) {
                if idx > 0 {
                    sender.send_stderr(&trimmed[..idx]).await;
                }
                return Ok(());
            }
            sender.send_stderr(trimmed).await;
        }
    };

    match tokio::time::timeout(timeout, async { tokio::join!(stdout_read, stderr_read) }).await
    {
        Ok((Ok(code), Ok(()))) => Ok(code),
        Ok((Err(e), _)) | Ok((_, Err(e))) => Err(e),
        Err(_) => Err(format!(
            "command timed out after {}s",
            timeout.as_secs()
        )),
    }
}
