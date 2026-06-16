# Changelog

All notable changes to this project will be documented in this file. The
format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Tags follow the Go module convention for the SDK (`go-sdk/vX.Y.Z`).

## [Unreleased]

## [0.3.0] - 2026-06-16

### Added

- **Preconfigured (rootless) network mode** — a documented mode where all
  privileged host networking is done once at setup time so the manager runs
  fully unprivileged afterward. New `go-sdk/scripts/ix-host-setup.sh` (root,
  idempotent) enables/persists `ip_forward`, installs the `ix-nat` nftables
  table, creates a pool of owner-attributed persistent TAPs (`ip tuntap add …
  user <svc>`), and writes a manifest (`/etc/ix/network.json`). With
  `ManagerConfig.PreconfiguredNetwork` (or `IX_PRECONFIGURED_NETWORK=1`) the
  manager skips `ensureHostNAT`/`ensureForwardAccept`/`ensureGatewayAddr`,
  verifies `ip_forward` by reading `/proc`, and allocates per-VM TAPs from the
  manifest pool instead of shelling out to `ip`. Per-VM TAP lifecycle is now
  behind a `netProvider` interface (`dynamicNet` for root mode, unchanged;
  `preconfiguredNet` for the pooled path). The manifest pool size is the hard
  concurrency cap; pool exhaustion returns a typed `ErrNetworkPoolExhausted`;
  missing TAPs are dropped (others keep working), all-missing fails fast with a
  "re-run ix-host-setup" error. Operator guide in
  `docs/handbook/preprovisioned-network.md`.
- **Prebuilt rootfs bundle publishing** — CI now builds each Dockerfile stage
  with an explicit `target:` (so `latest`/`base` carries `ixd`) across a
  per-stage matrix (`base`, `browser`, `browser-vm`) with tier suffixes and
  per-stage GHA cache scopes. A new `build-bundle` job, on `v*` tags, assembles
  `firecracker + jailer + vmlinux.bin + base.ext4` (ixd baked) into
  `ix-bundle-<ixver>-sdk<x.y>.tar.zst` and publishes it as a GitHub release
  asset, so consumers download a ready `/opt/ix` tree instead of building a
  rootfs on the host. New `scripts/sandbox/` tooling: `install-firecracker.sh`,
  `build-rootfs-ext4.sh`, `ix-init.sh`, `ix-stage0.sh`, `pack-bundle.sh`.
  Consumer doc in `docs/handbook/bundle.md`.

### Fixed

- **`ix-init` did not export `PATH`** — kernel-spawned init has an empty
  environment, so in-VM `python3` resolved via `/bin` (usrmerge), computed
  `sys.prefix=/`, and silently dropped `/usr/local/lib/pythonX/dist-packages`
  (every pip-installed package invisible). `ix-init` now exports `PATH` at the
  top.
- **Published images shipped without `ixd`** — the base Dockerfile stage now
  bakes `ixd` into `/usr/local/bin/ixd`, and `build-rootfs-ext4.sh` fails loudly
  (instead of warning) when `ixd` is neither built nor present in the image — a
  rootfs without `ixd` boots into a dead VM the host only sees as a health
  timeout.

## [0.2.0] - 2026-06-05

### Added

- **Shared browser tier** — a single browser-tier VM (Chrome + Pinchtab, no
  ixd/Python/Node) is shared across all sandboxes instead of each VM carrying
  its own Chrome. Per-chat sandboxes reach it through a host-side **Browser
  Gateway** with an egress matcher; the daemon selects the
  `RemoteSharedBrowserBackend` via `IX_BROWSER_MODE`. New standalone
  `browser-vm` Dockerfile stage builds the tier image.
- **`browser_wait` tool** end-to-end, plus browser interaction fixes.
- **Persistent shell sessions** — `session_id` on shell exec runs commands in a
  long-lived `bash -l` via a nonce-sentinel protocol instead of fork+exec+login
  shell per call (ShellPersistent 12.9 ms → 6.9 ms).
- **Per-VM TAP devices + host NAT** (`nft`) for outbound networking, replacing
  the orphaned passt approach. Snapshot-restored VMs remain vsock-only.
- **Read-only shared rootfs + per-VM scratch disk** — all VMs boot the same
  read-only rootfs image and write through a whole-root overlayfs onto a
  private sparse scratch disk (`ix-stage0`). Fixes fleet-wide ext4 corruption
  and a cross-tenant persistence bug from the previous shared read-write
  rootfs. `/workspace` is bind-mounted directly on the scratch disk, taking
  overlayfs out of the agent file-ops hot path (file R+W 1.4 ms there).
- **Creation tracing** — `IX_TRACE` emits per-phase timings for VM creation
  (spawn/API socket/config/scratch/TAP/boot) in the Go SDK.
- **Browser-tier benchmarks** — inaugural baseline in `daemon/BENCHMARKS.md`
  (Eval 1.05 ms, Action 104 ms, Navigate 208 ms, Screenshot 378 ms, E2E 1.90 s).
- **Handbook** — plain-language operator documentation in `docs/handbook/`.

### Changed

- **Performance overhaul** (the v0.2 section of `daemon/BENCHMARKS.md`),
  2.2–4.9x vs the previously published v0.1 numbers: serial console off
  (`8250.nr_uarts=0` + `quiet`), `RUST_LOG=warn` default, vsock HTTP
  keep-alive, concurrent REPL stdout/stderr reads (removes a fixed 10 ms drain
  per exec), scratch pre-copy pool (snapshot clone is an `os.Rename`, 11 µs vs
  ~12 ms `cp --sparse`), 1 ms readiness polling with an immediate first probe.
  Headline numbers: creation from snapshot 15 ms, Python code exec 6.0 ms,
  E2E snapshot cycle 59 ms.
- **Two-phase destroy** (semantics change) — `Destroy` returns after process
  kill + TAP release + renaming the VM dir to a tombstone; scratch-disk
  deletion completes asynchronously. Disk reclaim is deferred under churn;
  `recover()` sweeps tombstones at next manager start.
- **Image tiers restructured** — the `full` tier is dropped; Node.js (from the
  nodejs.org tarball) moves into `base`; `browser-vm` is slimmed to Chrome +
  Pinchtab only. Jupyter/ZMQ kernel stack removed from the image and from
  `ix-code` (replaced by the stdin/stdout sentinel REPL).

### Fixed

- **Browser eval returned 500 for any non-string result** — Pinchtab's
  `/evaluate` returns `{"result": <raw JSON value>}` but both backends declared
  `result: String`; `1+1` failed. Result conversion is now shared in
  `ix-browser/src/eval.rs`.
- **Transient screenshot 500s** on slow cold-Chrome captures — Pinchtab's 30 s
  action timeout tied the daemon's 30 s client timeout. Timeouts now form a
  strictly increasing chain: Pinchtab 60 s < gateway client 75 s < daemon
  capture 90 s; the SDK keeps error bodies instead of discarding them.

## [0.1.1] - 2026-05-28

First tagged release.

- ix sandbox runtime: Rust daemon (`ixd`) with shell, polyglot REPL
  (Python/JS/Bash), file ops, fetch, egress firewall, and browser over
  REST + SSE; Go SDK with pool-based lifecycle management.
- Migration from Docker containers to Firecracker MicroVMs with vsock
  transport (10.5x faster E2E vs the Docker/TCP baseline).
- Snapshot/restore: golden snapshots with a pre-warmed Python kernel,
  sub-50 ms restores.
- SDK aligned with the Oasis sandbox contract; Go module renamed to
  `github.com/nevindra/ix/go-sdk`.

[Unreleased]: https://github.com/nevindra/ix/compare/go-sdk/v0.3.0...HEAD
[0.3.0]: https://github.com/nevindra/ix/compare/go-sdk/v0.2.0...go-sdk/v0.3.0
[0.2.0]: https://github.com/nevindra/ix/compare/go-sdk/v0.1.1...go-sdk/v0.2.0
[0.1.1]: https://github.com/nevindra/ix/releases/tag/go-sdk/v0.1.1
