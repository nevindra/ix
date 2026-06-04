//go:build integration

package ix

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestPreflightGuestOverlayfs verifies the guest kernel has overlayfs compiled
// in (boot args use `nomodules`, so it must be built-in, CONFIG_OVERLAY_FS=y).
// Run BEFORE implementing the ro-rootfs change: after it, a kernel without
// overlayfs cannot boot at all.
func TestPreflightGuestOverlayfs(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:       rootfsImage(),
		KernelPath:        kernelPath(),
		FCBinary:          fcBinary(),
		DefaultTTL:        2 * time.Minute,
		DisableNetworking: true,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "preflight-ovl", TTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer sb.Close()

	// End-anchored: the /proc/filesystems line is "nodev\toverlay", so a
	// start anchor would never match; "overlay$" hits exactly the fs name.
	res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "grep -q 'overlay$' /proc/filesystems"})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("guest kernel lacks built-in overlayfs (CONFIG_OVERLAY_FS=y required); rebuild the kernel before proceeding (exit %d)", res.ExitCode)
	}
}

// TestPreflightRelativeDrivePath verifies Firecracker accepts a RELATIVE
// path_on_host for a drive, resolved against the process working directory.
// The whole snapshot-restore design depends on this (the relative path is
// recorded in snapshot.state and re-resolved per clone). No VM is booted —
// only the drive PUT is exercised.
func TestPreflightRelativeDrivePath(t *testing.T) {
	fcBin := fcBinary()
	if fcBin == "" {
		p, err := exec.LookPath("firecracker")
		if err != nil {
			t.Skip("firecracker not found; set IX_FC_BINARY")
		}
		fcBin = p
	}

	dir := t.TempDir()
	// The backing file just has to exist and be openable.
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	apiSock := filepath.Join(dir, "fc.sock")
	cmd := exec.Command(fcBin, "--api-sock", apiSock)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	if err := waitForFile(apiSock, 5*time.Second); err != nil {
		t.Fatalf("firecracker API socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := fcPut(ctx, fcAPIClient(apiSock), "/drives/test", map[string]any{
		"drive_id":       "test",
		"path_on_host":   "disk.img", // RELATIVE — must resolve against cmd.Dir
		"is_root_device": false,
		"is_read_only":   false,
	})
	if err != nil {
		t.Fatalf("Firecracker rejected a relative drive path_on_host: %v\n"+
			"FALLBACK REQUIRED: per-VM mount namespaces (see spec, Risks #1)", err)
	}
}
