package ix

import "testing"

func TestGatewayURLFromAddr(t *testing.T) {
	cases := map[string]string{
		"169.254.0.1:9100": "http://169.254.0.1:9100",
		"0.0.0.0:9100":     "http://0.0.0.0:9100",
		"":                 "http://169.254.0.1:9100",
	}
	for in, want := range cases {
		if got := gatewayURLFromAddr(in); got != want {
			t.Errorf("gatewayURLFromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBrowserTierEnv(t *testing.T) {
	// Default: token only, no SSRF opt-in.
	env := browserTierEnv(ManagerConfig{GatewayToken: "tok"})
	if len(env) != 1 || env[0] != "PINCHTAB_TOKEN=tok" {
		t.Errorf("token-only env = %v", env)
	}

	// CIDRs join comma-separated for the kernel cmdline (no spaces allowed).
	env = browserTierEnv(ManagerConfig{
		GatewayToken:               "tok",
		BrowserTrustedResolveCIDRs: []string{"169.254.0.0/16", "10.0.0.0/8"},
	})
	want := "PINCHTAB_TRUSTED_RESOLVE_CIDRS=169.254.0.0/16,10.0.0.0/8"
	if len(env) != 2 || env[1] != want {
		t.Errorf("cidr env = %v, want second entry %q", env, want)
	}

	// Empty config: no env at all.
	if env := browserTierEnv(ManagerConfig{}); len(env) != 0 {
		t.Errorf("empty config env = %v, want none", env)
	}
}
