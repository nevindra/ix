//go:build !integration

package ix

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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

