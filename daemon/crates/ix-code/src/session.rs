use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};

use ix_core::sse::SseSender;
use ix_core::types::CodeRequest;

use crate::kernel::{normalize_language, Kernel};

/// Default pool size for Python kernels.
const PYTHON_POOL_SIZE: usize = 2;

/// Manages per-language kernel instances with a pre-warmed pool.
///
/// On construction, background tasks boot `PYTHON_POOL_SIZE` Python REPL
/// processes.  When `execute()` needs a kernel and none is active for the
/// requested language, it grabs one from the pool (instant) instead of booting
/// on demand.  After a kernel is grabbed, a background replenishment task
/// boots a replacement.
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

    /// Spawn background tasks to pre-boot REPL kernels for each configured language.
    fn spawn_warmup(&self) {
        for (lang, &count) in &self.pool_sizes {
            for i in 0..count {
                let pool = Arc::clone(&self.pool);
                let lang = lang.clone();
                tokio::spawn(async move {
                    debug!(language = %lang, index = i, "pre-warming REPL kernel");
                    match Kernel::start(&lang).await {
                        Ok(kernel) => {
                            let mut pool_guard = pool.lock().await;
                            pool_guard.entry(lang.clone()).or_default().push(kernel);
                            info!(language = %lang, index = i, "REPL kernel pre-warmed");
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
            match Kernel::start(&lang).await {
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
        let lang = normalize_language(&req.language).to_string();
        let timeout = req.timeout.map(Duration::from_secs);

        // 1. Get or create kernel
        let mut active = self.active.lock().await;
        if !active.contains_key(&lang) {
            let pooled = {
                let mut pool = self.pool.lock().await;
                pool.get_mut(&lang).and_then(|v| v.pop())
            };
            match pooled {
                Some(kernel) => {
                    info!(language = %lang, "grabbed pre-warmed kernel from pool");
                    active.insert(lang.clone(), kernel);
                    self.replenish(&lang);
                }
                None => {
                    info!(language = %lang, "no pooled kernel, booting on demand");
                    match Kernel::start(&lang).await {
                        Ok(kernel) => {
                            active.insert(lang.clone(), kernel);
                        }
                        Err(e) => {
                            error!(language = %lang, error = %e, "failed to start kernel");
                            sender
                                .send_error(
                                    &format!("failed to start {lang} kernel: {e}"),
                                    Some(1),
                                )
                                .await;
                            return;
                        }
                    }
                }
            }
        }

        let kernel = active.get_mut(&lang).unwrap();

        // 2. Check kernel is alive
        if !kernel.is_alive() {
            warn!(language = %lang, "kernel process dead, removing");
            active.remove(&lang);
            drop(active);
            // Retry once
            let mut kernel = match Kernel::start(&lang).await {
                Ok(k) => k,
                Err(e) => {
                    sender
                        .send_error(&format!("kernel restart failed: {e}"), Some(1))
                        .await;
                    return;
                }
            };
            let result = kernel.execute(&req.code, timeout).await;
            let elapsed = start.elapsed();
            match result {
                Ok((stdout, stderr)) => {
                    if !stderr.is_empty() {
                        sender.send_stderr(&stderr).await;
                    }
                    if !stdout.is_empty() {
                        sender.send_stdout(&stdout).await;
                    }
                    sender
                        .send_complete(0, elapsed.as_millis() as u64)
                        .await;
                }
                Err(e) => {
                    sender.send_error(&format!("{e}"), Some(1)).await;
                }
            }
            self.active.lock().await.insert(lang, kernel);
            return;
        }

        // 3. Execute code
        let result = kernel.execute(&req.code, timeout).await;
        let elapsed = start.elapsed();

        match result {
            Ok((stdout, stderr)) => {
                if !stderr.is_empty() {
                    sender.send_stderr(&stderr).await;
                }
                if !stdout.is_empty() {
                    sender.send_stdout(&stdout).await;
                }
                sender
                    .send_complete(0, elapsed.as_millis() as u64)
                    .await;
            }
            Err(e) => {
                // Kernel might be dead — remove it so next call creates a fresh one
                warn!(language = %lang, error = %e, "execution failed, removing kernel");
                active.remove(&lang);
                sender.send_error(&format!("{e}"), Some(1)).await;
            }
        }
    }

    /// Shut down all running kernels (both active and pooled)
    pub async fn shutdown(&self) {
        let mut active = self.active.lock().await;
        active.clear(); // Drop kills processes via Kernel::drop()
        let mut pool = self.pool.lock().await;
        pool.clear();
    }
}
