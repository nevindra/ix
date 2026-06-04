//go:build !integration

package ix

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCopySparse verifies a byte-for-byte copy (sparseness is best-effort via
// cp flags; content equality is the contract).
func TestCopySparse(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.ext4")
	content := []byte("fake image content 1234567890")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "dst.ext4")
	if err := copySparse(src, dst); err != nil {
		t.Fatalf("copySparse: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q want %q", got, content)
	}
}

func TestCopySparseSrcMissing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst.ext4")
	if err := copySparse("/nonexistent/src.ext4", dst); err == nil {
		t.Fatal("expected error for missing source")
	}
}

// TestEnsureScratchTemplate verifies the template is created sparse at the
// requested size, formatted ext4, and that an existing template is preserved.
func TestEnsureScratchTemplate(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}

	path := filepath.Join(t.TempDir(), "scratch-template.ext4")
	const sizeMB = 16

	if err := ensureScratchTemplate(path, sizeMB); err != nil {
		t.Fatalf("ensureScratchTemplate: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != sizeMB<<20 {
		t.Errorf("size = %d, want %d", st.Size(), int64(sizeMB)<<20)
	}

	// ext4 superblock magic 0xEF53 at offset 1024+56.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1024+56); err != nil {
		t.Fatal(err)
	}
	if magic[0] != 0x53 || magic[1] != 0xEF {
		t.Errorf("not an ext4 image: magic = %x", magic)
	}

	// Idempotent: second call must not recreate the file. Plant a sentinel
	// byte and verify it survives (deterministic, unlike ModTime comparison).
	wf, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.WriteAt([]byte{0xAA}, 0); err != nil {
		wf.Close()
		t.Fatal(err)
	}
	wf.Close()
	if err := ensureScratchTemplate(path, sizeMB); err != nil {
		t.Fatalf("second ensureScratchTemplate: %v", err)
	}
	sentinel := make([]byte, 1)
	if _, err := f.ReadAt(sentinel, 0); err != nil {
		t.Fatal(err)
	}
	if sentinel[0] != 0xAA {
		t.Error("template was recreated; expected idempotent no-op")
	}
}
