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
}

// TestSnapshotManagerNotReadyByDefault verifies that a freshly constructed
// SnapshotManager reports Ready() == false before CreateGolden is called.
func TestSnapshotManagerNotReadyByDefault(t *testing.T) {
	sm := newTestSnapshotManager("/tmp/ix-snapshots/not-ready")
	if sm.Ready() {
		t.Fatal("SnapshotManager should not be ready before CreateGolden is called")
	}
}

// TestCopyRootfs verifies that copyRootfs produces a byte-for-byte copy of the
// source file.
func TestCopyRootfs(t *testing.T) {
	// Create a temp source file with known content.
	src, err := os.CreateTemp(t.TempDir(), "rootfs-src-*.ext4")
	if err != nil {
		t.Fatalf("create src file: %v", err)
	}
	defer src.Close()

	content := []byte("fake rootfs image content for testing 1234567890")
	if _, err := src.Write(content); err != nil {
		t.Fatalf("write src content: %v", err)
	}
	src.Close()

	dst := filepath.Join(t.TempDir(), "rootfs-dst.ext4")

	if err := copyRootfs(src.Name(), dst); err != nil {
		t.Fatalf("copyRootfs: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst file: %v", err)
	}

	if string(got) != string(content) {
		t.Errorf("dst content mismatch:\n  got:  %q\n  want: %q", got, content)
	}
}

// TestCopyRootfsSrcMissing verifies that copyRootfs returns an error when the
// source file does not exist.
func TestCopyRootfsSrcMissing(t *testing.T) {
	src := "/nonexistent/path/that/does/not/exist.ext4"
	dst := filepath.Join(t.TempDir(), "rootfs-dst.ext4")

	if err := copyRootfs(src, dst); err == nil {
		t.Fatal("copyRootfs should return an error for a missing source file")
	}
}
