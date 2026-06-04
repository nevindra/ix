# Rootfs Isolation (RO Base + Per-VM Scratch) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **NO COMMITS:** Per the user's global preferences, do NOT run `git commit` at any point. Edit and stage files; leave the working tree dirty. The user reviews and commits batched changes themselves. Any "commit" steps you might expect from TDD cadence are intentionally absent.

**Goal:** Stop every Firecracker VM from mounting the shared `base.ext4` read-write (filesystem-corruption + cross-tenant-persistence bug) by attaching the rootfs read-only and giving each VM a private sparse scratch disk via a whole-root overlayfs.

**Architecture:** A new tier-agnostic PID-1 script `/sbin/ix-stage0` mounts `/dev/vdb` (per-VM scratch ext4), builds `overlayfs(lower=/, upper=scratch)`, pivots, and execs the unchanged per-tier init. The host attaches the rootfs with `is_read_only: true` and registers the scratch drive under the **relative** path `"scratch.ext4"` (resolved against Firecracker's CWD = the per-VM socket dir, same trick as `vsock.uds`), which makes snapshot restore work: each clone gets its own copy of the golden VM's scratch at the recorded relative path.

**Tech Stack:** Go (go-sdk), POSIX sh (guest init), Firecracker API, ext4/overlayfs, `cp --reflink=auto --sparse=always`.

**Spec:** `docs/superpowers/specs/2026-06-04-rootfs-isolation-design.md`

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `go-sdk/preflight_integration_test.go` | Create | Preflight: kernel overlayfs + FC relative drive path |
| `go-sdk/scratch.go` | Create | `copySparse`, `ensureScratchTemplate`, `scratchFileName` |
| `go-sdk/scratch_test.go` | Create | Unit tests for the above |
| `go-sdk/vmm.go` | Modify | Boot args `ro`+stage0; `runDir`/`scratchTemplate` backend fields; ro rootfs drive; scratch drive |
| `go-sdk/vmm_test.go` | Modify | Boot-arg expectations; `vmDir` test |
| `go-sdk/manager.go` | Modify | `RunDir`/`ScratchSizeMB` config + wiring |
| `go-sdk/manager_test.go` | Modify | Config default tests |
| `go-sdk/reaper.go` | Modify | `recover()` scans `RunDir` too |
| `go-sdk/snapshot.go` | Modify | Preserve golden scratch; per-clone scratch copy; delete `copyRootfs`; fix false comment |
| `go-sdk/snapshot_test.go` | Modify | Drop `copyRootfs` tests (moved); `scratchGoldenPath` test |
| `go-sdk/scripts/ix-stage0.sh` | Create | Guest stage-0 overlay/pivot init |
| `go-sdk/scripts/build-rootfs-ext4.sh` | Modify | Install stage0 in all tiers; create `/scratch` |
| `go-sdk/rootfs_isolation_integration_test.go` | Create | Corruption-regression + isolation + snapshot-clone tests |
| `docs/handbook/02-architecture.md`, `docs/handbook/05-operations.md`, `CLAUDE.md` | Modify | Document the new disk model + rollout |

---

### Task 1: Preflight verification tests

Verifies the two external assumptions **before** any production code changes. If either fails, STOP and report to the user (kernel rebuild or mount-namespace fallback per the spec's "Risks and fallbacks").

**Files:**
- Create: `go-sdk/preflight_integration_test.go`

- [ ] **Step 1: Write the preflight tests**

```go
//go:build integration

package ix

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestPreflightGuestOverlayfs verifies the guest kernel has overlayfs compiled
// in (boot args use `nomodules`, so it must be built-in, CONFIG_OVERLAY_FS=y).
// Run BEFORE implementing the ro-rootfs change: after it, a kernel without
// overlayfs cannot boot at all.
func TestPreflightGuestOverlayfs(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "preflight-ovl", TTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer sb.Close()

	res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "grep -c overlay /proc/filesystems"})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("guest kernel lacks built-in overlayfs (CONFIG_OVERLAY_FS=y required); rebuild the kernel before proceeding. output: %s", res.Output)
	}
}

// TestPreflightRelativeDrivePath verifies Firecracker accepts a RELATIVE
// path_on_host for a drive, resolved against the process working directory.
// The whole snapshot-restore design depends on this (the relative path is
// recorded in snapshot.state and re-resolved per clone). No VM is booted —
// only the drive PUT is exercised.
func TestPreflightRelativeDrivePath(t *testing.T) {
	fcBin := fcBinary()
	if fcBin == "" {
		p, err := exec.LookPath("firecracker")
		if err != nil {
			t.Skip("firecracker not found; set IX_FC_BINARY")
		}
		fcBin = p
	}

	dir := t.TempDir()
	// The backing file just has to exist and be openable.
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	apiSock := filepath.Join(dir, "fc.sock")
	cmd := exec.Command(fcBin, "--api-sock", apiSock)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	if err := waitForFile(apiSock, 5*time.Second); err != nil {
		t.Fatalf("firecracker API socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := fcPut(ctx, fcAPIClient(apiSock), "/drives/test", map[string]any{
		"drive_id":       "test",
		"path_on_host":   "disk.img", // RELATIVE — must resolve against cmd.Dir
		"is_root_device": false,
		"is_read_only":   false,
	})
	if err != nil {
		t.Fatalf("Firecracker rejected a relative drive path_on_host: %v\n"+
			"FALLBACK REQUIRED: per-VM mount namespaces (see spec, Risks #1)", err)
	}
}
```

- [ ] **Step 2: Run the relative-path preflight (no VM needed)**

Run: `cd go-sdk && go test -tags=integration -run TestPreflightRelativeDrivePath -v ./...`
Expected: PASS (or SKIP if no firecracker binary on this machine — then run on the KVM host)

- [ ] **Step 3: Run the kernel overlayfs preflight (needs KVM host)**

Run: `cd go-sdk && go test -tags=integration -run TestPreflightGuestOverlayfs -v ./...`
Expected: PASS

- [ ] **Step 4: GATE — if either preflight FAILED, stop here and report to the user.** Kernel missing overlayfs → operator must rebuild the kernel with `CONFIG_OVERLAY_FS=y`. Relative path rejected → design fallback discussion needed (mount namespaces). Do not continue to Task 2 on failure.

---

### Task 2: `scratch.go` — sparse copy + scratch template helpers

`copyRootfs` (snapshot.go:301) is generalized into `copySparse` and moves to the new file; `ensureScratchTemplate` builds the empty per-host template image. Pure host-side helpers, fully unit-testable.

**Files:**
- Create: `go-sdk/scratch.go`
- Create: `go-sdk/scratch_test.go`
- Modify: `go-sdk/snapshot.go` (delete `copyRootfs`)
- Modify: `go-sdk/snapshot_test.go` (delete `TestCopyRootfs*` — superseded by `TestCopySparse*`)

- [ ] **Step 1: Write the failing tests**

Create `go-sdk/scratch_test.go`:

```go
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

	// Idempotent: second call must not recreate the file.
	before := st.ModTime()
	if err := ensureScratchTemplate(path, sizeMB); err != nil {
		t.Fatalf("second ensureScratchTemplate: %v", err)
	}
	st2, _ := os.Stat(path)
	if !st2.ModTime().Equal(before) {
		t.Error("template was recreated; expected idempotent no-op")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd go-sdk && go test -run 'TestCopySparse|TestEnsureScratchTemplate' ./...`
Expected: FAIL — `undefined: copySparse`, `undefined: ensureScratchTemplate`

- [ ] **Step 3: Implement `go-sdk/scratch.go`**

```go
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

	tmp := path + ".tmp"
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
```

- [ ] **Step 4: Delete `copyRootfs` from `go-sdk/snapshot.go`**

Remove this entire function (snapshot.go:299-307):

```go
// copyRootfs copies src to dst using cp with --reflink=auto and --sparse=always
// for efficient copy-on-write on supported filesystems.
func copyRootfs(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp rootfs %s → %s: %w: %s", src, dst, err, out)
	}
	return nil
}
```

Also remove the now-stale TODO comment in `Restore` (snapshot.go:184-188) — it will be fully replaced in Task 7; for now replace the 5 comment lines with nothing (the real replacement comes with the scratch-copy code).

Keep the `os/exec` import only if still used elsewhere in snapshot.go (it is — `exec.CommandContext` in Restore); leave imports alone.

- [ ] **Step 5: Delete `TestCopyRootfs` and `TestCopyRootfsSrcMissing` from `go-sdk/snapshot_test.go`** (superseded by `TestCopySparse*`; delete both functions and their comments entirely).

- [ ] **Step 6: Run the full unit suite**

Run: `cd go-sdk && go test ./... -count=1`
Expected: PASS (including new `TestCopySparse*`, `TestEnsureScratchTemplate`)

---

### Task 3: Kernel boot args — `ro` + `init=/sbin/ix-stage0`

**Files:**
- Modify: `go-sdk/vmm.go:79-104` (`buildKernelBootArgs`)
- Modify: `go-sdk/vmm_test.go` (`TestBuildKernelBootArgs`, `TestBuildKernelBootArgsEmpty`)

- [ ] **Step 1: Update the tests to the new expectations (failing first)**

In `go-sdk/vmm_test.go`, in `TestBuildKernelBootArgs` replace:

```go
	// Must contain init path.
	if !strings.Contains(args, "init=/sbin/ix-init") {
		t.Errorf("boot args missing init path: %s", args)
	}
```

with:

```go
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
```

In `TestBuildKernelBootArgsEmpty` replace `"init=/sbin/ix-init",` with `"init=/sbin/ix-stage0",` in the `required` list.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs ./...`
Expected: FAIL — missing `init=/sbin/ix-stage0`, missing ` ro `

- [ ] **Step 3: Update `buildKernelBootArgs` in `go-sdk/vmm.go`**

Replace (vmm.go:91-93):

```go
		"root=/dev/vda",
		"rw",
		"init=/sbin/ix-init",
```

with:

```go
		"root=/dev/vda",
		"ro", // the rootfs is one shared image for all VMs — writes go to the per-VM scratch disk via overlayfs (see ix-stage0)
		"init=/sbin/ix-stage0",
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs ./...`
Expected: PASS

---

### Task 4: `startVMCold` — ro rootfs drive, per-VM scratch drive, `runDir`

**Files:**
- Modify: `go-sdk/vmm.go:107-115` (backend struct), `vmm.go:131-149` (doc comment + socketDir), `vmm.go:227-235` (drives)
- Modify: `go-sdk/vmm_test.go` (new `TestVMDir`)

- [ ] **Step 1: Write the failing `vmDir` test**

Append to `go-sdk/vmm_test.go`:

```go
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
```

Add `"os"` and `"path/filepath"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go-sdk && go test -run TestVMDir ./...`
Expected: FAIL — `fb.vmDir undefined`

- [ ] **Step 3: Add backend fields and `vmDir`**

In `go-sdk/vmm.go`, extend the struct (vmm.go:107-115):

```go
// firecrackerBackend implements VM lifecycle management using Firecracker.
type firecrackerBackend struct {
	fcBinary        string
	kernelPath      string
	rootfsImage     string
	logger          *slog.Logger
	snapshot        *SnapshotManager // optional; when set, startVM uses snapshot restore
	tapAlloc        *tapAllocator    // TAP index allocator (set by the manager; nil only in tests that never cold-boot)
	disableNet      bool             // skip TAP setup entirely (vsock-only VM)
	runDir          string           // base dir for per-VM dirs (sockets + scratch); empty = os.TempDir()
	scratchTemplate string           // empty sparse ext4 copied per VM as the scratch drive; empty = no scratch (bare test backends)
}

// vmDir returns the per-VM runtime directory (Firecracker CWD: API socket,
// vsock UDS, and the per-VM scratch disk all live here).
func (fb *firecrackerBackend) vmDir(sandboxID string) string {
	base := fb.runDir
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "ix-"+sandboxID)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd go-sdk && go test -run TestVMDir ./...`
Expected: PASS

- [ ] **Step 5: Use `vmDir` + create the scratch disk in `startVMCold`**

In `go-sdk/vmm.go` replace (vmm.go:149):

```go
	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
```

with:

```go
	socketDir := fb.vmDir(sandboxID)
```

Immediately after the `MkdirAll` block (vmm.go:150-152), insert:

```go
	// Per-VM scratch disk: every VM gets a private writable ext4 copied from
	// the empty template. The rootfs itself is attached read-only below — the
	// guest writes only to this scratch via overlayfs (ix-stage0).
	if fb.scratchTemplate != "" {
		if err := copySparse(fb.scratchTemplate, filepath.Join(socketDir, scratchFileName)); err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("create scratch disk: %w", err)
		}
	}
```

Update the function doc comment (vmm.go:134): change `1. Allocate CID and create socket dir at /tmp/ix-{sandboxID}` to `1. Allocate CID and create the per-VM dir (runDir/ix-{sandboxID}) with its scratch disk`.

- [ ] **Step 6: Make the rootfs drive read-only and attach the scratch drive**

Replace (vmm.go:227-235):

```go
	if err := fcPut(ctx, apiClient, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsImage,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set rootfs drive: %w", err)
	}
```

with:

```go
	// The rootfs is one shared image for every VM on this host: it MUST be
	// read-only at the VMM level. Concurrent rw mounts of one ext4 image from
	// multiple guest kernels corrupt the filesystem and let one chat's writes
	// persist into the template all future VMs boot from.
	if err := fcPut(ctx, apiClient, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsImage,
		"is_root_device": true,
		"is_read_only":   true,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set rootfs drive: %w", err)
	}

	// Per-VM writable scratch (overlay upper layer). Registered with a
	// RELATIVE path resolved against cmd.Dir (= socketDir), like vsock.uds:
	// snapshot.state records the relative string, so each restored clone
	// re-resolves it to its own scratch copy.
	if fb.scratchTemplate != "" {
		if err := fcPut(ctx, apiClient, "/drives/scratch", map[string]any{
			"drive_id":       "scratch",
			"path_on_host":   scratchFileName,
			"is_root_device": false,
			"is_read_only":   false,
		}); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set scratch drive: %w", err)
		}
	}
```

- [ ] **Step 7: Build + full unit suite**

Run: `cd go-sdk && go build ./... && go test ./... -count=1`
Expected: PASS

---

### Task 5: Manager config — `RunDir`, `ScratchSizeMB`, wiring

**Files:**
- Modify: `go-sdk/manager.go:29-59` (config), `:62-111` (`applyDefaults`), `:151-201` (`NewManager`)
- Modify: `go-sdk/manager_test.go`

- [ ] **Step 1: Write the failing defaults test**

Append to `go-sdk/manager_test.go`:

```go
func TestApplyDefaultsScratchAndRunDir(t *testing.T) {
	cfg := ManagerConfig{RootfsImage: "/opt/ix/rootfs/base.ext4"}
	cfg.applyDefaults()

	if cfg.ScratchSizeMB != 10240 {
		t.Errorf("ScratchSizeMB = %d, want 10240", cfg.ScratchSizeMB)
	}
	// RunDir defaults next to the rootfs image — NOT os.TempDir(), which is
	// often tmpfs (scratch disks must live on real disk, not host RAM).
	if cfg.RunDir != "/opt/ix/rootfs/run" {
		t.Errorf("RunDir = %q, want /opt/ix/rootfs/run", cfg.RunDir)
	}

	// Explicit values are preserved.
	cfg2 := ManagerConfig{
		RootfsImage:   "/opt/ix/rootfs/base.ext4",
		RunDir:        "/var/lib/ix/run",
		ScratchSizeMB: 2048,
	}
	cfg2.applyDefaults()
	if cfg2.RunDir != "/var/lib/ix/run" || cfg2.ScratchSizeMB != 2048 {
		t.Errorf("explicit values overridden: RunDir=%q ScratchSizeMB=%d", cfg2.RunDir, cfg2.ScratchSizeMB)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go-sdk && go test -run TestApplyDefaultsScratchAndRunDir ./...`
Expected: FAIL — `cfg.ScratchSizeMB undefined`

- [ ] **Step 3: Add the config fields**

In `go-sdk/manager.go`, after the `DisableNetworking` field (manager.go:58), add to `ManagerConfig`:

```go
	// Per-VM disk isolation. The rootfs is attached read-only and shared;
	// each VM writes to a private sparse scratch disk (overlay upper layer).
	RunDir        string // base dir for per-VM runtime dirs (sockets + scratch disks); default: <dir of RootfsImage>/run. Must NOT be on tmpfs.
	ScratchSizeMB int64  // per-VM scratch disk size in MB (sparse; allocates only what is written); default 10240
```

In `applyDefaults` (before the closing brace of the function), add:

```go
	if c.ScratchSizeMB == 0 {
		c.ScratchSizeMB = 10240
	}
	if c.RunDir == "" && c.RootfsImage != "" {
		// Default next to the rootfs image: that path is operator-managed real
		// disk. os.TempDir() would risk tmpfs — sparse scratch growth would
		// silently consume host RAM.
		c.RunDir = filepath.Join(filepath.Dir(c.RootfsImage), "run")
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd go-sdk && go test -run TestApplyDefaultsScratchAndRunDir ./...`
Expected: PASS

- [ ] **Step 5: Wire the template + runDir into the backend in `NewManager`**

In `go-sdk/manager.go`, after the `FCBinary` validation (manager.go:166-168) and before the `maxConc` block, insert:

```go
	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return nil, fmt.Errorf("create run dir %q: %w", cfg.RunDir, err)
	}
	scratchTemplate := filepath.Join(cfg.RunDir, "scratch-template.ext4")
	if err := ensureScratchTemplate(scratchTemplate, cfg.ScratchSizeMB); err != nil {
		return nil, fmt.Errorf("scratch template: %w", err)
	}
```

And extend the backend literal (manager.go:187-194):

```go
		vmm: &firecrackerBackend{
			fcBinary:        cfg.FCBinary,
			kernelPath:      cfg.KernelPath,
			rootfsImage:     cfg.RootfsImage,
			logger:          cfg.Logger,
			tapAlloc:        newTapAllocator(0),
			disableNet:      cfg.DisableNetworking,
			runDir:          cfg.RunDir,
			scratchTemplate: scratchTemplate,
		},
```

- [ ] **Step 6: Build + full unit suite**

Run: `cd go-sdk && go build ./... && go test ./... -count=1`
Expected: PASS

---

### Task 6: `recover()` scans `RunDir`

Orphaned per-VM dirs now live under `RunDir`; keep scanning `os.TempDir()` too so leftovers from pre-RunDir versions still get cleaned.

**Files:**
- Modify: `go-sdk/reaper.go:78-128`

- [ ] **Step 1: Update `recover`**

In `go-sdk/reaper.go`, replace (reaper.go:106-126):

```go
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil // non-fatal
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "ix-") {
			continue
		}
		fullPath := filepath.Join(tmpDir, entry.Name())
		if active[fullPath] {
			continue
		}
		m.logger.Info("recover: removing orphaned socket dir", "path", fullPath)
		if err := os.RemoveAll(fullPath); err != nil {
			m.logger.Warn("recover: remove failed", "path", fullPath, "error", err)
		}
	}

	return nil
```

with:

```go
	// Scan RunDir (current per-VM dirs: sockets + scratch disks) and
	// os.TempDir() (leftovers from versions that predate RunDir).
	scanDirs := []string{m.cfg.RunDir, os.TempDir()}
	if scanDirs[0] == scanDirs[1] {
		scanDirs = scanDirs[:1]
	}
	for _, dir := range scanDirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // non-fatal
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if !strings.HasPrefix(entry.Name(), "ix-") {
				continue
			}
			fullPath := filepath.Join(dir, entry.Name())
			if active[fullPath] {
				continue
			}
			m.logger.Info("recover: removing orphaned socket dir", "path", fullPath)
			if err := os.RemoveAll(fullPath); err != nil {
				m.logger.Warn("recover: remove failed", "path", fullPath, "error", err)
			}
		}
	}

	return nil
```

Note: `ix-golden-snapshot` under os.TempDir() starts with `ix-` — it is already protected via `active[m.cfg.SnapshotDir]` (reaper.go:94-96). The scratch template (`scratch-template.ext4`) is a file, not a dir, so the `IsDir` guard skips it.

- [ ] **Step 2: Build + full unit suite**

Run: `cd go-sdk && go build ./... && go test ./... -count=1`
Expected: PASS

---

### Task 7: Snapshot path — preserve golden scratch, per-clone copy

**Files:**
- Modify: `go-sdk/snapshot.go` (`scratchGoldenPath`, `Ready`, `CreateGolden`, `Restore`)
- Modify: `go-sdk/snapshot_test.go`

- [ ] **Step 1: Write the failing path test**

In `go-sdk/snapshot_test.go`, extend `TestSnapshotManagerPaths`:

```go
	wantScratch := filepath.Join(dir, "scratch.golden.ext4")
	if got := sm.scratchGoldenPath(); got != wantScratch {
		t.Errorf("scratchGoldenPath: got %q, want %q", got, wantScratch)
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go-sdk && go test -run TestSnapshotManagerPaths ./...`
Expected: FAIL — `sm.scratchGoldenPath undefined`

- [ ] **Step 3: Add `scratchGoldenPath` and harden `Ready`**

In `go-sdk/snapshot.go`, after `memPath()` (snapshot.go:64-66), add:

```go
// scratchGoldenPath returns the path of the golden VM's preserved scratch
// disk. Every restored clone boots from a copy of this file: the clone's
// guest kernel resumes with in-memory ext4 state (journal position, page
// cache) that must byte-match the backing file as it was at pause time.
func (sm *SnapshotManager) scratchGoldenPath() string {
	return filepath.Join(sm.snapshotDir, "scratch.golden.ext4")
}
```

Replace `Ready()` (snapshot.go:70-74):

```go
// Ready returns true if a golden snapshot exists and is ready to clone from.
// It is safe to call from multiple goroutines.
func (sm *SnapshotManager) Ready() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !sm.ready {
		return false
	}
	// The golden scratch is required to clone; guard against a snapshot dir
	// that was created by an older version or partially deleted.
	_, err := os.Stat(sm.scratchGoldenPath())
	return err == nil
}
```

- [ ] **Step 4: Preserve the golden scratch in `CreateGolden`**

In `go-sdk/snapshot.go`, right after the `/snapshot/create` `fcPut` succeeds and before the `sm.logger.Info("snapshot: golden snapshot created successfully")` line, insert:

```go
	// Preserve the golden VM's scratch disk (still paused; Firecracker
	// flushed block IO during snapshot creation). Restore copies this per
	// clone — see scratchGoldenPath.
	if err := copySparse(filepath.Join(handle.SocketDir, scratchFileName), sm.scratchGoldenPath()); err != nil {
		return fmt.Errorf("preserve golden scratch: %w", err)
	}
```

- [ ] **Step 5: Per-clone scratch copy in `Restore`**

In `go-sdk/snapshot.go`'s `Restore`, the old comment block (the "Skip rootfs copy …" / "TODO: if rootfs corruption is observed…" lines, removed in Task 2) sat between the `MkdirAll` block and the `apiSocket :=` line. At that spot, insert:

```go
	// The rootfs is shared by all clones and attached read-only at the VMM
	// level; guests write to their private scratch disk via overlayfs. Give
	// this clone its own scratch: a byte-identical copy of the golden VM's
	// scratch at pause time, at the relative path recorded in snapshot.state
	// (Firecracker re-resolves "scratch.ext4" against this process's cwd).
	if err := copySparse(sm.scratchGoldenPath(), filepath.Join(socketDir, scratchFileName)); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("create clone scratch: %w", err)
	}
```

Also in `Restore`, replace the socketDir computation:

```go
	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
```

with:

```go
	socketDir := sm.backend.vmDir(sandboxID)
```

- [ ] **Step 6: Run tests**

Run: `cd go-sdk && go test ./... -count=1`
Expected: PASS. Note `TestSnapshotManagerNotReadyByDefault` still passes (`ready=false` short-circuits before the stat).

---

### Task 8: Guest stage-0 + rootfs build script

**Files:**
- Create: `go-sdk/scripts/ix-stage0.sh`
- Modify: `go-sdk/scripts/build-rootfs-ext4.sh`

- [ ] **Step 1: Create `go-sdk/scripts/ix-stage0.sh`**

```sh
#!/bin/sh
# ix-stage0 — pre-init for ALL ix VM tiers (base, browser, browser-vm).
#
# The rootfs (/dev/vda) is attached READ-ONLY at the Firecracker level: it is
# one shared image for every VM on the host. All writes go to the per-VM
# scratch disk (/dev/vdb) through a whole-root overlayfs, so the per-tier init
# scripts (ix-init / browser-vm-init) need no changes — their writes land in
# the overlay upper layer transparently.
#
# Kernel boot args: root=/dev/vda ro init=/sbin/ix-stage0
#
# Any failure here exits PID 1 → kernel panic (panic=1) → Firecracker exits →
# the host surfaces a boot/health timeout. Debug with IX_VM_CONSOLE=1.

set -e

# The docker-exported rootfs has no static device nodes; /dev/vdb only
# appears once devtmpfs is mounted. `|| true` tolerates kernels that
# auto-mount devtmpfs (CONFIG_DEVTMPFS_MOUNT).
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true

# Per-VM writable scratch disk. The /scratch mountpoint is baked into the
# image by build-rootfs-ext4.sh.
mount /dev/vdb /scratch

mkdir -p /scratch/upper /scratch/work /scratch/newroot
mount -t overlay overlay \
  -o lowerdir=/,upperdir=/scratch/upper,workdir=/scratch/work \
  /scratch/newroot

# Pivot into the overlay. put_old must exist inside the new root; mkdir here
# writes to the upper layer (the overlay root is writable).
cd /scratch/newroot
mkdir -p old_root
pivot_root . old_root

# Detach the old root tree (incl. the raw vda and scratch mounts). The
# overlayfs holds its own references to the lower layer and upper dir, so a
# lazy unmount is safe — this just hides the raw devices from the guest.
umount -l /old_root

# chroot . re-anchors root/cwd defensively (older kernels don't retarget
# PID 1 on pivot_root), then hand off to the per-tier init unchanged.
exec chroot . /sbin/ix-init
```

- [ ] **Step 2: Make it executable**

Run: `chmod +x go-sdk/scripts/ix-stage0.sh && ls -l go-sdk/scripts/ix-stage0.sh`
Expected: `-rwxr-xr-x`

- [ ] **Step 3: Install stage0 + `/scratch` in `build-rootfs-ext4.sh`**

In `go-sdk/scripts/build-rootfs-ext4.sh`:

(a) In `create_directories()`, add `/scratch` to the created dirs:

```bash
create_directories() {
  local temp_dir="$1"

  echo "Creating required directories in rootfs..."
  sudo mkdir -p "${temp_dir}/run/ix"
  sudo mkdir -p "${temp_dir}/workspace"
  sudo mkdir -p "${temp_dir}/sbin"
  sudo mkdir -p "${temp_dir}/usr/local/bin"
  sudo mkdir -p "${temp_dir}/scratch"
  echo "✓ Directories created"
}
```

(b) Add a new function after `create_init_script()`:

```bash
install_stage0() {
  local temp_dir="$1"

  echo "Installing ix-stage0 (ro-rootfs overlay pre-init)..."
  sudo install -m 755 "${SCRIPT_DIR}/ix-stage0.sh" "${temp_dir}/sbin/ix-stage0"
  echo "✓ ix-stage0 installed"
}
```

(c) In `main()`, call it for ALL tiers — replace:

```bash
  # Populate rootfs
  create_directories "$TEMP_ROOTFS"
  if [[ "$TIER" == "browser-vm" ]]; then
    echo "browser-vm tier: skipping ixd + ix-init (PID 1 is browser-vm-init from the image)"
    install_browser_vm_init "$TEMP_ROOTFS"
  else
    copy_daemon_binary "$TEMP_ROOTFS"
    create_init_script "$TEMP_ROOTFS"
  fi
```

with:

```bash
  # Populate rootfs
  create_directories "$TEMP_ROOTFS"
  install_stage0 "$TEMP_ROOTFS"
  if [[ "$TIER" == "browser-vm" ]]; then
    echo "browser-vm tier: skipping ixd + ix-init (PID 1 is browser-vm-init from the image)"
    install_browser_vm_init "$TEMP_ROOTFS"
  else
    copy_daemon_binary "$TEMP_ROOTFS"
    create_init_script "$TEMP_ROOTFS"
  fi
```

Note: for the browser-vm tier, `/sbin/ix-init` is already a symlink to `/usr/local/bin/browser-vm-init` (`install_browser_vm_init`), so stage0's final `exec chroot . /sbin/ix-init` reaches the right init on every tier. Kernel boot args point at `init=/sbin/ix-stage0` for all tiers (Task 3) — the PID-1 comment in `install_browser_vm_init` becomes slightly stale; update its echo line from `(PID 1 is browser-vm-init from the image)` to `(init is browser-vm-init via ix-stage0)`.

- [ ] **Step 4: Shellcheck (if available)**

Run: `shellcheck go-sdk/scripts/ix-stage0.sh || true`
Expected: no errors (warnings acceptable; `|| true` keeps this advisory)

---

### Task 9: Rebuild images + integration regression tests

This task needs the KVM host (Firecracker + Docker + sudo).

**Files:**
- Create: `go-sdk/rootfs_isolation_integration_test.go`

- [ ] **Step 1: Rebuild the daemon binary and base rootfs**

```bash
cd daemon && cargo build --release --target x86_64-unknown-linux-musl -p ix-server
docker build -f cmd/Dockerfile --target base -t ix:base .
cd ../go-sdk && sudo IX_ROOTFS_SIZE=2048 scripts/build-rootfs-ext4.sh base
```

Expected: `✓ ix-stage0 installed` in the output; image written to `/opt/ix/rootfs/base.ext4`.

(If the browser-tier is exercised on this host, also rebuild `browser-vm`: `docker build -f cmd/Dockerfile --target browser-vm -t ix:browser-vm daemon/ && sudo scripts/build-rootfs-ext4.sh browser-vm`.)

- [ ] **Step 2: Delete stale golden snapshots**

Run: `rm -rf /tmp/ix-golden-snapshot`
(Old snapshots encode rw-on-shared drive state and a pre-stage0 memory image; they must never be restored against the new images.)

- [ ] **Step 3: Write the regression tests**

Create `go-sdk/rootfs_isolation_integration_test.go`:

```go
//go:build integration

package ix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// hashFile returns the sha256 of a file. Used to prove the shared rootfs
// image is bit-identical before and after VM activity.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestRootfsImmutableUnderConcurrentWrites is the corruption-regression test
// for the shared-writable-rootfs bug: N concurrent VMs hammer /workspace and
// the shared base image must come out bit-identical.
func TestRootfsImmutableUnderConcurrentWrites(t *testing.T) {
	before := hashFile(t, rootfsImage())

	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	const vms = 2
	type res struct{ id string; err error }
	done := make(chan res, vms)
	for i := 0; i < vms; i++ {
		go func(i int) {
			id := fmt.Sprintf("iso-writer-%d", i)
			sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: id, TTL: 3 * time.Minute})
			if err != nil {
				done <- res{id, fmt.Errorf("create: %w", err)}
				return
			}
			defer sb.Close()
			// Write through the page cache and force writeback so the bytes
			// actually reach the block device (the old bug only manifests
			// after writeback).
			r, err := sb.Shell(ctx, sandbox.ShellRequest{
				Command: "dd if=/dev/urandom of=/workspace/blob bs=1M count=64 2>/dev/null && sync && echo OK",
			})
			if err != nil {
				done <- res{id, fmt.Errorf("shell: %w", err)}
				return
			}
			if r.ExitCode != 0 || !strings.Contains(r.Output, "OK") {
				done <- res{id, fmt.Errorf("write failed (exit %d): %s", r.ExitCode, r.Output)}
				return
			}
			done <- res{id, nil}
		}(i)
	}
	for i := 0; i < vms; i++ {
		r := <-done
		if r.err != nil {
			t.Fatalf("%s: %v", r.id, r.err)
		}
		_ = mgr.Destroy(ctx, r.id)
	}

	after := hashFile(t, rootfsImage())
	if before != after {
		t.Fatalf("SHARED ROOTFS MUTATED: sha256 %s → %s — VM writes reached the base image", before, after)
	}
}

// TestWorkspaceIsolation: one VM's /workspace files must be invisible to
// another VM, and / must actually be the overlay (not raw vda).
func TestWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	sbA, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "iso-a", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	defer sbA.Close()
	sbB, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "iso-b", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	defer sbB.Close()

	// Root must be an overlay mount.
	r, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "awk '$2==\"/\"{print $3}' /proc/mounts"})
	if err != nil {
		t.Fatalf("mounts: %v", err)
	}
	if !strings.Contains(r.Output, "overlay") {
		t.Fatalf("guest / is not overlayfs: %q", r.Output)
	}

	if _, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "echo tenant-a-secret > /workspace/secret.txt && sync"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	rb, err := sbB.Shell(ctx, sandbox.ShellRequest{Command: "ls /workspace/ && cat /workspace/secret.txt 2>&1 || true"})
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if strings.Contains(rb.Output, "tenant-a-secret") || strings.Contains(rb.Output, "secret.txt") {
		t.Fatalf("ISOLATION BREACH: VM B sees VM A's workspace: %s", rb.Output)
	}
}

// TestSnapshotCloneIsolation: snapshot-restored clones must not corrupt the
// base image nor see each other's writes. This exercises the relative
// "scratch.ext4" path recorded in snapshot.state.
func TestSnapshotCloneIsolation(t *testing.T) {
	before := hashFile(t, rootfsImage())

	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  3 * time.Minute,
		UseSnapshot: true,
		SnapshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// CreateGolden runs async from NewManager; wait for it.
	deadline := time.Now().Add(3 * time.Minute)
	for !mgr.vmm.snapshot.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("golden snapshot not ready within 3m")
		}
		time.Sleep(time.Second)
	}

	sbA, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "clone-a", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create clone A: %v", err)
	}
	defer sbA.Close()
	sbB, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "clone-b", TTL: 3 * time.Minute})
	if err != nil {
		t.Fatalf("create clone B: %v", err)
	}
	defer sbB.Close()

	if _, err := sbA.Shell(ctx, sandbox.ShellRequest{Command: "echo clone-a-data > /workspace/a.txt && sync"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	rb, err := sbB.Shell(ctx, sandbox.ShellRequest{Command: "ls /workspace/"})
	if err != nil {
		t.Fatalf("ls B: %v", err)
	}
	if strings.Contains(rb.Output, "a.txt") {
		t.Fatalf("CLONE ISOLATION BREACH: clone B sees clone A's scratch: %s", rb.Output)
	}

	after := hashFile(t, rootfsImage())
	if before != after {
		t.Fatalf("SHARED ROOTFS MUTATED by snapshot clones: %s → %s", before, after)
	}
}
```

- [ ] **Step 4: Run the new integration tests**

Run: `cd go-sdk && go test -tags=integration -run 'TestRootfsImmutable|TestWorkspaceIsolation|TestSnapshotCloneIsolation' -v -timeout 20m ./...`
Expected: PASS, all three. If a VM fails to boot, re-run with `IX_VM_CONSOLE=1` to see stage-0 output on stderr.

- [ ] **Step 5: Run the full pre-existing integration suite (regression sweep)**

Run: `cd go-sdk && go test -tags=integration -v -timeout 30m ./...`
Expected: PASS — in particular `TestIntegrationCreateAndShell` (shell/code/file ops on the overlay root), `TestColdBootNetworking` (resolv.conf write now lands in the overlay), and any browser-tier tests on this host.

---

### Task 10: Documentation

**Files:**
- Modify: `docs/handbook/02-architecture.md` (disk model)
- Modify: `docs/handbook/05-operations.md` (rollout + RunDir guidance)
- Modify: `CLAUDE.md` (VMM-layer bullet)

- [ ] **Step 1: `CLAUDE.md`** — in the "Go SDK architecture" section, update the **VMM layer** bullet: after "configures VM via Firecracker API (PUT boot source, rootfs, machine config, network-interface, vsock)", change the parenthetical to "(PUT boot source, read-only rootfs + per-VM scratch disk, machine config, network-interface, vsock)" and append a sentence: "The rootfs is shared read-only across all VMs; each VM writes through a whole-root overlayfs onto a private sparse scratch disk (`ix-stage0`)."

- [ ] **Step 2: `docs/handbook/02-architecture.md`** — in the VM/disk section, document: shared ro `base.ext4`, per-VM `scratch.ext4` under `RunDir`, the relative-path snapshot mechanism, and the stage-0 overlay/pivot boot sequence. Follow the file's existing heading style; a short subsection ("Disk model: immutable base + per-VM scratch") with the guest/host layout diagram from the spec is enough.

- [ ] **Step 3: `docs/handbook/05-operations.md`** — add a rollout note: rebuilding images is REQUIRED for this version (`ix-stage0` + `/scratch`), stale golden snapshots must be deleted (`SnapshotDir`), guest kernels need `CONFIG_OVERLAY_FS=y`, and `RunDir` must not be on tmpfs (default: `<rootfs dir>/run`). Mention `ScratchSizeMB` as the per-sandbox disk quota.

- [ ] **Step 4: Final verification**

Run: `cd go-sdk && go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS, no vet issues.

---

## Self-review notes (already applied)

- **Spec coverage:** ro rootfs drive (T4), `ro` boot args + stage0 (T3), scratch template + per-VM copy (T2/T4/T5), RunDir-not-tmpfs (T5), recover() migration (T6), golden-scratch preserve + per-clone copy + Ready hardening (T7), guest stage-0 + build script all tiers (T8), corruption-regression/isolation/snapshot-clone tests + image rebuild + stale-snapshot deletion (T9), docs (T10), preflight risks #1/#2 (T1). Browser-tier needs no code change (shared `startVMCold`); its boot is regression-covered by the existing integration suite in T9 step 5.
- **Type consistency:** `scratchFileName` const (T2) used in T4/T7; `vmDir` (T4) used in T7; `copySparse` (T2) used in T4/T7; `ensureScratchTemplate` (T2) used in T5; backend fields `runDir`/`scratchTemplate` (T4) set in T5.
- **Out of scope per spec:** browser-tier state-disk mounting, per-request scratch sizing, reaper scratch accounting.
