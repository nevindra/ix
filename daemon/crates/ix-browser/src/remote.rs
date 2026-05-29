use std::time::Duration;

use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    EgressPolicy, NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::{Error, Result};

use crate::backend::BrowserBackend;
use crate::pinchtab::{build_snapshot_path, build_text_path};

/// Browser backend that proxies every call to a shared Browser Gateway over
/// HTTP. Selected when `IX_BROWSER_MODE=remote=<url>`.
pub struct RemoteSharedBrowserBackend {
    client: reqwest::Client,
    gateway_url: String,
    chat_id: String,
    /// Pre-serialised egress policy, sent on every request so the gateway can
    /// enforce it before forwarding to pinchtab.
    egress_policy_header: String,
    auth_token: Option<String>,
}

impl RemoteSharedBrowserBackend {
    pub fn new(
        gateway_url: String,
        chat_id: String,
        egress: &EgressPolicy,
        auth_token: Option<String>,
    ) -> Self {
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .expect("failed to build reqwest client");
        let egress_policy_header =
            serde_json::to_string(egress).unwrap_or_else(|_| "{}".to_string());
        Self {
            client,
            gateway_url: gateway_url.trim_end_matches('/').to_string(),
            chat_id,
            egress_policy_header,
            auth_token,
        }
    }

    fn apply_headers(&self, rb: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        let mut rb = rb
            .header("X-IX-Chat-Id", &self.chat_id)
            .header("X-IX-Egress-Policy", &self.egress_policy_header);
        if let Some(ref tok) = self.auth_token {
            rb = rb.header("Authorization", format!("Bearer {tok}"));
        }
        rb
    }

    async fn post_json<B, T>(&self, path: &str, body: &B) -> Result<T>
    where
        B: serde::Serialize,
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.post(&url).json(body))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse gateway response: {e}")))
    }

    async fn get_json<T>(&self, path: &str) -> Result<T>
    where
        T: serde::de::DeserializeOwned,
    {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.get(&url))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .json::<T>()
            .await
            .map_err(|e| Error::Internal(format!("failed to parse gateway response: {e}")))
    }

    async fn get_bytes(&self, path: &str) -> Result<Vec<u8>> {
        let url = format!("{}{}", self.gateway_url, path);
        let resp = self
            .apply_headers(self.client.get(&url))
            .send()
            .await
            .map_err(|e| Error::Internal(format!("gateway request failed: {e}")))?;
        map_gateway_error(resp)
            .await?
            .bytes()
            .await
            .map(|b| b.to_vec())
            .map_err(|e| Error::Internal(format!("failed to read gateway bytes: {e}")))
    }
}

/// Map the gateway's HTTP status to the same `ix_core::Error` variants the
/// route layer already handles, so error mapping is identical to in-VM pinchtab.
async fn map_gateway_error(resp: reqwest::Response) -> Result<reqwest::Response> {
    let status = resp.status();
    if status.is_success() {
        return Ok(resp);
    }
    let body = resp
        .text()
        .await
        .unwrap_or_else(|_| "unknown error".to_string());
    Err(match status.as_u16() {
        400 => Error::BadRequest(format!("gateway: {body}")),
        403 => Error::Forbidden(format!("gateway: {body}")),
        404 => Error::NotFound(format!("gateway: {body}")),
        503 => Error::Unavailable(format!("gateway: {body}")),
        _ => Error::Internal(format!("gateway HTTP {status}: {body}")),
    })
}

#[async_trait]
impl BrowserBackend for RemoteSharedBrowserBackend {
    async fn navigate(&self, url: &str) -> Result<NavigateResult> {
        self.post_json("/v1/browser/navigate", &serde_json::json!({ "url": url }))
            .await
    }

    async fn screenshot(&self) -> Result<Vec<u8>> {
        self.get_bytes("/v1/browser/screenshot?raw=true").await
    }

    async fn action(&self, action: BrowserAction) -> Result<BrowserResult> {
        self.post_json("/v1/browser/action", &action).await
    }

    async fn snapshot(&self, opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        let path = format!("/v1/browser{}", build_snapshot_path(&opts));
        self.get_json(&path).await
    }

    async fn text(&self, opts: TextOpts) -> Result<BrowserTextResult> {
        let path = format!("/v1/browser{}", build_text_path(&opts));
        self.get_json(&path).await
    }

    async fn pdf(&self) -> Result<Vec<u8>> {
        self.get_bytes("/v1/browser/pdf").await
    }

    async fn eval(&self, expr: &str) -> Result<String> {
        #[derive(serde::Deserialize)]
        struct EvalResponse {
            result: String,
        }
        let resp: EvalResponse = self
            .post_json("/v1/browser/evaluate", &serde_json::json!({ "expression": expr }))
            .await?;
        Ok(resp.result)
    }

    async fn find(&self, query: &str) -> Result<BrowserFindResult> {
        self.post_json("/v1/browser/find", &serde_json::json!({ "query": query }))
            .await
    }

    fn available(&self) -> bool {
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::{EgressPolicy, PolicyMode};
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn policy() -> EgressPolicy {
        EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["example.com".to_string()],
        }
    }

    #[tokio::test]
    async fn navigate_sends_headers_and_parses_result() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            .and(header("X-IX-Chat-Id", "chat-1"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "url": "https://example.com",
                "title": "Example"
            })))
            .mount(&server)
            .await;

        let backend = RemoteSharedBrowserBackend::new(
            server.uri(),
            "chat-1".to_string(),
            &policy(),
            None,
        );
        let result = backend.navigate("https://example.com").await.unwrap();
        assert_eq!(result.url, "https://example.com");
        assert_eq!(result.title, "Example");
    }

    #[tokio::test]
    async fn navigate_sends_egress_policy_header_as_json() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            // wiremock's `header` matcher splits values on commas, which breaks
            // on JSON; assert the raw header value verbatim with a closure.
            .and(|req: &wiremock::Request| {
                req.headers
                    .get("x-ix-egress-policy")
                    .and_then(|v| v.to_str().ok())
                    == Some(r#"{"enabled":true,"mode":"allowlist","rules":["example.com"]}"#)
            })
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "url": "https://example.com", "title": "Example"
            })))
            .mount(&server)
            .await;

        let backend = RemoteSharedBrowserBackend::new(
            server.uri(), "chat-1".to_string(), &policy(), None,
        );
        assert!(backend.navigate("https://example.com").await.is_ok());
    }

    #[tokio::test]
    async fn available_is_true_for_remote() {
        let backend = RemoteSharedBrowserBackend::new(
            "http://127.0.0.1:1".to_string(), "c".to_string(), &policy(), None,
        );
        assert!(backend.available());
    }

    #[tokio::test]
    async fn screenshot_returns_bytes() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/v1/browser/screenshot"))
            .respond_with(ResponseTemplate::new(200).set_body_bytes(vec![0x89, 0x50, 0x4E, 0x47]))
            .mount(&server)
            .await;
        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let bytes = backend.screenshot().await.unwrap();
        assert_eq!(bytes, vec![0x89, 0x50, 0x4E, 0x47]);
    }

    #[tokio::test]
    async fn snapshot_builds_query_and_parses() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/v1/browser/snapshot"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "url": "https://example.com", "title": "Example", "nodes": []
            })))
            .mount(&server)
            .await;
        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let snap = backend.snapshot(SnapshotOpts::default()).await.unwrap();
        assert_eq!(snap.url, "https://example.com");
    }

    #[tokio::test]
    async fn gateway_403_maps_to_forbidden() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            .respond_with(ResponseTemplate::new(403).set_body_string("egress denied: evil.com"))
            .mount(&server)
            .await;
        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let err = backend.navigate("https://evil.com").await.unwrap_err();
        assert!(matches!(err, ix_core::Error::Forbidden(_)));
    }

    #[tokio::test]
    async fn gateway_503_maps_to_unavailable() {
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/browser/navigate"))
            .respond_with(ResponseTemplate::new(503).set_body_string("browser-vm down"))
            .mount(&server)
            .await;
        let backend =
            RemoteSharedBrowserBackend::new(server.uri(), "c".into(), &policy(), None);
        let err = backend.navigate("https://example.com").await.unwrap_err();
        assert!(matches!(err, ix_core::Error::Unavailable(_)));
    }
}
