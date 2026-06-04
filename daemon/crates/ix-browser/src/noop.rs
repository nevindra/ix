use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    BrowserWaitOpts, BrowserWaitResult, NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::{Error, Result};

use crate::backend::BrowserBackend;

/// Browser backend for "light" sandboxes with no browser capability.
/// `available()` is false and every method returns `Error::Unavailable`,
/// so browser routes respond 503 without spawning Chrome/pinchtab.
pub struct NoopBrowserBackend;

impl NoopBrowserBackend {
    pub fn new() -> Self {
        NoopBrowserBackend
    }
}

impl Default for NoopBrowserBackend {
    fn default() -> Self {
        Self::new()
    }
}

fn unavailable<T>() -> Result<T> {
    Err(Error::Unavailable("browser disabled for this sandbox".into()))
}

#[async_trait]
impl BrowserBackend for NoopBrowserBackend {
    async fn navigate(&self, _url: &str) -> Result<NavigateResult> {
        unavailable()
    }
    async fn screenshot(&self) -> Result<Vec<u8>> {
        unavailable()
    }
    async fn action(&self, _action: BrowserAction) -> Result<BrowserResult> {
        unavailable()
    }
    async fn snapshot(&self, _opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        unavailable()
    }
    async fn text(&self, _opts: TextOpts) -> Result<BrowserTextResult> {
        unavailable()
    }
    async fn pdf(&self) -> Result<Vec<u8>> {
        unavailable()
    }
    async fn eval(&self, _expr: &str) -> Result<String> {
        unavailable()
    }
    async fn find(&self, _query: &str) -> Result<BrowserFindResult> {
        unavailable()
    }
    async fn wait(&self, _opts: BrowserWaitOpts) -> Result<BrowserWaitResult> {
        unavailable()
    }
    fn available(&self) -> bool {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn noop_is_unavailable() {
        let b = NoopBrowserBackend::new();
        assert!(!b.available());

        // Every method must report unavailable so browser routes 503 cleanly.
        let action = BrowserAction {
            action_type: "click".into(),
            element_ref: Some("1".into()),
            x: None,
            y: None,
            text: None,
            key: None,
            direction: None,
            value: None,
        };
        assert!(matches!(
            b.navigate("https://example.com").await,
            Err(Error::Unavailable(_))
        ));
        assert!(matches!(b.screenshot().await, Err(Error::Unavailable(_))));
        assert!(matches!(b.action(action).await, Err(Error::Unavailable(_))));
        assert!(matches!(
            b.snapshot(SnapshotOpts::default()).await,
            Err(Error::Unavailable(_))
        ));
        assert!(matches!(
            b.text(TextOpts::default()).await,
            Err(Error::Unavailable(_))
        ));
        assert!(matches!(b.pdf().await, Err(Error::Unavailable(_))));
        assert!(matches!(b.eval("1+1").await, Err(Error::Unavailable(_))));
        assert!(matches!(b.find("button").await, Err(Error::Unavailable(_))));
        assert!(matches!(
            b.wait(BrowserWaitOpts {
                kind: "selector".into(),
                value: Some("#x".into()),
                timeout_ms: None,
                state: None,
            })
            .await,
            Err(Error::Unavailable(_))
        ));
    }
}
