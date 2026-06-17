package ix

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHealth_RuntimeAndPoolFields(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	fcbin := filepath.Join(dir, "firecracker")
	if err := os.WriteFile(kernel, []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfs, []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fcbin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &IXManager{
		cfg: ManagerConfig{
			KernelPath:    kernel,
			RootfsImage:   rootfs,
			FCBinary:      fcbin,
			PoolSize:      2,
			UseSnapshot:   false,
			DefaultEgress: &EgressPolicy{Enabled: true, Mode: "allow", Rules: []string{"pypi.org", "*.github.com"}},
		},
		sandboxes: map[string]*IXSandbox{"s1": {}},
	}
	m.restartsTotal.Store(3)
	m.failuresTotal.Store(1)

	h, err := m.Health(context.Background())
	if err != nil {
		t.Fatalf("Health err = %v", err)
	}
	if h.Runtime.Backend != "firecracker" {
		t.Errorf("Backend = %q, want firecracker", h.Runtime.Backend)
	}
	if !h.Runtime.KernelOK || !h.Runtime.RootfsOK || !h.Runtime.FCBinaryOK {
		t.Errorf("runtime file checks should pass: %+v", h.Runtime)
	}
	if h.Pool.Configured != 2 || h.Pool.Active != 1 || h.Pool.Ready != 0 {
		t.Errorf("pool occupancy = %+v", h.Pool)
	}
	if h.Pool.Restarts != 3 || h.Pool.Failed != 1 {
		t.Errorf("pool counters = %+v", h.Pool)
	}
	if h.Egress.Mode != "allow" || h.Egress.RuleCount != 2 || !h.Egress.Enabled {
		t.Errorf("egress = %+v", h.Egress)
	}
	if h.Snapshot.Enabled || h.Snapshot.Ready {
		t.Errorf("snapshot should be disabled: %+v", h.Snapshot)
	}
}

func TestHealth_MissingFilesNotReady(t *testing.T) {
	m := &IXManager{
		cfg:       ManagerConfig{KernelPath: "/nope/vmlinux", RootfsImage: "/nope/rootfs", FCBinary: "/nope/fc"},
		sandboxes: map[string]*IXSandbox{},
	}
	h, err := m.Health(context.Background())
	if err != nil {
		t.Fatalf("Health err = %v", err)
	}
	if h.Ready {
		t.Error("Ready must be false when runtime files are missing")
	}
	if h.Runtime.KernelOK || h.Runtime.RootfsOK || h.Runtime.FCBinaryOK {
		t.Errorf("all file checks should be false: %+v", h.Runtime)
	}
}
