use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    BrowserWaitOpts, BrowserWaitResult, NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::Result;

#[async_trait]
pub trait BrowserBackend: Send + Sync {
    async fn navigate(&self, url: &str) -> Result<NavigateResult>;
    async fn screenshot(&self) -> Result<Vec<u8>>;
    async fn action(&self, action: BrowserAction) -> Result<BrowserResult>;
    async fn snapshot(&self, opts: SnapshotOpts) -> Result<BrowserSnapshot>;
    async fn text(&self, opts: TextOpts) -> Result<BrowserTextResult>;
    async fn pdf(&self) -> Result<Vec<u8>>;
    async fn eval(&self, expr: &str) -> Result<String>;
    async fn find(&self, query: &str) -> Result<BrowserFindResult>;
    /// Block until a page condition is met or the deadline elapses. A timeout
    /// is NOT an error: the result has `satisfied: false` plus a detail.
    async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult>;
    fn available(&self) -> bool;
}

#[cfg(test)]
pub mod mock {
    use super::BrowserBackend;
    use async_trait::async_trait;
    use ix_core::types::{
        BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
        BrowserWaitOpts, BrowserWaitResult, NavigateResult, SnapshotNode, SnapshotOpts, TextOpts,
    };
    use ix_core::{Error, Result};

    /// A test double for [`BrowserBackend`].
    ///
    /// All methods return the values stored in the public fields; by default
    /// every method returns an error so that tests must opt-in to the
    /// responses they care about.
    pub struct MockBrowser {
        pub is_available: bool,
        pub navigate_response: Option<NavigateResult>,
        pub screenshot_response: Option<Vec<u8>>,
        pub action_response: Option<BrowserResult>,
        pub snapshot_response: Option<BrowserSnapshot>,
        pub text_response: Option<BrowserTextResult>,
        pub pdf_response: Option<Vec<u8>>,
        pub eval_response: Option<String>,
        pub find_response: Option<BrowserFindResult>,
        pub wait_response: Option<BrowserWaitResult>,
    }

    impl Default for MockBrowser {
        fn default() -> Self {
            Self {
                is_available: true,
                navigate_response: None,
                screenshot_response: None,
                action_response: None,
                snapshot_response: None,
                text_response: None,
                pdf_response: None,
                eval_response: None,
                find_response: None,
                wait_response: None,
            }
        }
    }

    impl MockBrowser {
        pub fn new() -> Self {
            Self::default()
        }

        pub fn unavailable() -> Self {
            Self {
                is_available: false,
                ..Self::default()
            }
        }

        /// Pre-configure a successful navigate response.
        pub fn with_navigate(mut self, url: &str, title: &str) -> Self {
            self.navigate_response = Some(NavigateResult {
                url: url.to_string(),
                title: title.to_string(),
            });
            self
        }

        /// Pre-configure a snapshot response with a single node.
        pub fn with_snapshot(mut self, url: &str, title: &str) -> Self {
            self.snapshot_response = Some(BrowserSnapshot {
                url: url.to_string(),
                title: title.to_string(),
                nodes: vec![SnapshotNode {
                    element_ref: "1".to_string(),
                    role: "button".to_string(),
                    name: "Click me".to_string(),
                }],
            });
            self
        }

        /// Pre-configure a text response.
        pub fn with_text(mut self, url: &str, title: &str, text: &str) -> Self {
            self.text_response = Some(BrowserTextResult {
                url: url.to_string(),
                title: title.to_string(),
                text: text.to_string(),
                truncated: false,
            });
            self
        }
    }

    #[async_trait]
    impl BrowserBackend for MockBrowser {
        async fn navigate(&self, _url: &str) -> Result<NavigateResult> {
            self.navigate_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: navigate not configured".into()))
        }

        async fn screenshot(&self) -> Result<Vec<u8>> {
            self.screenshot_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: screenshot not configured".into()))
        }

        async fn action(&self, _action: BrowserAction) -> Result<BrowserResult> {
            self.action_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: action not configured".into()))
        }

        async fn snapshot(&self, _opts: SnapshotOpts) -> Result<BrowserSnapshot> {
            self.snapshot_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: snapshot not configured".into()))
        }

        async fn text(&self, _opts: TextOpts) -> Result<BrowserTextResult> {
            self.text_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: text not configured".into()))
        }

        async fn pdf(&self) -> Result<Vec<u8>> {
            self.pdf_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: pdf not configured".into()))
        }

        async fn eval(&self, _expr: &str) -> Result<String> {
            self.eval_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: eval not configured".into()))
        }

        async fn find(&self, _query: &str) -> Result<BrowserFindResult> {
            self.find_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: find not configured".into()))
        }

        async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
            self.wait_response
                .clone()
                .ok_or_else(|| Error::Internal("MockBrowser: wait not configured".into()))
        }

        fn available(&self) -> bool {
            self.is_available
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::backend::mock::MockBrowser;
    use ix_core::types::{SnapshotOpts, TextOpts};

    // ── Object safety ────────────────────────────────────────────────────────

    /// Verifies that `BrowserBackend` is object-safe: it can be stored as a
    /// trait object behind a `Box<dyn BrowserBackend>`.
    #[test]
    fn trait_is_object_safe() {
        let mock = MockBrowser::new();
        // This would be a compile error if the trait were not object-safe.
        let _boxed: Box<dyn BrowserBackend> = Box::new(mock);
    }

    /// Verify we can store a trait object in an `Arc` (required for
    /// shared-state use in ix-server).
    #[test]
    fn trait_object_in_arc() {
        use std::sync::Arc;
        let mock = MockBrowser::new();
        let _arc: Arc<dyn BrowserBackend> = Arc::new(mock);
    }

    // ── MockBrowser::available ───────────────────────────────────────────────

    #[test]
    fn mock_available_true_by_default() {
        let mock = MockBrowser::new();
        assert!(mock.available());
    }

    #[test]
    fn mock_unavailable_returns_false() {
        let mock = MockBrowser::unavailable();
        assert!(!mock.available());
    }

    // ── MockBrowser methods ──────────────────────────────────────────────────

    #[tokio::test]
    async fn mock_navigate_returns_configured_value() {
        let mock = MockBrowser::new().with_navigate("https://example.com", "Example");
        let result = mock.navigate("https://example.com").await.unwrap();
        assert_eq!(result.url, "https://example.com");
        assert_eq!(result.title, "Example");
    }

    #[tokio::test]
    async fn mock_navigate_unconfigured_returns_error() {
        let mock = MockBrowser::new();
        assert!(mock.navigate("https://example.com").await.is_err());
    }

    #[tokio::test]
    async fn mock_snapshot_returns_configured_value() {
        let mock = MockBrowser::new().with_snapshot("https://example.com", "Example");
        let result = mock.snapshot(SnapshotOpts::default()).await.unwrap();
        assert_eq!(result.url, "https://example.com");
        assert_eq!(result.nodes.len(), 1);
        assert_eq!(result.nodes[0].role, "button");
    }

    #[tokio::test]
    async fn mock_text_returns_configured_value() {
        let mock = MockBrowser::new().with_text("https://example.com", "Example", "Hello world");
        let result = mock.text(TextOpts::default()).await.unwrap();
        assert_eq!(result.text, "Hello world");
        assert!(!result.truncated);
    }

    #[tokio::test]
    async fn mock_screenshot_unconfigured_returns_error() {
        let mock = MockBrowser::new();
        assert!(mock.screenshot().await.is_err());
    }

    #[tokio::test]
    async fn mock_pdf_unconfigured_returns_error() {
        let mock = MockBrowser::new();
        assert!(mock.pdf().await.is_err());
    }

    #[tokio::test]
    async fn mock_eval_unconfigured_returns_error() {
        let mock = MockBrowser::new();
        assert!(mock.eval("1+1").await.is_err());
    }

    #[tokio::test]
    async fn mock_find_unconfigured_returns_error() {
        let mock = MockBrowser::new();
        assert!(mock.find("submit button").await.is_err());
    }

    #[tokio::test]
    async fn mock_wait_returns_configured_value() {
        use ix_core::types::{BrowserWaitOpts, BrowserWaitResult};
        let mut mock = MockBrowser::new();
        mock.wait_response = Some(BrowserWaitResult {
            satisfied: true,
            kind: "selector".to_string(),
            elapsed_ms: 840,
            detail: None,
        });
        let result = mock
            .wait(BrowserWaitOpts {
                kind: "selector".to_string(),
                value: Some("#login".to_string()),
                timeout_ms: None,
                state: None,
            })
            .await
            .unwrap();
        assert!(result.satisfied);
        assert_eq!(result.elapsed_ms, 840);
    }

    #[tokio::test]
    async fn mock_wait_unconfigured_returns_error() {
        use ix_core::types::BrowserWaitOpts;
        let mock = MockBrowser::new();
        assert!(mock
            .wait(BrowserWaitOpts {
                kind: "load".to_string(),
                value: None,
                timeout_ms: None,
                state: None,
            })
            .await
            .is_err());
    }
}
