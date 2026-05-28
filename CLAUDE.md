# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

ix is a sandbox runtime for [Oasis](https://github.com/nevindra/oasis), a Go AI agent framework. It has two halves:

- **Rust daemon** (`daemon/`) — an HTTP server that runs *inside* each sandbox (Docker container or Firecracker MicroVM) and exposes shell, code execution, file ops, browser, fetch, and egress control over REST + SSE.
- **Go SDK** (`go-sdk/`) — the host-side library that manages sandbox lifecycles (create/destroy/pool/snapshot) and proxies every operation to the daemon via HTTP.

## Build & Test Commands

### Rust daemon

```bash
# All from daemon/ directory
cd daemon

cargo test --all                    # run all tests
cargo test -p ix-shell              # test a single crate
cargo test -p ix-server -- --test-threads=1  # integration tests (must be serial)

cargo build --release -p ix-server  # build the ixd binary
cargo build --release --target x86_64-unknown-linux-musl -p ix-server  # static musl build (CI uses this)

cargo bench -p ix-code              # benchmarks for a single crate
```

### Go SDK

```bash
# All from go-sdk/ directory
cd go-sdk

go test ./... -count=1              # unit tests
go test -run TestManager ./...      # run a specific test
go test -bench=. ./...              # benchmarks

# Integration tests require a running daemon
go test -tags=integration ./...
```

### Docker image

```bash
docker build -f daemon/cmd/Dockerfile daemon/  # builds multi-stage (base → browser → full)
docker run --shm-size=2g -p 8080:8080 <image>  # Chrome needs --shm-size=2g
```

## Architecture

### Daemon crate graph

```
ix-server (binary: ixd)
├── ix-core      shared types, config, error → HTTP mapping, SSE channel primitives
├── ix-shell     bash execution with streaming, timeout, process group cleanup
├── ix-code      polyglot REPL (Python/JS/Bash) via stdin/stdout sentinel protocol
├── ix-files     read/write/edit/glob/grep/tree/stat/upload/download
├── ix-fetch     HTTP fetch (raw or readable-text extraction) + web search (Startpage)
├── ix-egress    DNS-level egress firewall (allowlist/denylist with wildcard domains)
└── ix-browser   headless Chrome via Pinchtab (navigate/screenshot/action/DOM/PDF/eval)
```

`ix-core` is the shared foundation — all other crates are leaf implementations consumed by `ix-server`.

### Key daemon patterns

- **Config is env vars only** — `DaemonConfig` reads `IX_ADDR`, `IX_SOCKET`, `IX_VSOCK_PORT`, `IX_WORKSPACE`, `IX_EGRESS_*`. No config files.
- **Transport priority** — server binds vsock (MicroVM) > Unix socket > TCP, selected at startup. On vsock, a `READY\n` signal is sent to the host.
- **SSE streaming** — `sse_channel(buffer)` returns `(SseSender, SseResponse)` with 15s keepalive pings. Route handlers spawn a task for the work and immediately return the SSE response.
- **Error handling** — `ix_core::Error` implements `IntoResponse` directly (maps to HTTP status codes). No error middleware.
- **Two API surfaces** — E2B-compatible routes (`/sandboxes/{id}/...`) and ix-native routes (`/v1/...`) on the same server.

### Go SDK architecture

- **`IXManager`** — pool-based lifecycle manager. Pre-warms VMs in a pool, auto-detects concurrency from host CPU/RAM, runs background goroutines for monitoring (10s), reaping (30s TTL + disk pressure), and pool replenishment.
- **`IXSandbox`** — pure HTTP proxy. Every method (Shell, ExecCode, ReadFile, BrowserNavigate, etc.) is a POST to the daemon's REST API. Streaming uses SSE with context-aware cancellation.
- **VMM layer** — Firecracker backend: allocates vsock CID, launches `passt` for user-mode networking, configures VM via Firecracker API (PUT boot source, rootfs, machine config, vsock), fires `InstanceStart`. Env vars pass via kernel boot args.
- **Snapshot/restore** — `CreateGolden` boots a temp VM, pre-warms Python kernel, pauses, writes a full snapshot. Restore loads snapshot and polls `/health` (no READY handshake needed since daemon was already running).
- **Vsock transport** — custom `http.Transport` that dials the vsock UDS, sends `CONNECT 1024\n`, reads `OK <port>\n`, then uses the connection as TCP to the daemon.

### Dockerfile stages

`daemon/cmd/Dockerfile` is a 4-stage build: `builder` (Rust musl) → `base` (Python + ixd) → `browser` (Node.js + Chrome + Pinchtab) → `full` (scientific Python + doc generation tools).

## Runtime

Production uses Firecracker MicroVMs (KVM) with `passt` for user-mode networking. The Docker image (`daemon/cmd/Dockerfile`) is used to build rootfs images and for Docker-based dev/CI. Design specs for ongoing work are in `docs/superpowers/specs/`.
