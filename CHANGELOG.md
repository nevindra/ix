# Changelog

All notable changes to this project will be documented in this file. The
format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Tags follow the Go module convention for the SDK (`go-sdk/vX.Y.Z`).

## [Unreleased]

### Fixed

- **Image builds ignored `Cargo.lock` and re-resolved the whole dependency
  graph.** `daemon/cmd/Dockerfile` copied `Cargo.toml` and `crates/` but never
  the lockfile, so every image was built against whatever crates.io had
  published most recently rather than against what was tested. It failed the
  way that class of bug always fails — with nobody having changed anything:
  `icu_*` 2.3.0 shipped requiring rustc 1.88, the builder is pinned to
  `rust:1.87-bookworm`, and image builds began erroring at dependency
  resolution while `cargo build` on a developer machine kept passing because it
  reads the lockfile. Confirmed by rebuilding commit `311d409`, whose CI passed
  on 2026-07-24: it fails today with the identical error, source unchanged.

  The lockfile is now copied and the build runs `--locked`, so a lockfile that
  has drifted from `Cargo.toml` is a loud error rather than a silent
  re-resolve. `cmd/Dockerfile.arm64` carried the same gap and got the same fix;
  no CI job builds it, so it would have failed only in someone's hands. Tagged
  `v0.3.3` (daemon); the Go SDK is unaffected.

## [0.3.4] - 2026-08-19

Tagged `go-sdk/v0.3.4` (SDK) and `v0.3.2` (daemon); this release changes both.

### Added

- **`POST /v1/file/hash` — sha256 inside the VM, so a host can tell what changed
  without moving it.** A host that mirrors the workspace into storage has to
  answer "which of these files must I pull back out?", and the only way to
  answer it was to download every file and hash it on the far side of the
  vsock — the full byte size of the workspace moved to discover that nothing
  changed. The route takes `{"paths": [...]}` and returns
  `{"hashes": [{"path", "hash", "size"}]}`, where `hash` is the lower-case hex
  sha256 and `size` is the length actually digested, counted while hashing
  rather than stat'd separately, so the two always describe the same read even
  if the file is rewritten the instant afterwards.

  Each file is streamed through a fixed 64 KB window, reused across every path
  in the batch, so hashing a multi-hundred-megabyte dataset costs the same
  memory as hashing a README. That is the constraint that shaped it: a sandbox
  VM's RAM budget is a few hundred megabytes and its workspace is allowed to be
  larger than that.

  **Best-effort, and it answers 200 either way.** A path that is missing,
  unreadable, or a directory is omitted from `hashes` rather than failing the
  call: the caller enumerated these paths a moment ago while a command was
  still writing to the workspace, so a file that has since been deleted is the
  ordinary case, and one vanished temp file must not cost it the digests of
  everything else. Match results by `path` and read an absent path as
  *unknown*, never as *unchanged*. Results follow request order.

  sha256 comes from `ring`, which rustls and hickory-proto already pull into
  every `ixd` build — no new crate, no new musl build surface.

- **`IXSandbox.HashFiles` implements oasis's `sandbox.FileHasher`.** oasis
  declares hashing as an optional capability beside `Sandbox`, detected by type
  assertion (`sandbox.AsFileHasher`), so a runtime that cannot hash degrades to
  downloading the bytes. ix can, which is what lets oasis's stat-and-hash change
  detector and its close-time flush skip files the backend already holds without
  transferring a single one of them. Needs an oasis that declares `FileHasher`
  and `GlobResult.Entries`.

- **`/v1/file/glob` now describes the files it lists.** The response gains
  `entries` — one `{path, size, mod_time, mod_time_nanos}` per hit, stat'd
  inside the VM in the round trip that already returned the names. Size plus
  mtime rules out most files as untouched before anything is hashed, let alone
  transferred. `files` and `truncated` are unchanged and the field is additive,
  so an older client sees exactly what it saw before.

  `mod_time` is RFC 3339 at second resolution; `mod_time_nanos` is nanoseconds
  since the Unix epoch, and it is the field that carries the information. An
  agent rewrites a file twice inside one second routinely, so a second-granular
  mtime cannot separate two versions of the same byte length, and a host
  comparing on it alone would take a real change for an unchanged file.

  `entries` is **not** index-aligned with `files`: a path the daemon could not
  stat stays in `files` and simply has no entry. Leaving it there says "found,
  but undescribed", which a caller can act on; dropping it would say "gone",
  which is a different and worse claim. Match on `path`.

  The Go SDK parses this into `sandbox.GlobResult.Entries`, preferring
  `mod_time_nanos` whenever the daemon reported it and falling back to parsing
  `mod_time`. A timestamp it cannot parse is left as the zero time, because an
  unknown mtime is safe — the caller hashes the file — and a wrong one is not.

### Changed

- **Requires oasis v0.36.0.** The `FileHasher` capability and
  `GlobResult.Entries` that this release implements were unpublished while it
  was being written, so `go-sdk` carried a `replace` onto a sibling checkout.
  Both ship in oasis v0.36.0, the `replace` is gone, and the module builds
  from a clean clone again.

### Fixed

- **`/v1/file/search` discarded every context line it was asked for.** The
  ripgrep backend passed `--before-context`/`--after-context`, so rg did the
  extra scan and emitted `context` JSON events — and the parsing loop handled
  only `match`, hard-coding `context_before`/`context_after` to empty. rg is
  the preferred backend whenever it is on `PATH`, so in practice the model read
  search results with no surrounding lines while the native fallback returned
  them: two backends, two answers to the same question, and the one in daily
  use was the broken one. Context events now buffer and attach to the match
  that follows, with begin/end events resetting the buffer so lines cannot leak
  across a file boundary. Truncation moved to a post-pass, which stops a kept
  match's `context_after` from being cut short.

- **File-carrying routes answered HTTP 400 on anything over 2 MiB.** axum's
  default body limit is right for the JSON control plane and wrong for file
  transfer — athena accepts uploads up to 100 MB, so a 3.4 MB PDF attached in
  chat reached `fetch_file` and the guest refused it. Nothing in the refusal
  said "too large": exceeding the limit fails `Multipart::next_field`, which
  maps to `Error::BadRequest`. `FILE_BODY_LIMIT` is now 128 MiB on both upload
  and both write routes, deliberately above athena's own ceiling so the host's
  limit is the one that speaks. `ixClient.upload` also read and discarded the
  response body before checking status, alone among the four request helpers,
  which is why the failure arrived as a bare status with the daemon's
  explanation thrown away.

## [0.3.3] - 2026-07-24

### Changed

- **Sandbox lifetime is now a sliding idle window instead of an absolute TTL.**
  `ManagerConfig.DefaultTTL` (and `CreateOpts.TTL`) is refreshed on every
  request, so a VM that keeps being used never expires; the reaper destroys a
  sandbox only after it has been idle for the full window. Enables warm reuse
  of a per-conversation sandbox across turns. Previously `expiresAt` was fixed
  at creation and a long-lived conversation was reaped mid-life.

### Added

- **In-flight guard.** A sandbox with an active request is never idle-reaped
  and never health-restarted — a VM serving an exec is alive by definition,
  even when a CPU-bound task starves its `/health` endpoint. Fixes spurious
  mid-task restarts of busy sandboxes.
- **Tunable health monitor** via `ManagerConfig.HealthInterval` (default 10s),
  `HealthTimeout` (default 5s, was a hardcoded 3s), and
  `HealthFailureThreshold` (default 3).

## [0.3.2] - 2026-06-18

### Added

- **`IXManager.Health()`** — a passive readiness snapshot (`sandbox.Health`)
  covering runtime (kernel / rootfs / Firecracker-binary readability and
  `/dev/kvm` access), pool state (configured / ready / active plus cumulative
  restarts and failures), egress, and snapshot status. It reads only
  already-tracked state — no VM is launched or mutated. Backed by new
  cumulative `restartsTotal` / `failuresTotal` atomic counters that outlive
  sandbox destruction (a current-map count would lose them).

### Changed

- **Adapted to oasis v0.21.0** — `IXSandbox.BrowserSnapshot` now returns
  `sandbox.PageSnapshot` (renamed from `sandbox.BrowserSnapshot`), and
  `sandbox.MCPRequest.Args` is now `json.RawMessage` (a raw JSON object)
  instead of `map[string]any`. The oasis requirement is bumped to v0.21.0.

## [0.3.1] - 2026-06-17

### Added

- Base image bakes `openpyxl` and `pandas` so sandboxes can generate `.xlsx`
  offline (restricted egress blocks runtime pip installs).

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

[Unreleased]: https://github.com/nevindra/ix/compare/go-sdk/v0.3.4...HEAD
[0.3.4]: https://github.com/nevindra/ix/compare/go-sdk/v0.3.3...go-sdk/v0.3.4
[0.3.3]: https://github.com/nevindra/ix/compare/go-sdk/v0.3.2...go-sdk/v0.3.3
[0.3.2]: https://github.com/nevindra/ix/compare/go-sdk/v0.3.0...go-sdk/v0.3.2
[0.3.1]: https://github.com/nevindra/ix/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/nevindra/ix/compare/go-sdk/v0.2.0...go-sdk/v0.3.0
[0.2.0]: https://github.com/nevindra/ix/compare/go-sdk/v0.1.1...go-sdk/v0.2.0
[0.1.1]: https://github.com/nevindra/ix/releases/tag/go-sdk/v0.1.1
