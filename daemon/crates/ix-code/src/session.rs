use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use bytes::Bytes;
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};
use zeromq::{SocketRecv, SocketSend};

use ix_core::sse::SseSender;
use ix_core::types::CodeRequest;
use ix_core::{Error, Result};

use crate::kernel::{frames_to_zmq, Kernel};
use crate::output::extract_best_output;
use crate::protocol::JupyterMessage;

/// Python warmup code executed silently on pre-warmed kernels.
/// Standard library imports are always available; scientific packages
/// are guarded by try/except for images without them.
const PYTHON_WARMUP_CODE: &str = r#"
import json, os, sys, re, pathlib, subprocess, shutil, tempfile
try:
    import numpy, pandas, matplotlib, matplotlib.pyplot
except ImportError:
    pass
"#;

/// Default pool size for Python kernels.
const PYTHON_POOL_SIZE: usize = 2;

/// Manages per-language kernel instances with a pre-warmed pool.
///
/// On construction, background tasks boot `PYTHON_POOL_SIZE` Python kernels
/// and run warmup imports on each.  When `execute()` needs a kernel and none
/// is active for the requested language, it grabs one from the pool (instant)
/// instead of booting on demand.  After a kernel is grabbed, a background
/// replenishment task boots a replacement.
pub struct KernelManager {
    /// Active kernels: language -> kernel (used for stateful sessions)
    active: Arc<Mutex<HashMap<String, Kernel>>>,
    /// Pool of pre-warmed, ready-to-use kernels per language
    pool: Arc<Mutex<HashMap<String, Vec<Kernel>>>>,
    /// Target pool sizes per language
    pool_sizes: HashMap<String, usize>,
}

impl KernelManager {
    pub fn new() -> Self {
        let mut pool_sizes = HashMap::new();
        pool_sizes.insert("python".to_string(), PYTHON_POOL_SIZE);

        let mgr = Self {
            active: Arc::new(Mutex::new(HashMap::new())),
            pool: Arc::new(Mutex::new(HashMap::new())),
            pool_sizes,
        };

        mgr.spawn_warmup();
        mgr
    }

    /// Spawn background tasks to pre-boot kernels for each configured language.
    fn spawn_warmup(&self) {
        for (lang, &count) in &self.pool_sizes {
            for i in 0..count {
                let pool = Arc::clone(&self.pool);
                let lang = lang.clone();
                tokio::spawn(async move {
                    debug!(language = %lang, index = i, "pre-warming kernel");
                    match boot_and_warmup(&lang).await {
                        Ok(kernel) => {
                            let mut pool_guard = pool.lock().await;
                            pool_guard.entry(lang.clone()).or_default().push(kernel);
                            info!(language = %lang, index = i, "kernel pre-warmed and added to pool");
                        }
                        Err(e) => {
                            warn!(language = %lang, index = i, error = %e, "failed to pre-warm kernel");
                        }
                    }
                });
            }
        }
    }

    /// Spawn a background task to replenish the pool for the given language.
    fn replenish(&self, language: &str) {
        let target = match self.pool_sizes.get(language) {
            Some(&n) if n > 0 => n,
            _ => return, // no pool configured for this language
        };
        let pool = Arc::clone(&self.pool);
        let lang = language.to_string();
        tokio::spawn(async move {
            // Check if replenishment is actually needed
            {
                let pool_guard = pool.lock().await;
                let current = pool_guard.get(&lang).map(|v| v.len()).unwrap_or(0);
                if current >= target {
                    debug!(language = %lang, current, target, "pool already at target, skipping replenish");
                    return;
                }
            }

            debug!(language = %lang, "replenishing pool");
            match boot_and_warmup(&lang).await {
                Ok(kernel) => {
                    let mut pool_guard = pool.lock().await;
                    pool_guard.entry(lang.clone()).or_default().push(kernel);
                    let new_size = pool_guard.get(&lang).map(|v| v.len()).unwrap_or(0);
                    info!(language = %lang, pool_size = new_size, "pool replenished");
                }
                Err(e) => {
                    warn!(language = %lang, error = %e, "failed to replenish pool");
                }
            }
        });
    }

    /// Execute code for the given request, streaming output via sender.
    pub async fn execute(&self, req: CodeRequest, sender: SseSender) {
        let start = Instant::now();

        let lang = normalize_language(&req.language);
        let timeout = req.timeout.map(Duration::from_secs);

        // 1. Try to use an existing active kernel for this language (stateful reuse)
        let mut active = self.active.lock().await;
        if !active.contains_key(&lang) {
            // 2. Try to grab a pre-warmed kernel from the pool
            let pooled = {
                let mut pool = self.pool.lock().await;
                pool.get_mut(&lang).and_then(|v| v.pop())
            };

            match pooled {
                Some(kernel) => {
                    info!(language = %lang, "grabbed pre-warmed kernel from pool");
                    active.insert(lang.clone(), kernel);
                    // Trigger background replenishment
                    self.replenish(&lang);
                }
                None => {
                    // 3. Fallback: boot a new kernel on demand
                    info!(language = %lang, "pool empty, starting kernel on demand");
                    match Kernel::start(&lang).await {
                        Ok(k) => {
                            active.insert(lang.clone(), k);
                        }
                        Err(e) => {
                            warn!(language = %lang, error = %e, "kernel start failed, falling back to process exec");
                            drop(active);
                            exec_code_via_process(&lang, &req.code, timeout, sender, start).await;
                            return;
                        }
                    }
                }
            }
        }

        let kernel = active.get_mut(&lang).unwrap();
        let session_id = kernel.session_id.clone();
        let key = kernel.connection.key.clone();

        // Build execute_request
        let msg = JupyterMessage::new(
            "execute_request",
            &session_id,
            serde_json::json!({
                "code": req.code,
                "silent": false,
                "store_history": true,
                "user_expressions": {},
                "allow_stdin": false,
                "stop_on_error": true,
            }),
        );
        let msg_id = msg.header.msg_id.clone();

        let zmq_msg = match frames_to_zmq(msg.serialize(&key)) {
            Ok(m) => m,
            Err(e) => {
                error!(error = %e, "failed to build ZMQ execute_request");
                sender
                    .send_error("internal error: failed to build message", Some(-1))
                    .await;
                sender
                    .send_complete(-1, start.elapsed().as_millis() as u64)
                    .await;
                return;
            }
        };

        if let Err(e) = kernel.shell_socket.send(zmq_msg).await {
            error!(error = %e, "failed to send execute_request");
            // Kernel is likely dead — remove it so next call grabs from pool / boots fresh
            let dead_lang = lang.clone();
            active.remove(&dead_lang);
            drop(active);
            sender
                .send_error(&format!("failed to send execute_request: {e}"), Some(-1))
                .await;
            sender
                .send_complete(-1, start.elapsed().as_millis() as u64)
                .await;
            return;
        }

        debug!(msg_id, "execute_request sent");

        // Drain IOPub events until we see idle status after execute_reply
        let result = drain_execution(
            kernel,
            &msg_id,
            &key,
            &sender,
            timeout,
            start,
        )
        .await;

        let elapsed_ms = start.elapsed().as_millis() as u64;
        match result {
            Ok(exit_code) => {
                sender.send_complete(exit_code, elapsed_ms).await;
            }
            Err(e) => {
                // On protocol/recv errors the kernel is likely dead — remove it
                warn!(language = %lang, error = %e, "execution failed, removing dead kernel");
                active.remove(&lang);
                sender.send_error(&e.to_string(), Some(-1)).await;
                sender.send_complete(-1, elapsed_ms).await;
            }
        }
    }

    /// Shut down all running kernels (both active and pooled)
    pub async fn shutdown(&self) {
        let mut active = self.active.lock().await;
        for (lang, kernel) in active.iter_mut() {
            info!(language = %lang, "shutting down active kernel");
            kernel.shutdown().await;
        }
        active.clear();

        let mut pool = self.pool.lock().await;
        for (lang, kernels) in pool.iter_mut() {
            for kernel in kernels.iter_mut() {
                info!(language = %lang, "shutting down pooled kernel");
                kernel.shutdown().await;
            }
        }
        pool.clear();
    }
}

/// Boot a kernel for the given language and run warmup code on it.
/// The warmup execution is silent — output is discarded entirely.
async fn boot_and_warmup(language: &str) -> Result<Kernel> {
    let mut kernel = Kernel::start(language).await?;

    let warmup_code = match language {
        "python" => Some(PYTHON_WARMUP_CODE),
        _ => None,
    };

    if let Some(code) = warmup_code {
        debug!(language, "running warmup imports");
        warmup_kernel(&mut kernel, code).await?;
        debug!(language, "warmup imports complete");
    }

    Ok(kernel)
}

/// Execute warmup code on a kernel, discarding all output.
/// Uses the same drain_execution loop but with no SSE sender side-effects.
async fn warmup_kernel(kernel: &mut Kernel, code: &str) -> Result<()> {
    let session_id = kernel.session_id.clone();
    let key = kernel.connection.key.clone();

    let msg = JupyterMessage::new(
        "execute_request",
        &session_id,
        serde_json::json!({
            "code": code,
            "silent": true,
            "store_history": false,
            "user_expressions": {},
            "allow_stdin": false,
            "stop_on_error": false,
        }),
    );
    let msg_id = msg.header.msg_id.clone();

    let zmq_msg = frames_to_zmq(msg.serialize(&key))?;
    kernel
        .shell_socket
        .send(zmq_msg)
        .await
        .map_err(|e| Error::Internal(format!("warmup send failed: {e}")))?;

    // Drain until execution completes — discard all messages
    drain_warmup(kernel, &msg_id, &key).await
}

/// Drain IOPub + shell messages for a warmup execution, discarding output.
/// Returns Ok(()) when the execute_reply + idle status are received.
async fn drain_warmup(kernel: &mut Kernel, msg_id: &str, key: &str) -> Result<()> {
    let mut shell_done = false;
    let mut saw_idle_after_exec = false;
    let timeout = Duration::from_secs(60); // generous timeout for warmup (importing packages)
    let deadline = tokio::time::Instant::now() + timeout;

    loop {
        if shell_done && saw_idle_after_exec {
            break;
        }

        let timeout_fut = tokio::time::sleep_until(deadline);

        if !shell_done {
            tokio::select! {
                _ = timeout_fut => {
                    warn!(msg_id, "warmup execution timed out");
                    return Err(Error::Timeout("warmup execution timed out".into()));
                }
                result = kernel.iopub_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("warmup iopub recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            if let Ok(msg) = JupyterMessage::deserialize(fv, key) {
                                if msg.header.msg_type == "status" {
                                    let state = msg.content.get("execution_state")
                                        .and_then(|v| v.as_str())
                                        .unwrap_or("");
                                    if state == "idle" && shell_done {
                                        saw_idle_after_exec = true;
                                    }
                                }
                                // All other messages (stream, error, etc.) are discarded
                            }
                        }
                    }
                }
                result = kernel.shell_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("warmup shell recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            if let Ok(reply) = JupyterMessage::deserialize(fv, key) {
                                if reply.header.msg_type == "execute_reply" {
                                    shell_done = true;
                                }
                            }
                        }
                    }
                }
            }
        } else {
            // Shell done, just drain iopub until idle
            tokio::select! {
                _ = timeout_fut => {
                    warn!(msg_id, "warmup timed out waiting for idle");
                    return Err(Error::Timeout("warmup timed out waiting for idle".into()));
                }
                result = kernel.iopub_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("warmup iopub recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            if let Ok(msg) = JupyterMessage::deserialize(fv, key) {
                                if msg.header.msg_type == "status" {
                                    let state = msg.content.get("execution_state")
                                        .and_then(|v| v.as_str())
                                        .unwrap_or("");
                                    if state == "idle" {
                                        saw_idle_after_exec = true;
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    Ok(())
}

/// Drain IOPub messages and the shell reply for one execution cycle.
/// Returns Ok(exit_code) when done, or Err on protocol error.
async fn drain_execution(
    kernel: &mut Kernel,
    msg_id: &str,
    key: &str,
    sender: &SseSender,
    timeout: Option<Duration>,
    _start: Instant,
) -> Result<i32> {
    let mut shell_done = false;
    let mut saw_idle_after_exec = false;
    let mut exit_code = 0i32;

    // Build the deadline as an Option<Instant> rather than a pinned future,
    // so we can reuse it across select! iterations.
    let deadline: Option<tokio::time::Instant> =
        timeout.map(|d| tokio::time::Instant::now() + d);

    loop {
        if shell_done && saw_idle_after_exec {
            break;
        }

        // Build a sleep future that either fires at the deadline or never resolves
        let timeout_fut = async {
            match deadline {
                Some(at) => tokio::time::sleep_until(at).await,
                None => std::future::pending::<()>().await,
            }
        };

        if !shell_done {
            tokio::select! {
                _ = timeout_fut => {
                    warn!(msg_id, "execution timed out");
                    return Err(Error::Timeout("code execution timed out".into()));
                }
                result = kernel.iopub_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("iopub recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            match JupyterMessage::deserialize(fv, key) {
                                Err(e) => warn!(error = %e, "failed to deserialize iopub message"),
                                Ok(msg) => handle_iopub_message(&msg, msg_id, sender, &mut saw_idle_after_exec, shell_done).await,
                            }
                        }
                    }
                }
                result = kernel.shell_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("shell recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            match JupyterMessage::deserialize(fv, key) {
                                Err(e) => warn!(error = %e, "failed to deserialize shell message"),
                                Ok(reply) => {
                                    if reply.header.msg_type == "execute_reply" {
                                        let status = reply.content.get("status").and_then(|v| v.as_str()).unwrap_or("error");
                                        debug!(msg_id, status, "got execute_reply");
                                        if status == "error" { exit_code = 1; }
                                        shell_done = true;
                                    }
                                }
                            }
                        }
                    }
                }
            }
        } else {
            // Shell done, just drain iopub until idle
            tokio::select! {
                _ = timeout_fut => {
                    warn!(msg_id, "timed out waiting for idle status");
                    return Err(Error::Timeout("timed out waiting for kernel idle".into()));
                }
                result = kernel.iopub_socket.recv() => {
                    match result {
                        Err(e) => return Err(Error::Internal(format!("iopub recv error: {e}"))),
                        Ok(frames) => {
                            let fv: Vec<Bytes> = frames.into_vec();
                            match JupyterMessage::deserialize(fv, key) {
                                Err(e) => warn!(error = %e, "failed to deserialize iopub message"),
                                Ok(msg) => handle_iopub_message(&msg, msg_id, sender, &mut saw_idle_after_exec, shell_done).await,
                            }
                        }
                    }
                }
            }
        }
    }

    Ok(exit_code)
}

/// Process a single IOPub message and send appropriate SSE events
async fn handle_iopub_message(
    msg: &JupyterMessage,
    _parent_msg_id: &str,
    sender: &SseSender,
    saw_idle_after_exec: &mut bool,
    shell_done: bool,
) {
    match msg.header.msg_type.as_str() {
        "stream" => {
            let name = msg.content.get("name").and_then(|v| v.as_str()).unwrap_or("stdout");
            let text = msg.content.get("text").and_then(|v| v.as_str()).unwrap_or("");
            if name == "stderr" {
                sender.send_stderr(text).await;
            } else {
                sender.send_stdout(text).await;
            }
        }

        "display_data" | "execute_result" => {
            if let Some(data) = msg.content.get("data") {
                let (result_type, content) = extract_best_output(data);
                sender.send_result(&result_type, &content).await;
            }
        }

        "error" => {
            let ename = msg.content.get("ename").and_then(|v| v.as_str()).unwrap_or("Error");
            let evalue = msg.content.get("evalue").and_then(|v| v.as_str()).unwrap_or("");
            let traceback = msg
                .content
                .get("traceback")
                .and_then(|v| v.as_array())
                .map(|tb| {
                    tb.iter()
                        .filter_map(|l| l.as_str())
                        .collect::<Vec<_>>()
                        .join("\n")
                })
                .unwrap_or_default();

            let error_text = if traceback.is_empty() {
                format!("{ename}: {evalue}")
            } else {
                format!("{ename}: {evalue}\n{traceback}")
            };
            sender.send_error(&error_text, Some(1)).await;
        }

        "status" => {
            let state = msg
                .content
                .get("execution_state")
                .and_then(|v| v.as_str())
                .unwrap_or("");
            if state == "idle" && shell_done {
                *saw_idle_after_exec = true;
            }
        }

        other => {
            debug!(msg_type = %other, "ignoring iopub message type");
        }
    }
}

/// Fallback: execute code by writing to a temp file and running it via a subprocess
pub async fn exec_code_via_process(
    language: &str,
    code: &str,
    timeout: Option<Duration>,
    sender: SseSender,
    start: Instant,
) {
    use tokio::io::{AsyncBufReadExt, BufReader};

    let (ext, interpreter) = match language {
        "python" | "python3" | "py" => ("py", vec!["python3"]),
        "javascript" | "js" | "node" => ("js", vec!["node"]),
        "bash" | "sh" | "shell" => ("sh", vec!["bash"]),
        other => {
            sender
                .send_error(&format!("unsupported language: {other}"), Some(-1))
                .await;
            sender
                .send_complete(-1, start.elapsed().as_millis() as u64)
                .await;
            return;
        }
    };

    // Write code to a temp file
    let tmp_path = format!("/tmp/ix-code-{}.{}", uuid::Uuid::new_v4(), ext);
    if let Err(e) = tokio::fs::write(&tmp_path, code).await {
        sender
            .send_error(&format!("failed to write temp file: {e}"), Some(-1))
            .await;
        sender
            .send_complete(-1, start.elapsed().as_millis() as u64)
            .await;
        return;
    }

    let mut cmd = tokio::process::Command::new(interpreter[0]);
    for arg in &interpreter[1..] {
        cmd.arg(arg);
    }
    cmd.arg(&tmp_path);
    cmd.stdout(std::process::Stdio::piped());
    cmd.stderr(std::process::Stdio::piped());

    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            let _ = tokio::fs::remove_file(&tmp_path).await;
            sender
                .send_error(&format!("failed to spawn: {e}"), Some(-1))
                .await;
            sender
                .send_complete(-1, start.elapsed().as_millis() as u64)
                .await;
            return;
        }
    };

    let stdout = child.stdout.take().expect("stdout piped");
    let stderr = child.stderr.take().expect("stderr piped");
    let mut stdout_lines = BufReader::new(stdout).lines();
    let mut stderr_lines = BufReader::new(stderr).lines();

    let deadline: Option<tokio::time::Instant> =
        timeout.map(|d| tokio::time::Instant::now() + d);

    let stream_result: Result<()> = async {
        loop {
            let timeout_fut = async {
                match deadline {
                    Some(at) => tokio::time::sleep_until(at).await,
                    None => std::future::pending::<()>().await,
                }
            };

            tokio::select! {
                _ = timeout_fut => {
                    let _ = child.kill().await;
                    return Err(Error::Timeout("process timed out".into()));
                }
                line = stdout_lines.next_line() => {
                    match line {
                        Ok(Some(l)) => { sender.send_stdout(&l).await; }
                        Ok(None) => {
                            while let Ok(Some(l)) = stderr_lines.next_line().await {
                                sender.send_stderr(&l).await;
                            }
                            break;
                        }
                        Err(_) => break,
                    }
                }
                line = stderr_lines.next_line() => {
                    match line {
                        Ok(Some(l)) => { sender.send_stderr(&l).await; }
                        Ok(None) => {
                            while let Ok(Some(l)) = stdout_lines.next_line().await {
                                sender.send_stdout(&l).await;
                            }
                            break;
                        }
                        Err(_) => break,
                    }
                }
            }
        }
        Ok(())
    }
    .await;

    let _ = tokio::fs::remove_file(&tmp_path).await;

    let elapsed_ms = start.elapsed().as_millis() as u64;
    match stream_result {
        Err(e) => {
            sender.send_error(&e.to_string(), Some(-1)).await;
            sender.send_complete(-1, elapsed_ms).await;
        }
        Ok(()) => {
            let exit_code = child
                .wait()
                .await
                .map(|s| s.code().unwrap_or(-1))
                .unwrap_or(-1);
            sender.send_complete(exit_code, elapsed_ms).await;
        }
    }
}

fn normalize_language(lang: &str) -> String {
    match lang.to_lowercase().as_str() {
        "py" | "python3" => "python".to_string(),
        "js" | "node" => "javascript".to_string(),
        "sh" | "shell" => "bash".to_string(),
        other => other.to_string(),
    }
}
