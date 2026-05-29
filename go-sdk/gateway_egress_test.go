package ix

import "testing"

// Tests mirror daemon/crates/ix-egress/src/policy.rs #[cfg(test)] cases exactly.

// --- domainMatches ---

func TestDomainMatches_ExactMatch(t *testing.T) {
	cases := []struct {
		domain, pattern string
		want            bool
	}{
		// exact hit
		{"pypi.org", "pypi.org", true},
		// exact miss
		{"other.org", "pypi.org", false},
		// case-insensitive exact (both directions)
		{"PyPI.ORG", "pypi.org", true},
		{"pypi.org", "PyPI.ORG", true},
		{"GITHUB.COM", "github.com", true},
	}
	for _, tc := range cases {
		got := domainMatches(tc.domain, tc.pattern)
		if got != tc.want {
			t.Errorf("domainMatches(%q, %q) = %v, want %v", tc.domain, tc.pattern, got, tc.want)
		}
	}
}

func TestDomainMatches_Wildcard(t *testing.T) {
	cases := []struct {
		domain, pattern string
		want            bool
	}{
		// basic wildcard hits
		{"api.github.com", "*.github.com", true},
		{"raw.githubusercontent.com", "*.githubusercontent.com", true},
		// wildcard does NOT match the bare base domain
		{"github.com", "*.github.com", false},
		{"npmjs.org", "*.npmjs.org", false},
		// multi-level subdomain must still match
		{"raw.api.github.com", "*.github.com", true},
		{"a.b.c.github.com", "*.github.com", true},
	}
	for _, tc := range cases {
		got := domainMatches(tc.domain, tc.pattern)
		if got != tc.want {
			t.Errorf("domainMatches(%q, %q) = %v, want %v", tc.domain, tc.pattern, got, tc.want)
		}
	}
}

// --- isAllowed ---

func TestEgressIsAllowed_AllowlistMode(t *testing.T) {
	policy := EgressPolicy{
		Enabled: true,
		Mode:    "allow",
		Rules:   []string{"pypi.org", "*.github.com"},
	}
	cases := []struct {
		domain string
		want   bool
	}{
		{"pypi.org", true},
		{"api.github.com", true},
		{"evil.com", false},
		// non-listed domains must be blocked
		{"attacker.io", false},
	}
	for _, tc := range cases {
		got := isAllowed(tc.domain, policy)
		if got != tc.want {
			t.Errorf("isAllowed(%q, allowlist) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestEgressIsAllowed_DenylistMode(t *testing.T) {
	policy := EgressPolicy{
		Enabled: true,
		Mode:    "deny",
		Rules:   []string{"evil.com", "bad.io"},
	}
	cases := []struct {
		domain string
		want   bool
	}{
		// listed → blocked
		{"evil.com", false},
		{"bad.io", false},
		// unlisted → allowed
		{"pypi.org", true},
		{"safe.org", true},
		{"github.com", true},
	}
	for _, tc := range cases {
		got := isAllowed(tc.domain, policy)
		if got != tc.want {
			t.Errorf("isAllowed(%q, denylist) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}

func TestEgressIsAllowed_DisabledAllowsEverything(t *testing.T) {
	// Disabled policy → allow all regardless of mode/rules.
	for _, mode := range []string{"allow", "deny", ""} {
		policy := EgressPolicy{
			Enabled: false,
			Mode:    mode,
			Rules:   []string{"pypi.org"},
		}
		domains := []string{"pypi.org", "anything.com", "blocked-if-enabled.org", "anything-at-all.example"}
		for _, d := range domains {
			if got := isAllowed(d, policy); !got {
				t.Errorf("isAllowed(%q, disabled mode=%q) = false, want true", d, mode)
			}
		}
	}
}

func TestEgressIsAllowed_EmptyRulesUsesDefaultAllowlist(t *testing.T) {
	// Empty rules + allowlist → DEFAULT_ALLOWLIST is used.
	policy := EgressPolicy{
		Enabled: true,
		Mode:    "allow",
		Rules:   []string{},
	}

	// These must be allowed (present in DEFAULT_ALLOWLIST).
	allowed := []string{
		"pypi.org",
		"api.github.com",
		"github.com",
		"registry.npmjs.org",
		"files.pythonhosted.org",
		"api.openai.com",
		"api.anthropic.com",
	}
	for _, d := range allowed {
		if got := isAllowed(d, policy); !got {
			t.Errorf("isAllowed(%q, empty rules allowlist) = false, want true (should be in default allowlist)", d)
		}
	}

	// These must be denied (absent from DEFAULT_ALLOWLIST).
	denied := []string{
		"random-site.example",
		"notindafaultlist.com",
		"not-allowed.com",
	}
	for _, d := range denied {
		if got := isAllowed(d, policy); got {
			t.Errorf("isAllowed(%q, empty rules allowlist) = true, want false (not in default allowlist)", d)
		}
	}
}

func TestDefaultAllowlistContents(t *testing.T) {
	// Spot-check key entries verbatim from Rust DEFAULT_ALLOWLIST.
	required := []string{
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
	set := make(map[string]bool, len(defaultAllowlist))
	for _, e := range defaultAllowlist {
		set[e] = true
	}
	for _, entry := range required {
		if !set[entry] {
			t.Errorf("defaultAllowlist missing %q", entry)
		}
	}
	// Length must match exactly (no extras, no omissions).
	if len(defaultAllowlist) != len(required) {
		t.Errorf("defaultAllowlist has %d entries, want %d", len(defaultAllowlist), len(required))
	}
}

func TestEgressIsAllowed_MultipleRulesMatchesAny(t *testing.T) {
	policy := EgressPolicy{
		Enabled: true,
		Mode:    "allow",
		Rules:   []string{"alpha.com", "beta.com", "*.gamma.com"},
	}
	cases := []struct {
		domain string
		want   bool
	}{
		{"alpha.com", true},
		{"beta.com", true},
		{"sub.gamma.com", true},
		{"delta.com", false},
	}
	for _, tc := range cases {
		got := isAllowed(tc.domain, policy)
		if got != tc.want {
			t.Errorf("isAllowed(%q, multi-rules allowlist) = %v, want %v", tc.domain, got, tc.want)
		}
	}
}
