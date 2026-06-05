//go:build !integration

package ix

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestSnapshotManager returns a SnapshotManager wired to a minimal
// firecrackerBackend.  Nothing is started; it is only used for structural tests.
func newTestSnapshotManager(snapshotDir string) *SnapshotManager {
	backend := &firecrackerBackend{
		fcBinary:    "/usr/bin/firecracker",
		kernelPath:  "/opt/ix/vmlinux.bin",
		rootfsImage: "/opt/ix/rootfs/base.ext4",
		logger:      slog.Default(),
	}
	return NewSnapshotManager(
		snapshotDir,
		"/opt/ix/rootfs/base.ext4",
		backend,
		1,   // vcpus
		512, // memMB
		slog.Default(),
	)
}

// TestSnapshotManagerPaths verifies that statePath and memPath derive from
// snapshotDir with the expected filenames.
func TestSnapshotManagerPaths(t *testing.T) {
	dir := "/tmp/ix-snapshots/test"
	sm := newTestSnapshotManager(dir)

	wantState := filepath.Join(dir, "snapshot.state")
	wantMem := filepath.Join(dir, "snapshot.mem")

	if got := sm.statePath(); got != wantState {
		t.Errorf("statePath: got %q, want %q", got, wantState)
	}
	if got := sm.memPath(); got != wantMem {
		t.Errorf("memPath: got %q, want %q", got, wantMem)
	}

	wantScratch := filepath.Join(dir, "scratch.golden.ext4")
	if got := sm.scratchGoldenPath(); got != wantScratch {
		t.Errorf("scratchGoldenPath: got %q, want %q", got, wantScratch)
	}
}

// TestSnapshotManagerNotReadyByDefault verifies that a freshly constructed
// SnapshotManager reports Ready() == false before CreateGolden is called.
func TestSnapshotManagerNotReadyByDefault(t *testing.T) {
	sm := newTestSnapshotManager("/tmp/ix-snapshots/not-ready")
	if sm.Ready() {
		t.Fatal("SnapshotManager should not be ready before CreateGolden is called")
	}
}

// TestSnapshotManagerReadyRequiresGoldenScratch verifies the Ready() stat
// guard: the ready flag alone is not enough — the preserved golden scratch
// must exist on disk (protects against stale or partially deleted dirs).
func TestSnapshotManagerReadyRequiresGoldenScratch(t *testing.T) {
	dir := t.TempDir()
	sm := newTestSnapshotManager(dir)

	sm.mu.Lock()
	sm.ready = true
	sm.mu.Unlock()

	if sm.Ready() {
		t.Fatal("Ready() must be false while scratch.golden.ext4 is missing")
	}

	if err := os.WriteFile(sm.scratchGoldenPath(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !sm.Ready() {
		t.Fatal("Ready() must be true once the ready flag is set and the golden scratch exists")
	}
}

func TestScratchPoolFillTakeRefill(t *testing.T) {
	dir := t.TempDir()
	// backend.runDir == dir so pool files and rename destinations share the
	// same filesystem (avoiding cross-device EXDEV errors from os.Rename).
	sm := &SnapshotManager{
		snapshotDir: dir,
		backend:     &firecrackerBackend{runDir: dir},
		logger:      slog.Default(),
	}
	if err := os.WriteFile(sm.scratchGoldenPath(), []byte("golden"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.startScratchPool()
	defer sm.StopScratchPool()

	take := func(name string) string {
		// Rename destination must be in dir (same filesystem as pool files).
		dst := filepath.Join(dir, name)
		deadline := time.Now().Add(5 * time.Second)
		for !sm.takePooledScratch(dst) {
			if time.Now().After(deadline) {
				t.Fatalf("pool never produced a scratch for %s", name)
			}
			time.Sleep(2 * time.Millisecond)
		}
		return dst
	}

	dst := take("clone-a.ext4")
	if b, _ := os.ReadFile(dst); string(b) != "golden" {
		t.Errorf("pooled scratch content = %q, want golden copy", b)
	}
	// Pool refills after consumption.
	take("clone-b.ext4")
}

// TestScratchPoolStopIsConcurrencySafe verifies that takePooledScratch and
// StopScratchPool (including a second idempotent call) can be called
// concurrently without data races, panics, or deadlocks. Run with -race.
func TestScratchPoolStopIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	sm := &SnapshotManager{
		snapshotDir: dir,
		backend:     &firecrackerBackend{runDir: dir},
		logger:      slog.Default(),
	}
	if err := os.WriteFile(sm.scratchGoldenPath(), []byte("golden"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.startScratchPool()

	const goroutines = 3
	const iters = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				dst := filepath.Join(dir, fmt.Sprintf("concurrent-%d-%d.ext4", g, i))
				sm.takePooledScratch(dst)
				// Clean up so the temp dir doesn't fill up.
				_ = os.Remove(dst)
			}
		}()
	}

	// Stop the pool while the goroutines are concurrently taking from it.
	sm.StopScratchPool()

	wg.Wait()

	// Second call must be safe (idempotency — must not panic with "close of
	// closed channel").
	sm.StopScratchPool()
}

func TestTakePooledScratchDisabledPool(t *testing.T) {
	sm := &SnapshotManager{snapshotDir: t.TempDir(), logger: slog.Default()}
	if sm.takePooledScratch(filepath.Join(t.TempDir(), "x.ext4")) {
		t.Error("takePooledScratch must return false when the pool was never started")
	}
}

