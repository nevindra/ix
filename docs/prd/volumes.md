# PRD — Volumes: a persistent disk for repeated work on one repository

Status: implemented · go-sdk 0.4.0 · 2026-09-16
Related: [ADR 0002](../adr/0002-no-pause-or-resume.md), [ADR 0003](../adr/0003-volume-is-a-cache-not-durability.md), [CONTEXT.md](../../CONTEXT.md)

## Problem

Athena wants to run repository-shaped tasks in a sandbox: clone a repo, check out a
branch, run a code-review skill, report. Today every run starts from an empty sandbox, so
every run clones the repository and reinstalls its dependencies before doing any work.
For a repository of any size that clone is the majority of the run's wall clock, and it
is repeated for every PR on the same repository.

The obvious fix, "keep the sandbox alive", is ruled out by ADR 0002. The other obvious
fix, "let Oasis persist `/workspace`", is ruled out by experience: Oasis persists per
file into the `files` table, and a `.git` directory or `node_modules` tree through that
path becomes thousands of rows and blobs. Athena already paid for that lesson once.

What is actually needed is smaller than either: a block device that survives the sandbox
and comes back next time, holding the checkout.

## Who this is for

**Athena**, calling the Go SDK. Athena chooses what goes on the disk, which key names it,
and what to do when it is busy or missing. ix attaches disks; it knows nothing about git.

Secondary: the **skill author** inside the guest, who needs one stable path to look at and
one rule: repo there → fetch, repo absent → clone.

## Goals

- A second run on the same repository does not clone. It fetches.
- Nothing about the repository reaches Oasis's `files` table.
- No change to ADR 0002: sandboxes remain alive-or-gone.
- Credentials never persist on the disk.
- The host cannot be filled up by forgotten disks.

## Non-goals

- Surviving host loss. The disk is a cache (ADR 0003). Re-clone is the recovery.
- Multi-host. Athena runs on one host today; a shared or replicated Volume store is not
  designed here.
- Concurrent readers or writers on one Volume. One sandbox at a time.
- Resizing a Volume after creation.
- Restoring a Volume-bearing sandbox from the golden snapshot. Cold boot only.
- Docker backend support. Volumes are a Firecracker feature.
- Any credential handling. Athena obtains tokens or SSH keys and writes them into the
  sandbox itself; ix provides nothing beyond the existing file API.

## Vocabulary

**Volume**: a host-local sparse ext4 file, attached to at most one sandbox at a time as a
read-write block device, mounted in the guest at the **Volume path** (default `/data`).
Named by a caller-chosen **Volume key**. To be added to CONTEXT.md.

The word *Workspace* is not used for this anywhere in ix. It is Oasis's word for the
opposite thing (CONTEXT.md, "Terms that belong to Oasis").

## Requirements

### R1 — Create a sandbox with a Volume attached

`sandbox.CreateOpts` belongs to Oasis, so the key is passed beside it rather
than inside it:

```go
func (m *IXManager) CreateWithVolume(ctx context.Context, opts sandbox.CreateOpts, key string) (sandbox.Sandbox, error)
```

When called:

- the Volume file is `<RunDir>/volumes/<key>.ext4`. If absent, ix creates it: sparse,
  size `ManagerConfig.VolumeSizeMB` (default 20480), ext4 **with** a journal (unlike
  the scratch template, which drops it because scratch dies with the VM).
- the sandbox boots **cold** through `startVMCold`, with the Volume as an extra
  read-write drive after the scratch (`/dev/vdc`). The golden snapshot and the
  pre-warmed pool are bypassed for this sandbox.
- the kernel boot args carry `ix.env.IX_VOLUME_PATH=<path>` (the existing env
  channel, so `buildKernelBootArgs` is untouched); `ix-stage0` mounts `/dev/vdc`
  there, under the overlay's new root, before `pivot_root`, with `noatime`. stage0
  mounts `/proc` itself to read the cmdline; nothing earlier in boot has.
- a health-monitor restart of the sandbox reattaches the same Volume.
- the Volume key is validated against `^[A-Za-z0-9._-]{1,64}$`. Anything else is
  rejected before a file path is formed.

Volume path is `ManagerConfig.VolumePath`, default `/data`. It must not be `/workspace`
or under it; ix refuses the config otherwise, because Oasis's mount layer walks
`/workspace` and would commit the repository into the `files` table.

### R2 — Exclusive attach

A Volume is attached to at most one live sandbox. A second `Create` naming a key that is
currently attached fails with `ErrVolumeBusy` before any VM is started.

ix does not queue. Athena already owns serialisation for sandbox work
(`execSem`, `serializeExecTools`) and can decide between waiting and falling back to an
ephemeral clone.

The attach lock is in-process state on the manager, released by `destroy`. On manager
start, `RunDir/volumes` is scanned and no Volume is considered attached; a crash cannot
leave a Volume permanently busy.

### R3 — Destroy syncs before killing

`destroy` on a Volume-bearing sandbox issues a best-effort `sync` through the existing
shell endpoint with a 2 s timeout, then kills Firecracker as today. Failure of the sync is
logged and ignored; the journal covers the crash case. The Volume file is **not**
deleted on destroy.

### R4 — Explicit delete and listing

```go
func (m *IXManager) DeleteVolume(key string) error   // ErrVolumeBusy if attached
func (m *IXManager) ListVolumes() ([]VolumeInfo, error)
```

`VolumeInfo` carries key, apparent size, allocated size, last-modified time, and whether
it is currently attached. `DeleteVolume` on an unknown key is not an error.

### R5 — Disk-pressure eviction

`reapDisk` today evicts the oldest sandbox when free space on the rootfs path is below
5 GB. Extend it: when free space is below the threshold **and there is no evictable
sandbox left**, delete the unattached Volume with the oldest mtime. Repeat on the next
tick until the threshold is met. Attached Volumes are never evicted.

Every eviction is logged at Info with the key and the freed bytes. There is no idle TTL
for Volumes; disk pressure is the only automatic trigger. One Volume per reaper tick,
matching the one-sandbox-per-tick pace of the existing eviction.

### R6 — Integration test

Two tests, serial, Firecracker, vsock-only so they run without root
(`volume_integration_test.go`): create with key → write a marker under the Volume path →
destroy → create with the same key → marker present; and a second attach on a held key
returns `ErrVolumeBusy`, `DeleteVolume` while attached returns `ErrVolumeBusy`, after
destroy + `DeleteVolume` a new create finds an empty Volume. Both pass on the reference
host against a rootfs carrying the new stage0.

## Sequencing

1. ADR 0003 and the CONTEXT.md entry, so the name is settled before code uses it.
2. `ix-stage0` mount (R1, guest half). Ships in the rootfs bundle; must be released
   before the SDK relies on it. Old stage0 ignores the drive harmlessly, so a
   mismatched pair degrades to "Volume attached but not mounted", not a boot failure.
3. SDK create path, Volume file creation, attach lock (R1, R2).
4. Sync-on-destroy, delete, list (R3, R4).
5. Reaper extension (R5).
6. Integration test (R6), handbook page, CHANGELOG.

## What Athena does with it

Recorded so the boundary is visible, not because ix implements any of it.

- Key: a hash of the canonical repository URL. One Volume per repository, not per
  branch. Parallel PRs on one repo hit `ErrVolumeBusy` and queue on Athena's side.
- Credentials: written into the sandbox after create via the existing file API, under
  the home directory, never under the Volume path. Token or SSH key lifetime is
  Athena's concern.
- Skill contract: if `<VolumePath>/repo/.git` exists, `git fetch` and check out the
  target ref; otherwise clone. Review output is written to `/workspace`, which Oasis
  persists as usual.
- Busy: Athena's choice between waiting and an ephemeral run without a Volume.

## Out of scope, and why

- **Volume in object storage (SeaweedFS).** Would give host mobility, but a sparse image
  does not survive an S3 round trip sparse, so every run would move the full image
  unless it were cached locally, which is this design plus a slower tier. Revisit when
  Athena runs on more than one host.
- **Copy-on-open (one canonical Volume, throwaway clone per run).** Removes
  `ErrVolumeBusy` at the cost of a copy per run and a merge-back problem. `copySparse`
  and reflink make the copy cheap on the right filesystem; the upgrade path is there if
  parallel review of one repo becomes common.
- **Attaching a Volume to a snapshot-restored sandbox.** Firecracker restores the drive
  set baked into the snapshot. Baking an empty slot and swapping the file underneath a
  restored kernel is possible but fragile (page cache from the golden run). The cold
  boot is ~1 s; not worth it.
- **Per-branch Volumes.** Storage multiplies with branches and the clone is repeated per
  branch anyway. A per-repository Volume with `git fetch` covers it.
- **Host-side `e2fsck` before attach.** Slow on a large Volume and needs e2fsprogs on
  the host. The journal handles the common case; add only if corruption is observed.

## Risks

- **A Volume mismatched with the rootfs bundle.** New SDK, old stage0: the drive attaches
  and nothing mounts; the skill sees an empty `/data` and clones every time. Detectable
  from the guest (`mountpoint -q /data`); the handbook should say so.
- **Sparse file on the wrong filesystem.** Same constraint as the scratch template:
  `RunDir` must not be tmpfs, and the filesystem must support sparse files. Already
  documented for scratch; the Volume inherits the rule.
- **A repository larger than the Volume.** Fixed size at create, no resize. The default
  is generous for source repositories; a monorepo with a multi-GB dependency tree needs
  the operator to raise `VolumeSizeMB` before the first create.
- **Eviction surprises.** A Volume evicted under disk pressure means one slow run, not
  data loss. Acceptable by ADR 0003, but Athena must not assume presence.
