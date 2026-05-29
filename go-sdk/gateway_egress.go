package ix

import "strings"

// gateway_egress.go — Go port of daemon/crates/ix-egress/src/policy.rs.
//
// Source of truth: daemon/crates/ix-egress/src/policy.rs (domain_matches,
// is_allowed, DEFAULT_ALLOWLIST). Behavior must remain identical to the Rust
// implementation; keep both in sync when either changes.
//
// Mode convention (verified from go-sdk/egress.go comment and
// daemon/crates/ix-core/src/config.rs:36-38):
//   Mode == "deny"  → Denylist  (blocked domains listed in Rules)
//   anything else   → Allowlist (allowed domains listed in Rules; default "allow")
// The Go SDK passes Mode directly as IX_EGRESS_MODE to the daemon
// (go-sdk/manager.go:434), and the daemon maps "deny" → Denylist, else Allowlist.

// defaultAllowlist is the exact port of DEFAULT_ALLOWLIST from
// daemon/crates/ix-egress/src/policy.rs, same entries, same order.
var defaultAllowlist = []string{
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
}

// domainMatches reports whether domain matches pattern.
//
// Two matching rules (mirroring Rust domain_matches):
//   - Wildcard "*.x.com": domain must end with ".x.com" — does NOT match
//     the bare base domain "x.com". Multi-level subdomains do match.
//   - Exact: case-insensitive equality.
func domainMatches(domain, pattern string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		// Wildcard: domain must end with ".<suffix>" (lowercased for safety).
		return strings.HasSuffix(strings.ToLower(domain), "."+strings.ToLower(suffix))
	}
	// Exact match is case-insensitive (DNS names are case-insensitive).
	return strings.EqualFold(domain, pattern)
}

// isAllowed reports whether domain should be permitted under policy.
//
// Logic (mirrors Rust is_allowed):
//  1. Disabled policy → always allow.
//  2. Rules set: use policy.Rules; if empty, fall back to defaultAllowlist.
//  3. Allowlist mode ("allow" or empty): allow iff any rule matches.
//  4. Denylist mode ("deny"): allow iff no rule matches.
func isAllowed(domain string, policy EgressPolicy) bool {
	if !policy.Enabled {
		return true
	}

	rules := policy.Rules
	if len(rules) == 0 {
		rules = defaultAllowlist
	}

	matched := false
	for _, rule := range rules {
		if domainMatches(domain, rule) {
			matched = true
			break
		}
	}

	if strings.EqualFold(policy.Mode, "deny") {
		return !matched
	}
	return matched
}
