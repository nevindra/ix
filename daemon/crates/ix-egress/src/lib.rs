pub mod dns;
pub mod policy;
pub mod resolver;

use dns::run_dns_proxy;
use ix_core::error::Result;
use ix_core::types::{EgressPatch, EgressPolicy};
use std::sync::Arc;
use tokio::sync::RwLock;
use tracing::info;

/// A running egress filter that intercepts DNS queries on `127.0.0.1:53`
/// and blocks resolution of non-allowed domains.
pub struct EgressFilter {
    policy: Arc<RwLock<EgressPolicy>>,
    shutdown_tx: Option<tokio::sync::oneshot::Sender<()>>,
}

impl EgressFilter {
    /// Start the egress filter with the given initial policy.
    ///
    /// Binds `127.0.0.1:53` and writes `nameserver 127.0.0.1` to `/etc/resolv.conf`.
    pub async fn start(policy: EgressPolicy) -> Result<Self> {
        let policy = Arc::new(RwLock::new(policy));
        let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel::<()>();

        let proxy_policy = Arc::clone(&policy);
        tokio::spawn(async move {
            if let Err(e) = run_dns_proxy(proxy_policy, shutdown_rx).await {
                tracing::error!("DNS proxy error: {e}");
            }
        });

        info!("EgressFilter started");
        Ok(Self {
            policy,
            shutdown_tx: Some(shutdown_tx),
        })
    }

    /// Return a snapshot of the current policy.
    pub fn get_policy(&self) -> EgressPolicy {
        // Synchronous snapshot — use try_read to avoid blocking; fall back to clone via blocking
        self.policy
            .try_read()
            .map(|g| g.clone())
            .unwrap_or_else(|_| EgressPolicy::default())
    }

    /// Apply a patch to the current policy.
    ///
    /// Rules listed in `patch.add` are appended; rules listed in `patch.remove` are dropped.
    pub fn update_policy(&self, patch: EgressPatch) {
        let policy = Arc::clone(&self.policy);
        tokio::spawn(async move {
            let mut guard = policy.write().await;
            if let Some(add) = patch.add {
                guard.rules.extend(add);
            }
            if let Some(remove) = patch.remove {
                guard.rules.retain(|r| !remove.contains(r));
            }
        });
    }

    /// Shut down the DNS proxy and restore `/etc/resolv.conf`.
    pub async fn shutdown(mut self) {
        if let Some(tx) = self.shutdown_tx.take() {
            let _ = tx.send(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::{EgressPatch, EgressPolicy, PolicyMode};

    /// Build an EgressFilter without actually binding port 53 or touching resolv.conf.
    /// We construct it directly to test the policy management logic in isolation.
    fn make_filter(policy: EgressPolicy) -> EgressFilter {
        EgressFilter {
            policy: Arc::new(RwLock::new(policy)),
            shutdown_tx: None,
        }
    }

    #[test]
    fn default_policy_is_disabled() {
        let filter = make_filter(EgressPolicy::default());
        let policy = filter.get_policy();
        assert!(!policy.enabled, "default policy must be disabled");
    }

    #[test]
    fn default_policy_is_allowlist_mode() {
        let filter = make_filter(EgressPolicy::default());
        let policy = filter.get_policy();
        assert!(matches!(policy.mode, PolicyMode::Allowlist));
    }

    #[test]
    fn default_policy_has_empty_rules() {
        let filter = make_filter(EgressPolicy::default());
        let policy = filter.get_policy();
        assert!(policy.rules.is_empty());
    }

    #[test]
    fn get_policy_returns_current_state() {
        let initial = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Denylist,
            rules: vec!["evil.com".to_string()],
        };
        let filter = make_filter(initial);
        let policy = filter.get_policy();
        assert!(policy.enabled);
        assert!(matches!(policy.mode, PolicyMode::Denylist));
        assert_eq!(policy.rules, vec!["evil.com"]);
    }

    #[tokio::test]
    async fn update_policy_add_appends_rules() {
        let rt = tokio::runtime::Handle::current();
        let filter = make_filter(EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["existing.com".to_string()],
        });

        let patch = EgressPatch {
            add: Some(vec!["new.com".to_string(), "another.io".to_string()]),
            remove: None,
        };
        filter.update_policy(patch);

        // update_policy spawns a task; yield to let it run
        rt.spawn(async {}).await.unwrap();
        tokio::task::yield_now().await;
        tokio::task::yield_now().await;

        let policy = filter.get_policy();
        assert!(policy.rules.contains(&"existing.com".to_string()));
        assert!(policy.rules.contains(&"new.com".to_string()));
        assert!(policy.rules.contains(&"another.io".to_string()));
    }

    #[tokio::test]
    async fn update_policy_remove_drops_rules() {
        let rt = tokio::runtime::Handle::current();
        let filter = make_filter(EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec![
                "keep.com".to_string(),
                "remove-me.com".to_string(),
                "also-keep.io".to_string(),
            ],
        });

        let patch = EgressPatch {
            add: None,
            remove: Some(vec!["remove-me.com".to_string()]),
        };
        filter.update_policy(patch);

        // Yield to let spawned task complete
        rt.spawn(async {}).await.unwrap();
        tokio::task::yield_now().await;
        tokio::task::yield_now().await;

        let policy = filter.get_policy();
        assert!(policy.rules.contains(&"keep.com".to_string()));
        assert!(policy.rules.contains(&"also-keep.io".to_string()));
        assert!(!policy.rules.contains(&"remove-me.com".to_string()));
    }

    #[tokio::test]
    async fn update_policy_add_and_remove_simultaneously() {
        let rt = tokio::runtime::Handle::current();
        let filter = make_filter(EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["old.com".to_string()],
        });

        let patch = EgressPatch {
            add: Some(vec!["fresh.com".to_string()]),
            remove: Some(vec!["old.com".to_string()]),
        };
        filter.update_policy(patch);

        rt.spawn(async {}).await.unwrap();
        tokio::task::yield_now().await;
        tokio::task::yield_now().await;

        let policy = filter.get_policy();
        assert!(policy.rules.contains(&"fresh.com".to_string()));
        assert!(!policy.rules.contains(&"old.com".to_string()));
    }
}
