package ix

import (
	"log/slog"
	"net/http"
	"time"
)

// NewGatewayForBrowserVM constructs a Gateway wired to a browser-tier VM over
// vsock. vsockUDS is the path to the VM's vsock Unix-domain socket; the vsock
// transport handles the CONNECT handshake, so PinchtabBaseURL's host is
// irrelevant to routing but must be a valid HTTP URL.
//
// Typical production usage:
//
//	gw := NewGatewayForBrowserVM("/run/ix/browser-vm.sock", token, 32, logger)
//	http.ListenAndServe(":9000", gw.Handler())
func NewGatewayForBrowserVM(vsockUDS, pinchtabToken string, maxInflight int, logger *slog.Logger) *Gateway {
	client := &http.Client{
		Transport: vsockTransport(vsockUDS),
		// Timeout order along the capture chain must be strictly increasing so
		// the most informative error wins: pinchtab actionSec (60s, set by
		// browser-vm-init.sh) < this client (75s) < the daemon's capture
		// timeout (90s, ix-browser remote.rs). At 60s this tied pinchtab's
		// handler timeout exactly, racing it to a generic client abort.
		Timeout: 75 * time.Second,
	}
	return NewGateway(GatewayConfig{
		// The host "browser-vm" is ignored by the vsock transport's DialContext;
		// it must be a valid HTTP URL so url.Parse in upstreamJSON succeeds.
		PinchtabBaseURL: "http://browser-vm",
		PinchtabClient:  client,
		PinchtabToken:   pinchtabToken,
		MaxInflight:     maxInflight,
		Logger:          logger,
	})
}
