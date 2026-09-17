//go:build !integration

package ix

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newVolumeTestManager(t *testing.T) *IXManager {
	t.Helper()
	cfg := ManagerConfig{RunDir: t.TempDir()}
	cfg.applyDefaults()
	return &IXManager{cfg: cfg, logger: slog.Default()}
}

func TestValidateVolumeKey(t *testing.T) {
	for _, ok := range []string{"a", "repo-1", "sha256.abc_DEF", strings.Repeat("x", 64)} {
		if err := validateVolumeKey(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "a b", strings.Repeat("x", 65), "é"} {
		if err := validateVolumeKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateVolumePath(t *testing.T) {
	for _, ok := range []string{"/data", "/mnt/repo", "/workspace2"} {
		if err := validateVolumePath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "data", "/", "/workspace", "/workspace/", "/workspace/repo", "/workspace/../workspace/x"} {
		if err := validateVolumePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestVolumeDefaults(t *testing.T) {
	cfg := ManagerConfig{}
	cfg.applyDefaults()
	if cfg.VolumeSizeMB != 20480 || cfg.VolumePath != "/data" {
		t.Errorf("defaults: size=%d path=%q", cfg.VolumeSizeMB, cfg.VolumePath)
	}
}

func TestVolumeAttachExclusive(t *testing.T) {
	m := newVolumeTestManager(t)
	if err := m.attachVolume("k", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := m.attachVolume("k", "s2"); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("second attach: got %v, want ErrVolumeBusy", err)
	}
	if err := m.DeleteVolume("k"); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("delete while attached: got %v", err)
	}
	m.releaseVolume("k")
	if err := m.attachVolume("k", "s2"); err != nil {
		t.Fatalf("attach after release: %v", err)
	}
	m.releaseVolume("") // no-op must not panic
}

func TestDeleteVolumeUnknownKeyIsNotAnError(t *testing.T) {
	m := newVolumeTestManager(t)
	if err := m.DeleteVolume("missing"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteVolume("../etc"); err == nil {
		t.Fatal("traversal key accepted")
	}
}

func TestVolumeDriveEnv(t *testing.T) {
	m := newVolumeTestManager(t)
	drives, env := m.volumeDrive("")
	if drives != nil || env != nil {
		t.Fatal("empty key must yield no drive")
	}
	drives, env = m.volumeDrive("k")
	if len(drives) != 1 || drives[0]["drive_id"] != "volume" || drives[0]["is_read_only"] != false {
		t.Fatalf("drive: %+v", drives)
	}
	if drives[0]["path_on_host"] != filepath.Join(m.cfg.RunDir, "volumes", "k.ext4") {
		t.Fatalf("path: %v", drives[0]["path_on_host"])
	}
	if env[volumeEnvKey] != "/data" {
		t.Fatalf("env: %v", env)
	}
}

func writeFakeVolume(t *testing.T, m *IXManager, key string, mtime time.Time) {
	t.Helper()
	p := m.volumeFile(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestListVolumesOldestFirstAndAttachedFlag(t *testing.T) {
	m := newVolumeTestManager(t)
	if vols, err := m.ListVolumes(); err != nil || vols != nil {
		t.Fatalf("no dir: %v %v", vols, err)
	}
	now := time.Now()
	writeFakeVolume(t, m, "new", now)
	writeFakeVolume(t, m, "old", now.Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(m.volumeDir(), "junk.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = m.attachVolume("new", "s")

	vols, err := m.ListVolumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 || vols[0].Key != "old" || vols[1].Key != "new" {
		t.Fatalf("order: %+v", vols)
	}
	if vols[0].Attached || !vols[1].Attached {
		t.Fatalf("attached flags: %+v", vols)
	}
}

func TestEvictOldestVolumeSkipsAttached(t *testing.T) {
	m := newVolumeTestManager(t)
	now := time.Now()
	writeFakeVolume(t, m, "attached-old", now.Add(-2*time.Hour))
	writeFakeVolume(t, m, "free-mid", now.Add(-time.Hour))
	writeFakeVolume(t, m, "free-new", now)
	_ = m.attachVolume("attached-old", "s")

	if !m.evictOldestVolume() {
		t.Fatal("expected an eviction")
	}
	if _, err := os.Stat(m.volumeFile("free-mid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("free-mid should be gone")
	}
	for _, k := range []string{"attached-old", "free-new"} {
		if _, err := os.Stat(m.volumeFile(k)); err != nil {
			t.Fatalf("%s should survive: %v", k, err)
		}
	}
	if !m.evictOldestVolume() {
		t.Fatal("second eviction expected")
	}
	if m.evictOldestVolume() {
		t.Fatal("only the attached volume remains; nothing to evict")
	}
}

func TestEnsureVolumeHasJournalAndIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	path := filepath.Join(t.TempDir(), "volumes", "k.ext4")
	if err := ensureVolume(path, 64); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 64<<20 {
		t.Fatalf("size %d", fi.Size())
	}
	before := fi.ModTime()
	if err := ensureVolume(path, 128); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(path); fi2.Size() != 64<<20 || !fi2.ModTime().Equal(before) {
		t.Fatal("ensureVolume recreated an existing volume")
	}

	if _, err := exec.LookPath("dumpe2fs"); err != nil {
		t.Skip("dumpe2fs not installed")
	}
	out, err := exec.Command("dumpe2fs", "-h", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "has_journal") {
		t.Error("volume has no journal — it must survive a power-cut destroy")
	}
}
