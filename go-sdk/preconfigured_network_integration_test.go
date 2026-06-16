//go:build integration

package ix

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestPreconfiguredManagerBoots is a sudo-gated acceptance test that proves
// the manager can boot a real VM as a regular (non-root) user when the host
// network has been pre-provisioned by ix-host-setup.
//
// One-time setup (run as root, once per host):
//
//	sudo go-sdk/scripts/ix-host-setup.sh --taps 4 --user "$USER"
//
// Then, as the regular user:
//
//	IX_PRECONFIGURED_NETWORK_TEST=1 \
//	  IX_ROOTFS_IMAGE=<path/to/base.ext4> \
//	  IX_KERNEL_PATH=<path/to/vmlinux.bin> \
//	  go test -tags=integration -run TestPreconfiguredManagerBoots ./...
func TestPreconfiguredManagerBoots(t *testing.T) {
	if os.Getenv("IX_PRECONFIGURED_NETWORK_TEST") != "1" {
		t.Skip("set IX_PRECONFIGURED_NETWORK_TEST=1 after running ix-host-setup as root")
	}
	rootfs := rootfsImage()
	kernel := kernelPath()
	if rootfs == "" || kernel == "" {
		t.Skip("set IX_ROOTFS_IMAGE and IX_KERNEL_PATH")
	}
	if os.Geteuid() == 0 {
		t.Fatal("this test must run as a regular (non-root) user to prove rootless operation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:          rootfs,
		KernelPath:           kernel,
		PreconfiguredNetwork: true,
		DefaultTTL:           2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager (preconfigured, non-root): %v", err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{
		SessionID: "preconfigured-net-test",
		TTL:       2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer sb.Close()

	res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo preconfigured-ok"})
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("shell exit %d, output=%s", res.ExitCode, res.Output)
	}
	t.Logf("shell output: %s", res.Output)
}
