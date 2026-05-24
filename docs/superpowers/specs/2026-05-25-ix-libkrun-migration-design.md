# ix libkrun Migration — 10x E2E Performance Design

**Date:** 2026-05-25
**Status:** Draft
**Goal:** Replace Docker with libkrun MicroVMs to achieve ~10x improvement on the full end-to-end agent cycle (create → execute → destroy).

## Current Baseline (v0.2, Docker)

| Operation | Measured | Bottleneck |
|---|---|---|
| Creation (cold) | 368ms | Docker ContainerStart (~200ms), network create (~50ms) |
| Creation (pool) | ~1ms | Pool grab — already fast |
| Shell echo | 42ms | Container namespace transitions (~22ms), fork+exec (~18ms) |
| File R+W | 45ms | Container namespace transitions (~43ms) |
| Code exec (warm) | 53ms | Namespace overhead + ZMQ round-trip |
| First code exec | 2,750ms | Python kernel cold boot inside container |
| E2E agent cycle | 393ms | Creation + 4 operations + destroy |

## Target Performance

| Operation | Current | Target | Required speedup |
|---|---|---|---|
| Creation (cold) | 368ms | <100ms | 3.7x |
| Shell echo | 42ms | <5ms | 8.4x |
| File R+W | 45ms | <6ms | 7.5x |
| Code exec (warm) | 53ms | <10ms | 5.3x |
| First code exec | 2,750ms | <300ms | 9.2x |
| **E2E cycle** | **393ms** | **<40ms** | **~10x** |
| **E2E cycle (pool)** | **393ms** | **<25ms** | **~16x** |

---

## Architecture

### Current (Docker)

```
Go SDK (IXManager)
  └── Docker API
        └── container
              └── ix daemon (Rust, HTTP+SSE on Unix socket)
```

### Proposed (libkrun)

```
Go SDK (IXManager)
  └── fork/exec ix-vmm
        └── libkrun MicroVM (KVM/HVF)
              └── ix daemon (Rust, HTTP+SSE on vsock)

Transport:  HTTP+SSE over vsock (port 1024)
Rootfs:     Host directory via virtiofs
Networking: TSI (transparent socket impersonation, no root, no TAP)
Readiness:  Daemon sends "READY\n" over vsock:1025 (no polling)
```

### Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| VMM | libkrun | Embedded library, no daemon, no root, macOS support, <100ms cold boot |
| Transport | HTTP+SSE over vsock | Keeps DX (curl, browser, Axum routes). ~2ms overhead acceptable. |
| Rootfs | virtiofs directory | No image build pipeline. Point at a host directory. |
| Networking | TSI | No TAP, no bridge, no root. Guest sockets proxied transparently. |
| Docker | Clean break | No backward compatibility. All sandboxes on libkrun. |
| Go SDK binding | `mishushakov/libkrun-go` | Existing Go bindings, wraps full API. |
| Platform | Linux primary, macOS best-effort | libkrun supports both via KVM/HVF. |

---

## Components

### 1. ix-vmm (new binary)

Thin Go binary (~100 lines) that configures and starts a libkrun VM. The Go SDK fork/execs this per sandbox because `krun_start_enter()` takes over the process and calls `exit()`.

```go
// cmd/ix-vmm/main.go
func main() {
    cfg := parseArgs()
    ctx, _ := krun.CreateContext()
    ctx.SetVMConfig(krun.VMConfig{NumVCPUs: cfg.VCPUs, RAMMiB: cfg.Memory})
    ctx.SetRoot(cfg.RootfsPath)
    ctx.SetExec(krun.ExecConfig{
        Path: "/usr/bin/ix",
        Args: []string{"/usr/bin/ix"},
        Env:  cfg.Env,
    })
    ctx.AddVsockPort(krun.VsockPortConfig{Port: 1024, Path: cfg.VsockPath})
    ctx.AddVsockPort(krun.VsockPortConfig{Port: 1025, Path: cfg.ReadyPath, Listen: true})
    ctx.StartEnter() // never returns
}
```

**Inputs:** rootfs path, vCPU count, memory MiB, vsock socket paths, env vars — all via CLI flags.
**Output:** A running MicroVM with the ix daemon listening on vsock:1024.

### 2. Go SDK Changes (go-sdk/)

The public API (`sandbox.Manager`, `sandbox.Sandbox`) stays identical. Internal changes:

| File | Change |
|---|---|
| `manager.go` | Replace Docker client with fork/exec of `ix-vmm`. Pool creates VMs. Remove all Docker imports. |
| `client.go` | Replace `unixSocketTransport()` with `vsockTransport()`. |
| `health.go` | Replace health polling with vsock Ready signal. Process monitoring replaces container monitoring. |
| `reaper.go` | Replace Docker container listing with child process tracking. |
| `sandbox.go` | `Close()` sends shutdown over vsock, then kills child process. |

**ManagerConfig:**

```go
type ManagerConfig struct {
    RootfsPath    string        // path to rootfs directory
    VMMBinary     string        // path to ix-vmm binary (default: search PATH)
    MaxConcurrent int
    DefaultTTL    time.Duration
    PerSandbox    sandbox.ResourceSpec
    MaxRestarts   int
    Logger        *slog.Logger
    DefaultEgress *EgressPolicy
    PoolSize      int
    PoolMinReady  int
    PoolWorkers   int
}
```

**Removed fields:** `Image`, `Runtime`, `SharedNetwork`.

**Creation flow:**

```
1. fork/exec ix-vmm             ~5ms
2. libkrun kernel boot           ~60-80ms
3. vsock Ready signal             ~1ms
4. Return IXSandbox               —
─────────────────────────────────
Total:                           ~70-90ms
```

**Readiness detection:**

No polling. The SDK creates a Unix socket for vsock:1025, fork/execs ix-vmm, then blocks reading from that socket. When the daemon writes `"READY\n"`, the SDK unblocks and proceeds.

**Sandbox teardown:**

```
1. Send POST /shutdown to daemon via vsock:1024
2. Daemon calls graceful shutdown (10s timeout)
3. If timeout: kill ix-vmm child process (SIGKILL)
4. Clean up vsock socket files
```

### 3. Daemon Changes (daemon/)

Minimal. Only the listener setup changes:

```rust
// Before (Unix socket):
let listener = UnixListener::bind(socket_path)?;
axum::serve(listener, app).await?;

// After (vsock):
let listener = VsockListener::bind(VMADDR_CID_ANY, 1024)?;
let ready = VsockStream::connect(VMADDR_CID_HOST, 1025)?;
ready.write_all(b"READY\n")?;
drop(ready);
axum::serve(listener, app).await?;
```

**New dependency:** `tokio-vsock` crate.

**Everything else stays:** All route handlers, SSE streaming, Jupyter kernel pool, egress filtering, file ops, browser bridge.

**Init behavior:** Daemon runs as PID 1 via `krun_set_exec`. Must handle SIGCHLD for child processes (shell commands, Python kernels).

### 4. Rootfs Build (new)

Script that produces a rootfs directory from the existing Docker image or from scratch:

```bash
#!/bin/bash
# scripts/build-rootfs.sh
set -euo pipefail

TIER=${1:-base}  # base, browser, full
OUT="/opt/ix/rootfs/${TIER}"

# Export from existing Docker image
docker export $(docker create "ix:${TIER}") | tar -xf - -C "${OUT}/"

# Copy the ix daemon binary
cp target/release/ix "${OUT}/usr/bin/ix"
```

**Image tiers:**

| Tier | Contents | Approx size |
|---|---|---|
| `base` | Ubuntu 24.04 + Python + Node.js + ix daemon | ~400MB |
| `browser` | base + Chrome + Pinchtab | ~1.5GB |
| `full` | browser + scientific Python packages | ~3GB |

Each tier is a directory on disk. `ManagerConfig.RootfsPath` points to it.

### 5. Networking (TSI)

libkrun's TSI transparently proxies guest socket syscalls to the host. No configuration needed.

- Guest calls `connect("api.github.com:443")` → libkrun proxies to host TCP
- Port exposure via `krun_set_port_map(["8080:8080"])`
- No TAP devices, no bridges, no iptables, no root privileges

**Egress policy:** The existing `ix-egress` crate handles DNS-based filtering inside the daemon. Unchanged — TSI doesn't bypass it.

**Limitation:** TSI only supports `SOCK_STREAM` and `SOCK_DGRAM`. No raw sockets. Sufficient for sandbox workloads.

---

## Performance Optimizations

### Optimization 1: VM Pool

Same pool concept as the current container pool, but with pre-booted VMs:

```go
PoolSize:    3   // keep 3 VMs pre-booted
PoolWorkers: 3   // fill in parallel
```

Pool grab is ~1ms (dequeue + assign session ID). Background workers replenish continuously.

With a warm pool, creation is effectively instant. The E2E cycle drops from ~50ms to ~25ms.

### Optimization 2: Persistent Shell Sessions

Instead of fork+exec per `Shell()` call (~18ms), keep a bash process alive per sandbox and pipe commands to its stdin:

```
First Shell() call:  spawn bash, keep alive     ~18ms
Subsequent calls:    write to stdin, read output ~1-2ms
```

The daemon already manages long-running processes (Jupyter kernels). Same pattern for shell sessions.

**Impact:** Shell echo drops from ~12ms to ~3ms.

### Optimization 3: Pre-warmed Python Kernels in Pool

VMs in the pool can pre-boot Python kernels during pool fill. When a sandbox is claimed from the pool, the kernel is already warm.

**Impact:** First code exec drops from ~300ms to ~10ms.

---

## What Gets Deleted

- All Docker imports and API calls in `go-sdk/`
- `EnsureImage()` (no Docker pull)
- `resolveConnection()`, `networkIDFromContainer()`
- `destroyContainer()`, `sweepOrphanedNetworks()`
- `initSharedNetwork()`, `allocateNetwork()`, `cleanupNetwork()`
- Unix socket temp directory management (`/tmp/ix-<id>/`)
- Docker network create/remove logic
- Container labels, restart policy configuration
- `Dockerfile` in `cmd/ix/` (replaced by rootfs build script)

---

## Projected Performance

| Operation | Current (v0.2) | Projected (cold) | Projected (pool) | Speedup (pool) |
|---|---|---|---|---|
| Creation | 368ms | ~80ms | ~1ms | **368x** |
| Shell echo | 42ms | ~12ms | ~3ms (persistent) | **14x** |
| File R+W | 45ms | ~6ms | ~6ms | **7.5x** |
| Code exec (warm) | 53ms | ~12ms | ~10ms | **5.3x** |
| First code exec | 2,750ms | ~300ms | ~10ms (pre-warmed) | **275x** |
| **E2E cycle** | **393ms** | **~50ms** | **~25ms** | **~16x** |

The E2E agent cycle with a warm pool achieves ~16x improvement. Individual operations range from 5-14x. File R+W at 7.5x is the closest to the 10x boundary — bounded by virtiofs latency and HTTP round-trips.

---

## Phases

| Phase | Scope | Deliverable |
|---|---|---|
| **Phase 1** | libkrun integration | ix-vmm binary, Go SDK refactor (Docker → libkrun), rootfs build script. Daemon unchanged — communicates via virtiofs shared Unix socket (same as today). Creation <100ms. |
| **Phase 2** | vsock transport | Daemon listener switches to vsock, Go SDK vsock transport, Ready signal replaces health polling. Operations drop ~3-5x from eliminated namespace overhead. |
| **Phase 3** | Performance polish | VM pool, persistent shell sessions, pre-warmed kernels. E2E <25ms with pool. |
| **Phase 4** | Benchmarks + docs | Full benchmark suite, comparison doc update with measured numbers. |

---

## Risks

| Risk | Mitigation |
|---|---|
| `libkrun-go` bindings incomplete or buggy | Bindings wrap full API (45 functions). 54 stars, active. Fallback: write CGo directly (~200 lines). |
| CGo build complexity (cross-compilation) | Linux primary. Static link libkrun. macOS via Homebrew. |
| `krun_start_enter()` process model | Each VM is a child process. Same lifecycle as Docker containers. Microsandbox validates this pattern. |
| virtiofs performance for file-heavy workloads | libkrun supports DAX mode for virtiofs. Benchmark and tune. |
| TSI networking limitations (no raw sockets) | Sufficient for sandbox workloads. Fall back to TAP if needed. |
| libkrunfw kernel too minimal (missing drivers) | Custom kernel via `krun_set_kernel()` as escape hatch. |

---

## Dependencies

| Dependency | Version | Purpose |
|---|---|---|
| `containers/libkrun` | 1.18.x | VMM library |
| `containers/libkrunfw` | latest | Bundled Linux kernel |
| `mishushakov/libkrun-go` | latest | Go bindings |
| `tokio-vsock` | latest | Rust vsock listener for daemon |

## Non-Goals

- Multi-node / cluster orchestration (CubeSandbox territory)
- E2B API compatibility
- Snapshot/restore (libkrun doesn't support it; cold boot is fast enough)
- CBOR/binary protocol (HTTP+SSE over vsock is sufficient)
- Windows host support
