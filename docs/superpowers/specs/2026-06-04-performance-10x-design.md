# ix Performance 10x + Full Benchmark Coverage — Design

Date: 2026-06-04
Status: Approved (brainstorming session)
Branch context: `feat/shared-browser-remote-backend`, after v0.6 (read-only rootfs +
per-VM scratch overlay + TAP networking).

## 1. Goal

Improve sandbox hot-path performance by ~10x vs the v0.6 numbers in
`daemon/BENCHMARKS.md`, and extend benchmark coverage to every operation that
matters in an agent cycle — including the browser path, which currently has
zero benchmarks.

Success bar (user decision: strict 10x vs v0.6, with honest reporting where a
hard floor exists):

| Metric | v0.6 | 10x goal | Expected outcome |
|---|---|---|---|
| ShellPersistent | 30.2 ms | 3.0 ms | Reachable (daemon sessions + conn reuse) |
| FileReadWrite | 19.4 ms | 1.9 ms | Reachable pending transport attribution |
| CodeExec (Python, warm) | 26.1 ms | 2.6 ms | Reachable (remove 10 ms drain + transport) |
| E2E snapshot cycle | 130.6 ms | 13 ms | Reachable on pool path; restore path lands ~30–40 ms |
| Creation (snapshot restore) | 75.1 ms | 7.5 ms | Likely floor-bound by Firecracker `snapshot/load`; remove every controllable ms, report the floor. Pool-grab (<1 ms) is the product answer. |

Where a metric is floor-bound, BENCHMARKS.md states the floor and what owns it
(e.g. Firecracker snapshot-load latency) rather than overselling.

## 2. Root-cause findings driving this design

These were verified by reading code, not guessed:

1. **Persistent shell sessions do not exist.** `go-sdk/sandbox.go:51` sends
   `session_id`; the string `session_id` appears nowhere in `daemon/`. Every
   `Shell()` fork+execs `bash -l -c` (login shell, ~18 ms per spawn per the
   ix-shell criterion bench). `BenchmarkShellPersistent` and
   `BenchmarkShellOneShot` currently measure the same thing.
2. **Every code exec pays a hardcoded +10 ms.** `daemon/crates/ix-code/src/kernel.rs:118-136`
   drains stderr with a 10 ms `tokio::time::timeout` per read; the REPL sentinel
   (`__IX_RESULT__`) is written to stdout only (`ix_repl.py`), so the first
   stderr poll always blocks the full 10 ms.
3. **The v0.6 "overlayfs FileReadWrite regression" theory is unproven.**
   `BenchmarkFileReadWrite` writes `/tmp/bench.txt`, and `go-sdk/scripts/ix-init.sh:14`
   mounts tmpfs on `/tmp` — overlayfs is not in that path. The 8→19 ms move is
   unattributed (guest kernel 5.10→6.1, transport, or n=5 noise at ±30–80%).
4. **Fixed-cost polling waste.** `snapshot.go` `waitHealthy` ticks at 10 ms with
   no immediate first check (every restore wastes ≥10 ms); `network.go`
   `waitForFile` ticks at 5 ms (API-socket wait).
5. **No HTTP connection reuse.** `shellExec`/`ExecCode` return on the `complete`
   SSE event, then `Close()` the body before EOF — the connection is discarded;
   every request re-pays UDS dial + vsock `CONNECT` handshake.
6. **Hot-path fork+exec on the host.** `Restore` runs `cp --sparse` per clone;
   TAP setup runs 3–4 `ip`/`nft` execs per cold boot.
7. **Synchronous destroy in every cycle.** `Destroy` blocks on
   `os.RemoveAll(socketDir)` including the sparse scratch file.
8. **Benchmark bugs.** `BenchmarkCreateFromPool` and `BenchmarkCreatePoolPreWarmed`
   contain `time.Sleep(100ms)` inside the timed loop, and a pool-miss silently
   falls back to (and measures) a cold boot. Published pool numbers are invalid.
9. **Guest serial console suspect.** `ixd` logs at `info` to stdout, which is
   the emulated serial console (`console=ttyS0` in boot args) — serial emulation
   is vmexit-per-byte slow. Needs measurement; likely a per-request tax.
10. **Browser path: zero benchmarks** for gateway forwarding, navigate,
    snapshot, action, eval, or per-chat `ensureChat` (instance+tab creation).

## 3. Phase 0 — Measurement before optimization

Everything later is validated against this. No optimization lands without a
before/after from this harness.

- **Fix benchmark bugs:** `b.StopTimer()/StartTimer()` around inter-iteration
  sleeps; pool benchmarks fail (`b.Fatal`) on pool-miss instead of silently
  measuring a cold boot.
- **Stable methodology:** raise `-benchtime`, run `-count` ≥ 5, compare with
  `benchstat` (mean ± CI). `go-sdk/scripts/run-benchmarks.sh` grows a
  before/after comparison mode.
- **Per-phase tracing behind `IX_TRACE=1`:**
  - Restore path: FC spawn / API-socket wait / `snapshot/load` PUT / health poll
    / scratch provision.
  - Request path: dial / `CONNECT` handshake / request write / first byte /
    body complete.
  - Output: structured log lines the benchmark harness can aggregate. This
    attributes finding #3 before any fix is chosen.
- **New benchmarks (full agent-path coverage):**
  - Browser: `BenchmarkBrowserNavigate`, `BrowserSnapshot`, `BrowserScreenshot`,
    `BrowserAction` (click), `BrowserEval`, `BenchmarkBrowserFirstUse`
    (gateway `ensureChat`: start instance + open tab), `BenchmarkBrowserE2E`
    (create → navigate → snapshot → action → text → destroy).
  - Browser target: local HTTP test-page server on the host, reachable from
    guests via the TAP gateway route (hermetic, default). Real-site variant
    (e.g. example.com) gated behind `IX_BENCH_REAL_SITE` (user decision:
    local + opt-in real site).
  - Files: `BenchmarkFileReadWriteWorkspace` (`/workspace` — exercises the
    scratch/overlay write path; the existing `/tmp` bench only measures tmpfs),
    `BenchmarkUploadDownload`, `BenchmarkGrep`, `BenchmarkGlob`.
  - Lifecycle: `BenchmarkDestroy` (semantics change in this work; it gets its
    own number).

## 4. Daemon changes (Rust)

### 4.1 Persistent shell sessions (headline fix)

New session manager in `ix-shell`:

- `ShellRequest` gains `session_id: Option<String>`.
- A session = one long-lived `bash` process (login shell cost paid once),
  stdin/stdout/stderr piped, own process group.
- Protocol per command: write command + `echo` of a unique sentinel carrying
  `$?` to stdout, sentinel to stderr; stream output lines as SSE until both
  sentinels; report exit code from the sentinel.
- Sessions keyed by `session_id`, stored in `AppState`; TTL eviction (idle
  timeout) + cap on session count; killed via process group like one-shot.
- Timeout semantics (kept simple and unambiguous): a per-command timeout kills
  the WHOLE session (process group), reports the timeout on the SSE stream, and
  the session is lazily recreated on the next command for that `session_id`.
  A timeout never leaves a wedged session behind.
- No `session_id` in the request → existing fork+exec path unchanged
  (`ShellOneShot` keeps full process isolation).
- Sentinel collision: sentinel includes a per-command random nonce; command
  output that happens to contain the sentinel string is a non-goal beyond the
  nonce (documented).

### 4.2 Remove the 10 ms stderr drain

- `ix_repl.py`: after each cell, write the sentinel to **stderr too** (flush
  both).
- `kernel.rs`: read stdout to sentinel and stderr to sentinel (two awaits, no
  `timeout`-based draining). Timeout still wraps the whole execution.
- Same change audited for the JS/bash REPL paths if they share the protocol.

### 4.3 SSE termination for connection reuse

- Server: after `send_complete`/`send_error`, the SSE stream ends (sender drop
  closes the channel — verify axum then terminates the chunked body).
- Client pairing in §5.1.

### 4.4 Guest logging / serial console

- Default `ixd` log level inside VMs to `warn` (env-overridable).
- Boot args: drop `console=ttyS0` unless host sets `IX_VM_CONSOLE=1` (also
  removes serial writes during kernel boot).
- Measure before/after with the Phase 0 harness — this is a suspect, not a
  confirmed cost.

## 5. Go SDK changes (host)

### 5.1 Connection reuse

- `sseReader.Close()` drains to EOF (bounded: small deadline + byte cap) so the
  keep-alive connection returns to the pool instead of being torn down.
- `vsockTransport`: enable idle pooling (`MaxIdleConns(PerHost)`,
  `IdleConnTimeout`) — one vsock `CONNECT` per pooled connection, not per
  request.
- Plain JSON POSTs (file ops) already read to EOF via `json.Decode` + body
  close; verify reuse with the `IX_TRACE=1` dial counter.

### 5.2 Poller latency

- `waitHealthy`/`waitHealthyAuth`: immediate first probe, then 1 ms tick
  (bounded by the same overall deadlines).
- `waitForFile`: immediate first stat, then 1 ms tick.

### 5.3 Pre-copied scratch pool

- Background goroutine maintains N (default: pool size + 2) clone-ready copies
  of `scratch.golden.ext4` in the snapshot dir.
- `Restore` consumes one via `os.Rename` (same filesystem, atomic, ~µs) instead
  of `cp --sparse` fork+exec in the hot path; empty pool falls back to the
  existing `copySparse`.
- Pool invalidated and rebuilt when `CreateGolden` rewrites the golden scratch.

### 5.4 Two-phase destroy (user-approved with notes)

- **Synchronous phase (caller waits):** kill FC process + reap; teardown TAP and
  release its index (CIDs are a monotonic counter today — nothing to release);
  atomic `os.Rename` of the VM dir to `ix-<id>.deleting.<nonce>`.
- **Async phase:** dedicated deleter goroutine `os.RemoveAll`s tombstones
  immediately (not on the 30 s reaper tick).
- **Startup sweep:** manager boot removes any `*.deleting.*` dirs under runDir.
- **Documented caveats (required by user):**
  - Disk reclaim lags by ms-to-seconds under churn; a create racing into that
    window on a nearly-full host can hit ENOSPC where the old code would not.
    The reaper's disk-pressure handling remains the backstop.
  - "Destroy returned" now means "VM dead, TAP/CID released, dir fenced" — not
    "disk clean". Tests/ops scripts must not assert dir absence immediately.
  - BENCHMARKS.md notes that cleanup moved off the measured path: same total IO,
    overlapped; sustained-churn throughput unchanged.

### 5.5 Stretch (cold path only, lowest priority)

- TAP create/addr/up via netlink syscalls (no `ip` exec).
- Parallelize independent Firecracker config PUTs.
- These improve `CreateCold` only; the tracker row does not depend on them.

## 6. Guest changes (rootfs / stage0)

### 6.1 `/workspace` direct-mount

`ix-stage0`: create `/scratch/workspace` and bind-mount it over the overlay's
`/workspace` (before the old-root detach). Agent file ops write raw ext4 —
overlay lookup/copy-up machinery out of the hot write path. The whole-root
overlay stays for everything else (pip installs, /etc writes, etc.).

### 6.2 Journal-less scratch

`ensureScratchTemplate`: `mkfs.ext4 -O ^has_journal`; stage0 mounts scratch
with `noatime`. The scratch is ephemeral by design; a journal buys nothing.
Snapshot-restore compatibility note: the golden scratch is byte-preserved at
pause time, so clones see a consistent (journal-less) filesystem exactly as the
guest kernel expects.

## 7. Testing

- **Unit:** session sentinel protocol (multiline output, exit codes, timeout →
  session recreated, nonce collision), tombstone naming + sweep, scratch-pool
  refill/fallback/invalidations, netlink arg builders (if §5.5 lands).
- **Integration (must stay green):** `TestRootfsImmutableUnderConcurrentWrites`,
  `TestWorkspaceIsolation`, `TestSnapshotCloneIsolation`.
- **New integration:** shell session state persists across `Shell()` calls
  within a sandbox and does NOT leak across sandboxes; `ShellOneShot` sees no
  session state; two-phase destroy leaves no tombstones after sweep; restored
  clone works when scratch came from the pre-copy pool.
- **Benchmarks:** full before/after `benchstat` table lands in BENCHMARKS.md as
  v0.7 with per-fix attribution (each optimization gets its own measured delta,
  via the Phase 0 harness).

## 8. Out of scope

UFFD/lazy-memory snapshot restore, dm-thin scratch clones, replacing HTTP+SSE
with a custom multiplexed protocol. Revisit only if the Phase 0 harness shows
we are still floor-bound after this pass.

## 9. Delivery order

1. Phase 0 harness + new benchmarks + bug fixes → publish corrected v0.6.1
   baseline.
2. Daemon: stderr sentinel (smallest, highest certainty), then persistent
   sessions, then SSE termination + logging.
3. SDK: conn reuse, pollers, scratch pool, two-phase destroy.
4. Guest: workspace bind-mount, journal-less scratch.
5. Stretch cold-path items only if time/results warrant.
6. Re-measure everything; write v0.7 section in BENCHMARKS.md with attribution
   and floor analysis.
