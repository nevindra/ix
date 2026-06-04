package ix

import (
	"fmt"
	"os"
	"os/exec"
)

// scratchFileName is the per-VM scratch disk file inside each VM's socket dir.
// Registered with Firecracker as a RELATIVE path so it resolves against the
// Firecracker process working directory (the socket dir) — the same mechanism
// as the relative vsock UDS path. Snapshot restore depends on this: the
// relative path baked into snapshot.state re-resolves per clone.
const scratchFileName = "scratch.ext4"

// copySparse copies src to dst preserving sparseness, using reflink (CoW)
// where the host filesystem supports it. Used for per-VM scratch disks and
// the golden snapshot's scratch template.
func copySparse(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s → %s: %w: %s", src, dst, err, out)
	}
	return nil
}

// ensureScratchTemplate creates an empty sparse ext4 image of sizeMB at path
// if one does not already exist. Idempotent: an existing template is left
// untouched. The template is built once per manager start and copied (sparse)
// per VM, which is much cheaper than running mkfs.ext4 on every boot.
func ensureScratchTemplate(path string, sizeMB int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	// Per-PID tmp name: two processes racing on the same template each build
	// their own file and the atomic rename below is last-writer-wins — both
	// produce an identical empty template, so either winning is fine. A stale
	// tmp from a crashed process is orphaned but harmless (and tiny: sparse).
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create scratch template: %w", err)
	}
	if err := f.Truncate(sizeMB << 20); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("truncate scratch template: %w", err)
	}
	f.Close()

	if out, err := exec.Command("mkfs.ext4", "-F", "-q", tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4 scratch template: %w: %s", err, out)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize scratch template: %w", err)
	}
	return nil
}
