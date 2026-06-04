# Rootfs Isolation: Read-Only Base + Per-VM Scratch Disk

**Date:** 2026-06-04
**Status:** Approved design, pending implementation plan

## Problem

Every Firecracker VM (pool entries, on-demand chats, health restarts, the golden
snapshot VM, and all snapshot clones) mounts the **same** rootfs file
(`/opt/ix/rootfs/base.ext4`) **read-write**:

- `vmm.go:227-235` — drive PUT uses the shared `rootfsImage` path with
  `is_read_only: false` for all VMs
- `vmm.go:91-92` — kernel boot args include `root=/dev/vda rw`, so guests
  actually mount it writable
- `ix-init.sh` writes `/etc/hostname` and `/etc/resolv.conf` on every boot, and
  `/workspace` (the target of all agent file ops) lives on the rootfs — there
  is no overlayfs or tmpfs protecting the root
- The comment in `snapshot.go:184-188` claiming "the VM uses overlayfs or
  tmpfs for writes internally, so the base image stays clean" is **false**;
  `copyRootfs()` exists and is tested but is never called from production code

### Impact

1. **Filesystem corruption (certainty over time).** Multiple guest kernels each
   hold independent block-allocation bitmaps, inode tables, and ext4 journals in
   their own page caches while writing to one block device. Snapshot clones make
   it worse: every clone resumes from *identical* journal state and diverges
   against the same device.
2. **Fleet-wide blast radius.** The corrupted artifact is the single shared
   `base.ext4` — pool replenishment, restarts, and golden snapshot creation all
   boot from it. End state is total outage requiring a rootfs rebuild.
3. **Cross-chat isolation breach.** Chat A's `/workspace` writes land on the
   device chat B reads (cross-read after writeback + cache eviction), and rootfs
   writes **survive VM destruction** — a compromised agent can persist a
   backdoor (e.g. replace `/usr/local/bin/ixd`) into the template every future
   VM boots from. Effectively a cross-tenant persistence vector.

It has not exploded yet because most test VMs are short-lived (guest dirty
pages are usually killed before writeback, default ~30 s), writes are small,
and benchmarks run one VM at a time. Any chat living past writeback with
`/workspace` activity flushes to the shared device.

## Constraints (decided during brainstorming)

- **Workspace must be disk-backed**, not RAM-backed (usage patterns unknown;
  default VM memory is only 256 MB, so tmpfs-backed workspace is unsafe).
- **Snapshot/restore is first-class** — the fix must work fully with
  `UseSnapshot`, including the fact that Firecracker bakes the drive
  `path_on_host` strings into `snapshot.state`.
- Workspace remains **ephemeral** (dies with the VM); that is the intended
  semantics today — the current "persistence" is the bug.

## Approaches considered

| | A: per-VM rootfs copy | B: ro base + tmpfs overlay | **C: ro base + per-VM scratch disk (chosen)** |
|---|---|---|---|
| Guest changes | none | init overlay | init overlay (stage-0) |
| Per-VM cost | 2 GB + 1–3 s on non-reflink hosts; cheap only on XFS/btrfs | ~0 | sparse file ≈ bytes written, ~ms to create |
| Workspace backing | disk (2 GB) | RAM — **violates constraint** | disk (scratch size) |
| Template poisoning | fixed by convention (base never opened rw) | fixed structurally | **fixed structurally** (`is_read_only: true` at the VMM level) |
| Host-FS dependency | reflink for acceptable perf | none | none |

B is eliminated by the disk-backed constraint. A is acceptable only on
reflink-capable hosts — an operational assumption that leaks. C is the standard
microVM-fleet pattern (immutable base + writable layer) and is the only option
that is simultaneously host-FS-agnostic, structurally immune to template
poisoning, and disk-efficient.

## Design

### Disk layout per VM

```
Host:
  /opt/ix/rootfs/base.ext4          SHARED, immutable (is_read_only: true)
  <RunDir>/scratch-template.ext4    empty sparse ext4, made once at manager start
  <RunDir>/ix-<id>/scratch.ext4     per-VM, sparse ext4, rw (drive "scratch")
  <RunDir>/ix-<id>/{fc.sock, vsock.uds}

Guest:
  /dev/vda   root, mounted ro       (boot args: root=/dev/vda ro)
  /dev/vdb   scratch, mounted rw at /scratch
  overlayfs: lowerdir=/, upperdir=/scratch/upper, workdir=/scratch/work
  pivot_root into the overlay; all writes (/etc, /workspace, pip installs,
  __pycache__) land in the scratch disk
```

### Guest: a new tier-agnostic stage-0 init

A single new script `/sbin/ix-stage0` (~25 lines) is installed in **all** rootfs
tiers by `build-rootfs-ext4.sh`. Kernel boot args change from
`init=/sbin/ix-init` to `init=/sbin/ix-stage0`:

1. Mount devtmpfs on `/dev` (so `/dev/vdb` exists)
2. Mount `/dev/vdb` on `/scratch` (directory pre-created in the image)
3. `mkdir /scratch/{upper,work,newroot}`
4. Mount the whole-root overlay at `/scratch/newroot`
5. `pivot_root`, lazy-unmount the old root, `exec /sbin/ix-init`

`ix-init.sh` (base/browser tiers) and `browser-vm-init.sh` (browser-vm tier)
**do not change** — their writes transparently land in the overlay upper.
Whole-root overlay (not per-directory targeted mounts) is deliberate: any
unanticipated write path just works instead of failing with EROFS.

### Host: flows

**Cold boot** (`startVMCold`):
1. Copy the scratch template → `socketDir/scratch.ext4`
   (`cp --reflink=auto --sparse=always`, ~ms since the template is empty)
2. PUT `/drives/rootfs` with `is_read_only: true`
3. PUT `/drives/scratch` with **relative** `path_on_host: "scratch.ext4"`
   (resolves against `cmd.Dir = socketDir`; precedent: the relative
   `vsock.uds` path already works this way)
4. Boot args: `root=/dev/vda ro` + `init=/sbin/ix-stage0`

**Golden snapshot** (`CreateGolden`):
1. Golden VM boots exactly like a cold boot, so `snapshot.state` records the
   relative `"scratch.ext4"` drive path
2. After `/snapshot/create` succeeds (VM paused, Firecracker has flushed block
   IO), copy the golden VM's scratch → `snapshotDir/scratch.golden.ext4`.
   This is the snapshot-consistent scratch template: its bytes match the
   guest kernel's in-memory ext4 state at pause time.

**Restore** (per clone):
1. Copy `scratch.golden.ext4` → `newSocketDir/scratch.ext4` (sparse, ~ms)
2. `/snapshot/load` — Firecracker reopens `"scratch.ext4"` relative to the new
   `cmd.Dir`, so every clone gets its own scratch that is byte-identical to
   snapshot time; guest journal/page-cache state stays consistent
3. The rootfs keeps its absolute shared path — safe because it is read-only at
   the VMM level. `Ready()` additionally requires `scratch.golden.ext4` to
   exist.

### Component changes

| Component | Change |
|---|---|
| `go-sdk/vmm.go` | Boot args `rw`→`ro`, `init=/sbin/ix-stage0`; rootfs drive `is_read_only: true`; create + attach per-VM scratch drive (relative path). `cleanupOnErr` / `cleanup` unchanged — `RemoveAll(socketDir)` already covers the scratch file |
| `go-sdk/manager.go` | New config: `ScratchSizeMB` (default 10240, matching the existing `Resources.Disk` default) and `RunDir` (default `<dir of RootfsImage>/run` — **not** `os.TempDir()`, because `/tmp` is often tmpfs and sparse scratch growth would eat host RAM). `NewManager` creates the scratch template at `<RunDir>/scratch-template.ext4` once (truncate + `mkfs.ext4`, idempotent). All socket dirs (cold boot and restore) move from `os.TempDir()` to `RunDir` |
| `go-sdk/snapshot.go` | `CreateGolden` preserves the golden scratch as `scratch.golden.ext4`; `Restore` copies it per clone before `/snapshot/load`; delete the false overlay comment; generalize `copyRootfs` → `copySparse` |
| `go-sdk/browser_tier.go` | No code change (scratch comes from `startVMCold`). The optional state disk shifts `vdb`→`vdc`; it is currently never mounted by the guest, so nothing breaks |
| `go-sdk/scripts/build-rootfs-ext4.sh` | Install `/sbin/ix-stage0` for all tiers; create `/scratch` in the image |
| New: `go-sdk/scripts/ix-stage0.sh` | The stage-0 overlay/pivot script |

### Error handling

- Scratch template or per-VM copy failure → boot error through existing cleanup
  paths
- Stage-0 failure in the guest (missing vdb, overlay mount failure) → init
  exits → `panic=1` → Firecracker exits → `waitReady`/health timeout surfaces
  the error; debug via `IX_VM_CONSOLE=1`
- Scratch full → guest gets ENOSPC on `/workspace` writes — a natural per-
  sandbox disk quota (finally gives `Resources.Disk` meaning); the VM stays up
- Host disk overcommit from sparse scratch growth → covered by the existing
  reaper `diskFreeGB` pressure check; finer-grained accounting is a follow-up

### Testing

- **Unit (no KVM):** boot-args content (`ro`, stage0); drive specs (rootfs ro,
  scratch relative path); scratch template creation (idempotent, sparse);
  `copySparse` preserves sparseness; config defaults (`RunDir`,
  `ScratchSizeMB`)
- **Integration (KVM):**
  1. **Corruption regression (the key test):** hash `base.ext4` → boot ≥2 VMs
     concurrently → each writes large files to `/workspace` + `sync` → destroy
     → hash must be identical
  2. Isolation: VM A's files invisible to VM B
  3. Snapshot path: golden + 2 concurrent clones writing workspaces; base and
     golden-template hashes unchanged
  4. ENOSPC: fill the scratch; VM stays healthy, write fails cleanly
  5. Browser tier boots healthy on a ro base
- **Preflight (implementation step #1):** verify Firecracker accepts a relative
  drive `path_on_host`; verify the guest kernel has `CONFIG_OVERLAY_FS=y`
  built-in (boot args use `nomodules`)

### Risks and fallbacks

1. **Firecracker rejects relative drive paths.** Cold-boot fallback is trivial
   (absolute per-VM path in `socketDir`). Snapshot fallback: run each
   Firecracker process in its own mount namespace and bind-mount the per-VM
   scratch over a fixed recorded path (the SDK already runs privileged — it
   creates TAP devices and nft rules).
2. **Guest kernel lacks built-in overlayfs.** One-time operator kernel rebuild
   with `CONFIG_OVERLAY_FS=y`.
3. **Overlayfs edge cases** (whiteouts, cross-layer rename). Rare in normal
   agent workloads; the integration suite exercises the common paths.

### Rollout (operator steps)

1. Rebuild all three rootfs images (adds `/sbin/ix-stage0` and `/scratch`)
2. **Delete stale golden snapshots** (`SnapshotDir`) — old snapshots still
   encode rw-on-shared semantics and must be regenerated
3. Verify/rebuild the guest kernel for overlayfs (one-time)
4. Update handbook (`02-architecture`, `05-operations`) and the CLAUDE.md
   VMM-layer notes

### Out of scope (recorded follow-ups)

- Browser-tier state disk is attached but never mounted by `browser-vm-init.sh`
  (`IX_BROWSER_STATE_DIR` lands on the rootfs) — pre-existing bug, now benign
  (writes go to the browser VM's overlay, i.e. truly ephemeral) but state
  persistence silently does nothing until fixed
- Per-request scratch sizing from `Resources.Disk` (v1 uses one global
  template size)
- Reaper accounting for per-VM scratch growth
