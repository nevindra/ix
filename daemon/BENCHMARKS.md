# ix Benchmark Results

Machine: AMD Ryzen 7 9700X 8-Core, Linux 6.18.7, x86_64
Rust: 1.87 (release profile, musl static)
Go: 1.26.1

---

## End-to-End Progress Tracker

All numbers measured with Go integration benchmarks (`benchtime=3x`).

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
| **Target** | **Firecracker** | **vsock UDS** | **<100ms** | **<3ms** | **<6ms** | **<10ms** | **<25ms** |

**v0.5 notes:** Replaced Jupyter/ZMQ kernel (15s boot) with stdin/stdout REPL (<100ms boot). Python code exec dropped from 15,100ms to 17ms — **888x faster**. REPL survives snapshot/restore because stdin/stdout pipes are kernel-managed IPC. E2E agent cycle with Python: 72ms.

### v0.5 vs v0.0 — full journey

| Benchmark | v0.0 (Docker/TCP) | v0.5 (Firecracker/snapshot/REPL) | Speedup |
|---|---|---|---|
| **Creation** | 854ms | **45ms** | **19x** |
| **Shell (persistent)** | — | **12ms** | — |
| **File R+W** | 80ms | **8ms** | **10x** |
| **Code exec (Python)** | 128ms | **17ms** | **7.5x** |
| **E2E agent cycle** | 753ms | **72ms** | **10.5x** |

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

# End-to-end benchmarks (Firecracker)
cd go-sdk && IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4 \
  IX_KERNEL_PATH=/opt/ix/firecracker/vmlinux.bin \
  IX_FC_BINARY=/opt/ix/firecracker/firecracker \
  go test -tags integration -bench . -benchmem -benchtime 5x -count 1 -timeout 600s

# Formatted comparison table
cd go-sdk && ./scripts/run-benchmarks.sh 5
```
