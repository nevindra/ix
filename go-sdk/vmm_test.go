//go:build !integration

package ix

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFirecrackerConfigValidation(t *testing.T) {
	t.Run("missing kernel path fails", func(t *testing.T) {
		cfg := FirecrackerConfig{
			Binary:      "/usr/bin/firecracker",
			KernelPath:  "",
			RootfsImage: "/opt/ix/rootfs/base.ext4",
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing kernel path")
		}
	})

	t.Run("missing rootfs image fails", func(t *testing.T) {
		cfg := FirecrackerConfig{
			Binary:      "/usr/bin/firecracker",
			KernelPath:  "/opt/ix/firecracker/vmlinux.bin",
			RootfsImage: "",
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing rootfs image")
		}
	})

	t.Run("valid config passes", func(t *testing.T) {
		cfg := FirecrackerConfig{
			Binary:      "/usr/bin/firecracker",
			KernelPath:  "/opt/ix/firecracker/vmlinux.bin",
			RootfsImage: "/opt/ix/rootfs/base.ext4",
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestBuildKernelBootArgs(t *testing.T) {
	env := []string{"IX_EGRESS_ENABLED=true", "IX_EGRESS_MODE=allow"}
	args := buildKernelBootArgs(env, nil, false)

	if args == "" {
		t.Fatal("expected non-empty boot args")
	}

	// Must contain the stage-0 init path (overlay/pivot pre-init).
	if !strings.Contains(args, "init=/sbin/ix-stage0") {
		t.Errorf("boot args missing stage-0 init path: %s", args)
	}

	// Root must be mounted READ-ONLY: the rootfs image is shared by all VMs.
	if !strings.Contains(args, " ro ") {
		t.Errorf("boot args missing ro root flag: %s", args)
	}
	if strings.Contains(args, " rw ") {
		t.Errorf("boot args must not mount root rw: %s", args)
	}

	// Console must be absent by default (withConsole=false).
	if strings.Contains(args, "console=ttyS0") {
		t.Errorf("boot args must not contain console=ttyS0 with withConsole=false: %s", args)
	}

	// Env vars must appear as ix.env.* entries.
	if !strings.Contains(args, "ix.env.IX_EGRESS_ENABLED=true") {
		t.Errorf("boot args missing ix.env.IX_EGRESS_ENABLED=true: %s", args)
	}
	if !strings.Contains(args, "ix.env.IX_EGRESS_MODE=allow") {
		t.Errorf("boot args missing ix.env.IX_EGRESS_MODE=allow: %s", args)
	}
}

func TestBuildKernelBootArgsWithNet(t *testing.T) {
	vn := deriveVMNet(0) // host 172.16.0.1, guest 172.16.0.2
	args := buildKernelBootArgs(nil, &vn, false)
	want := "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off:8.8.8.8"
	if !strings.Contains(args, want) {
		t.Errorf("boot args missing %q\n%s", want, args)
	}
}

func TestBuildKernelBootArgsNoNet(t *testing.T) {
	args := buildKernelBootArgs(nil, nil, false)
	if strings.Contains(args, "ip=") {
		t.Errorf("boot args should have no ip= when net is nil: %s", args)
	}
}

func TestVMMHandleFields(t *testing.T) {
	h := VMMHandle{
		SocketDir: "/tmp/ix-test",
		VsockPath: "/tmp/ix-test/vsock.uds",
		APISocket: "/tmp/ix-test/fc.sock",
		CID:       3,
	}

	if h.SocketDir == "" || h.VsockPath == "" || h.APISocket == "" {
		t.Fatal("VMMHandle string fields must not be empty")
	}
	if h.CID < 3 {
		t.Fatalf("CID must be >= 3 (0-2 are reserved), got %d", h.CID)
	}
}

func TestAllocateCID(t *testing.T) {
	// Reset to a known state so this test is deterministic regardless of order.
	// We record the starting point and verify relative ordering.
	before := cidCounter.Load()

	first := allocateCID()
	second := allocateCID()
	third := allocateCID()

	// Each call must return strictly increasing values.
	if first <= before {
		t.Errorf("first CID %d must be > before %d", first, before)
	}
	if second != first+1 {
		t.Errorf("second CID %d must be first+1 (%d)", second, first+1)
	}
	if third != second+1 {
		t.Errorf("third CID %d must be second+1 (%d)", third, second+1)
	}
	// All must be >= 3 (the minimum valid CID).
	for _, cid := range []uint32{first, second, third} {
		if cid < 3 {
			t.Errorf("CID %d is below the minimum of 3", cid)
		}
	}

	// Concurrent safety: run many goroutines and verify no duplicates.
	const goroutines = 50
	results := make([]uint32, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = allocateCID()
		}()
	}
	wg.Wait()

	seen := make(map[uint32]bool, goroutines)
	for _, cid := range results {
		if seen[cid] {
			t.Errorf("duplicate CID allocated: %d", cid)
		}
		seen[cid] = true
	}
}

func TestBuildKernelBootArgsEmpty(t *testing.T) {
	args := buildKernelBootArgs(nil, nil, false)

	if args == "" {
		t.Fatal("expected non-empty boot args even with empty env slice")
	}

	// All base args must be present (console omitted: withConsole=false).
	for _, required := range []string{
		"reboot=k",
		"panic=1",
		"pci=off",
		"init=/sbin/ix-stage0",
	} {
		if !strings.Contains(args, required) {
			t.Errorf("boot args missing %q: %s", required, args)
		}
	}

	// Quiet mode: no console=ttyS0, but 8250.nr_uarts=0 and quiet must be present.
	if strings.Contains(args, "console=ttyS0") {
		t.Errorf("boot args must not contain console=ttyS0 with withConsole=false: %s", args)
	}

	// Default RUST_LOG=warn must be injected when no env is provided.
	if !strings.Contains(args, "ix.env.RUST_LOG=warn") {
		t.Errorf("boot args missing default ix.env.RUST_LOG=warn: %s", args)
	}
}

func TestBuildKernelBootArgsSpecialChars(t *testing.T) {
	// Env vars with equals signs in the value and spaces are passed through as-is.
	env := []string{
		"FOO=bar=baz",     // value contains '='
		"MSG=hello world", // value contains space
		"EMPTY=",          // empty value
	}
	args := buildKernelBootArgs(env, nil, false)

	if !strings.Contains(args, "ix.env.FOO=bar=baz") {
		t.Errorf("boot args missing ix.env.FOO=bar=baz: %s", args)
	}
	if !strings.Contains(args, "ix.env.MSG=hello world") {
		t.Errorf("boot args missing ix.env.MSG=hello world: %s", args)
	}
	if !strings.Contains(args, "ix.env.EMPTY=") {
		t.Errorf("boot args missing ix.env.EMPTY=: %s", args)
	}
}

func TestVMDir(t *testing.T) {
	fb := &firecrackerBackend{runDir: "/var/lib/ix/run"}
	if got := fb.vmDir("abc123"); got != "/var/lib/ix/run/ix-abc123" {
		t.Errorf("vmDir = %q, want /var/lib/ix/run/ix-abc123", got)
	}

	// Empty runDir falls back to os.TempDir() (bare test backends).
	fb2 := &firecrackerBackend{}
	got := fb2.vmDir("abc123")
	want := filepath.Join(os.TempDir(), "ix-abc123")
	if got != want {
		t.Errorf("vmDir fallback = %q, want %q", got, want)
	}
}

func TestBuildKernelBootArgsConsoleGating(t *testing.T) {
	quiet := buildKernelBootArgs(nil, nil, false)
	if strings.Contains(quiet, "console=ttyS0") {
		t.Errorf("console must be absent without IX_VM_CONSOLE: %s", quiet)
	}
	for _, want := range []string{"8250.nr_uarts=0", "quiet"} {
		if !strings.Contains(quiet, want) {
			t.Errorf("missing %q in quiet boot args: %s", want, quiet)
		}
	}

	loud := buildKernelBootArgs(nil, nil, true)
	if !strings.Contains(loud, "console=ttyS0") {
		t.Errorf("console must be present with IX_VM_CONSOLE: %s", loud)
	}
}

func TestBuildKernelBootArgsDefaultRustLog(t *testing.T) {
	args := buildKernelBootArgs([]string{"FOO=bar"}, nil, false)
	if !strings.Contains(args, "ix.env.RUST_LOG=warn") {
		t.Errorf("expected default RUST_LOG=warn: %s", args)
	}
	// Caller-provided RUST_LOG wins.
	args = buildKernelBootArgs([]string{"RUST_LOG=debug"}, nil, false)
	if strings.Contains(args, "ix.env.RUST_LOG=warn") {
		t.Errorf("default must not override caller RUST_LOG: %s", args)
	}
	if !strings.Contains(args, "ix.env.RUST_LOG=debug") {
		t.Errorf("caller RUST_LOG missing: %s", args)
	}
}

func TestBuildDriveSpec(t *testing.T) {
	spec := buildDriveSpec("state", "/var/lib/ix/state.ext4", false)
	if spec["drive_id"] != "state" {
		t.Errorf("drive_id = %v, want state", spec["drive_id"])
	}
	if spec["path_on_host"] != "/var/lib/ix/state.ext4" {
		t.Errorf("path_on_host = %v", spec["path_on_host"])
	}
	if spec["is_root_device"] != false {
		t.Errorf("is_root_device = %v, want false", spec["is_root_device"])
	}
	if spec["is_read_only"] != false {
		t.Errorf("is_read_only = %v, want false", spec["is_read_only"])
	}

	ro := buildDriveSpec("ro-disk", "/var/lib/ix/ro.ext4", true)
	if ro["is_read_only"] != true {
		t.Errorf("is_read_only = %v, want true", ro["is_read_only"])
	}
	if ro["is_root_device"] != false {
		t.Errorf("is_root_device = %v, want false", ro["is_root_device"])
	}
}

// TestCleanupTombstonesSocketDir: cleanup must fence the dir synchronously
// (rename — the sandbox path is immediately reusable) and delete it shortly
// after in the background.
func TestCleanupTombstonesSocketDir(t *testing.T) {
	base := t.TempDir()
	sd := filepath.Join(base, "ix-tombtest")
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "scratch.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	fb := &firecrackerBackend{}
	fb.cleanup(&VMMHandle{SocketDir: sd})

	// Synchronous guarantee: the original path is gone the moment cleanup returns.
	if _, err := os.Stat(sd); !os.IsNotExist(err) {
		t.Errorf("socket dir still present after cleanup: %v", err)
	}

	// Caller-visible latency: rename is O(1); a synchronous RemoveAll of a
	// populated dir is not. Populate with many files to make the difference
	// observable, then require cleanup to return fast.
	sd2 := filepath.Join(base, "ix-tombtest2")
	if err := os.MkdirAll(sd2, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(sd2, fmt.Sprintf("f%04d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	fb.cleanup(&VMMHandle{SocketDir: sd2})
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("cleanup blocked %v — delete must be async", elapsed)
	}

	// Async guarantee: all tombstones disappear shortly after.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(base)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tombstone never deleted: %v", entries)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
