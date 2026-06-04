//go:build integration

package ix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// hashFile returns the sha256 of a file. Used to prove the shared rootfs
// image is bit-identical before and after VM activity.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestRootfsImmutableUnderConcurrentWrites is the corruption-regression test
// for the shared-writable-rootfs bug: N concurrent VMs hammer /workspace and
// the shared base image must come out bit-identical.
func TestRootfsImmutableUnderConcurrentWrites(t *testing.T) {
	before := hashFile(t, rootfsImage())

	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	const vms = 2
	type res struct {
		id  string
		err error
	}
	done := make(chan res, vms)
	for i := 0; i < vms; i++ {
		go func(i int) {
			id := fmt.Sprintf("iso-writer-%d", i)
			sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: id, TTL: 3 * time.Minute})
			if err != nil {
				done <- res{id, fmt.Errorf("create: %w", err)}
				return
			}
			defer sb.Close()
			// Write through the page cache and force writeback so the bytes
			// actually reach the block device (the old bug only manifests
			// after writeback).
			r, err := sb.Shell(ctx, sandbox.ShellRequest{
				Command: "dd if=/dev/urandom of=/workspace/blob bs=1M count=64 2>/dev/null && sync && echo OK",
			})
			if err != nil {
				done <- res{id, fmt.Errorf("shell: %w", err)}
				return
			}
			if r.ExitCode != 0 || !strings.Contains(r.Output, "OK") {
				done <- res{id, fmt.Errorf("write failed (exit %d): %s", r.ExitCode, r.Output)}
				return
			}
			done <- res{id, nil}
		}(i)
	}
	for i := 0; i < vms; i++ {
		r := <-done
		if r.err != nil {
			t.Fatalf("%s: %v", r.id, r.err)
		}
		_ = mgr.Destroy(ctx, r.id)
	}

	after := hashFile(t, rootfsImage())
	if before != after {
		t.Fatalf("SHARED ROOTFS MUTATED: sha256 %s → %s — VM writes reached the base image", before, after)
	}
}

// TestWorkspaceIsolation: one VM's /workspace files must be invisible to
// another VM, and / must actually be the overlay (not raw vda).
func TestWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	sbA, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "iso-a", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	defer sbA.Close()
	sbB, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "iso-b", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	defer sbB.Close()

	// Root must be an overlay mount.
	r, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "awk '$2==\"/\"{print $3}' /proc/mounts"})
	if err != nil {
		t.Fatalf("mounts: %v", err)
	}
	if !strings.Contains(r.Output, "overlay") {
		t.Fatalf("guest / is not overlayfs: %q", r.Output)
	}

	if _, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "echo tenant-a-secret > /workspace/secret.txt && sync"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	// ls only — a `cat` error message would itself contain "secret.txt" and
	// false-positive the substring assertion below.
	rb, err := sbB.Shell(ctx, sandbox.ShellRequest{Command: "ls /workspace/"})
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if strings.Contains(rb.Output, "tenant-a-secret") || strings.Contains(rb.Output, "secret.txt") {
		t.Fatalf("ISOLATION BREACH: VM B sees VM A's workspace: %s", rb.Output)
	}
}

// TestSnapshotCloneIsolation: snapshot-restored clones must not corrupt the
// base image nor see each other's writes. This exercises the relative
// "scratch.ext4" path recorded in snapshot.state.
func TestSnapshotCloneIsolation(t *testing.T) {
	before := hashFile(t, rootfsImage())

	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
		UseSnapshot: true,
		SnapshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// CreateGolden runs async from NewManager; wait for it.
	deadline := time.Now().Add(3 * time.Minute)
	for !mgr.vmm.snapshot.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("golden snapshot not ready within 3m")
		}
		time.Sleep(time.Second)
	}

	sbA, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "clone-a", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create clone A: %v", err)
	}
	defer sbA.Close()
	sbB, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "clone-b", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create clone B: %v", err)
	}
	defer sbB.Close()

	if _, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "echo clone-a-data > /workspace/a.txt && sync"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	rb, err := sbB.Shell(ctx, sandbox.ShellRequest{Command: "ls /workspace/"})
	if err != nil {
		t.Fatalf("ls B: %v", err)
	}
	if strings.Contains(rb.Output, "a.txt") {
		t.Fatalf("CLONE ISOLATION BREACH: clone B sees clone A's scratch: %s", rb.Output)
	}

	after := hashFile(t, rootfsImage())
	if before != after {
		t.Fatalf("SHARED ROOTFS MUTATED by snapshot clones: %s → %s", before, after)
	}
}
