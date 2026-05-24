use ix_core::types::{EgressPolicy, PolicyMode};

/// Default allowlist used when egress is enabled but no rules are configured.
pub const DEFAULT_ALLOWLIST: &[&str] = &[
    "pypi.org",
    "*.pypi.org",
    "files.pythonhosted.org",
    "registry.npmjs.org",
    "*.npmjs.org",
    "pkg.go.dev",
    "github.com",
    "*.github.com",
    "*.githubusercontent.com",
    "gitlab.com",
    "*.gitlab.com",
    "api.openai.com",
    "api.anthropic.com",
    "*.ubuntu.com",
    "*.debian.org",
    "dl.google.com",
];

/// Check whether `domain` is allowed by the given `EgressPolicy`.
///
/// Returns `true` if the domain should be resolved, `false` if it should be blocked.
pub fn is_allowed(domain: &str, policy: &EgressPolicy) -> bool {
    if !policy.enabled {
        return true;
    }

    let rules: Vec<&str> = if policy.rules.is_empty() {
        DEFAULT_ALLOWLIST.to_vec()
    } else {
        policy.rules.iter().map(|s| s.as_str()).collect()
    };

    let matched = rules.iter().any(|rule| domain_matches(domain, rule));

    match policy.mode {
        PolicyMode::Allowlist => matched,
        PolicyMode::Denylist => !matched,
    }
}

/// Returns true if `domain` matches `pattern`.
///
/// Matching rules:
/// - Exact match: `"pypi.org"` matches `"pypi.org"` only.
/// - Wildcard `"*.x.com"`: suffix match — the domain must end with `.x.com`
///   (i.e. `"*.github.com"` matches `"api.github.com"`, and `"*.githubusercontent.com"`
///   matches `"raw.githubusercontent.com"`).
fn domain_matches(domain: &str, pattern: &str) -> bool {
    if let Some(suffix) = pattern.strip_prefix("*.") {
        // Wildcard: domain must end with ".<suffix>" — does NOT match the base domain itself.
        domain.ends_with(&format!(".{suffix}"))
    } else {
        // Exact match (case-insensitive for DNS)
        domain.eq_ignore_ascii_case(pattern)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_exact_match() {
        assert!(domain_matches("pypi.org", "pypi.org"));
        assert!(!domain_matches("other.org", "pypi.org"));
    }

    #[test]
    fn test_wildcard_match() {
        assert!(domain_matches("api.github.com", "*.github.com"));
        assert!(domain_matches("raw.githubusercontent.com", "*.githubusercontent.com"));
        // Wildcard does NOT match the base domain itself unless it's the suffix
        assert!(!domain_matches("github.com", "*.github.com"));
    }

    #[test]
    fn test_allowlist_mode() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["pypi.org".to_string(), "*.github.com".to_string()],
        };
        assert!(is_allowed("pypi.org", &policy));
        assert!(is_allowed("api.github.com", &policy));
        assert!(!is_allowed("evil.com", &policy));
    }

    #[test]
    fn test_denylist_mode() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Denylist,
            rules: vec!["evil.com".to_string()],
        };
        assert!(is_allowed("pypi.org", &policy));
        assert!(!is_allowed("evil.com", &policy));
    }

    #[test]
    fn test_disabled_policy_allows_all() {
        let policy = EgressPolicy {
            enabled: false,
            mode: PolicyMode::Denylist,
            rules: vec!["pypi.org".to_string()],
        };
        assert!(is_allowed("pypi.org", &policy));
        assert!(is_allowed("anything.com", &policy));
    }

    #[test]
    fn test_default_allowlist_used_when_no_rules() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec![],
        };
        assert!(is_allowed("pypi.org", &policy));
        assert!(is_allowed("api.github.com", &policy));
        assert!(!is_allowed("not-allowed.com", &policy));
    }

    // --- Additional coverage ---

    #[test]
    fn exact_match_is_case_insensitive() {
        assert!(domain_matches("PyPI.ORG", "pypi.org"));
        assert!(domain_matches("pypi.org", "PyPI.ORG"));
        assert!(domain_matches("GITHUB.COM", "github.com"));
    }

    #[test]
    fn wildcard_matches_multi_level_subdomain() {
        // "raw.api.github.com" should match "*.github.com"
        assert!(domain_matches("raw.api.github.com", "*.github.com"));
        assert!(domain_matches("a.b.c.github.com", "*.github.com"));
    }

    #[test]
    fn wildcard_does_not_match_base_domain() {
        assert!(!domain_matches("github.com", "*.github.com"));
        assert!(!domain_matches("npmjs.org", "*.npmjs.org"));
    }

    #[test]
    fn non_matching_domain_rejected_in_allowlist() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["pypi.org".to_string()],
        };
        assert!(!is_allowed("evil.com", &policy));
        assert!(!is_allowed("attacker.io", &policy));
    }

    #[test]
    fn denylist_listed_domain_blocked_unlisted_allowed() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Denylist,
            rules: vec!["evil.com".to_string(), "bad.io".to_string()],
        };
        assert!(!is_allowed("evil.com", &policy));
        assert!(!is_allowed("bad.io", &policy));
        assert!(is_allowed("safe.org", &policy));
        assert!(is_allowed("github.com", &policy));
    }

    #[test]
    fn empty_rules_in_allowlist_blocks_everything_via_default_allowlist() {
        // When rules is empty, DEFAULT_ALLOWLIST is used — non-listed domains are blocked.
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec![],
        };
        assert!(!is_allowed("random-site.example", &policy));
        assert!(!is_allowed("notindafaultlist.com", &policy));
    }

    #[test]
    fn default_allowlist_contains_expected_entries() {
        // Spot-check key entries from DEFAULT_ALLOWLIST
        assert!(DEFAULT_ALLOWLIST.contains(&"pypi.org"));
        assert!(DEFAULT_ALLOWLIST.contains(&"*.pypi.org"));
        assert!(DEFAULT_ALLOWLIST.contains(&"github.com"));
        assert!(DEFAULT_ALLOWLIST.contains(&"*.github.com"));
        assert!(DEFAULT_ALLOWLIST.contains(&"*.githubusercontent.com"));
        assert!(DEFAULT_ALLOWLIST.contains(&"registry.npmjs.org"));
        assert!(DEFAULT_ALLOWLIST.contains(&"api.openai.com"));
        assert!(DEFAULT_ALLOWLIST.contains(&"api.anthropic.com"));
        assert!(DEFAULT_ALLOWLIST.contains(&"files.pythonhosted.org"));
    }

    #[test]
    fn multiple_rules_matches_any_one() {
        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec![
                "alpha.com".to_string(),
                "beta.com".to_string(),
                "*.gamma.com".to_string(),
            ],
        };
        assert!(is_allowed("alpha.com", &policy));
        assert!(is_allowed("beta.com", &policy));
        assert!(is_allowed("sub.gamma.com", &policy));
        assert!(!is_allowed("delta.com", &policy));
    }

    #[test]
    fn trailing_dot_is_handled() {
        // DNS names sometimes arrive with trailing dot (FQDN); strip before matching
        // The caller (dns.rs) strips it, so test that stripped names match correctly
        let stripped = "github.com";
        assert!(domain_matches(stripped, "github.com"));

        let policy = EgressPolicy {
            enabled: true,
            mode: PolicyMode::Allowlist,
            rules: vec!["github.com".to_string()],
        };
        // After stripping the trailing dot (as dns.rs does), matching must work
        assert!(is_allowed("github.com", &policy));
    }

    #[test]
    fn disabled_policy_allows_even_with_denylist_rules() {
        let policy = EgressPolicy {
            enabled: false,
            mode: PolicyMode::Allowlist,
            rules: vec!["only-this.com".to_string()],
        };
        // Disabled means everything is allowed regardless of mode/rules
        assert!(is_allowed("anything-at-all.example", &policy));
        assert!(is_allowed("blocked-if-enabled.org", &policy));
    }
}
