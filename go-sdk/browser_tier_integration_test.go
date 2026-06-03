//go:build integration

package ix_test

import (
	"context"
	"os"
	"testing"
	"time"

	ix "github.com/nevindra/ix/go-sdk"
	"github.com/nevindra/oasis/sandbox"
)

// Requires env: SANDBOX_ROOTFS (base ext4), SANDBOX_KERNEL, SANDBOX_BROWSER_ROOTFS (browser-vm ext4).
func TestBrowserTierEndToEnd(t *testing.T) {
	base := os.Getenv("SANDBOX_ROOTFS")
	kernel := os.Getenv("SANDBOX_KERNEL")
	browserImg := os.Getenv("SANDBOX_BROWSER_ROOTFS")
	if base == "" || kernel == "" || browserImg == "" {
		t.Skip("set SANDBOX_ROOTFS, SANDBOX_KERNEL, SANDBOX_BROWSER_ROOTFS to run")
	}

	ctx := context.Background()
	mgr, err := ix.NewManager(ctx, ix.ManagerConfig{
		RootfsImage:       base,
		KernelPath:        kernel,
		BrowserMode:       "remote",
		BrowserVMImage:    browserImg,
		BrowserVMMemoryMB: 4096,
		// The gateway must bind the link-local IP the manager pins on ixgw0, not
		// loopback: a per-chat VM's 127.0.0.1 is its OWN loopback, so it could
		// never reach a host-loopback gateway. 169.254.0.1 is routable from the
		// guest via its per-VM TAP default route. (Also the applyDefaults value.)
		GatewayListenAddr: "169.254.0.1:9100",
	})
	if err != nil {
		t.Fatalf("NewManager (with browser tier): %v", err)
	}
	defer mgr.Close()

	yes := true
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-browser", TTL: 5 * time.Minute, Browser: &yes})
	if err != nil {
		t.Fatalf("create browser sandbox: %v", err)
	}
	if err := sb.BrowserNavigate(ctx, "https://example.com"); err != nil {
		t.Fatalf("navigate via shared tier: %v", err)
	}

	no := false
	light, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-light", TTL: 5 * time.Minute, Browser: &no})
	if err != nil {
		t.Fatalf("create light sandbox: %v", err)
	}
	info, err := light.WorkspaceInfo(ctx)
	if err != nil {
		t.Fatalf("workspace info: %v", err)
	}
	if info.Browser {
		t.Error("light sandbox should report Browser=false")
	}
}
