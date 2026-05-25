use crate::types::EgressPolicy;

#[derive(Debug, Clone)]
pub struct DaemonConfig {
    pub addr: String,
    pub workspace: String,
    pub egress: EgressPolicy,
    pub socket: Option<String>,
    pub vsock_port: Option<u32>,
    pub vsock_ready_port: Option<u32>,
}

impl DaemonConfig {
    pub fn from_env() -> Self {
        let addr = std::env::var("IX_ADDR").unwrap_or_else(|_| "0.0.0.0:8080".to_string());
        let socket = std::env::var("IX_SOCKET").ok();
        let vsock_port = std::env::var("IX_VSOCK_PORT").ok().and_then(|v| v.parse().ok());
        let vsock_ready_port = std::env::var("IX_VSOCK_READY_PORT").ok().and_then(|v| v.parse().ok());
        let workspace =
            std::env::var("IX_WORKSPACE").unwrap_or_else(|_| "/workspace".to_string());

        let egress_enabled = std::env::var("IX_EGRESS_ENABLED")
            .map(|v| v == "true" || v == "1")
            .unwrap_or(false);

        let egress_mode =
            std::env::var("IX_EGRESS_MODE").unwrap_or_else(|_| "allow".to_string());

        let egress_rules: Vec<String> = std::env::var("IX_EGRESS_RULES")
            .unwrap_or_default()
            .split(',')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect();

        let mode = match egress_mode.as_str() {
            "deny" => crate::types::PolicyMode::Denylist,
            _ => crate::types::PolicyMode::Allowlist,
        };

        Self {
            addr,
            workspace,
            socket,
            vsock_port,
            vsock_ready_port,
            egress: EgressPolicy {
                enabled: egress_enabled,
                mode,
                rules: egress_rules,
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::PolicyMode;
    use std::sync::Mutex;

    /// A global lock that every config test must hold while mutating env vars.
    /// This prevents races when cargo runs tests in parallel threads.
    static ENV_LOCK: Mutex<()> = Mutex::new(());

    /// All IX_* env var keys we care about for config tests.
    const ALL_KEYS: &[&str] = &[
        "IX_ADDR",
        "IX_WORKSPACE",
        "IX_SOCKET",
        "IX_EGRESS_ENABLED",
        "IX_EGRESS_RULES",
        "IX_EGRESS_MODE",
    ];

    /// Run `f` with exactly the env vars in `vars` set (others from `ALL_KEYS`
    /// are cleared), then restore everything.  Holds `ENV_LOCK` for the
    /// duration so tests don't race each other.
    fn with_env<F: FnOnce() -> DaemonConfig>(vars: &[(&str, &str)], f: F) -> DaemonConfig {
        let _guard = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());

        // Save originals of every key we might touch.
        let saved: Vec<(&str, Option<String>)> = ALL_KEYS
            .iter()
            .map(|k| (*k, std::env::var(k).ok()))
            .collect();

        // Clear all, then apply the requested overrides.
        for k in ALL_KEYS {
            unsafe { std::env::remove_var(k) };
        }
        for (k, v) in vars {
            unsafe { std::env::set_var(k, v) };
        }

        let result = f();

        // Restore originals.
        for (k, orig) in saved {
            match orig {
                Some(val) => unsafe { std::env::set_var(k, val) },
                None => unsafe { std::env::remove_var(k) },
            }
        }

        result
    }

    #[test]
    fn default_values_when_no_env_vars_set() {
        // with_env clears all IX_* vars before calling from_env.
        let cfg = with_env(&[], DaemonConfig::from_env);
        assert_eq!(cfg.addr, "0.0.0.0:8080");
        assert_eq!(cfg.workspace, "/workspace");
        assert!(!cfg.egress.enabled);
        assert!(cfg.egress.rules.is_empty());
        assert!(matches!(cfg.egress.mode, PolicyMode::Allowlist));
    }

    #[test]
    fn custom_addr_from_ix_addr() {
        let cfg = with_env(&[("IX_ADDR", "127.0.0.1:9090")], DaemonConfig::from_env);
        assert_eq!(cfg.addr, "127.0.0.1:9090");
    }

    #[test]
    fn egress_enabled_from_ix_egress_enabled_true() {
        let cfg = with_env(
            &[("IX_EGRESS_ENABLED", "true")],
            DaemonConfig::from_env,
        );
        assert!(cfg.egress.enabled);
    }

    #[test]
    fn egress_enabled_from_ix_egress_enabled_1() {
        let cfg = with_env(&[("IX_EGRESS_ENABLED", "1")], DaemonConfig::from_env);
        assert!(cfg.egress.enabled);
    }

    #[test]
    fn egress_rules_parsed_from_comma_separated() {
        let cfg = with_env(
            &[
                ("IX_EGRESS_ENABLED", "true"),
                ("IX_EGRESS_RULES", "api.example.com, *.internal.net, 10.0.0.0/8"),
            ],
            DaemonConfig::from_env,
        );
        assert_eq!(cfg.egress.rules.len(), 3);
        assert_eq!(cfg.egress.rules[0], "api.example.com");
        assert_eq!(cfg.egress.rules[1], "*.internal.net");
        assert_eq!(cfg.egress.rules[2], "10.0.0.0/8");
    }

    #[test]
    fn egress_mode_defaults_to_allowlist() {
        let cfg = with_env(&[("IX_EGRESS_ENABLED", "true")], DaemonConfig::from_env);
        assert!(matches!(cfg.egress.mode, PolicyMode::Allowlist));
    }

    #[test]
    fn egress_mode_denylist_from_deny() {
        let cfg = with_env(
            &[("IX_EGRESS_ENABLED", "true"), ("IX_EGRESS_MODE", "deny")],
            DaemonConfig::from_env,
        );
        assert!(matches!(cfg.egress.mode, PolicyMode::Denylist));
    }

    #[test]
    fn egress_mode_unknown_falls_back_to_allowlist() {
        let cfg = with_env(
            &[("IX_EGRESS_MODE", "something_weird")],
            DaemonConfig::from_env,
        );
        assert!(matches!(cfg.egress.mode, PolicyMode::Allowlist));
    }

    #[test]
    fn empty_egress_rules_string_yields_empty_vec() {
        let cfg = with_env(
            &[("IX_EGRESS_ENABLED", "true"), ("IX_EGRESS_RULES", "")],
            DaemonConfig::from_env,
        );
        assert!(cfg.egress.rules.is_empty());
    }
}
