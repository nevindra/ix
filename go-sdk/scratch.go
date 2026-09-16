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
	// Scratch is ephemeral by design (per-VM, deleted on destroy): no journal.
	return mkfsSparse(path, sizeMB, false)
}

// mkfsSparse creates a sparse ext4 image at path via a temp file + rename, so
// a crash mid-mkfs never leaves a half-formatted image at the final path.
func mkfsSparse(path string, sizeMB int64, journal bool) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	if err := f.Truncate(sizeMB << 20); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("truncate image: %w", err)
	}
	f.Close()

	args := []string{"-F", "-q"}
	if !journal {
		args = append(args, "-O", "^has_journal")
	}
	args = append(args, tmp)
	if out, err := exec.Command("mkfs.ext4", args...).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize image: %w", err)
	}
	return nil
}
