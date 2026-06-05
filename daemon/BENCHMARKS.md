# ix Benchmark Results

Machine: AMD Ryzen 7 9700X 8-Core, Linux 6.18.7, x86_64
Rust: 1.87 (release profile, musl static)
Go: 1.26.1

---

## End-to-End Progress Tracker

All numbers measured with Go integration benchmarks (`benchtime=3x` through
v0.1; from v0.1.1 on: `benchtime=10x`, `count=5`, medians, benchstat for
significance).

### Optimization history

| Version | VMM | Transport | Creation | ShellPersistent | FileReadWrite | CodeExec (Python) | E2E (with Python) |
|---|---|---|---|---|---|---|---|
| v0.0 | Docker | TCP | 854ms | — | 80ms | 128ms | 753ms |
| v0.1 | Firecracker | vsock UDS | 75ms snapshot | 30ms | 19ms | 26ms | 131ms |
| **v0.2** | **Firecracker** | **vsock UDS** | **15ms snapshot** | **6.9ms** | **8.5ms (1.4ms /workspace)** | **6.0ms** | **59ms** |
| **Target** | **Firecracker** | **vsock UDS** | **<100ms** | **<3ms** | **<6ms** | **<10ms** | **<25ms** |

**v0.1 notes:** Security/correctness milestone, not a perf one — every VM previously
mounted the SAME rootfs image read-write (fleet-wide ext4 corruption + cross-tenant
persistence bug). Now: shared read-only rootfs + per-VM sparse scratch disk via a
whole-root overlayfs (`ix-stage0`). The numbers conflate TWO changes that landed
together: per-VM TAP + host NAT networking (there was previously no working egress)
and the disk isolation itself — TAP setup was never benchmarked separately. n=5 runs
are noisy (±30-80% run-to-run on creation paths); the most reproducible signal is
FileReadWrite 8→19ms (overlayfs on the /workspace write path).

**v0.1.1 notes:** Not a code release — same SDK/daemon as v0.1, benchmarks fixed
and re-run to create an honest baseline for v0.2 (28ms snapshot create, 13ms shell,
16ms code exec, 78ms E2E — full numbers in its section below). The published v0.1
numbers were inflated 1.7–2.4x by benchmark bugs (pool benches raced the filler and
measured pool-miss cold boots; creation benches timed Destroy inside the loop) plus
host conditions. All v0.2 claims are judged against v0.1.1, not v0.1.

**v0.2 notes:** Perf overhaul — daemon-side persistent shell sessions, concurrent
REPL stdout/stderr reads (removed a fixed 10 ms drain per exec), serial console off
(`8250.nr_uarts=0` + `quiet`), `RUST_LOG=warn` default, vsock HTTP keep-alive, a
scratch pre-copy pool (snapshot-clone scratch is an `os.Rename`, 11 µs vs ~12 ms
`cp --sparse`), 1 ms readiness polling with an immediate first probe, two-phase
destroy, and `/workspace` bind-mounted directly on the scratch disk (overlayfs out
of the agent file-ops hot path). Targets hit: Creation 15 ms (<100), CodeExec
6.0 ms (<10), File R+W on /workspace 1.4 ms (<6). Still open: ShellPersistent
6.9 ms (<3 target), E2E 59 ms (<25 — now dominated by Destroy's kill+wait).
Inaugural browser-tier benchmarks added (see v0.2 section).

### v0.2 — perf overhaul: shell sessions, REPL drain fix, quiet boot, scratch pool, two-phase destroy (2026-06-04)

VMM: Firecracker v1.15.1, kernel vmlinux 6.1.155, host kernel 7.0.0-22
Method: frozen test binaries (pre/post), `benchtime 10x`, `count 5`, medians;
compared against the v0.1.1 baseline (identical pre-optimization code, same
fixed benchmarks, same host, same day) so every delta is code-attributable.
Raw runs: `go-sdk/bench-results/{clean-v061,clean-v07}.txt`.

```
BenchmarkCreateCold-16                5    181111641 ns/op   123329 B/op   1033 allocs/op
BenchmarkCreateFromPool-16            5     11015149 ns/op     6556 B/op     59 allocs/op
BenchmarkCreateFromSnapshot-16        5     15332078 ns/op    71329 B/op    431 allocs/op
BenchmarkCreatePoolPreWarmed-16       5     12902181 ns/op    56609 B/op    326 allocs/op
BenchmarkShellEcho-16                 5      8643657 ns/op    26762 B/op    146 allocs/op
BenchmarkShellOneShot-16              5     14016377 ns/op    25809 B/op    142 allocs/op
BenchmarkShellPersistent-16           5      6914378 ns/op    20658 B/op    140 allocs/op
BenchmarkCodeExecPython-16            5      7114640 ns/op    23638 B/op    130 allocs/op
BenchmarkCodeExecSnapshot-16          5      5980391 ns/op    21340 B/op    129 allocs/op
BenchmarkCodeExecFirstCall-16         5    300172218 ns/op   168844 B/op   1259 allocs/op
BenchmarkFileReadWrite-16             5      8465065 ns/op    25302 B/op    199 allocs/op
BenchmarkFileReadWriteWorkspace-16    5      1444664 ns/op    19008 B/op    173 allocs/op
BenchmarkUploadDownload-16            5      3524907 ns/op   125254 B/op    187 allocs/op
BenchmarkGrep-16                      5      6732356 ns/op    43591 B/op    201 allocs/op
BenchmarkGlob-16                      5      6425389 ns/op    11936 B/op    145 allocs/op
BenchmarkEndToEnd-16                  5    103666316 ns/op   218994 B/op   1510 allocs/op
BenchmarkE2EAgentCycle-16             5    114286679 ns/op   223676 B/op   1603 allocs/op
BenchmarkE2ESnapshotCycle-16          5     58873477 ns/op   136974 B/op    887 allocs/op
BenchmarkDestroy-16                   5     24638811 ns/op     2952 B/op     29 allocs/op
```

**Per-fix attribution (benchstat v0.1.1 → v0.2, p=0.008 unless n.s.):**

| Benchmark | v0.1.1 | v0.2 | Δ | What did it |
|---|---|---|---|---|
| CreateCold | 405.3ms | 181.1ms | **-55%** | serial console off (`8250.nr_uarts=0` + `quiet`) — boot log writes to ttyS0 were the biggest cold-boot cost |
| CreateFromSnapshot | 27.8ms | 15.3ms | **-45%** | scratch pre-copy pool (clone = `os.Rename`, 11µs vs ~12ms `cp --sparse`) + immediate-first-probe 1ms health tick |
| CreatePoolPreWarmed | 23.9ms | 12.9ms | **-46%** | faster exec path + pool-grab alloc cuts (-60% B/op, -68% allocs) |
| CreateFromPool | 10.4ms | 11.0ms | n.s. | already a map grab + handshake |
| ShellPersistent | 12.9ms | 6.9ms | **-46%** | daemon-side persistent bash sessions (`session_id`): command runs in a long-lived `bash -l` via nonce sentinel — no fork+exec+login-shell per call |
| ShellEcho | 12.5ms | 8.6ms | **-31%** | `RUST_LOG=warn` default + quiet console + vsock HTTP keep-alive |
| ShellOneShot | 13.7ms | 14.0ms | n.s. | fresh fork+exec by design — control group |
| CodeExecPython | 18.3ms | 7.1ms | **-61%** | REPL stdout/stderr read concurrently (`tokio::join!`); the fixed 10ms stderr drain per exec is gone |
| CodeExecSnapshot | 16.0ms | 6.0ms | **-63%** | same |
| CodeExecFirstCall | 477.8ms | 300.2ms | **-37%** | cold-boot wins above (guest kernel boot still dominates) |
| FileReadWrite (/tmp) | 8.6ms | 8.5ms | n.s. | untouched path (see open questions) |
| FileReadWriteWorkspace | 1.45ms | 1.44ms | n.s. | already at the vsock+HTTP floor |
| UploadDownload / Grep / Glob | 3.4 / 6.4 / 6.4ms | 3.5 / 6.7 / 6.4ms | n.s. | untouched paths |
| EndToEnd | 162.0ms | 103.7ms | **-36%** | sum of the above |
| E2EAgentCycle | 167.8ms | 114.3ms | **-32%** | sum of the above |
| E2ESnapshotCycle | 77.8ms | 58.9ms | **-24%** | now dominated by Destroy (24.6ms of 58.9) |
| Destroy | 26.3ms | 24.6ms | wall n.s.; **-35% B/op, -26% allocs** | two-phase destroy — scratch `RemoveAll` off the call path; kill+wait dominates wall time |

**Where creation time goes now (IX_TRACE medians; n=970 cold, 185 restore):**

```
CreateCold 181ms total
  host:  spawn 0.1 + apisock 1.1 + config 0.9 + scratch 1.9 + tap 4.2
         + instancestart 7.7                              ≈ 16ms
  guest: kernel boot + ix-stage0 (mount vdb, overlay,
         pivot_root) + ixd start + READY                  ≈ 165ms  ← next frontier

CreateFromSnapshot 15.3ms total
  host:  spawn 0.3 + apisock 1.1 + load 3.1 + scratch 0.01
         + health 7.9                                     ≈ 12.4ms
  scratch clone is a rename of a pre-copied pool file (was ~12ms cp --sparse);
  the health poll (VM resume → first /health 200) is now the dominant cost
```

**Strict-10x scorecard (the v0.2 goal was 10x vs published v0.1):**

| Metric | v0.1 published | 10x target | v0.2 | vs v0.1 | vs v0.1.1 (code only) |
|---|---|---|---|---|---|
| Creation (snapshot) | 75.1ms | 7.5ms | 15.3ms | **4.9x** | 1.8x |
| ShellPersistent | 30.2ms | 3.0ms | 6.9ms | **4.4x** | 1.9x |
| FileReadWrite | 19.4ms | 1.9ms | 8.5ms /tmp · 1.4ms /workspace | **2.3x · 13.5x** | 1.0x |
| CodeExec (snapshot) | 26.1ms | 2.6ms | 6.0ms | **4.4x** | 2.7x |
| E2E (snapshot cycle) | 130.6ms | 13.1ms | 58.9ms | **2.2x** | 1.3x |

Verdict: strict 10x reached only on /workspace file ops. Honest split: roughly
half the headline gap closed by fixing measurement (v0.1.1), the other half by
code (1.3–2.7x per op). The remaining distance is floors, not slack:

- **vsock+HTTP round-trip ≈ 0.7ms/request** (FileReadWriteWorkspace = 2
  requests = 1.44ms). Targets below ~2 round-trips need request batching or a
  different transport, not faster handlers.
- **ShellPersistent 6.9ms** = SSE channel setup + bash sentinel turnaround;
  next step would be a binary exec protocol, not shell tweaks.
- **E2ESnapshotCycle 58.9ms** = Destroy 24.6 + Create 15.3 + ops ~12 + SSE
  overhead. Destroy's kill+wait is the single biggest slice — candidate:
  fire-and-forget destroy at the manager level (wait moved off the caller).

**Two-phase destroy (semantics change, caveats):** `Destroy` now returns after
the synchronous phase — process kill + TAP release + rename of the VM dir to an
`ix-<id>.deleting.<n>` tombstone. Scratch-disk deletion (`RemoveAll`, up to
10 GB sparse) completes asynchronously.

- Disk reclaim is deferred: under heavy churn transient disk usage exceeds what
  the live-VM count implies, and the reaper's disk-pressure check can fire on
  tombstone backlog.
- A crash between phases leaves tombstones; `recover()` sweeps all `ix-*`
  prefixed dirs (tombstones included) at next manager start.
- Caller-visible latency barely moved in the benchmark (kill+wait dominates;
  the benchmark's scratch is nearly empty) — the real win is that a *written-to*
  scratch no longer blocks the call for its full `RemoveAll` duration.

**Browser tier — inaugural baseline (hermetic local page, 2026-06-04):**

Path measured: per-chat guest ixd → vsock → host gateway → shared browser-tier
VM (pinchtab) → Chrome CDP. The page server runs on the host gateway IP, so
numbers measure our stack, not the internet. pinchtab's SSRF guard blocks
link-local targets by default — benches set
`BrowserTrustedResolveCIDRs: ["169.254.0.0/16"]` (wired through
`PINCHTAB_TRUSTED_RESOLVE_CIDRS` → `trustedResolveCIDRs` in pinchtab config).
Real-site run is opt-in via `IX_BENCH_REAL_SITE=https://example.com`.

```
BenchmarkBrowserEval-16          5      1054601 ns/op    19129 B/op    215 allocs/op
BenchmarkBrowserSnapshot-16      5      7847962 ns/op    20815 B/op    222 allocs/op
BenchmarkBrowserAction-16        5    104476374 ns/op    23373 B/op    224 allocs/op
BenchmarkBrowserNavigate-16      5    208232177 ns/op    30136 B/op    293 allocs/op
BenchmarkBrowserScreenshot-16    5    378448262 ns/op    57554 B/op    197 allocs/op
BenchmarkBrowserFirstUse-16      5   1715606721 ns/op   269004 B/op   2246 allocs/op
BenchmarkBrowserE2E-16           5   1901138987 ns/op   332300 B/op   2970 allocs/op
```

| Benchmark | median | what it measures |
|---|---|---|
| BrowserEval | **1.05ms** | trivial JS eval — the floor of the daemon → gateway → pinchtab → Chrome CDP path |
| BrowserSnapshot | **7.8ms** | accessibility-tree snapshot of the current page |
| BrowserAction | 104ms | coordinate click round-trip |
| BrowserNavigate | 208ms | steady-state navigation, warm tab |
| BrowserScreenshot | 378ms | full-page screenshot (5/5 runs, 358–403ms spread) |
| BrowserFirstUse | 1.72s | first browser op of a chat: gateway ensureChat (pinchtab instance + tab) + navigate |
| BrowserE2E | 1.90s | create (pool) → navigate → snapshot → action → text → destroy |

No prior numbers exist — this is the baseline future work is measured against.
Numbers are from the post-fix rerun (`v0.7-browser-postfix2.txt`, all 7
benchmarks 5/5 PASS) after the eval and capture-timeout fixes below; the
initial v0.2 run had two failing benchmarks. BrowserEval at 1.05ms is the
measured floor of the entire cross-VM browser path — every other op's cost is
Chrome work, not plumbing. Note: snapshot-restored VMs are vsock-only (no
TAP), so browser benches use the cold-boot pool, never `UseSnapshot`.

**Bugs found by the v0.2 browser run (both fixed and verified by rerun):**

- `BenchmarkBrowserEval` failed 5/5 in the initial run. Root cause: pinchtab's
  `/evaluate` returns `{"result": <raw JSON value>}` — the bench's `1+1` comes
  back as `{"result": 2}`, a number — while both daemon backends declared
  `result: String`, so every non-string expression 500'd ("error decoding
  response body"). Eval had only ever worked for string-valued expressions, in
  local AND remote mode. Fix: parse as `serde_json::Value`, pass strings
  through, render everything else as JSON text (shared `ix-browser/src/eval.rs`).
  Verified: post-fix rerun passes 5/5, median **1.05ms**.
- `BenchmarkBrowserScreenshot` hit transient HTTP 500s (1/5, then 2/5 runs):
  cold-Chrome first captures stalled ~30s into a timeout tie — pinchtab's
  ActionTimeout (30s) raced the daemon's reqwest client (30s global), and the
  SDK's `getRaw` discarded the error body so the loser was unidentifiable.
  Fix: timeout chain made strictly increasing — pinchtab `actionSec` 60s <
  gateway pinchtab-client 75s < daemon capture timeout 90s (per-request,
  screenshot/pdf only) — so a slow capture finishes, and if it truly wedges,
  pinchtab's descriptive error surfaces. `getRaw` now includes the error body.
  Verified: post-fix rerun passes 5/5 with a tight 358–403ms spread.

**Open questions (v0.2):**

- File ops on `/tmp` are ~6x slower than `/workspace` (8.5ms vs 1.4ms per
  write+read pair) in BOTH v0.1.1 and v0.2 — pre-existing, not a regression.
  `/tmp` should be tmpfs (fast); root cause TBD.

### v0.1.1 — baseline re-measurement, fixed benchmarks (2026-06-04)

Same SDK/daemon code as v0.1 — only the benchmarks changed. Re-run to create an
honest baseline for v0.2, because two benchmark bugs inflated the published
v0.1 numbers:

- `CreateFromPool` / `CreatePoolPreWarmed` raced the pool filler and mostly
  measured pool-miss cold boots (published 293ms / 370ms). Fixed with
  `waitPoolFill` + a `poolHits` assertion: a real pool grab is ~10ms.
- Creation benches timed `Destroy` inside the loop; it now runs untimed.

New benchmarks added: `FileReadWriteWorkspace` (the scratch-backed agent
workspace path — `FileReadWrite` hits `/tmp`), `UploadDownload` (64 KB
multipart), `Grep` / `Glob` (50-file tree), `Destroy` (teardown isolated).

```
BenchmarkCreateCold-16                5    405315145 ns/op   121840 B/op   1011 allocs/op
BenchmarkCreateFromPool-16            5     10393541 ns/op     7167 B/op     71 allocs/op
BenchmarkCreateFromSnapshot-16        5     27828883 ns/op    65018 B/op    403 allocs/op
BenchmarkCreatePoolPreWarmed-16       5     23894015 ns/op   141188 B/op   1005 allocs/op
BenchmarkShellEcho-16                 5     12520957 ns/op    20594 B/op    140 allocs/op
BenchmarkShellOneShot-16              5     13739701 ns/op    20500 B/op    137 allocs/op
BenchmarkShellPersistent-16           5     12921170 ns/op    18061 B/op    135 allocs/op
BenchmarkCodeExecPython-16            5     18323056 ns/op    18565 B/op    125 allocs/op
BenchmarkCodeExecSnapshot-16          5     16013502 ns/op    16214 B/op    123 allocs/op
BenchmarkCodeExecFirstCall-16         5    477813513 ns/op   159456 B/op   1237 allocs/op
BenchmarkFileReadWrite-16             5      8649234 ns/op    23544 B/op    204 allocs/op
BenchmarkFileReadWriteWorkspace-16    5      1454534 ns/op    19828 B/op    179 allocs/op
BenchmarkUploadDownload-16            5      3403842 ns/op   126892 B/op    192 allocs/op
BenchmarkGrep-16                      5      6403338 ns/op    44035 B/op    205 allocs/op
BenchmarkGlob-16                      5      6382405 ns/op    12353 B/op    148 allocs/op
BenchmarkEndToEnd-16                  5    162002083 ns/op   191664 B/op   1590 allocs/op
BenchmarkE2EAgentCycle-16             5    167753289 ns/op   244404 B/op   1918 allocs/op
BenchmarkE2ESnapshotCycle-16          5     77774376 ns/op   122262 B/op    867 allocs/op
BenchmarkDestroy-16                   5     26282256 ns/op     4553 B/op     39 allocs/op
```

Published v0.1 vs v0.1.1 on identical code: ShellPersistent 30.2→12.9ms,
CodeExecSnapshot 26.1→16.0ms, CreateFromSnapshot 75.1→27.8ms, E2ESnapshotCycle
130.6→77.8ms. That 1.7–2.4x gap is measurement methodology plus host
conditions, not code — which is why v0.2 is judged against v0.1.1, not v0.1.

### v0.1 — read-only rootfs + per-VM scratch overlay (2026-06-04)

VMM: Firecracker v1.15.1, kernel vmlinux 6.1.155, host kernel 7.0.0-22
Disk: shared ro rootfs (`is_read_only: true`) + per-VM sparse scratch ext4 (10 GB)
mounted as a whole-root overlayfs by the `ix-stage0` pre-init
Networking: per-VM TAP + host nft NAT (masquerade on any non-TAP egress)
Creation: snapshot restore copies the golden VM's scratch per clone

```
BenchmarkCreateCold-16             5    611625304 ns/op    182561 B/op    1486 allocs/op
BenchmarkCreateFromPool-16         5    293006213 ns/op    190016 B/op    1571 allocs/op
BenchmarkShellEcho-16              5     38901756 ns/op     34776 B/op     227 allocs/op
BenchmarkCodeExecPython-16         5     30462067 ns/op     24433 B/op     206 allocs/op
BenchmarkCodeExecFirstCall-16      5    803637476 ns/op    218177 B/op    1625 allocs/op
BenchmarkFileReadWrite-16          5     19387336 ns/op     42692 B/op     292 allocs/op
BenchmarkShellPersistent-16        5     30243795 ns/op     29688 B/op     217 allocs/op
BenchmarkShellOneShot-16           5     37892683 ns/op     32961 B/op     222 allocs/op
BenchmarkCreatePoolPreWarmed-16    5    369968969 ns/op    350310 B/op    2737 allocs/op
BenchmarkE2EAgentCycle-16          5    264875729 ns/op    328800 B/op    2844 allocs/op
BenchmarkEndToEnd-16               5    262790774 ns/op    319161 B/op    2750 allocs/op
BenchmarkCreateFromSnapshot-16     5     75111280 ns/op     91192 B/op     538 allocs/op
BenchmarkE2ESnapshotCycle-16       5    130644213 ns/op    132953 B/op     971 allocs/op
BenchmarkCodeExecSnapshot-16       5     26086306 ns/op     23696 B/op     191 allocs/op
```

**What this bought:** the previous design mounted one shared `base.ext4` read-write in
every concurrent VM — guaranteed ext4 corruption under load, cross-chat workspace
leakage, and writes that persisted into the template all future VMs boot from.
All three are now structurally impossible and regression-tested
(`TestRootfsImmutableUnderConcurrentWrites`, `TestWorkspaceIsolation`,
`TestSnapshotCloneIsolation`).

**Where the new time goes (vs the pre-isolation build):**

```
CreateCold: +~190ms       TAP create + addr + up (3x ip exec) + nft, scratch
                          template copy (cp --sparse exec), scratch drive PUT,
                          guest stage0 (mount vdb + overlay + pivot_root)
CreateFromSnapshot: +30ms golden-scratch copy per clone (cp --sparse exec)
Shell/File/Code: +8-23ms  every guest path lookup now traverses overlayfs;
                          /workspace writes hit the overlay upper (copy-up
                          machinery) instead of raw ext4
```

---

## Internal Daemon Benchmarks (Criterion)

All sub-microsecond — never the bottleneck.

### ix-code — Jupyter Protocol & Kernel Pool

| Benchmark | Time | Throughput |
|---|---|---|
| JupyterMessage::serialize | 692 ns | ~1.45M msg/s |
| JupyterMessage::deserialize | 741 ns | ~1.35M msg/s |
| HMAC-SHA256 sign | 397 ns | ~2.52M signs/s |
| extract_best_output (multi-mime) | 22 ns | ~45M/s |
| extract_best_output (text only) | 35 ns | ~29M/s |
| Kernel pool grab (async Mutex) | 152 ns | — |
| Kernel pool grab (sync Mutex) | 63 ns | — |

### ix-core — SSE & Serialization

| Benchmark | Time |
|---|---|
| SSE send_stdout | 476 ns |
| SSE send_complete | 508 ns |
| SSE send_result | 546 ns |
| Deserialize ShellRequest | 77 ns |
| Serialize FileContent (250 lines) | 1.67 us |
| Serialize GrepResult (20 matches) | 1.78 us |
| Serialize WebSearchResult (10 items) | 773 ns |

### ix-egress — DNS Policy Matching

| Benchmark | Time |
|---|---|
| Exact match (pypi.org) | 20 ns |
| Wildcard match (api.github.com) | 55 ns |
| Wildcard miss (evil.com) | 51 ns |
| 100 wildcard rules, hit last | 2.45 us |
| 100 wildcard rules, miss all | 2.41 us |
| Allowlist mode (default rules) | 357 ns |
| Denylist mode (default rules) | 358 ns |

### ix-fetch — HTML Parsing

| Benchmark | Time |
|---|---|
| Readability extraction (50 KB) | 133 us |
| Readability extraction (100 KB) | 253 us |
| Startpage parse (10 results) | 39 us |
| Startpage parse (50 results) | 186 us |

### ix-files — File Operations

| Benchmark | Time |
|---|---|
| Read + cat-n format (100 lines) | 14 us |
| Read + cat-n format (1,000 lines) | 89 us |
| Read + cat-n format (10,000 lines) | 231 us |
| Grep 100 files | 3.76 ms |
| Glob 100 files | 3.14 ms |
| Edit unique replace (1,000 lines) | 75 us |

### ix-shell — Process Spawning

| Benchmark | Time |
|---|---|
| Spawn `echo hello` | 18.3 ms |
| Throughput (1,000 lines) | 20.9 ms |

---

## Comparison with Competitors

| Metric | ix v0.2 (Firecracker) | OpenSandbox | CubeSandbox |
|---|---|---|---|
| Creation (cold) | 181ms | ~0.92s (K8s) | — |
| Creation (snapshot) | **15ms** | — | **<60ms** |
| Creation (pool) | 11ms | — | — |
| Shell echo e2e | **8.6ms** | — | — |
| File R+W | 8.5ms (**1.4ms** /workspace) | — | — |
| Code exec (warm) | **6.0ms** | 50-200ms | — |
| Per-sandbox memory | ~512 MB | ~50 MB | **<5 MB** |

---

## How to Run

```bash
# Internal daemon benchmarks (Criterion)
cd daemon && cargo bench

# End-to-end benchmarks (Firecracker). ALWAYS pass -run '^$' — without it the
# test binary runs every integration test before the benchmarks.
cd go-sdk && sudo IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4 \
  IX_KERNEL_PATH=/opt/ix/firecracker/vmlinux.bin \
  IX_FC_BINARY=/opt/ix/firecracker/firecracker \
  go test -tags integration -run '^$' -bench . -benchmem -benchtime 10x -count 5 -timeout 3600s

# Browser benchmarks: run as a SEPARATE pass. Each browser bench instance
# boots a browser-tier VM (up to 60s health wait) and the Browser* benches
# run before the core ones — a stale browser-vm image or SSRF misconfig burns
# the whole run's wall clock and exits FAIL before you see core numbers.
# (Without IX_BROWSER_VM_IMAGE the browser benches b.Skip cleanly.)
cd go-sdk && sudo IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4 \
  IX_KERNEL_PATH=/opt/ix/firecracker/vmlinux.bin \
  IX_FC_BINARY=/opt/ix/firecracker/firecracker \
  IX_BROWSER_VM_IMAGE=/opt/ix/rootfs/browser-vm.ext4 \
  go test -tags integration -run '^$' -bench Browser -benchmem -benchtime 10x -count 5 -timeout 3600s
# Optional realism check on top: IX_BENCH_REAL_SITE=https://example.com

# Formatted comparison table / benchstat compare of two result files
cd go-sdk && ./scripts/run-benchmarks.sh 5
```
