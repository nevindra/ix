//go:build integration

package ix

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// Volume lifecycle (PRD volumes.md R6). Needs a rootfs whose ix-stage0 mounts
// /dev/vdc at IX_VOLUME_PATH; on an older rootfs the mountpoint check fails
// first with a clear message. vsock-only: no host NAT, so no root needed.
func newVolumeManager(t *testing.T) *IXManager {
	t.Helper()
	if rootfsImage() == "" || kernelPath() == "" {
		t.Skip("set IX_ROOTFS_IMAGE, IX_KERNEL_PATH, IX_FC_BINARY to run")
	}
	mgr, err := NewManager(context.Background(), ManagerConfig{
		RootfsImage:       rootfsImage(),
		KernelPath:        kernelPath(),
		FCBinary:          fcBinary(),
		DefaultTTL:        2 * time.Minute,
		DisableNetworking: true,
		VolumeSizeMB:      256,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func TestVolumeSurvivesDestroy(t *testing.T) {
	ctx := context.Background()
	mgr := newVolumeManager(t)
	const key = "it-volume"
	t.Cleanup(func() { _ = mgr.DeleteVolume(key) })

	sb, err := mgr.CreateWithVolume(ctx, sandbox.CreateOpts{SessionID: "vol-a"}, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "mountpoint -q /data && echo mounted"}); err != nil || !strings.Contains(res.Output, "mounted") {
		t.Fatalf("/data not a mountpoint (rootfs stage0 too old?): err=%v output=%q", err, res.Output)
	}
	if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{Path: "/data/marker.txt", Content: "run 1"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := mgr.Destroy(ctx, "vol-a"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(mgr.volumeFile(key)); err != nil {
		t.Fatalf("volume file gone after destroy: %v", err)
	}

	sb2, err := mgr.CreateWithVolume(ctx, sandbox.CreateOpts{SessionID: "vol-b"}, key)
	if err != nil {
		t.Fatalf("create again: %v", err)
	}
	got, err := sb2.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/data/marker.txt"})
	if err != nil {
		t.Fatalf("read marker on second attach: %v", err)
	}
	if !strings.Contains(got.Content, "run 1") {
		t.Fatalf("marker lost: %q", got.Content)
	}
	_ = mgr.Destroy(ctx, "vol-b")
}

func TestVolumeBusyAndDelete(t *testing.T) {
	ctx := context.Background()
	mgr := newVolumeManager(t)
	const key = "it-volume-busy"
	t.Cleanup(func() { _ = mgr.DeleteVolume(key) })

	sb, err := mgr.CreateWithVolume(ctx, sandbox.CreateOpts{SessionID: "busy-a"}, key)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := mgr.CreateWithVolume(ctx, sandbox.CreateOpts{SessionID: "busy-b"}, key); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("second attach: got %v, want ErrVolumeBusy", err)
	}
	if err := mgr.DeleteVolume(key); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("delete while attached: got %v", err)
	}
	if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{Path: "/data/marker.txt", Content: "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := mgr.Destroy(ctx, "busy-a"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	vols, err := mgr.ListVolumes()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range vols {
		if v.Key == key {
			found = true
			if v.Attached {
				t.Fatal("still attached after destroy")
			}
		}
	}
	if !found {
		t.Fatalf("volume not listed: %+v", vols)
	}
	if err := mgr.DeleteVolume(key); err != nil {
		t.Fatalf("delete: %v", err)
	}

	sb3, err := mgr.CreateWithVolume(ctx, sandbox.CreateOpts{SessionID: "busy-c"}, key)
	if err != nil {
		t.Fatalf("create after delete: %v", err)
	}
	if _, err := sb3.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/data/marker.txt"}); err == nil {
		t.Fatal("marker survived DeleteVolume; expected an empty volume")
	}
	_ = mgr.Destroy(ctx, "busy-c")
}
