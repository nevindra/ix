# ix Go SDK

Go library for managing sandboxed code execution environments using libkrun MicroVMs.

Each sandbox is a lightweight KVM virtual machine running the `ix` daemon, communicating with the host over vsock. The SDK handles VM lifecycle, pooling, health monitoring, and graceful teardown.

## Architecture

```
Your Go app
  └── ix.NewManager()         ← manages VMs, pool, lifecycle
        └── ix-vmm process    ← libkrun MicroVM (KVM)
              └── ix daemon   ← PID 1, HTTP+SSE on vsock:1024
```

## Prerequisites

- Linux x86_64 or arm64 with KVM (`/dev/kvm` must exist)
- [libkrun](https://github.com/containers/libkrun) v1.18+
- [libkrunfw](https://github.com/containers/libkrunfw) (bundled guest kernel)
- Go 1.22+
- Docker (only for building the rootfs image)

### Install libkrun

```bash
# Ubuntu/Debian — build from source
sudo apt install build-essential libclang-dev
git clone https://github.com/containers/libkrun.git
cd libkrun && make && sudo make install

# Install the guest kernel
git clone https://github.com/containers/libkrunfw.git
cd libkrunfw && make && sudo make install

# Verify
ldconfig -p | grep libkrun
```

### Build ix-vmm

```bash
cd cmd/ix-vmm
go build -o ix-vmm .
sudo install ix-vmm /usr/local/bin/
```

### Build the rootfs

```bash
# Requires the ix Docker image to exist locally
docker build -t ix:base -f ../daemon/Dockerfile ../daemon

# Export to a rootfs directory
./scripts/build-rootfs.sh base
# Output: /opt/ix/rootfs/base
```

## Usage

```go
package main

import (
    "context"
    "fmt"
    "log"

    ix "github.com/nevindra/oasis-sandbox-ix"
    "github.com/nevindra/oasis/sandbox"
)

func main() {
    ctx := context.Background()

    mgr, err := ix.NewManager(ctx, ix.ManagerConfig{
        RootfsPath:     "/opt/ix/rootfs/base",
        PoolSize:       3,
        PreWarmKernels: []string{"python"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer mgr.Close()

    sb, err := mgr.Create(ctx, sandbox.CreateOpts{
        SessionID: "my-session",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer mgr.Destroy(ctx, "my-session")

    result, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo hello"})
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.Output)
}
```

## Configuration

```go
ix.ManagerConfig{
    RootfsPath     string        // required: path to rootfs directory
    VMMBinary      string        // ix-vmm binary path (default: search PATH)
    MaxConcurrent  int           // max simultaneous VMs (default: auto-detect)
    DefaultTTL     time.Duration // sandbox lifetime (default: 1h)
    PerSandbox     ix.ResourceSpec{
        VCPUs  int   // default: 1
        Memory int64 // bytes, default: 512 MiB
    }
    MaxRestarts    int           // health restarts before circuit break (default: 3)
    DefaultEgress  *ix.EgressPolicy // DNS-based egress filtering
    PoolSize       int           // pre-warmed VMs (default: 0 = disabled)
    PoolMinReady   int           // replenish threshold (default: 1)
    PoolWorkers    int           // parallel pool fill (default: 3)
    PreWarmKernels []string      // languages to pre-boot in pool (e.g., ["python"])
}
```

## Deployment

### Single host

1. Install libkrun + libkrunfw
2. Build ix-vmm and place in PATH
3. Build rootfs via `scripts/build-rootfs.sh`
4. Import the SDK and call `ix.NewManager()`

### Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `IX_ROOTFS` | Override rootfs path | `/opt/ix/rootfs/base` |
| `IX_VMM_BINARY` | Override ix-vmm binary path | Search PATH |

### Rootfs tiers

| Tier | Contents | Size |
|---|---|---|
| `base` | Ubuntu 24.04 + Python + Node.js + ix daemon | ~400 MB |
| `browser` | base + Chrome + Pinchtab | ~1.5 GB |
| `full` | browser + scientific Python packages | ~3 GB |

Build a specific tier:

```bash
./scripts/build-rootfs.sh browser
# or set custom output:
IX_ROOTFS_OUT=/my/path ./scripts/build-rootfs.sh full
```

### Egress policy

```go
mgr, _ := ix.NewManager(ctx, ix.ManagerConfig{
    RootfsPath: "/opt/ix/rootfs/base",
    DefaultEgress: &ix.EgressPolicy{
        Enabled: true,
        Mode:    "allow",
        Rules:   []string{"pypi.org", "*.github.com", "api.openai.com"},
    },
})
```

## Benchmarks

Benchmarks require a working libkrun stack (KVM + libkrun + rootfs + ix-vmm).

### Run the full suite

```bash
export IX_ROOTFS=/opt/ix/rootfs/base
export IX_VMM_BINARY=/usr/local/bin/ix-vmm

# Quick run (5 iterations per benchmark)
./scripts/run-benchmarks.sh

# More iterations for stable numbers
./scripts/run-benchmarks.sh 20
```

### Run individual benchmarks

```bash
go test -bench=BenchmarkCreateCold -benchtime=10x -tags=integration -count=1 .
go test -bench=BenchmarkShellPersistent -benchtime=50x -tags=integration -count=1 .
go test -bench=BenchmarkEndToEnd -benchtime=10x -tags=integration -count=1 .
```

### Available benchmarks

| Benchmark | What it measures |
|---|---|
| `BenchmarkCreateCold` | VM creation from scratch (no pool) |
| `BenchmarkCreateFromPool` | Grab pre-booted VM from pool |
| `BenchmarkCreatePoolPreWarmed` | Pool grab + first code exec with pre-warmed kernel |
| `BenchmarkShellPersistent` | Shell with persistent bash session (steady-state) |
| `BenchmarkShellOneShot` | Shell with fresh fork+exec per call |
| `BenchmarkCodeExecPython` | Python execution after kernel is warm |
| `BenchmarkCodeExecFirstCall` | Cold kernel boot + first execution |
| `BenchmarkFileReadWrite` | File write + read round-trip |
| `BenchmarkEndToEnd` | Full agent cycle: create + shell + file + code + destroy |
| `BenchmarkE2EAgentCycle` | Same as above with pre-warmed pool |

### Performance targets

| Operation | Docker (v0.2) | Target (cold) | Target (pool) |
|---|---|---|---|
| Creation | 368 ms | < 100 ms | < 1 ms |
| Shell echo | 42 ms | < 12 ms | < 3 ms |
| File R+W | 45 ms | < 6 ms | < 6 ms |
| Code exec (warm) | 53 ms | < 10 ms | < 10 ms |
| First code exec | 2,750 ms | < 300 ms | < 10 ms |
| E2E agent cycle | 393 ms | < 50 ms | < 25 ms |

## Tests

```bash
# Unit tests (no external dependencies)
go test ./...

# Integration tests (requires full stack)
go test -tags=integration -v ./...
```

## Project structure

```
go-sdk/
  manager.go        VM lifecycle, pool, concurrency
  sandbox.go        Sandbox methods (shell, code, files, browser)
  client.go         HTTP+SSE client for ix daemon
  health.go         Health monitoring and auto-restart
  reaper.go         TTL enforcement and disk pressure eviction
  egress.go         Egress policy types
  cmd/ix-vmm/       Thin binary that boots a libkrun VM
  scripts/
    build-rootfs.sh     Export Docker image to rootfs directory
    run-benchmarks.sh   Run benchmarks and print comparison table
```
