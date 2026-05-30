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
