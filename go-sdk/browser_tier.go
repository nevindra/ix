package ix

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// defaultGatewayListenAddr is the host address the browser-tier Gateway binds
// by default — a link-local address reachable from guests via their per-VM TAP
// route (the manager pins it on the ixgw0 dummy interface). Shared by
// applyDefaults and gatewayURLFromAddr so the two never silently diverge.
const defaultGatewayListenAddr = "169.254.0.1:9100"

// defaultGatewayToken is the fixed internal token shared by the host Gateway and
// the guest pinchtab (browser-vm-init defaults to the same value) when no
// operator token is set. pinchtab rejects an empty token, so a default is
// required for zero-config boot. Not externally reachable (link-local + vsock).
const defaultGatewayToken = "ix-internal"

// browserTier owns the shared browser-tier VM and its Gateway HTTP server.
// The TCP listener is owned by server: http.Server.Shutdown closes it, so it is
// not retained separately.
type browserTier struct {
	vmm     *VMMHandle
	gateway *Gateway
	server  *http.Server
}

// gatewayURLFromAddr turns a bind address into the URL per-chat daemons use.
// Empty addr falls back to the default bind.
func gatewayURLFromAddr(addr string) string {
	if addr == "" {
		addr = defaultGatewayListenAddr
	}
	return "http://" + addr
}

// browserTierEnv builds the env slice passed to the browser-tier VM
// (surfaced to the guest as ix.env.* kernel cmdline params, parsed by
// browser-vm-init into pinchtab's config).
func browserTierEnv(cfg ManagerConfig) []string {
	var env []string
	if cfg.GatewayToken != "" {
		env = append(env, "PINCHTAB_TOKEN="+cfg.GatewayToken)
	}
	if len(cfg.BrowserTrustedResolveCIDRs) > 0 {
		env = append(env, "PINCHTAB_TRUSTED_RESOLVE_CIDRS="+strings.Join(cfg.BrowserTrustedResolveCIDRs, ","))
	}
	return env
}

// startBrowserTier boots the browser-tier VM, waits for pinchtab to become
// healthy over vsock (no READY handshake — pinchtab doesn't send one), then
// serves the Gateway on cfg.GatewayListenAddr. On success it returns a
// browserTier and the gateway URL per-chat daemons should use.
func startBrowserTier(ctx context.Context, fb *firecrackerBackend, cfg ManagerConfig) (*browserTier, string, error) {
	if cfg.BrowserVMImage == "" {
		return nil, "", fmt.Errorf("BrowserMode=remote requires BrowserVMImage")
	}

	// Optional persistent state disk as a second drive.
	var extra []driveSpec
	if cfg.BrowserStateImage != "" {
		extra = append(extra, buildDriveSpec("state", cfg.BrowserStateImage, false))
	}

	handle, err := fb.startVMCold(ctx, "browser-tier", 2, cfg.BrowserVMMemoryMB, cfg.BrowserVMImage, browserTierEnv(cfg), extra, false)
	if err != nil {
		return nil, "", fmt.Errorf("boot browser-tier VM: %w", err)
	}

	// Wait for pinchtab /health via the vsock transport. pinchtab requires the
	// Bearer token on /health, and Chrome cold-start can exceed the snapshot 10 s
	// budget, so use a longer authenticated wait.
	guestHTTP := &http.Client{Transport: vsockTransport(handle.VsockPath), Timeout: 2 * time.Second}
	if err := waitHealthyAuth(ctx, guestHTTP, cfg.GatewayToken, 60*time.Second); err != nil {
		fb.cleanup(handle)
		return nil, "", fmt.Errorf("browser-tier pinchtab health: %w", err)
	}

	gw := NewGatewayForBrowserVM(handle.VsockPath, cfg.GatewayToken, cfg.MaxInflightOrDefault(), cfg.Logger)
	gw.Start(ctx)

	ln, err := net.Listen("tcp", cfg.GatewayListenAddr)
	if err != nil {
		gw.Stop()
		fb.cleanup(handle)
		return nil, "", fmt.Errorf("gateway listen %s: %w", cfg.GatewayListenAddr, err)
	}
	// Derive the URL from the actual bound address so an ephemeral port (":0")
	// resolves to the real port that per-chat daemons must dial.
	boundAddr := ln.Addr().String()
	srv := &http.Server{Handler: gw.Handler()}
	go func() {
		// ErrServerClosed is the normal shutdown signal; anything else means the
		// gateway stopped serving unexpectedly and per-chat browser calls will fail.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cfg.Logger.Error("browser-tier gateway server stopped", "error", err)
		}
	}()

	cfg.Logger.Info("browser tier up", "vm_cid", handle.CID, "gateway", boundAddr)
	return &browserTier{vmm: handle, gateway: gw, server: srv}, gatewayURLFromAddr(boundAddr), nil
}

func (t *browserTier) stop(fb *firecrackerBackend) {
	if t == nil {
		return
	}
	if t.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = t.server.Shutdown(ctx)
		cancel()
	}
	if t.gateway != nil {
		t.gateway.Stop()
	}
	if t.vmm != nil {
		fb.cleanup(t.vmm)
	}
}
