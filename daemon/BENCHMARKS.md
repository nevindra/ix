# ix Benchmark Results

Machine: AMD Ryzen 7 9700X 8-Core, Linux 6.18.7, x86_64
Rust: 1.87 (release profile, musl static)
Go: 1.26.1

---

## End-to-End Progress Tracker

All numbers measured with Go integration benchmarks (`benchtime=3x` through
v0.6; from v0.6.1 on: `benchtime=10x`, `count=5`, medians, benchstat for
significance).

### Optimization history

| Version | VMM | Transport | Creation | ShellPersistent | FileReadWrite | CodeExec (Python) | E2E (with Python) |
|---|---|---|---|---|---|---|---|
| v0.0 | Docker | TCP | 854ms | — | 80ms | 128ms | 753ms |
| v0.1 | Docker | Unix socket | 849ms | — | 46ms | 53ms | 422ms |
| v0.2 | Docker | Unix socket | **368ms** | — | 45ms | FAIL | 393ms |
| v0.3 | Firecracker | vsock UDS | 935ms cold | 20ms | 9ms | 15,100ms (Jupyter) | — |
| v0.3.1 | Firecracker | vsock UDS | 471ms cold | 15ms | 7ms | 15,100ms (Jupyter) | — |
| v0.4 | Firecracker | vsock UDS | 47ms snapshot | 15ms | 6ms | 15,100ms (Jupyter) | 78ms (no code) |
| **v0.5** | **Firecracker** | **vsock UDS** | **45ms snapshot** | **12ms** | **8ms** | **17ms (stdin REPL)** | **72ms** |
| **v0.6** | **Firecracker** | **vsock UDS** | **75ms snapshot** | **30ms** | **19ms** | **26ms** | **131ms** |
| **v0.6.1** | **Firecracker** | **vsock UDS** | **28ms snapshot** | **13ms** | **8.6ms** | **16ms** | **78ms** |
| **v0.7** | **Firecracker** | **vsock UDS** | **15ms snapshot** | **6.9ms** | **8.5ms (1.4ms /workspace)** | **6.0ms** | **59ms** |
| **Target** | **Firecracker** | **vsock UDS** | **<100ms** | **<3ms** | **<6ms** | **<10ms** | **<25ms** |

**v0.5 notes:** Replaced Jupyter/ZMQ kernel (15s boot) with stdin/stdout REPL (<100ms boot). Python code exec dropped from 15,100ms to 17ms — **888x faster**. REPL survives snapshot/restore because stdin/stdout pipes are kernel-managed IPC. E2E agent cycle with Python: 72ms.

**v0.6 notes:** Security/correctness release, not a perf release — every VM previously
mounted the SAME rootfs image read-write (fleet-wide ext4 corruption + cross-tenant
persistence bug). Now: shared read-only rootfs + per-VM sparse scratch disk via a
whole-root overlayfs (`ix-stage0`). The regression vs v0.5 conflates TWO changes that
landed in between: per-VM TAP + host NAT networking (v0.5 had no working egress) and
the disk isolation itself — TAP setup was never benchmarked separately. n=5 runs are
noisy (±30-80% run-to-run on creation paths); the most reproducible signal is
FileReadWrite 8→19ms (overlayfs on the /workspace write path). Candidate follow-ups:
bind-mount the scratch disk directly at /workspace (takes overlayfs out of the agent
file-ops hot path), journal-less scratch ext4, `benchtime 20x` for stable numbers.

**v0.6.1 notes:** Not a code release — same SDK/daemon as v0.6, benchmarks fixed
and re-run to create an honest baseline for v0.7. The published v0.6 numbers were
inflated 1.7–2.4x by benchmark bugs (pool benches raced the filler and measured
pool-miss cold boots; creation benches timed Destroy inside the loop) plus host
conditions. All v0.7 claims are judged against v0.6.1, not v0.6.

**v0.7 notes:** Perf overhaul — daemon-side persistent shell sessions, concurrent
REPL stdout/stderr reads (removed a fixed 10 ms drain per exec), serial console off
(`8250.nr_uarts=0` + `quiet`), `RUST_LOG=warn` default, vsock HTTP keep-alive, a
scratch pre-copy pool (snapshot-clone scratch is an `os.Rename`, 11 µs vs ~12 ms
`cp --sparse`), 1 ms readiness polling with an immediate first probe, two-phase
destroy, and `/workspace` bind-mounted directly on the scratch disk (overlayfs out
of the agent file-ops hot path). Targets hit: Creation 15 ms (<100), CodeExec
6.0 ms (<10), File R+W on /workspace 1.4 ms (<6). Still open: ShellPersistent
6.9 ms (<3 target), E2E 59 ms (<25 — now dominated by Destroy's kill+wait).
Inaugural browser-tier benchmarks added (see v0.7 section).

### v0.5 vs v0.0 — full journey

| Benchmark | v0.0 (Docker/TCP) | v0.5 (Firecracker/snapshot/REPL) | Speedup |
|---|---|---|---|
| **Creation** | 854ms | **45ms** | **19x** |
| **Shell (persistent)** | — | **12ms** | — |
| **File R+W** | 80ms | **8ms** | **10x** |
| **Code exec (Python)** | 128ms | **17ms** | **7.5x** |
| **E2E agent cycle** | 753ms | **72ms** | **10.5x** |

### v0.7 — perf overhaul: shell sessions, REPL drain fix, quiet boot, scratch pool, two-phase destroy (2026-06-04)

VMM: Firecracker v1.15.1, kernel vmlinux 6.1.155, host kernel 7.0.0-22
Method: frozen test binaries (pre/post), `benchtime 10x`, `count 5`, medians;
compared against the v0.6.1 baseline (identical pre-optimization code, same
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

**Per-fix attribution (benchstat v0.6.1 → v0.7, p=0.008 unless n.s.):**

| Benchmark | v0.6.1 | v0.7 | Δ | What did it |
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

**Strict-10x scorecard (the v0.7 goal was 10x vs published v0.6):**

| Metric | v0.6 published | 10x target | v0.7 | vs v0.6 | vs v0.6.1 (code only) |
|---|---|---|---|---|---|
| Creation (snapshot) | 75.1ms | 7.5ms | 15.3ms | **4.9x** | 1.8x |
| ShellPersistent | 30.2ms | 3.0ms | 6.9ms | **4.4x** | 1.9x |
| FileReadWrite | 19.4ms | 1.9ms | 8.5ms /tmp · 1.4ms /workspace | **2.3x · 13.5x** | 1.0x |
| CodeExec (snapshot) | 26.1ms | 2.6ms | 6.0ms | **4.4x** | 2.7x |
| E2E (snapshot cycle) | 130.6ms | 13.1ms | 58.9ms | **2.2x** | 1.3x |

Verdict: strict 10x reached only on /workspace file ops. Honest split: roughly
half the headline gap closed by fixing measurement (v0.6.1), the other half by
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
initial v0.7 run had two failing benchmarks. BrowserEval at 1.05ms is the
measured floor of the entire cross-VM browser path — every other op's cost is
Chrome work, not plumbing. Note: snapshot-restored VMs are vsock-only (no
TAP), so browser benches use the cold-boot pool, never `UseSnapshot`.

**Bugs found by the v0.7 browser run (both fixed and verified by rerun):**

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

**Open questions (v0.7):**

- File ops on `/tmp` are ~6x slower than `/workspace` (8.5ms vs 1.4ms per
  write+read pair) in BOTH v0.6.1 and v0.7 — pre-existing, not a regression.
  `/tmp` should be tmpfs (fast); root cause TBD.

### v0.6.1 — baseline re-measurement, fixed benchmarks (2026-06-04)

Same SDK/daemon code as v0.6 — only the benchmarks changed. Re-run to create an
honest baseline for v0.7, because two benchmark bugs inflated the published
v0.6 numbers:

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

Published v0.6 vs v0.6.1 on identical code: ShellPersistent 30.2→12.9ms,
CodeExecSnapshot 26.1→16.0ms, CreateFromSnapshot 75.1→27.8ms, E2ESnapshotCycle
130.6→77.8ms. That 1.7–2.4x gap is measurement methodology plus host
conditions, not code — which is why v0.7 is judged against v0.6.1, not v0.6.

### v0.6 — read-only rootfs + per-VM scratch overlay (2026-06-04)

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

**Where the new time goes (vs v0.5):**

```
CreateCold: +~190ms       TAP create + addr + up (3x ip exec) + nft, scratch
                          template copy (cp --sparse exec), scratch drive PUT,
                          guest stage0 (mount vdb + overlay + pivot_root)
CreateFromSnapshot: +30ms golden-scratch copy per clone (cp --sparse exec)
Shell/File/Code: +8-23ms  every guest path lookup now traverses overlayfs;
                          /workspace writes hit the overlay upper (copy-up
                          machinery) instead of raw ext4
```

### v0.5 — Firecracker + snapshot + stdin REPL (2026-05-25)

VMM: Firecracker v1.15.1, kernel vmlinux-5.10.245
Transport: HTTP+SSE over Firecracker vsock UDS proxy
Code execution: stdin/stdout REPL (replaced Jupyter/ZMQ)
Creation: snapshot/restore (no kernel boot)

```
BenchmarkCreateCold-16            5    424088063 ns/op    112262 B/op     672 allocs/op
BenchmarkCreateFromSnapshot-16    5     45218424 ns/op     64673 B/op     327 allocs/op
BenchmarkShellPersistent-16       5     12120983 ns/op     20337 B/op     118 allocs/op
BenchmarkShellOneShot-16          5     15090712 ns/op     17700 B/op     124 allocs/op
BenchmarkFileReadWrite-16         5      7706302 ns/op     31667 B/op     192 allocs/op
BenchmarkCodeExecSnapshot-16      5     17000149 ns/op     30016 B/op     117 allocs/op
BenchmarkE2ESnapshotCycle-16      5     72022564 ns/op    131425 B/op     770 allocs/op
```

### v0.3 vs v0.2 (Docker baseline)

| Benchmark | Docker v0.2 | Firecracker v0.3 | Change | What it measures |
|---|---|---|---|---|
| **CreateCold** | 368ms | 935ms | 2.5x slower | Full kernel boot + init + daemon start |
| **ShellEcho** | 42ms | 25ms | **1.7x faster** | `echo hello` round-trip on running sandbox |
| **ShellPersistent** | — | 20ms | — | Persistent bash session `echo hello` |
| **ShellOneShot** | — | 21ms | — | Fresh fork+exec per command |
| **FileReadWrite** | 45ms | **9ms** | **5.0x faster** | Write + read round-trip |
| **CodeExecPython** | FAIL | 15.1s | — | Warm kernel `x=42` (kernel warmup slow) |

**What improved:** Operations on a running sandbox are 1.7-5x faster. Firecracker eliminates Docker's container namespace transitions (~22-43ms overhead per operation). File I/O sees the biggest gain because virtiofs block device access has no namespace overhead.

**What regressed:** Cold start is 2.5x slower (935ms vs 368ms). Firecracker boots a full Linux kernel + runs init + starts daemon, vs Docker which shares the host kernel. The VM pool eliminates this from the hot path.

**What's broken:** CodeExecPython takes 15s — the Python kernel warmup inside Firecracker is very slow. Needs investigation (likely Jupyter/ZMQ startup overhead in the VM).

### Remaining work (Phase 1)

| Item | Status | Expected impact |
|---|---|---|
| VM pool benchmarks | Not measured | CreateCold → <1ms (pool grab) |
| Pre-warmed Python kernel | Not measured | CodeExecPython → ~10ms |
| E2E agent cycle (pool) | Not measured | ~25ms target |
| passt network overhead | Not measured | Baseline for Phase 2 vsock proxy |
| Cold start optimization | Not started | Boot args tuning, initrd, kernel config |

---

## Detailed Results

### v0.3 — Firecracker + passt (2026-05-25)

VMM: Firecracker v1.15.1, kernel vmlinux-5.10.245
Transport: HTTP+SSE over Firecracker vsock UDS proxy
Networking: passt (user-mode, no root)
Rootfs: ext4 image (2 GB, exported from ix:base Docker image)

```
BenchmarkCreateCold-16           3    935166295 ns/op    115762 B/op     678 allocs/op
BenchmarkShellEcho-16            3     24728948 ns/op     20661 B/op     136 allocs/op
BenchmarkShellPersistent-16      3     20210943 ns/op     25850 B/op     121 allocs/op
BenchmarkShellOneShot-16         3     21334284 ns/op     17653 B/op     131 allocs/op
BenchmarkFileReadWrite-16        3      9317124 ns/op     23464 B/op     198 allocs/op
BenchmarkCodeExecPython-16       3  15109767692 ns/op     59490 B/op     276 allocs/op
```

**Where time goes (v0.3 Firecracker):**

```
CreateCold: 935ms total
  ├── Firecracker process start       ~5ms
  ├── API socket ready                ~5ms
  ├── VM config (5x PUT)              ~5ms
  ├── Linux kernel boot               ~850ms
  ├── ix-init (mount, env parse)      ~10ms
  ├── ixd daemon start                ~50ms
  └── READY signal over vsock         ~10ms

ShellEcho: 25ms total (was 42ms)
  ├── vsock CONNECT handshake         ~1ms
  ├── HTTP + SSE overhead             ~2ms
  ├── fork+exec (bash -l -c "echo")   ~18ms (unchanged)
  └── Remaining overhead              ~4ms (was ~22ms with Docker)

FileReadWrite: 9ms total (was 45ms)
  ├── 2x vsock CONNECT + HTTP         ~4ms
  ├── File write (tokio::fs)          ~0.1ms
  ├── File read + format              ~0.01ms
  └── Remaining overhead              ~5ms (was ~43ms with Docker)
```

### v0.2 — Docker P0 optimizations (2026-05-24)

Docker 29.5.2, ix:base image (914 MB), Unix domain socket transport.
Two-phase waitReady (10ms socket poll + 25ms health poll), parallel pool fill.

| Benchmark | v0.1 | v0.2 (P0) | Change | Allocs/op |
|---|---|---|---|---|
| CreateCold | 849ms | **368ms** | **2.3x faster** | 838 |
| CreateFromPool | 529ms | **465ms** | 1.1x faster | 1,023 |
| ShellEcho | 50ms | **42ms** | 1.2x faster | 183 |
| CodeExecPython | 53ms | **FAIL** | — | — |
| CodeExecFirstCall | 2,442ms | **2,750ms** | ~1.0x | 1,033 |
| FileReadWrite | 46ms | **45ms** | ~1.0x | 250 |
| EndToEnd | 422ms | **393ms** | **1.1x faster** | 1,335 |

### v0.1 — Unix socket transport (2026-05-24)

| Benchmark | TCP (v0.0) | Unix socket (v0.1) | Speedup |
|---|---|---|---|
| CreateCold | 854ms | 849ms | 1.0x |
| ShellEcho | 126ms | **50ms** | **2.5x** |
| CodeExecPython | 128ms | **53ms** | **2.4x** |
| FileReadWrite | 80ms | **46ms** | **1.7x** |
| EndToEnd | 753ms | **422ms** | **1.8x** |

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

| Metric | ix v0.2 (Docker) | ix v0.3 (Firecracker) | OpenSandbox | CubeSandbox |
|---|---|---|---|---|
| Creation (cold) | 368ms | 935ms | ~0.92s (K8s) | **<60ms** (snapshot) |
| Creation (pool) | ~1ms | not measured | — | — |
| Shell echo e2e | 42ms | **25ms** | — | — |
| File R+W | 45ms | **9ms** | — | — |
| Code exec (warm) | 53ms (v0.1) | — | 50-200ms | — |
| Per-sandbox memory | ~2 GB | ~512 MB | ~50 MB | **<5 MB** |

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
