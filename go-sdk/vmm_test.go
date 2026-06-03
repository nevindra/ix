//go:build !integration

package ix

import (
	"strings"
	"sync"
	"testing"
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
	args := buildKernelBootArgs(env, nil)

	if args == "" {
		t.Fatal("expected non-empty boot args")
	}

	// Must contain init path.
	if !strings.Contains(args, "init=/sbin/ix-init") {
		t.Errorf("boot args missing init path: %s", args)
	}

	// Must contain console spec.
	if !strings.Contains(args, "console=ttyS0") {
		t.Errorf("boot args missing console: %s", args)
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
	args := buildKernelBootArgs(nil, &vn)
	want := "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off:8.8.8.8"
	if !strings.Contains(args, want) {
		t.Errorf("boot args missing %q\n%s", want, args)
	}
}

func TestBuildKernelBootArgsNoNet(t *testing.T) {
	args := buildKernelBootArgs(nil, nil)
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
	args := buildKernelBootArgs(nil, nil)

	if args == "" {
		t.Fatal("expected non-empty boot args even with empty env slice")
	}

	// All base args must be present.
	for _, required := range []string{
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"pci=off",
		"init=/sbin/ix-init",
	} {
		if !strings.Contains(args, required) {
			t.Errorf("boot args missing %q: %s", required, args)
		}
	}

	// No ix.env. entries should appear.
	if strings.Contains(args, "ix.env.") {
		t.Errorf("boot args should not contain ix.env.* with empty env: %s", args)
	}
}

func TestBuildKernelBootArgsSpecialChars(t *testing.T) {
	// Env vars with equals signs in the value and spaces are passed through as-is.
	env := []string{
		"FOO=bar=baz",     // value contains '='
		"MSG=hello world", // value contains space
		"EMPTY=",          // empty value
	}
	args := buildKernelBootArgs(env, nil)

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
