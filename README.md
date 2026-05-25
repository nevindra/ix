# oasis-sandbox-ix

Sandbox runtime for [Oasis](https://github.com/nevindra/oasis), the Go AI agent framework. Each sandbox is a lightweight [Firecracker](https://github.com/firecracker-microvm/firecracker) MicroVM running the `ix` daemon, communicating with the host over vsock.

```
Your Go app
  └── ix.NewManager()           ← manages VMs, pool, lifecycle
        └── firecracker process ← Firecracker MicroVM (KVM)
              └── ix daemon     ← PID 1, HTTP+SSE on vsock:1024
```

## Prerequisites

- Linux x86_64 with KVM (`/dev/kvm` must exist)
- [Firecracker](https://github.com/firecracker-microvm/firecracker) v1.12+
- Go 1.22+
- Docker (only for building the rootfs image)

### Install Firecracker

```bash
cd go-sdk
./scripts/install-firecracker.sh
# Installs firecracker and jailer to /usr/local/bin/
```

### Build the rootfs

```bash
# Requires the ix Docker image to exist locally
docker build -t ix:base -f daemon/cmd/Dockerfile daemon/

# Export to an ext4 rootfs image
cd go-sdk
./scripts/build-rootfs-ext4.sh base
# Output: /opt/ix/rootfs/base.ext4
```

#### Rootfs tiers

| Tier | Contents | Size |
|---|---|---|
| `base` | Ubuntu 24.04 + Python + Node.js + ix daemon | ~400 MB |
| `browser` | base + Chrome + Pinchtab | ~1.5 GB |
| `full` | browser + scientific Python packages | ~3 GB |

Build a specific tier:

```bash
./scripts/build-rootfs-ext4.sh browser
# or set custom output:
IX_ROOTFS_OUT=/my/path ./scripts/build-rootfs-ext4.sh full
```

## Install

```bash
go get github.com/nevindra/oasis-sandbox-ix
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
        RootfsPath:     "/opt/ix/rootfs/base.ext4",
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
    RootfsPath     string        // required: path to rootfs ext4 image
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

### Egress policy

```go
mgr, _ := ix.NewManager(ctx, ix.ManagerConfig{
    RootfsPath: "/opt/ix/rootfs/base.ext4",
    DefaultEgress: &ix.EgressPolicy{
        Enabled: true,
        Mode:    "allow",
        Rules:   []string{"pypi.org", "*.github.com", "api.openai.com"},
    },
})
```

### Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `IX_ROOTFS` | Override rootfs path | `/opt/ix/rootfs/base.ext4` |

## Daemon

The Rust daemon (`daemon/`) runs inside each VM as PID 1 and exposes REST + SSE endpoints for shell execution, code execution (Python/JS/Bash), file operations, HTTP fetch, web search, browser automation, and DNS-based egress filtering.

The runtime image is also published to GHCR for Docker-based usage:

```bash
docker run --shm-size=2g -p 8080:8080 ghcr.io/nevindra/oasis-sandbox-ix:latest
```

## Tests

```bash
# Go SDK — unit tests
cd go-sdk && go test ./... -count=1

# Go SDK — integration tests (requires full stack)
cd go-sdk && go test -tags=integration -v ./...

# Rust daemon
cd daemon && cargo test --all
```

## Benchmarks

Benchmarks require a working Firecracker stack (KVM + firecracker + rootfs).

```bash
export IX_ROOTFS=/opt/ix/rootfs/base.ext4

# Quick run (5 iterations per benchmark)
cd go-sdk && ./scripts/run-benchmarks.sh

# More iterations for stable numbers
./scripts/run-benchmarks.sh 20

# Individual benchmarks
go test -bench=BenchmarkCreateCold -benchtime=10x -tags=integration -count=1 .
```

| Operation | Docker (v0.2) | Target (cold) | Target (pool) |
|---|---|---|---|
| Creation | 368 ms | < 100 ms | < 1 ms |
| Shell echo | 42 ms | < 12 ms | < 3 ms |
| File R+W | 45 ms | < 6 ms | < 6 ms |
| Code exec (warm) | 53 ms | < 10 ms | < 10 ms |
| First code exec | 2,750 ms | < 300 ms | < 10 ms |
| E2E agent cycle | 393 ms | < 50 ms | < 25 ms |

## License
