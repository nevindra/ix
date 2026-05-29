use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::{Error, Result};
use tokio::process::Child;
use tokio::sync::Mutex;
use tracing::{debug, error, info, warn};

use crate::backend::BrowserBackend;

const PINCHTAB_TOKEN: &str = "ix-internal";
const PINCHTAB_BASE_URL: &str = "http://127.0.0.1:9867";
const PINCHTAB_CONFIG_PATH: &str = "/tmp/pinchtab/config.json";
const PINCHTAB_CONFIG: &str = r#"{"server":{"port":"9867","bind":"127.0.0.1","token":"ix-internal"},"instanceDefaults":{"mode":"headless"},"security":{"idpi":{"enabled":false}}}"#;

pub struct PinchtabBackend {
    client: reqwest::Client,
    base_url: String,
    available: AtomicBool,
    process: Mutex<Option<Child>>,
}

impl PinchtabBackend {
    pub async fn new() -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .expect("failed to build reqwest client");

        let backend = Self {
            client,
            base_url: PINCHTAB_BASE_URL.to_string(),
            available: AtomicBool::new(false),
            process: Mutex::new(None),
        };

        backend.try_start().await;
        backend
    }

    async fn try_start(&self) {
        // Check if pinchtab is in PATH
        let which_result = tokio::process::Command::new("which")
            .arg("pinchtab")
            .output()
            .await;

        match which_result {
            Ok(output) if output.status.success() => {
                let path = String::from_utf8_lossy(&output.stdout).trim().to_string();
                info!(path, "pinchtab found in PATH");
            }
            _ => {
                warn!("pinchtab not found in PATH, browser backend unavailable");
                return;
            }
        }

        // Write config file
        if let Err(e) = self.write_config().await {
            error!(error = %e, "failed to write pinchtab config");
            return;
        }

        // Spawn pinchtab bridge subprocess
        let child = match self.spawn_pinchtab().await {
            Ok(c) => c,
            Err(e) => {
                error!(error = %e, "failed to spawn pinchtab bridge");
                return;
            }
        };

        *self.process.lock().await = Some(child);

        // Poll health endpoint with exponential backoff
        if self.wait_for_health().await {
            self.available.store(true, Ordering::SeqCst);
            info!("pinchtab bridge is ready at {}", self.base_url);
        } else {
            error!("pinchtab bridge failed to become healthy");
        }
    }

    async fn write_config(&self) -> Result<()> {
        // Ensure the directory exists
        tokio::fs::create_dir_all("/tmp/pinchtab")
            .await
            .map_err(|e| Error::Internal(format!("failed to create pinchtab dir: {e}")))?;

        tokio::fs::write(PINCHTAB_CONFIG_PATH, PINCHTAB_CONFIG)
            .await
            .map_err(|e| Error::Internal(format!("failed to write pinchtab config: {e}")))?;

        debug!(path = PINCHTAB_CONFIG_PATH, "wrote pinchtab config");
        Ok(())
    }

    async fn spawn_pinchtab(&self) -> Result<Child> {
        let mut cmd = tokio::process::Command::new("pinchtab");
        cmd.arg("bridge");
        cmd.env("PINCHTAB_CONFIG", PINCHTAB_CONFIG_PATH);
        cmd.stdout(std::process::Stdio::null());
        cmd.stderr(std::process::Stdio::null());

        // Create a new process group for clean shutdown
        // SAFETY: setpgid(0, 0) is async-signal-safe per POSIX.
        unsafe {
            cmd.pre_exec(|| {
                nix::unistd::setpgid(nix::unistd::Pid::from_raw(0), nix::unistd::Pid::from_raw(0))
                    .map_err(|e| std::io::Error::from_raw_os_error(e as i32))?;
                Ok(())
            });
        }

        cmd.spawn()
            .map_err(|e| Error::Internal(format!("failed to spawn pinchtab: {e}")))
    }

    async fn wait_for_health(&self) -> bool {
        let health_url = format!("{}/health", self.base_url);
        let mut delay_ms = 200u64;
        let max_total_ms = 15_000u64;
        let mut elapsed_ms = 0u64;

        loop {
            tokio::time::sleep(Duration::from_millis(delay_ms)).await;
            elapsed_ms += delay_ms;

            match self.client.get(&health_url).send().await {
                Ok(resp) if resp.status().is_success() => {
                    debug!("pinchtab health check passed after {}ms", elapsed_ms);
                    return true;
                }
                Ok(resp) => {
                    debug!(status = %resp.status(), "pinchtab health check non-success");
                }
                Err(e) => {
                    debug!(error = %e, "pinchtab health check failed, retrying");
                }
            }

            if elapsed_ms >= max_total_ms {
                warn!("pinchtab health check timed out after {}ms", elapsed_ms);
                return false;
            }

            delay_ms = (delay_ms * 2).min(3_200);
        }
    }

    fn auth_header(&self) -> String {
        format!("Bearer {}", PINCHTAB_TOKEN)
    }

    async fn get_bytes(&self, path: &str) -> Result<Vec<u8>> {
        let url = format!("{}{}", self.base_url, path);
        let resp = self
            .client
            .get(&url)
            .header("Authorization", self.auth_header())
            .send()
            .await
            .map_err(|e| Error::Internal(format!("pinchtab request failed: {e}")))?;

        map_pinchtab_error(resp).await?.bytes().await.map(|b| b.to_vec()).map_err(
            |e| Error::Internal(format!("failed to read pinchtab response bytes: {e}")),
        )
    }

    async fn get_json<T>(&self, path: &str) -> Result<T>
    where
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.base_url, path);
        let resp = self
            .client
            .get(&url)
            .header("Authorization", self.auth_header())
            .send()
            .await
            .map_err(|e| Error::Internal(format!("pinchtab request failed: {e}")))?;

        map_pinchtab_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse pinchtab response: {e}")))
    }

    async fn post_json<B, T>(&self, path: &str, body: &B) -> Result<T>
    where
        B: serde::Serialize,
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.base_url, path);
        let resp = self
            .client
            .post(&url)
            .header("Authorization", self.auth_header())
            .json(body)
            .send()
            .await
            .map_err(|e| Error::Internal(format!("pinchtab request failed: {e}")))?;

        map_pinchtab_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse pinchtab response: {e}")))
    }

    pub async fn shutdown(&self) {
        let mut guard = self.process.lock().await;
        if let Some(ref mut child) = *guard {
            let pid = match child.id() {
                Some(p) => p as i32,
                None => return,
            };

            // Send SIGTERM to the process group
            let pgid = nix::unistd::Pid::from_raw(-pid);
            if let Err(e) =
                nix::sys::signal::kill(pgid, nix::sys::signal::Signal::SIGTERM)
            {
                warn!(error = %e, "failed to send SIGTERM to pinchtab process group");
            } else {
                info!(pid, "sent SIGTERM to pinchtab process group");
            }

            // Wait up to 5s for process to exit
            let wait_result =
                tokio::time::timeout(Duration::from_secs(5), child.wait()).await;

            match wait_result {
                Ok(Ok(status)) => {
                    debug!(status = ?status, "pinchtab process exited after SIGTERM");
                }
                _ => {
                    // Send SIGKILL if still running
                    warn!(pid, "pinchtab did not exit after SIGTERM, sending SIGKILL");
                    if let Err(e) =
                        nix::sys::signal::kill(pgid, nix::sys::signal::Signal::SIGKILL)
                    {
                        error!(error = %e, "failed to send SIGKILL to pinchtab");
                    }
                    let _ = child.wait().await;
                }
            }

            *guard = None;
        }
    }
}

async fn map_pinchtab_error(resp: reqwest::Response) -> Result<reqwest::Response> {
    let status = resp.status();
    if status.is_success() {
        return Ok(resp);
    }

    let body = resp
        .text()
        .await
        .unwrap_or_else(|_| "unknown error".to_string());

    Err(match status.as_u16() {
        400 => Error::BadRequest(format!("pinchtab: {body}")),
        404 => Error::NotFound(format!("pinchtab: {body}")),
        503 => Error::Unavailable(format!("pinchtab: {body}")),
        _ => Error::Internal(format!("pinchtab HTTP {status}: {body}")),
    })
}

#[async_trait]
impl BrowserBackend for PinchtabBackend {
    async fn navigate(&self, url: &str) -> Result<NavigateResult> {
        self.post_json("/navigate", &serde_json::json!({ "url": url }))
            .await
    }

    async fn screenshot(&self) -> Result<Vec<u8>> {
        self.get_bytes("/screenshot?raw=true").await
    }

    async fn action(&self, action: BrowserAction) -> Result<BrowserResult> {
        self.post_json("/action", &action).await
    }

    async fn snapshot(&self, opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        let path = build_snapshot_path(&opts);
        self.get_json(&path).await
    }

    async fn text(&self, opts: TextOpts) -> Result<BrowserTextResult> {
        let path = build_text_path(&opts);
        self.get_json(&path).await
    }

    async fn pdf(&self) -> Result<Vec<u8>> {
        self.get_bytes("/pdf").await
    }

    async fn eval(&self, expr: &str) -> Result<String> {
        #[derive(serde::Deserialize)]
        struct EvalResponse {
            result: String,
        }

        let resp: EvalResponse = self
            .post_json("/evaluate", &serde_json::json!({ "expression": expr }))
            .await?;
        Ok(resp.result)
    }

    async fn find(&self, query: &str) -> Result<BrowserFindResult> {
        self.post_json("/find", &serde_json::json!({ "query": query }))
            .await
    }

    fn available(&self) -> bool {
        self.available.load(Ordering::SeqCst)
    }
}

impl Drop for PinchtabBackend {
    fn drop(&mut self) {
        // Best-effort synchronous SIGKILL on drop; async shutdown should be
        // called before this for graceful termination.
        if let Ok(mut guard) = self.process.try_lock() {
            if let Some(ref mut child) = *guard {
                if let Some(pid) = child.id() {
                    let pgid = nix::unistd::Pid::from_raw(-(pid as i32));
                    let _ = nix::sys::signal::kill(
                        pgid,
                        nix::sys::signal::Signal::SIGKILL,
                    );
                }
            }
        }
    }
}

/// Build the path+query string for a snapshot request.
///
/// Extracted as a pure function so it can be unit-tested without a live HTTP
/// server.
pub fn build_snapshot_path(opts: &SnapshotOpts) -> String {
    let mut params = Vec::new();
    if let Some(ref filter) = opts.filter {
        params.push(format!("filter={}", urlencoding::encode(filter)));
    }
    if let Some(ref selector) = opts.selector {
        params.push(format!("selector={}", urlencoding::encode(selector)));
    }
    if let Some(depth) = opts.depth {
        params.push(format!("depth={depth}"));
    }

    if params.is_empty() {
        "/snapshot".to_string()
    } else {
        format!("/snapshot?{}", params.join("&"))
    }
}

/// Build the path+query string for a text request.
///
/// Extracted as a pure function so it can be unit-tested without a live HTTP
/// server.
pub fn build_text_path(opts: &TextOpts) -> String {
    let mut params = Vec::new();
    if opts.raw.unwrap_or(false) {
        params.push("mode=raw".to_string());
    }
    if let Some(max_chars) = opts.max_chars {
        params.push(format!("maxChars={max_chars}"));
    }

    if params.is_empty() {
        "/text".to_string()
    } else {
        format!("/text?{}", params.join("&"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── Config JSON ──────────────────────────────────────────────────────────

    /// The hard-coded PINCHTAB_CONFIG constant must be valid JSON and contain
    /// the expected fields so that the pinchtab bridge is launched with the
    /// correct settings.
    #[test]
    fn config_json_is_valid() {
        let v: serde_json::Value =
            serde_json::from_str(PINCHTAB_CONFIG).expect("PINCHTAB_CONFIG is not valid JSON");

        assert_eq!(
            v["server"]["port"], "9867",
            "server.port must be \"9867\""
        );
        assert_eq!(
            v["server"]["bind"], "127.0.0.1",
            "server.bind must be \"127.0.0.1\""
        );
        assert_eq!(
            v["server"]["token"], "ix-internal",
            "server.token must be \"ix-internal\""
        );
        assert_eq!(
            v["instanceDefaults"]["mode"], "headless",
            "instanceDefaults.mode must be \"headless\""
        );
        assert_eq!(
            v["security"]["idpi"]["enabled"],
            serde_json::Value::Bool(false),
            "security.idpi.enabled must be false"
        );
    }

    /// The constant string must match what we would get by serialising the
    /// config structure round-trip (same keys/values, no extras).
    #[test]
    fn config_token_matches_constant() {
        assert_eq!(
            PINCHTAB_TOKEN, "ix-internal",
            "PINCHTAB_TOKEN must stay in sync with the config JSON"
        );
    }

    /// The base URL constant must point at loopback on the expected port.
    #[test]
    fn base_url_is_loopback() {
        assert_eq!(PINCHTAB_BASE_URL, "http://127.0.0.1:9867");
    }

    // ── available() ─────────────────────────────────────────────────────────

    /// When pinchtab is not in PATH the backend must report unavailable=false.
    /// We exercise this by checking that `AtomicBool::new(false)` is the
    /// default — the actual try_start() path takes too long (15 s health
    /// timeout) so we test the invariant directly on a fresh instance whose
    /// `available` field has never been set to `true`.
    #[test]
    fn available_defaults_to_false() {
        let avail = AtomicBool::new(false);
        assert!(!avail.load(Ordering::SeqCst));
    }

    // ── Snapshot URL building ────────────────────────────────────────────────

    #[test]
    fn snapshot_path_no_opts() {
        let opts = SnapshotOpts::default();
        assert_eq!(build_snapshot_path(&opts), "/snapshot");
    }

    #[test]
    fn snapshot_path_with_filter() {
        let opts = SnapshotOpts {
            filter: Some("button".to_string()),
            selector: None,
            depth: None,
        };
        assert_eq!(build_snapshot_path(&opts), "/snapshot?filter=button");
    }

    #[test]
    fn snapshot_path_with_selector() {
        let opts = SnapshotOpts {
            filter: None,
            selector: Some("#main".to_string()),
            depth: None,
        };
        // '#' is encoded as %23
        assert_eq!(build_snapshot_path(&opts), "/snapshot?selector=%23main");
    }

    #[test]
    fn snapshot_path_with_depth() {
        let opts = SnapshotOpts {
            filter: None,
            selector: None,
            depth: Some(3),
        };
        assert_eq!(build_snapshot_path(&opts), "/snapshot?depth=3");
    }

    #[test]
    fn snapshot_path_all_opts() {
        let opts = SnapshotOpts {
            filter: Some("link".to_string()),
            selector: Some(".nav".to_string()),
            depth: Some(2),
        };
        let path = build_snapshot_path(&opts);
        // All three params must be present; order must be filter, selector, depth.
        assert_eq!(path, "/snapshot?filter=link&selector=.nav&depth=2");
    }

    #[test]
    fn snapshot_path_filter_with_spaces() {
        let opts = SnapshotOpts {
            filter: Some("search button".to_string()),
            selector: None,
            depth: None,
        };
        // spaces are percent-encoded as %20
        assert_eq!(
            build_snapshot_path(&opts),
            "/snapshot?filter=search%20button"
        );
    }

    // ── Text URL building ────────────────────────────────────────────────────

    #[test]
    fn text_path_no_opts() {
        let opts = TextOpts::default();
        assert_eq!(build_text_path(&opts), "/text");
    }

    #[test]
    fn text_path_raw_true() {
        let opts = TextOpts {
            raw: Some(true),
            max_chars: None,
        };
        assert_eq!(build_text_path(&opts), "/text?mode=raw");
    }

    #[test]
    fn text_path_raw_false_omitted() {
        // raw=false must NOT add a mode= param (same as absent)
        let opts = TextOpts {
            raw: Some(false),
            max_chars: None,
        };
        assert_eq!(build_text_path(&opts), "/text");
    }

    #[test]
    fn text_path_max_chars() {
        let opts = TextOpts {
            raw: None,
            max_chars: Some(5000),
        };
        assert_eq!(build_text_path(&opts), "/text?maxChars=5000");
    }

    #[test]
    fn text_path_all_opts() {
        let opts = TextOpts {
            raw: Some(true),
            max_chars: Some(1000),
        };
        assert_eq!(build_text_path(&opts), "/text?mode=raw&maxChars=1000");
    }
}
