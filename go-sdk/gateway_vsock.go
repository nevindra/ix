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
		Timeout:   60 * time.Second,
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
