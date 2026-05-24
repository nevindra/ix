package ix

// EgressPolicy configures DNS-based egress filtering for a sandbox.
type EgressPolicy struct {
	Enabled bool     `json:"enabled"`
	Mode    string   `json:"mode"`  // "allow" or "deny"
	Rules   []string `json:"rules"` // domain patterns: "pypi.org", "*.github.com"
}

// EgressPatch modifies the egress policy at runtime.
type EgressPatch struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}
