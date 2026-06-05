# ix Performance 10x + Full Benchmark Coverage — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **GIT POLICY (user's global preference — overrides the usual commit-per-task flow):**
> Do NOT run `git commit` at any point. Edit files and leave the working tree
> dirty; the user reviews and commits batched changes themselves. Any "Commit"
> step you would normally add is intentionally absent from this plan.

**Goal:** 10x the v0.6 hot-path numbers in `daemon/BENCHMARKS.md` (shell, file I/O, code exec, E2E, creation) and add benchmarks for everything an agent does — including the browser path, which has none.

**Architecture:** Three layers change. Daemon (Rust): real persistent shell sessions keyed by the `session_id` the SDK already sends; sentinel on stderr to kill a hardcoded 10 ms drain; quieter guest logging. Go SDK: HTTP connection reuse over vsock, immediate-first-check pollers, pre-copied scratch pool for snapshot restore, two-phase destroy (tombstone rename + async delete). Guest: `/workspace` bind-mounted straight onto the scratch ext4 (bypassing overlayfs), journal-less scratch. Phase 0 builds the measurement harness first so every fix is attributed; Phase 6 re-measures and documents v0.7.

**Tech Stack:** Rust (tokio, axum, ix-* crates), Go 1.26 (go test benchmarks, benchstat), Firecracker v1.15.1, bash/ext4/overlayfs guest plumbing.

**Spec:** `docs/superpowers/specs/2026-06-04-performance-10x-design.md`

**Verification environment:** Rust unit tests run anywhere (`cd daemon && cargo test -p <crate>`). Go unit tests run anywhere (`cd go-sdk && go test ./... -count=1`). Integration benchmarks need KVM + the rootfs/kernel/firecracker artifacts (see BENCHMARKS.md "How to Run"); browser benchmarks additionally need `IX_BROWSER_VM_IMAGE`.

**Known constraint discovered during design:** snapshot-restored VMs have no TAP (vsock-only, `Net: nil`), so they cannot reach the browser gateway. Browser benchmarks therefore use a cold-boot pool (`PoolSize`), never `UseSnapshot`.

---

## Phase 0 — Measurement harness (baseline BEFORE any optimization)

### Task 1: Fix the pool benchmark bugs

**Files:**
- Modify: `go-sdk/manager.go` (add pool-hit counter to `IXManager`)
- Modify: `go-sdk/bench_test.go` (`BenchmarkCreateFromPool`, `BenchmarkCreatePoolPreWarmed`, and move Destroy out of timed regions in creation benchmarks)

- [ ] **Step 1: Add a pool-hit counter to IXManager**

In `go-sdk/manager.go`, add to the `IXManager` struct (it already imports `sync/atomic`):

```go
	// poolHits counts Create() calls served from the pre-warmed pool.
	// Benchmarks use it to fail loudly instead of silently timing a cold boot.
	poolHits atomic.Int64
```

In `Create()`, inside the `if entry != nil` fast path (right before `return sb, nil`), add:

```go
		m.poolHits.Add(1)
```

- [ ] **Step 2: Fix BenchmarkCreateFromPool**

Replace the benchmark loop in `go-sdk/bench_test.go` (currently sleeps 100 ms *inside* the timed loop):

```go
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Replenishment is async: never time a pool-miss (it would silently
		// measure a ~600 ms cold boot instead of a pool grab).
		b.StopTimer()
		waitPoolFill(mgr, 1)
		b.StartTimer()

		sid := fmt.Sprintf("bench-pool-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}

		// Destroy is measured by BenchmarkDestroy; keep it out of "creation".
		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}

	if got := mgr.poolHits.Load(); got < int64(b.N) {
		b.Fatalf("pool misses: only %d/%d creates were pool hits", got, b.N)
	}
```

- [ ] **Step 3: Apply the same pattern to BenchmarkCreatePoolPreWarmed**

Same loop shape: `StopTimer → waitPoolFill(mgr, 1) → StartTimer → Create → ExecCode → StopTimer → Destroy → StartTimer`, delete the `time.Sleep(100 * time.Millisecond)`, and add the same `poolHits` assertion after the loop. The `ExecCode` call stays inside the timed region (that's the point of the benchmark).

- [ ] **Step 4: Move Destroy out of the timed region in BenchmarkCreateCold and BenchmarkCreateFromSnapshot**

Wrap each `mgr.Destroy(...)` call in `b.StopTimer()` / `b.StartTimer()` exactly as in Step 2. Leave `BenchmarkE2EAgentCycle`, `BenchmarkEndToEnd`, and `BenchmarkE2ESnapshotCycle` untouched — a "cycle" legitimately includes teardown.

- [ ] **Step 5: Compile-check**

Run: `cd go-sdk && go vet -tags=integration ./... && go build ./...`
Expected: clean exit, no output.

---

### Task 2: IX_TRACE phase tracing

**Files:**
- Create: `go-sdk/trace.go`
- Modify: `go-sdk/snapshot.go` (`Restore`), `go-sdk/vmm.go` (`startVMCold`), `go-sdk/vmm_vsock.go` (`vsockTransport`)

- [ ] **Step 1: Create `go-sdk/trace.go`**

```go
package ix

import (
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// traceEnabled gates hot-path phase logging. Set IX_TRACE=1 when running
// benchmarks to attribute where creation/request time goes. Read once at
// process start — tracing is a measurement mode, not a runtime toggle.
var traceEnabled = os.Getenv("IX_TRACE") != ""

// tracePhase logs one named phase's elapsed time when IX_TRACE is set.
// Call as: defer tracePhase(logger, "restore", "load", time.Now()) — or with
// an explicit start for sequential phases.
func tracePhase(logger *slog.Logger, op, phase string, start time.Time) {
	if !traceEnabled || logger == nil {
		return
	}
	logger.Info("trace", "op", op, "phase", phase, "us", time.Since(start).Microseconds())
}

// dialCount counts vsock UDS dials across the process. With working HTTP
// keep-alive this stays near one per sandbox; one per REQUEST means the
// connection pool is broken (see TestSSEConnectionReuse).
var dialCount atomic.Int64

// DialCount returns the number of vsock dials so far (test/diagnostics hook).
func DialCount() int64 { return dialCount.Load() }
```

- [ ] **Step 2: Instrument `Restore` in `go-sdk/snapshot.go`**

Bracket each sequential phase. Pattern (apply around the existing code, no logic changes):

```go
	t := time.Now()
	// ... existing scratch provisioning (copySparse / later: takePooledScratch)
	tracePhase(sm.logger, "restore", "scratch", t)

	t = time.Now()
	// ... existing cmd.Start()
	tracePhase(sm.logger, "restore", "spawn", t)

	t = time.Now()
	// ... existing waitForFile(apiSocket, ...)
	tracePhase(sm.logger, "restore", "apisock", t)

	t = time.Now()
	// ... existing fcPut(ctx, apiClient, "/snapshot/load", ...)
	tracePhase(sm.logger, "restore", "load", t)

	t = time.Now()
	// ... existing waitHealthy(ctx, guestHTTP)
	tracePhase(sm.logger, "restore", "health", t)
```

- [ ] **Step 3: Instrument `startVMCold` in `go-sdk/vmm.go`**

Same pattern with op `"coldboot"` around: scratch copy (`"scratch"`), TAP setup (`"tap"`), `cmd.Start` (`"spawn"`), `waitForFile` (`"apisock"`), the block of config PUTs (`"config"`), and the InstanceStart PUT (`"instancestart"`). Use `fb.logger`.

- [ ] **Step 4: Count dials in `vsockTransport` (`go-sdk/vmm_vsock.go`)**

First line inside the `DialContext` func:

```go
			dialCount.Add(1)
```

- [ ] **Step 5: Compile-check**

Run: `cd go-sdk && go build ./... && go test -run TestNothing ./... -count=1`
Expected: builds; tests trivially pass (no test named TestNothing runs).

---

### Task 3: run-benchmarks.sh — repeatability + benchstat

**Files:**
- Modify: `go-sdk/scripts/run-benchmarks.sh`
- Modify: `.gitignore` (add `go-sdk/bench-results/`)

- [ ] **Step 1: Add compare mode, COUNT env, and result persistence**

In `run-benchmarks.sh`, immediately after `set -euo pipefail`:

```bash
# Compare two saved runs: ./scripts/run-benchmarks.sh compare old.txt new.txt
if [[ "${1:-}" == "compare" ]]; then
    command -v benchstat >/dev/null || {
        echo "benchstat not found: go install golang.org/x/perf/cmd/benchstat@latest" >&2
        exit 1
    }
    exec benchstat "$2" "$3"
fi
```

After `ITERATIONS="${1:-5}"` add:

```bash
# COUNT > 1 repeats every benchmark for benchstat-grade variance data.
COUNT="${COUNT:-1}"
RESULTS_DIR="${REPO_ROOT}/bench-results"
mkdir -p "${RESULTS_DIR}"
OUT_FILE="${RESULTS_DIR}/bench-$(date +%Y%m%d-%H%M%S).txt"
```

Change the `go test` invocation to use `-count="${COUNT}"` (replacing `-count=1`) and persist output:

```bash
RAW_OUTPUT=$(cd "${REPO_ROOT}" && go test \
    -bench=. \
    -benchtime="${ITERATIONS}x" \
    -tags=integration \
    -count="${COUNT}" \
    -timeout=60m \
    2>&1 | tee "${OUT_FILE}")

echo ""
echo "Raw results saved to: ${OUT_FILE}  (compare runs with: $0 compare old.txt new.txt)"
```

- [ ] **Step 2: Make `get_ns` average across repeated counts**

Replace the body of `get_ns` so multiple lines (COUNT>1) average instead of taking the first:

```bash
get_ns() {
    local name="$1"
    local ns
    ns=$(printf '%s\n' "${RAW_OUTPUT}" \
        | grep -E "^${name}[^0-9a-zA-Z]" \
        | awk '{for(i=1;i<=NF;i++) if($(i+1)=="ns/op") {sum+=$i; n++}} END {if(n) printf "%d", sum/n}')
    printf '%s' "${ns:-N/A}"
}
```

(The `[^0-9a-zA-Z]` guard stops `BenchmarkFileReadWrite` from also matching `BenchmarkFileReadWriteWorkspace`.)

- [ ] **Step 3: Add `go-sdk/bench-results/` to `.gitignore`**

- [ ] **Step 4: Syntax-check**

Run: `bash -n go-sdk/scripts/run-benchmarks.sh`
Expected: no output, exit 0.

---

### Task 4: New file/lifecycle benchmarks

**Files:**
- Modify: `go-sdk/bench_test.go`

- [ ] **Step 1: Add a shared setup helper**

`bench_test.go` is `package ix`, so internals are reachable. Add near `waitPoolFill`:

```go
// benchSandbox creates a manager + one sandbox for op-latency benchmarks and
// registers cleanup. Pass UseSnapshot/PoolSize via cfg overrides.
func benchSandbox(b *testing.B, sid string, mutate func(*ManagerConfig)) (*IXManager, sandbox.Sandbox) {
	b.Helper()
	ctx := context.Background()
	cfg := ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mgr, err := NewManager(ctx, cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { mgr.Close() })
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = mgr.Destroy(context.Background(), sb.(*IXSandbox).id) })
	return mgr, sb
}
```

- [ ] **Step 2: Add the new op benchmarks**

```go
// BenchmarkFileReadWriteWorkspace measures write+read on /workspace — the
// scratch-backed agent workspace path. BenchmarkFileReadWrite hits /tmp,
// which ix-init mounts as tmpfs, so it never exercises the disk write path.
func BenchmarkFileReadWriteWorkspace(b *testing.B) {
	ctx := context.Background()
	_, sb := benchSandbox(b, "bench-file-ws", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    "/workspace/bench.txt",
			Content: "hello",
		}); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}
		if _, err := sb.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/workspace/bench.txt"}); err != nil {
			b.Fatalf("ReadFile: %v", err)
		}
	}
}

// BenchmarkUploadDownload measures a 64 KB multipart upload + raw download.
func BenchmarkUploadDownload(b *testing.B) {
	ctx := context.Background()
	_, sb := benchSandbox(b, "bench-updown", nil)
	payload := bytes.Repeat([]byte("x"), 64<<10)
	ix := sb.(*IXSandbox)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ix.UploadFile(ctx, "/workspace/blob.bin", bytes.NewReader(payload)); err != nil {
			b.Fatalf("UploadFile: %v", err)
		}
		rc, err := ix.DownloadFile(ctx, "/workspace/blob.bin")
		if err != nil {
			b.Fatalf("DownloadFile: %v", err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			b.Fatalf("read download: %v", err)
		}
		rc.Close()
	}
}

// BenchmarkGrep measures a content search across 50 pre-written files.
func BenchmarkGrep(b *testing.B) {
	ctx := context.Background()
	_, sb := benchSandbox(b, "bench-grep", nil)
	for i := 0; i < 50; i++ {
		content := fmt.Sprintf("line one\nneedle-%d in file\nline three\n", i)
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    fmt.Sprintf("/workspace/grep/f%02d.txt", i),
			Content: content,
		}); err != nil {
			b.Fatalf("setup WriteFile: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.GrepFiles(ctx, sandbox.GrepRequest{
			Pattern: "needle",
			Path:    "/workspace/grep",
		}); err != nil {
			b.Fatalf("GrepFiles: %v", err)
		}
	}
}

// BenchmarkGlob measures pattern matching over the same 50-file tree.
func BenchmarkGlob(b *testing.B) {
	ctx := context.Background()
	_, sb := benchSandbox(b, "bench-glob", nil)
	for i := 0; i < 50; i++ {
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    fmt.Sprintf("/workspace/glob/f%02d.txt", i),
			Content: "x",
		}); err != nil {
			b.Fatalf("setup WriteFile: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.GlobFiles(ctx, sandbox.GlobRequest{
			Pattern: "*.txt",
			Path:    "/workspace/glob",
		}); err != nil {
			b.Fatalf("GlobFiles: %v", err)
		}
	}
}

// BenchmarkDestroy isolates sandbox teardown (creation runs untimed).
// Destroy semantics change to two-phase in this release — this benchmark
// tracks the caller-visible latency, not total cleanup IO.
func BenchmarkDestroy(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		UseSnapshot: true,
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.vmm.snapshot != nil && mgr.vmm.snapshot.Ready() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if mgr.vmm.snapshot == nil || !mgr.vmm.snapshot.Ready() {
		b.Fatal("golden snapshot not ready after 120s")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sid := fmt.Sprintf("bench-destroy-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		b.StartTimer()

		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}
```

Add `"bytes"` and `"io"` to the file's imports.

- [ ] **Step 3: Compile-check**

Run: `cd go-sdk && go vet -tags=integration ./...`
Expected: clean.

- [ ] **Step 4 (only on a KVM host with artifacts): smoke one new benchmark**

Run: `cd go-sdk && IX_ROOTFS_IMAGE=... IX_KERNEL_PATH=... IX_FC_BINARY=... go test -tags integration -bench BenchmarkFileReadWriteWorkspace -benchtime 3x -count 1 -timeout 600s`
Expected: benchmark completes with an ns/op figure.

---

### Task 5: Browser benchmarks + local test page server

**Files:**
- Create: `go-sdk/bench_browser_test.go`

- [ ] **Step 1: Create the file with helpers**

```go
//go:build integration

package ix

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// benchPageHTML is the hermetic navigation target: served from the host so
// browser benchmarks measure OUR stack (gateway + pinchtab + Chrome), not the
// internet. A button mutates the DOM so action benchmarks have a side effect.
const benchPageHTML = `<!DOCTYPE html>
<html><head><title>ix bench page</title></head>
<body>
<h1 id="title">ix benchmark page</h1>
<button id="btn" onclick="document.getElementById('out').textContent='clicked'">Click me</button>
<div id="out"></div>
<p>Stable filler content for snapshot and text extraction benchmarks.</p>
</body></html>`

// browserBenchEnv builds a manager with the shared browser tier and starts a
// local page server on the gateway IP (routable from guests via TAP).
// Skips unless IX_BROWSER_VM_IMAGE is set.
//
// NOTE: UseSnapshot is incompatible with browser sandboxes (snapshot-restored
// VMs are vsock-only — no TAP, no route to the gateway), so the pool is the
// fast-create path here.
func browserBenchEnv(b *testing.B) (*IXManager, string) {
	b.Helper()
	img := os.Getenv("IX_BROWSER_VM_IMAGE")
	if img == "" {
		b.Skip("set IX_BROWSER_VM_IMAGE to run browser benchmarks")
	}
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:    rootfsImage(),
		KernelPath:     kernelPath(),
		FCBinary:       fcBinary(),
		BrowserMode:    "remote",
		BrowserVMImage: img,
		PoolSize:       2,
		DefaultTTL:     10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { mgr.Close() })

	// The manager pinned 169.254.0.1 on ixgw0; bind the page server there so
	// guest Chrome can reach it through its TAP default route.
	ln, err := net.Listen("tcp", "169.254.0.1:0")
	if err != nil {
		b.Fatalf("bind page server on gateway IP: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, benchPageHTML)
	})}
	go srv.Serve(ln) //nolint:errcheck
	b.Cleanup(func() { srv.Close() })

	return mgr, "http://" + ln.Addr().String() + "/"
}

// browserBenchSandbox creates one browser-enabled sandbox and navigates it
// once so per-op benchmarks measure steady-state (instance + tab exist).
func browserBenchSandbox(b *testing.B, mgr *IXManager, pageURL, sid string) sandbox.Sandbox {
	b.Helper()
	ctx := context.Background()
	yes := true
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = mgr.Destroy(context.Background(), sb.(*IXSandbox).id) })
	if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
		b.Fatalf("warmup navigate: %v", err)
	}
	return sb
}
```

- [ ] **Step 2: Add the per-op benchmarks**

```go
// BenchmarkBrowserNavigate measures steady-state navigation (instance/tab warm).
func BenchmarkBrowserNavigate(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-nav")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("BrowserNavigate: %v", err)
		}
	}
}

// BenchmarkBrowserSnapshot measures accessibility-snapshot extraction.
func BenchmarkBrowserSnapshot(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-snap")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserSnapshot(ctx, sandbox.SnapshotOpts{}); err != nil {
			b.Fatalf("BrowserSnapshot: %v", err)
		}
	}
}

// BenchmarkBrowserScreenshot measures full-page screenshot capture.
func BenchmarkBrowserScreenshot(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-shot")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserScreenshot(ctx); err != nil {
			b.Fatalf("BrowserScreenshot: %v", err)
		}
	}
}

// BenchmarkBrowserAction measures a coordinate click round-trip.
func BenchmarkBrowserAction(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-act")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sb.BrowserAction(ctx, sandbox.BrowserAction{
			Type: "click", X: 100, Y: 100,
		}); err != nil {
			b.Fatalf("BrowserAction: %v", err)
		}
	}
}

// BenchmarkBrowserEval measures a trivial JS evaluation round-trip — the
// floor of the gateway → pinchtab → Chrome → back path.
func BenchmarkBrowserEval(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-eval")
	ix := sb.(*IXSandbox)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.BrowserEval(ctx, "1+1"); err != nil {
			b.Fatalf("BrowserEval: %v", err)
		}
	}
}

// BenchmarkBrowserFirstUse measures the first browser op of a fresh chat:
// gateway ensureChat (start pinchtab instance + open tab) + navigation.
// Sandbox creation runs untimed.
func BenchmarkBrowserFirstUse(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	yes := true

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		waitPoolFill(mgr, 1)
		sid := fmt.Sprintf("bench-br-first-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		b.StartTimer()

		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("first navigate: %v", err)
		}

		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkBrowserE2E measures the full browser agent cycle:
// create (pool) → navigate → snapshot → action → text → destroy.
func BenchmarkBrowserE2E(b *testing.B) {
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	yes := true

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		waitPoolFill(mgr, 1)
		b.StartTimer()

		sid := fmt.Sprintf("bench-br-e2e-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid, Browser: &yes})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		ix := sb.(*IXSandbox)
		if err := sb.BrowserNavigate(ctx, pageURL); err != nil {
			b.Fatalf("BrowserNavigate: %v", err)
		}
		if _, err := sb.BrowserSnapshot(ctx, sandbox.SnapshotOpts{}); err != nil {
			b.Fatalf("BrowserSnapshot: %v", err)
		}
		if _, err := sb.BrowserAction(ctx, sandbox.BrowserAction{Type: "click", X: 100, Y: 100}); err != nil {
			b.Fatalf("BrowserAction: %v", err)
		}
		if _, err := ix.BrowserText(ctx, sandbox.TextOpts{}); err != nil {
			b.Fatalf("BrowserText: %v", err)
		}
		if err := mgr.Destroy(ctx, ix.id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkBrowserNavigateRealSite is the opt-in realism check. Hermetic
// benchmarks above are the defaults; set IX_BENCH_REAL_SITE=https://example.com
// to also measure a real network fetch (noisy by nature).
func BenchmarkBrowserNavigateRealSite(b *testing.B) {
	site := os.Getenv("IX_BENCH_REAL_SITE")
	if site == "" {
		b.Skip("set IX_BENCH_REAL_SITE (e.g. https://example.com) to run")
	}
	ctx := context.Background()
	mgr, pageURL := browserBenchEnv(b)
	sb := browserBenchSandbox(b, mgr, pageURL, "bench-br-real")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sb.BrowserNavigate(ctx, site); err != nil {
			b.Fatalf("BrowserNavigate(%s): %v", site, err)
		}
	}
}
```

- [ ] **Step 3: Compile-check**

Run: `cd go-sdk && go vet -tags=integration ./...`
Expected: clean.

---

### Task 6: Record the corrected baseline (v0.6.1)

**Files:**
- Modify: `daemon/BENCHMARKS.md`

- [ ] **Step 1 (KVM host): run the full suite with the fixed harness, 5 repetitions**

Run: `cd go-sdk && COUNT=5 IX_TRACE=1 IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4 IX_KERNEL_PATH=/opt/ix/firecracker/vmlinux.bin IX_FC_BINARY=/opt/ix/firecracker/firecracker ./scripts/run-benchmarks.sh 10`
Expected: completes; raw output saved under `go-sdk/bench-results/`. Keep this file — it is the "before" input for the final benchstat comparison.

- [ ] **Step 2: Add a v0.6.1 section to BENCHMARKS.md**

Insert above the v0.6 section:

```markdown
### v0.6.1 — corrected measurement baseline (2026-06-04)

Same code as v0.6; harness fixes only. Pool benchmarks previously timed a
100 ms sleep inside the loop and silently measured cold boots on pool-miss;
creation benchmarks included Destroy. From v0.6.1 on: pool benchmarks fail on
pool-miss, Destroy is measured separately (BenchmarkDestroy), and all runs use
COUNT=5 via benchstat. New coverage: /workspace file I/O, upload/download,
grep/glob, destroy, and the full browser suite (hermetic local page server;
real-site opt-in via IX_BENCH_REAL_SITE).

<paste the benchstat-formatted v0.6.1 table here>

IX_TRACE=1 phase attribution for restore + request paths:
<paste the aggregated trace summary here>
```

- [ ] **Step 3: Sanity-check the trace output answers the open question**

The FileReadWrite 8→19 ms regression must now be attributable (dial counts, phase timings). Note the finding in the v0.6.1 section — it decides how much Task 12 (conn reuse) is expected to recover.

---

## Phase 1 — Daemon quick wins

### Task 7: Sentinel on stderr — kill the hardcoded 10 ms drain

**Files:**
- Modify: `daemon/crates/ix-code/src/ix_repl.py`
- Modify: `daemon/crates/ix-code/src/kernel.rs`

- [ ] **Step 1: Add a test hook so unit tests can run the repo-local repl.py**

In `kernel.rs`, split `Kernel::start`:

```rust
    pub async fn start(language: &str) -> Result<Self> {
        let (cmd_path, args) = language_command(language)?;
        Self::start_with_command(language, &cmd_path, &args).await
    }

    /// Start a kernel with an explicit command. Lets unit tests run the
    /// repo-local repl.py instead of the image-baked /usr/lib/ix/repl.py.
    pub async fn start_with_command(language: &str, cmd_path: &str, args: &[String]) -> Result<Self> {
        info!(language, cmd = %cmd_path, "starting REPL kernel");
        // ... existing body of start() from `let mut child = Command::new(...)`
        //     onward, using `cmd_path` and `args` ...
    }
```

- [ ] **Step 2: Write the failing tests (kernel.rs `tests` module)**

```rust
    async fn start_local_python() -> Kernel {
        let repl = concat!(env!("CARGO_MANIFEST_DIR"), "/src/ix_repl.py");
        Kernel::start_with_command("python", "python3", &["-u".into(), repl.into()])
            .await
            .expect("python3 must be installed to run ix-code tests")
    }

    #[tokio::test]
    async fn stderr_is_captured_and_does_not_cost_a_drain_timeout() {
        let mut k = start_local_python().await;
        let timeout = Some(std::time::Duration::from_secs(10));
        // Warm up: first execute absorbs interpreter startup.
        k.execute("x = 1", timeout).await.unwrap();

        let t = std::time::Instant::now();
        let (out, err) = k
            .execute("import sys; sys.stderr.write('boom\\n')", timeout)
            .await
            .unwrap();
        let elapsed = t.elapsed();

        assert!(err.contains("boom"), "stderr lost: {err:?}");
        assert!(out.is_empty(), "unexpected stdout: {out:?}");
        // The old protocol drained stderr with a fixed 10 ms timeout on every
        // execute. A warm one-liner must come in far below that.
        assert!(
            elapsed < std::time::Duration::from_millis(8),
            "execute took {elapsed:?} — stderr drain timeout is back?"
        );
    }

    #[tokio::test]
    async fn stdout_and_stderr_both_captured_in_one_cell() {
        let mut k = start_local_python().await;
        let timeout = Some(std::time::Duration::from_secs(10));
        let (out, err) = k
            .execute(
                "import sys\nprint('to-out')\nsys.stderr.write('to-err\\n')",
                timeout,
            )
            .await
            .unwrap();
        assert!(out.contains("to-out"), "stdout: {out:?}");
        assert!(err.contains("to-err"), "stderr: {err:?}");
    }
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd daemon && cargo test -p ix-code`
Expected: `stderr_is_captured_and_does_not_cost_a_drain_timeout` FAILS on the elapsed assertion (~10 ms); the capture test may pass (drain already captured stderr).

- [ ] **Step 4: Write the sentinel to stderr in all three REPLs**

`ix_repl.py` — both places that write the stdout sentinel (the empty-code `continue` branch and the end of the main loop) become:

```python
        sys.stdout.write("__IX_RESULT__\n")
        sys.stdout.flush()
        sys.stderr.write("__IX_RESULT__\n")
        sys.stderr.flush()
```

`kernel.rs` JavaScript REPL string — after `process.stdout.write('__IX_RESULT__\n');` add:

```javascript
        process.stderr.write('__IX_RESULT__\n');
```

`kernel.rs` bash REPL string — after the stdout sentinel lines add:

```python
    sys.stderr.write('__IX_RESULT__\n')
    sys.stderr.flush()
```

- [ ] **Step 5: Replace the drain loop with concurrent read-to-sentinel**

In `Kernel::execute`, replace everything from `// Read stdout until sentinel` through the stderr drain loop with:

```rust
        // Read stdout and stderr concurrently, each until its own sentinel
        // (the REPL writes __IX_RESULT__ to BOTH streams after every cell).
        // Concurrent reads prevent a pipe-buffer deadlock when a cell writes
        // >64 KB to one stream; no timeout-based draining is needed anymore.
        let stdout = &mut self.stdout;
        let stderr = &mut self.stderr;

        let stdout_future = async {
            let mut output = String::new();
            let mut line = String::new();
            loop {
                line.clear();
                let n = stdout
                    .read_line(&mut line)
                    .await
                    .map_err(|e| Error::Internal(format!("read stdout: {e}")))?;
                if n == 0 {
                    return Err(Error::Internal("REPL process closed stdout".into()));
                }
                if line.trim_end() == SENTINEL_RESULT {
                    break;
                }
                output.push_str(&line);
            }
            Ok::<String, Error>(output)
        };

        let stderr_future = async {
            let mut output = String::new();
            let mut line = String::new();
            loop {
                line.clear();
                let n = stderr
                    .read_line(&mut line)
                    .await
                    .map_err(|e| Error::Internal(format!("read stderr: {e}")))?;
                if n == 0 {
                    break; // REPL closed stderr — return what we have
                }
                if line.trim_end() == SENTINEL_RESULT {
                    break;
                }
                output.push_str(&line);
            }
            Ok::<String, Error>(output)
        };

        let joined = async { tokio::join!(stdout_future, stderr_future) };

        let (stdout_output, stderr_output) = if let Some(dur) = timeout {
            match tokio::time::timeout(dur, joined).await {
                Ok((o, e)) => (o?, e?),
                Err(_) => return Err(Error::Internal("code execution timed out".into())),
            }
        } else {
            let (o, e) = joined.await;
            (o?, e?)
        };

        Ok((stdout_output, stderr_output))
```

(The split field borrows `&mut self.stdout` / `&mut self.stderr` are disjoint and compile.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd daemon && cargo test -p ix-code`
Expected: all PASS, including the <8 ms assertion.

- [ ] **Step 7: Full daemon test sweep**

Run: `cd daemon && cargo test --all`
Expected: all green (ix-server integration tests run serially elsewhere; default `cargo test --all` must stay green).

---

### Task 8: Quiet guest — console gating + default RUST_LOG=warn

**Files:**
- Modify: `go-sdk/vmm.go` (`buildKernelBootArgs`, call site in `startVMCold`)
- Modify: `go-sdk/vmm_test.go` (boot-args tests)

- [ ] **Step 1: Update/add the boot-args unit tests first**

In `vmm_test.go`, update existing `buildKernelBootArgs` tests for the new signature and add:

```go
func TestBuildKernelBootArgsConsoleGating(t *testing.T) {
	quiet := buildKernelBootArgs(nil, nil, false)
	if strings.Contains(quiet, "console=ttyS0") {
		t.Errorf("console must be absent without IX_VM_CONSOLE: %s", quiet)
	}
	for _, want := range []string{"8250.nr_uarts=0", "quiet"} {
		if !strings.Contains(quiet, want) {
			t.Errorf("missing %q in quiet boot args: %s", want, quiet)
		}
	}

	loud := buildKernelBootArgs(nil, nil, true)
	if !strings.Contains(loud, "console=ttyS0") {
		t.Errorf("console must be present with IX_VM_CONSOLE: %s", loud)
	}
}

func TestBuildKernelBootArgsDefaultRustLog(t *testing.T) {
	args := buildKernelBootArgs([]string{"FOO=bar"}, nil, false)
	if !strings.Contains(args, "ix.env.RUST_LOG=warn") {
		t.Errorf("expected default RUST_LOG=warn: %s", args)
	}
	// Caller-provided RUST_LOG wins.
	args = buildKernelBootArgs([]string{"RUST_LOG=debug"}, nil, false)
	if strings.Contains(args, "ix.env.RUST_LOG=warn") {
		t.Errorf("default must not override caller RUST_LOG: %s", args)
	}
	if !strings.Contains(args, "ix.env.RUST_LOG=debug") {
		t.Errorf("caller RUST_LOG missing: %s", args)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs ./... -count=1`
Expected: FAIL (signature mismatch / missing args).

- [ ] **Step 3: Implement**

New `buildKernelBootArgs` (replaces the current one):

```go
// buildKernelBootArgs constructs the kernel command line for the Firecracker VM.
// Environment variables from envSlice are injected as ix.env.KEY=VALUE entries
// so the ix-init script can read them from /proc/cmdline. When net is non-nil,
// an ip= argument autoconfigures eth0 at boot.
//
// withConsole routes the guest serial console to Firecracker stdout (set
// IX_VM_CONSOLE=1 on the host). Default is OFF: every serial byte is an
// emulated-UART vmexit, so console output taxes both boot time and any guest
// process that logs to stdout. 8250.nr_uarts=0 removes the serial driver
// entirely; quiet suppresses printk to the (absent) console.
func buildKernelBootArgs(envSlice []string, net *vmNet, withConsole bool) string {
	var parts []string
	if withConsole {
		parts = append(parts, "console=ttyS0")
	} else {
		parts = append(parts, "8250.nr_uarts=0", "quiet")
	}
	parts = append(parts,
		"reboot=k",
		"panic=1",
		"pci=off",
		"nomodules",
		"random.trust_cpu=on",
		"i8042.noaux",
		"i8042.nomux",
		"i8042.nopnp",
		"i8042.dumbkbd",
		"root=/dev/vda",
		"ro", // shared rootfs: all writes go to the per-VM scratch via overlayfs (ix-stage0)
		"init=/sbin/ix-stage0",
	)
	if net != nil {
		parts = append(parts, fmt.Sprintf(
			"ip=%s::%s:%s::eth0:off:8.8.8.8", net.guestIP, net.hostIP, net.mask))
	}
	hasRustLog := false
	for _, e := range envSlice {
		if strings.HasPrefix(e, "RUST_LOG=") {
			hasRustLog = true
		}
		parts = append(parts, "ix.env."+e)
	}
	if !hasRustLog {
		// ixd logs at info per request; with the console wired to an emulated
		// UART that is measurable per-op latency. warn keeps real problems.
		parts = append(parts, "ix.env.RUST_LOG=warn")
	}
	return strings.Join(parts, " ")
}
```

Call site in `startVMCold`:

```go
	bootArgs := buildKernelBootArgs(envSlice, vn, os.Getenv("IX_VM_CONSOLE") != "")
```

- [ ] **Step 4: Run to verify pass**

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs ./... -count=1`
Expected: PASS. Then `go test ./... -count=1` — all green.

---

## Phase 2 — Persistent shell sessions (daemon)

### Task 9: `session_id` on ShellRequest

**Files:**
- Modify: `daemon/crates/ix-core/src/types.rs`

- [ ] **Step 1: Add the field**

```rust
#[derive(Debug, Deserialize)]
pub struct ShellRequest {
    pub command: String,
    #[serde(default)]
    pub cwd: Option<String>,
    #[serde(default)]
    pub timeout: Option<u64>,
    /// When set, the command runs in a persistent bash session keyed by this
    /// id (state persists across calls). Absent = one-shot fork+exec.
    #[serde(default)]
    pub session_id: Option<String>,
}
```

- [ ] **Step 2: Verify**

Run: `cd daemon && cargo test -p ix-core -p ix-shell`
Expected: compiles, existing tests green (serde `default` keeps old payloads valid).

---

### Task 10: SessionManager in ix-shell

**Files:**
- Create: `daemon/crates/ix-shell/src/session.rs`
- Modify: `daemon/crates/ix-shell/src/lib.rs`
- Modify: `daemon/crates/ix-shell/src/tests.rs`

- [ ] **Step 1: Write the failing tests**

Append to the `tests` module in `daemon/crates/ix-shell/src/tests.rs` (reuse the existing `test_channel`/`collect_from_rx` helpers and event accessors):

```rust
    use crate::session::SessionManager;
    use std::sync::Arc;

    fn session_req(sid: &str, command: &str, timeout: Option<u64>) -> ShellRequest {
        // Build via serde so the struct literal stays in one place.
        serde_json::from_value(serde_json::json!({
            "command": command,
            "session_id": sid,
            "timeout": timeout,
        }))
        .unwrap()
    }

    async fn run_in_session(mgr: &Arc<SessionManager>, req: ShellRequest) -> Vec<(String, String)> {
        let (sender, rx) = test_channel(64);
        mgr.execute(req, sender).await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        collect_from_rx(rx).await
    }

    fn stdout_text(events: &[(String, String)]) -> String {
        events_of_type(events, "stdout")
            .iter()
            .map(|d| {
                serde_json::from_str::<serde_json::Value>(d).unwrap()["text"]
                    .as_str()
                    .unwrap()
                    .to_string()
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    #[tokio::test]
    async fn session_state_persists_across_commands() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "X=42", None)).await;
        let events = run_in_session(&mgr, session_req("s1", "echo $X", None)).await;
        assert_eq!(stdout_text(&events), "42");
        let complete = complete_event(&events).expect("complete event");
        assert_eq!(complete["exit_code"], 0);
    }

    #[tokio::test]
    async fn sessions_are_isolated_from_each_other() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("a", "X=aaa", None)).await;
        let events = run_in_session(&mgr, session_req("b", "echo \"X=[$X]\"", None)).await;
        assert_eq!(stdout_text(&events), "X=[]");
    }

    #[tokio::test]
    async fn session_reports_nonzero_exit_code() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "false", None)).await;
        let complete = complete_event(&events).expect("complete event");
        assert_eq!(complete["exit_code"], 1);
    }

    #[tokio::test]
    async fn session_captures_stderr() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "echo oops >&2", None)).await;
        let errs = events_of_type(&events, "stderr");
        assert!(
            errs.iter().any(|d| d.contains("oops")),
            "stderr missing: {events:?}"
        );
    }

    #[tokio::test]
    async fn session_handles_output_without_trailing_newline() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "printf no-newline", None)).await;
        assert_eq!(stdout_text(&events), "no-newline");
    }

    #[tokio::test]
    async fn session_timeout_kills_and_recreates() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "sleep 5", Some(1))).await;
        assert!(error_event(&events).is_some(), "expected timeout error");
        // Session must be recreated transparently on the next command.
        let events = run_in_session(&mgr, session_req("s1", "echo back", None)).await;
        assert_eq!(stdout_text(&events), "back");
    }

    #[tokio::test]
    async fn session_survives_user_exit_by_recreating() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "exit 0", None)).await;
        let events = run_in_session(&mgr, session_req("s1", "echo alive", None)).await;
        assert_eq!(stdout_text(&events), "alive");
    }

    #[tokio::test]
    async fn session_is_fast_after_first_command() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "true", None)).await; // pays bash -l once
        let t = std::time::Instant::now();
        let (sender, rx) = test_channel(64);
        mgr.execute(session_req("s1", "echo hot", None), sender).await;
        let elapsed = t.elapsed();
        drop(rx);
        // In-guest persistent round-trip must be way below fork+exec (~18 ms).
        assert!(
            elapsed < Duration::from_millis(5),
            "session round-trip took {elapsed:?}"
        );
    }
```

- [ ] **Step 2: Run to verify failure**

Run: `cd daemon && cargo test -p ix-shell`
Expected: FAIL to compile (`crate::session` does not exist).

- [ ] **Step 3: Implement `daemon/crates/ix-shell/src/session.rs`**

```rust
//! Persistent bash sessions. One long-lived `bash -l` per session_id: the
//! login-shell profile cost (~18 ms) is paid once, then each command is a
//! stdin write + sentinel-delimited read (<1 ms in-guest).
//!
//! Protocol per command:
//!   <cwd line, when requested>
//!   <user command lines>
//!   printf '__IX_DONE_<nonce>__ %d\n' "$?"
//!   printf '__IX_DONE_<nonce>__\n' >&2
//!
//! The nonce is unique per command, so command output containing the literal
//! sentinel of a *previous* command cannot terminate the read early. Output
//! produced without a trailing newline lands on the sentinel line; the reader
//! splits it back apart. Known limitation (documented): a command that itself
//! consumes stdin (bare `cat`, heredoc typos) swallows the sentinel printf
//! lines and runs until the timeout kills the session — same failure class as
//! any REPL. One-shot exec (no session_id) remains available for isolation.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStderr, ChildStdin, ChildStdout, Command};
use tokio::sync::Mutex;
use tracing::{info, warn};

use ix_core::sse::SseSender;
use ix_core::types::ShellRequest;

use crate::signal::kill_process_group;

/// Idle sessions are killed by the eviction loop after this long.
const SESSION_IDLE_TTL: Duration = Duration::from_secs(600);
/// Leak guard: one VM serves one chat, so this should never be reached.
const MAX_SESSIONS: usize = 16;
/// Commands in a session get a default timeout: an unbalanced quote/heredoc
/// makes bash swallow the sentinel and hang forever otherwise.
const DEFAULT_TIMEOUT_SECS: u64 = 300;

static NONCE: AtomicU64 = AtomicU64::new(0);

struct Session {
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
    stderr: BufReader<ChildStderr>,
    child: Child,
    pgid: i32,
    last_used: Instant,
}

impl Session {
    async fn spawn() -> std::io::Result<Self> {
        let mut cmd = Command::new("bash");
        cmd.arg("-l"); // login env, paid once per session
        cmd.stdin(std::process::Stdio::piped());
        cmd.stdout(std::process::Stdio::piped());
        cmd.stderr(std::process::Stdio::piped());
        // Own process group so timeout kills children too (same as exec.rs).
        // SAFETY: setpgid(0, 0) is async-signal-safe per POSIX.
        unsafe {
            cmd.pre_exec(|| {
                nix::unistd::setpgid(
                    nix::unistd::Pid::from_raw(0),
                    nix::unistd::Pid::from_raw(0),
                )
                .map_err(|e| std::io::Error::from_raw_os_error(e as i32))?;
                Ok(())
            });
        }
        let mut child = cmd.spawn()?;
        let pgid = child.id().unwrap_or_default() as i32;
        let stdin = child.stdin.take().expect("stdin piped");
        let stdout = BufReader::new(child.stdout.take().expect("stdout piped"));
        let stderr = BufReader::new(child.stderr.take().expect("stderr piped"));
        Ok(Self {
            stdin,
            stdout,
            stderr,
            child,
            pgid,
            last_used: Instant::now(),
        })
    }

    fn alive(&mut self) -> bool {
        matches!(self.child.try_wait(), Ok(None))
    }

    async fn kill(&mut self) {
        kill_process_group(self.pgid).await;
        let _ = self.child.start_kill();
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.child.start_kill();
    }
}

/// Single-quote a string for safe interpolation into a bash line.
fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

pub struct SessionManager {
    /// session_id -> Some(session) when idle, None while a command is running
    /// (a concurrent command for the same id gets "session busy").
    sessions: Mutex<HashMap<String, Option<Session>>>,
}

impl SessionManager {
    /// Construct shared and start the idle-eviction loop.
    pub fn new_shared() -> Arc<Self> {
        let mgr = Arc::new(Self {
            sessions: Mutex::new(HashMap::new()),
        });
        let weak = Arc::downgrade(&mgr);
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(Duration::from_secs(60));
            loop {
                tick.tick().await;
                let Some(mgr) = weak.upgrade() else { return };
                mgr.evict_idle().await;
            }
        });
        mgr
    }

    async fn evict_idle(&self) {
        let mut sessions = self.sessions.lock().await;
        let now = Instant::now();
        let expired: Vec<String> = sessions
            .iter()
            .filter_map(|(id, slot)| match slot {
                Some(s) if now.duration_since(s.last_used) > SESSION_IDLE_TTL => {
                    Some(id.clone())
                }
                _ => None, // busy (None) or fresh sessions stay
            })
            .collect();
        for id in expired {
            if let Some(Some(mut s)) = sessions.remove(&id) {
                info!(session = %id, "evicting idle shell session");
                s.kill().await;
            }
        }
    }

    /// Execute `req.command` in the persistent session `req.session_id`,
    /// streaming output via `sender`. Caller guarantees session_id is Some.
    pub async fn execute(&self, req: ShellRequest, sender: SseSender) {
        let start = Instant::now();
        let sid = req
            .session_id
            .clone()
            .expect("route dispatches here only with session_id");

        // Claim the session (or spawn one), leaving a busy marker in the map.
        let mut session = {
            let mut sessions = self.sessions.lock().await;
            match sessions.get_mut(&sid) {
                Some(slot @ Some(_)) => {
                    let mut s = slot.take().expect("matched Some");
                    if s.alive() {
                        s
                    } else {
                        info!(session = %sid, "session process died; respawning");
                        match Session::spawn().await {
                            Ok(ns) => ns,
                            Err(e) => {
                                sessions.remove(&sid);
                                sender
                                    .send_error(&format!("spawn session: {e}"), Some(-1))
                                    .await;
                                return;
                            }
                        }
                    }
                }
                Some(None) => {
                    sender
                        .send_error("session busy: a command is already running", Some(-1))
                        .await;
                    return;
                }
                None => {
                    if sessions.len() >= MAX_SESSIONS {
                        // Evict the oldest idle session to stay bounded.
                        if let Some(oldest) = sessions
                            .iter()
                            .filter_map(|(id, slot)| {
                                slot.as_ref().map(|s| (id.clone(), s.last_used))
                            })
                            .min_by_key(|(_, t)| *t)
                            .map(|(id, _)| id)
                        {
                            if let Some(Some(mut s)) = sessions.remove(&oldest) {
                                warn!(evicted = %oldest, "session cap reached");
                                s.kill().await;
                            }
                        }
                    }
                    match Session::spawn().await {
                        Ok(s) => s,
                        Err(e) => {
                            sender
                                .send_error(&format!("spawn session: {e}"), Some(-1))
                                .await;
                            return;
                        }
                    }
                }
            }
        };
        // Mark busy while we run without holding the map lock.
        self.sessions.lock().await.insert(sid.clone(), None);

        let outcome = run_command(&mut session, &req, &sender).await;
        let elapsed_ms = start.elapsed().as_millis() as u64;

        let mut sessions = self.sessions.lock().await;
        match outcome {
            Ok(exit_code) => {
                session.last_used = Instant::now();
                sessions.insert(sid, Some(session));
                sender.send_complete(exit_code, elapsed_ms).await;
            }
            Err(msg) => {
                // Timeout or IO failure: the session state is unknowable —
                // kill it; the next command transparently respawns.
                session.kill().await;
                sessions.remove(&sid);
                sender.send_error(&msg, Some(-1)).await;
                sender.send_complete(-1, elapsed_ms).await;
            }
        }
    }

    /// Kill all sessions (daemon shutdown).
    pub async fn shutdown(&self) {
        let mut sessions = self.sessions.lock().await;
        for (_, slot) in sessions.drain() {
            if let Some(mut s) = slot {
                s.kill().await;
            }
        }
    }
}

/// Write the command + sentinels, stream output until both sentinels.
/// Ok(exit_code) on completion; Err(message) on timeout/IO error (caller
/// kills the session).
async fn run_command(
    session: &mut Session,
    req: &ShellRequest,
    sender: &SseSender,
) -> std::result::Result<i32, String> {
    let nonce = NONCE.fetch_add(1, Ordering::Relaxed);
    let sentinel = format!("__IX_DONE_{}_{}__", session.pgid, nonce);

    let mut block = String::new();
    if let Some(cwd) = &req.cwd {
        // cd persists for the session lifetime — documented session semantics.
        block.push_str(&format!("cd {}\n", shell_quote(cwd)));
    }
    block.push_str(&req.command);
    if !req.command.ends_with('\n') {
        block.push('\n');
    }
    block.push_str(&format!("printf '{sentinel} %d\\n' \"$?\"\n"));
    block.push_str(&format!("printf '{sentinel}\\n' >&2\n"));

    session
        .stdin
        .write_all(block.as_bytes())
        .await
        .map_err(|e| format!("write session stdin: {e}"))?;
    session
        .stdin
        .flush()
        .await
        .map_err(|e| format!("flush session stdin: {e}"))?;

    let timeout = Duration::from_secs(req.timeout.unwrap_or(DEFAULT_TIMEOUT_SECS));
    let stdout = &mut session.stdout;
    let stderr = &mut session.stderr;

    let stdout_read = async {
        let mut line = String::new();
        loop {
            line.clear();
            let n = stdout
                .read_line(&mut line)
                .await
                .map_err(|e| format!("read session stdout: {e}"))?;
            if n == 0 {
                return Err("session closed stdout".to_string());
            }
            let trimmed = line.trim_end_matches('\n');
            if let Some(idx) = trimmed.find(&sentinel) {
                // Output without trailing newline lands on the sentinel line.
                if idx > 0 {
                    sender.send_stdout(&trimmed[..idx]).await;
                }
                let code = trimmed[idx + sentinel.len()..]
                    .trim()
                    .parse::<i32>()
                    .unwrap_or(-1);
                return Ok(code);
            }
            sender.send_stdout(trimmed).await;
        }
    };

    let stderr_read = async {
        let mut line = String::new();
        loop {
            line.clear();
            let n = stderr
                .read_line(&mut line)
                .await
                .map_err(|e| format!("read session stderr: {e}"))?;
            if n == 0 {
                return Err("session closed stderr".to_string());
            }
            let trimmed = line.trim_end_matches('\n');
            if let Some(idx) = trimmed.find(&sentinel) {
                if idx > 0 {
                    sender.send_stderr(&trimmed[..idx]).await;
                }
                return Ok(());
            }
            sender.send_stderr(trimmed).await;
        }
    };

    match tokio::time::timeout(timeout, async { tokio::join!(stdout_read, stderr_read) }).await
    {
        Ok((Ok(code), Ok(()))) => Ok(code),
        Ok((Err(e), _)) | Ok((_, Err(e))) => Err(e),
        Err(_) => Err(format!(
            "command timed out after {}s",
            timeout.as_secs()
        )),
    }
}
```

- [ ] **Step 4: Export from lib.rs**

```rust
mod exec;
pub mod session;
mod signal;

pub use exec::execute_shell;
pub use session::SessionManager;
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd daemon && cargo test -p ix-shell`
Expected: all PASS, including the <5 ms hot-path assertion.

---

### Task 11: Wire sessions into the server + Go integration test

**Files:**
- Modify: `daemon/crates/ix-server/src/state.rs`
- Modify: `daemon/crates/ix-server/src/main.rs`
- Modify: `daemon/crates/ix-server/src/routes/shell.rs`
- Create: `go-sdk/shell_session_integration_test.go`

- [ ] **Step 1: AppState gains the manager**

`state.rs`:

```rust
pub struct AppState {
    pub config: ix_core::config::DaemonConfig,
    pub browser: Arc<dyn ix_browser::BrowserBackend>,
    pub kernels: Arc<ix_code::KernelManager>,
    pub shell_sessions: Arc<ix_shell::SessionManager>,
    pub egress: Option<Arc<ix_egress::EgressFilter>>,
    pub start_time: std::time::Instant,
}
```

`main.rs` — next to `let kernels = ...`:

```rust
    let shell_sessions = ix_shell::SessionManager::new_shared();
```

and add `shell_sessions: shell_sessions.clone(),` to the `AppState` literal. If main has a shutdown path that calls `kernels.shutdown()`, add `shell_sessions.shutdown().await;` beside it.

- [ ] **Step 2: Route dispatch**

`routes/shell.rs`:

```rust
pub async fn shell_exec(
    State(state): State<Arc<AppState>>,
    Json(req): Json<ShellRequest>,
) -> SseResponse {
    let (sender, sse) = ix_core::sse::sse_channel(32);
    if req.session_id.is_some() {
        let sessions = state.shell_sessions.clone();
        tokio::spawn(async move {
            sessions.execute(req, sender).await;
        });
    } else {
        tokio::spawn(async move {
            ix_shell::execute_shell(req, sender).await;
        });
    }
    sse
}
```

(Change `State(_state)` to `State(state)`; `e2b_shell` keeps delegating.)

- [ ] **Step 3: Build + full daemon tests**

Run: `cd daemon && cargo build -p ix-server && cargo test --all`
Expected: green.

- [ ] **Step 4: Go integration test**

Create `go-sdk/shell_session_integration_test.go`:

```go
//go:build integration

package ix

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestShellSessionPersistence: Shell() shares one bash session per sandbox;
// ShellOneShot() must NOT see session state. Requires a rootfs rebuilt with
// the session-aware ixd (Task 17).
func TestShellSessionPersistence(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-shell-session"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "export IX_SESSION_PROBE=hello"}); err != nil {
		t.Fatalf("set var: %v", err)
	}
	res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo $IX_SESSION_PROBE"})
	if err != nil {
		t.Fatalf("read var: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("session state lost: %q", res.Output)
	}

	one, err := sb.(*IXSandbox).ShellOneShot(ctx, sandbox.ShellRequest{Command: "echo [$IX_SESSION_PROBE]"})
	if err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	if strings.Contains(one.Output, "hello") {
		t.Errorf("one-shot leaked session state: %q", one.Output)
	}
}
```

- [ ] **Step 5: Compile-check** (runs for real after Task 17's rootfs rebuild)

Run: `cd go-sdk && go vet -tags=integration ./...`
Expected: clean.

---

## Phase 3 — SDK transport

### Task 12: SSE drain-to-EOF + keep-alive connection pool

**Files:**
- Modify: `go-sdk/client.go` (`sseReader.Close`)
- Modify: `go-sdk/vmm_vsock.go` (`vsockTransport`)
- Modify: `go-sdk/client_test.go` (fake vsock proxy + reuse test)

- [ ] **Step 1: Write the failing test**

Append to `go-sdk/client_test.go`:

```go
// fakeVsockProxy speaks the Firecracker vsock UDS handshake
// ("CONNECT <port>\n" -> "OK <port>\n") and then serves HTTP on the
// connection, counting dials so tests can assert keep-alive reuse.
type fakeVsockProxy struct {
	dials atomic.Int64
}

type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// bufferedConn replays bytes the handshake reader already buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func startFakeVsockProxy(t *testing.T, sockPath string, handler http.Handler) *fakeVsockProxy {
	t.Helper()
	p := &fakeVsockProxy{}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	httpConns := &chanListener{ch: make(chan net.Conn, 16), addr: ln.Addr()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(httpConns.ch)
				return
			}
			p.dials.Add(1)
			go func(conn net.Conn) {
				r := bufio.NewReader(conn)
				if _, err := r.ReadString('\n'); err != nil { // CONNECT 1024\n
					conn.Close()
					return
				}
				if _, err := conn.Write([]byte("OK 1024\n")); err != nil {
					conn.Close()
					return
				}
				httpConns.ch <- &bufferedConn{Conn: conn, r: r}
			}(conn)
		}
	}()
	srv := &http.Server{Handler: handler}
	go srv.Serve(httpConns) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return p
}

// TestSSEConnectionReuse: consecutive SSE requests must reuse one vsock
// connection. Before the drain-on-Close fix, every request re-dialed.
func TestSSEConnectionReuse(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vsock.uds")
	proxy := startFakeVsockProxy(t, sock, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: complete\ndata: {\"exit_code\":0,\"elapsed_ms\":1}\n\n")
	}))

	client := newClient("http://localhost", &http.Client{Transport: vsockTransport(sock)})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rd, err := client.postSSE(ctx, "/v1/shell/exec", map[string]any{"command": "true"})
		if err != nil {
			t.Fatalf("postSSE %d: %v", i, err)
		}
		for rd.Next() {
			if rd.Event() == "complete" {
				break
			}
		}
		rd.Close()
	}

	if got := proxy.dials.Load(); got != 1 {
		t.Errorf("expected 1 vsock dial across 3 SSE requests, got %d", got)
	}
}
```

Add any missing imports (`bufio`, `net`, `net/http`, `path/filepath`, `sync/atomic`, `fmt`, `context`).

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestSSEConnectionReuse ./... -count=1`
Expected: FAIL with `expected 1 vsock dial ... got 3`.

- [ ] **Step 3: Implement the drain + pool settings**

`client.go` — replace `sseReader.Close`:

```go
// Close finishes the SSE body and releases the connection.
//
// It drains to EOF first (bounded): the daemon ends the stream right after
// the terminal event, so the drain normally reads zero bytes and lets the
// HTTP/1.1 keep-alive pool reuse the connection. Closing without reaching
// EOF would discard the connection and force a fresh vsock dial + CONNECT
// handshake on the next request.
func (r *sseReader) Close() error {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.body, 64<<10))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		// Stream still open (e.g. abandoning a long-running command mid-way):
		// give up on reuse and hard-close below.
	}
	r.cancel()
	return r.body.Close()
}
```

Add `"time"` to client.go imports.

`vmm_vsock.go` — extend the transport (DialContext body unchanged apart from the Task 2 counter):

```go
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialCount.Add(1)
			// ... existing dial + CONNECT handshake ...
		},
		// The daemon terminates SSE streams after the final event, so
		// connections come back to this pool instead of re-dialing the UDS
		// + CONNECT handshake per request.
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
```

Add `"time"` to vmm_vsock.go imports.

- [ ] **Step 4: Run to verify pass**

Run: `cd go-sdk && go test -run TestSSEConnectionReuse ./... -count=1`
Expected: PASS. Then `go test ./... -count=1` — all green.

---

### Task 13: Pollers — immediate first check, 1 ms tick

**Files:**
- Modify: `go-sdk/network.go` (`waitForFile`)
- Modify: `go-sdk/snapshot.go` (`waitHealthyAuth`)
- Modify: `go-sdk/network_test.go`

- [ ] **Step 1: Write the failing test**

In `network_test.go`:

```go
func TestWaitForFileExistingReturnsImmediately(t *testing.T) {
	f := filepath.Join(t.TempDir(), "exists")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := waitForFile(f, time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Millisecond {
		t.Errorf("existing file took %v — first check must not wait for a tick", elapsed)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestWaitForFileExisting ./... -count=1`
Expected: FAIL (~5 ms first tick).

- [ ] **Step 3: Implement**

`network.go`:

```go
// waitForFile polls for the existence of a file with a timeout. The first
// check is immediate (a freshly-forked Firecracker binds its API socket in
// well under a tick); subsequent checks run every 1 ms.
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("file not found after %v: %s", timeout, path)
		}
		<-ticker.C
	}
}
```

`snapshot.go` — restructure `waitHealthyAuth` to probe first, tick at 1 ms:

```go
func waitHealthyAuth(ctx context.Context, httpClient *http.Client, bearer string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Probe immediately — a snapshot-restored daemon is usually already
		// serving, so waiting a tick before the first probe is pure latency.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/health", nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("guest daemon health check timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd go-sdk && go test ./... -count=1`
Expected: all green.

---

## Phase 4 — Restore + destroy hot path

### Task 14: Pre-copied scratch pool

**Files:**
- Modify: `go-sdk/snapshot.go`
- Modify: `go-sdk/manager.go` (stop pool in `Close`)
- Modify: `go-sdk/snapshot_test.go`

- [ ] **Step 1: Write the failing test**

In `snapshot_test.go`:

```go
func TestScratchPoolFillTakeRefill(t *testing.T) {
	dir := t.TempDir()
	sm := &SnapshotManager{snapshotDir: dir, logger: slog.Default()}
	if err := os.WriteFile(sm.scratchGoldenPath(), []byte("golden"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.startScratchPool()
	defer sm.StopScratchPool()

	take := func(name string) string {
		dst := filepath.Join(dir, name)
		deadline := time.Now().Add(5 * time.Second)
		for !sm.takePooledScratch(dst) {
			if time.Now().After(deadline) {
				t.Fatalf("pool never produced a scratch for %s", name)
			}
			time.Sleep(2 * time.Millisecond)
		}
		return dst
	}

	dst := take("clone-a.ext4")
	if b, _ := os.ReadFile(dst); string(b) != "golden" {
		t.Errorf("pooled scratch content = %q, want golden copy", b)
	}
	// Pool refills after consumption.
	take("clone-b.ext4")
}

func TestTakePooledScratchDisabledPool(t *testing.T) {
	sm := &SnapshotManager{snapshotDir: t.TempDir(), logger: slog.Default()}
	if sm.takePooledScratch(filepath.Join(t.TempDir(), "x.ext4")) {
		t.Error("takePooledScratch must return false when the pool was never started")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestScratchPool -run TestTakePooled ./... -count=1` (run both names: `go test -run 'TestScratchPool|TestTakePooled' ./... -count=1`)
Expected: FAIL to compile (methods missing).

- [ ] **Step 3: Implement in `snapshot.go`**

Add fields to `SnapshotManager`:

```go
	// scratchPool holds clone-ready copies of the golden scratch. Restore
	// renames one in (~µs) instead of paying a cp fork+exec on the hot path.
	scratchPool chan string
	poolStop    chan struct{}
```

Add methods:

```go
// scratchPoolSize is how many clone-ready scratch copies are kept warm.
const scratchPoolSize = 4

// startScratchPool launches the background filler. Idempotent per manager:
// CreateGolden calls it once the golden scratch exists.
func (sm *SnapshotManager) startScratchPool() {
	if sm.scratchPool != nil {
		return
	}
	sm.scratchPool = make(chan string, scratchPoolSize)
	sm.poolStop = make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-sm.poolStop:
				return
			default:
			}
			p := filepath.Join(sm.snapshotDir, fmt.Sprintf("scratch.pool.%d.ext4", i))
			if err := copySparse(sm.scratchGoldenPath(), p); err != nil {
				sm.logger.Warn("scratch pool copy failed", "error", err)
				select {
				case <-sm.poolStop:
					return
				case <-time.After(time.Second):
				}
				continue
			}
			select {
			case sm.scratchPool <- p:
			case <-sm.poolStop:
				_ = os.Remove(p)
				return
			}
		}
	}()
}

// StopScratchPool stops the filler and removes unconsumed pool files.
func (sm *SnapshotManager) StopScratchPool() {
	if sm.poolStop == nil {
		return
	}
	close(sm.poolStop)
	for {
		select {
		case p := <-sm.scratchPool:
			_ = os.Remove(p)
		default:
			return
		}
	}
}

// takePooledScratch moves one pre-copied scratch into dst. False = pool
// empty or disabled; the caller falls back to copySparse.
func (sm *SnapshotManager) takePooledScratch(dst string) bool {
	if sm.scratchPool == nil {
		return false
	}
	select {
	case p := <-sm.scratchPool:
		if err := os.Rename(p, dst); err != nil {
			sm.logger.Warn("scratch pool rename failed; falling back to copy", "error", err)
			_ = os.Remove(p)
			return false
		}
		return true
	default:
		return false
	}
}
```

In `Restore`, replace the `copySparse(...)` clone-scratch block with:

```go
	// Per-clone scratch: prefer a pre-copied pool entry (rename, ~µs); fall
	// back to a sparse copy when the pool is empty.
	scratchDst := filepath.Join(socketDir, scratchFileName)
	if !sm.takePooledScratch(scratchDst) {
		if err := copySparse(sm.scratchGoldenPath(), scratchDst); err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("create clone scratch: %w", err)
		}
	}
```

In `CreateGolden`, after `sm.ready = true; sm.mu.Unlock()` add:

```go
	sm.startScratchPool()
```

- [ ] **Step 4: Stop the pool on manager close**

In `manager.go` `Close()`, alongside existing teardown:

```go
	if m.vmm != nil && m.vmm.snapshot != nil {
		m.vmm.snapshot.StopScratchPool()
	}
```

- [ ] **Step 5: Run to verify pass**

Run: `cd go-sdk && go test ./... -count=1`
Expected: all green (`"time"` import needed in snapshot.go if absent — it is already imported).

---

### Task 15: Two-phase destroy (tombstone + async delete)

**Files:**
- Modify: `go-sdk/vmm.go` (`cleanup`)
- Modify: `go-sdk/reaper.go` (comment on `recover` covering tombstones)
- Modify: `go-sdk/vmm_test.go`

- [ ] **Step 1: Write the failing test**

In `vmm_test.go`:

```go
// TestCleanupTombstonesSocketDir: cleanup must fence the dir synchronously
// (rename — the sandbox path is immediately reusable) and delete it shortly
// after in the background.
func TestCleanupTombstonesSocketDir(t *testing.T) {
	base := t.TempDir()
	sd := filepath.Join(base, "ix-tombtest")
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "scratch.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	fb := &firecrackerBackend{}
	fb.cleanup(&VMMHandle{SocketDir: sd})

	// Synchronous guarantee: the original path is gone the moment cleanup returns.
	if _, err := os.Stat(sd); !os.IsNotExist(err) {
		t.Errorf("socket dir still present after cleanup: %v", err)
	}

	// Async guarantee: the tombstone disappears shortly after.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(base)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tombstone never deleted: %v", entries)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestCleanupTombstones ./... -count=1`
Expected: PASS-or-FAIL depending on timing — with the current synchronous `RemoveAll` the *first* assertion passes but the test documents the new contract; it will still pass. To make the new behavior observable, ALSO add this stricter latency probe to the same test, which fails for a large synchronous delete and passes for rename+async:

```go
	// Caller-visible latency: rename is O(1); a synchronous RemoveAll of a
	// populated dir is not. Populate with many files to make the difference
	// observable, then require cleanup to return fast.
	sd2 := filepath.Join(base, "ix-tombtest2")
	if err := os.MkdirAll(sd2, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(sd2, fmt.Sprintf("f%04d", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	fb.cleanup(&VMMHandle{SocketDir: sd2})
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("cleanup blocked %v — delete must be async", elapsed)
	}
```

Run again; Expected: FAIL on the latency assertion (sync RemoveAll of 2000 files).

- [ ] **Step 3: Implement in `vmm.go`**

Add near the top of the file:

```go
// tombstoneCounter disambiguates concurrent tombstones for the same dir name.
var tombstoneCounter atomic.Uint64
```

Replace the socket-dir removal in `cleanup`:

```go
	if handle.SocketDir != "" {
		// Two-phase destroy: synchronously rename the dir to a tombstone so
		// the sandbox path is fenced (no new VM can collide with stale files),
		// then delete in the background. Same total IO, off the caller's
		// latency path. Trade-offs (documented in BENCHMARKS.md v0.7):
		//   - disk reclaim lags by ms-to-seconds under churn (ENOSPC window
		//     on a nearly-full host; the reaper's disk-pressure path remains
		//     the backstop)
		//   - "Destroy returned" no longer implies "disk clean"; a manager
		//     crash can orphan tombstones — recover() sweeps ix-* dirs
		//     (tombstones keep the ix- prefix) at startup.
		tomb := fmt.Sprintf("%s.deleting.%d", handle.SocketDir, tombstoneCounter.Add(1))
		if err := os.Rename(handle.SocketDir, tomb); err != nil {
			// Rename failed (already gone, cross-device, ...) — fall back to
			// the old synchronous delete rather than leak.
			_ = os.RemoveAll(handle.SocketDir)
			return
		}
		go func() { _ = os.RemoveAll(tomb) }()
	}
```

- [ ] **Step 4: Document the sweep in `reaper.go`**

Extend the comment on `recover` (no logic change — tombstones are `ix-<id>.deleting.<n>`, matched by the existing `strings.HasPrefix(entry.Name(), "ix-")` scan and never in the `active` set):

```go
// recover scans RunDir (current per-VM dirs) and /tmp (leftovers from
// pre-RunDir versions) for orphaned ix-* socket directories left by a previous
// manager instance and removes them. This includes two-phase-destroy
// tombstones ("ix-<id>.deleting.<n>") orphaned by a crash between the rename
// and the background delete. VM process recovery is not supported in
// Phase 1 — we cannot reliably identify which processes belong to us across
// restarts without a process registry.
```

- [ ] **Step 5: Run to verify pass**

Run: `cd go-sdk && go test ./... -count=1`
Expected: all green, tombstone test included.

---

## Phase 5 — Guest image

### Task 16: Workspace bind-mount, noatime, journal-less scratch

**Files:**
- Modify: `go-sdk/scripts/ix-stage0.sh`
- Modify: `go-sdk/scratch.go` (`ensureScratchTemplate`)
- Modify: `go-sdk/scratch_test.go`

- [ ] **Step 1: Write the failing test for the journal-less template**

In `scratch_test.go`:

```go
func TestScratchTemplateHasNoJournal(t *testing.T) {
	if _, err := exec.LookPath("dumpe2fs"); err != nil {
		t.Skip("dumpe2fs not installed")
	}
	path := filepath.Join(t.TempDir(), "scratch.ext4")
	if err := ensureScratchTemplate(path, 64); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("dumpe2fs", "-h", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs: %v: %s", err, out)
	}
	if strings.Contains(string(out), "has_journal") {
		t.Error("scratch template has a journal — ephemeral disks must be journal-less")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd go-sdk && go test -run TestScratchTemplateHasNoJournal ./... -count=1`
Expected: FAIL (journal present) — or SKIP without dumpe2fs; if skipped, verify manually once with `dumpe2fs -h`.

- [ ] **Step 3: Implement the mkfs change in `scratch.go`**

```go
	// ^has_journal: the scratch is ephemeral by design (per-VM, deleted on
	// destroy) — journaling buys crash-consistency nobody reads, and costs
	// extra writes on the agent's /workspace hot path.
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", "-O", "^has_journal", tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4 scratch template: %w: %s", err, out)
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd go-sdk && go test -run TestScratchTemplateHasNoJournal ./... -count=1`
Expected: PASS (or SKIP).

- [ ] **Step 5: Update `ix-stage0.sh`**

Replace the mount/overlay section (between the devtmpfs mount and the `cd /scratch/newroot` line) with:

```sh
# Per-VM writable scratch disk. The /scratch mountpoint is baked into the
# image by build-rootfs-ext4.sh. noatime: every read would otherwise write
# an atime update to the scratch on the agent's file-op hot path.
mount -o noatime /dev/vdb /scratch

mkdir -p /scratch/upper /scratch/work /scratch/newroot /scratch/workspace
mount -t overlay overlay \
  -o lowerdir=/,upperdir=/scratch/upper,workdir=/scratch/work \
  /scratch/newroot

# /workspace hot path: bind the scratch's own directory OVER the overlay's
# /workspace so agent file ops write raw ext4 — no overlayfs lookup/copy-up
# machinery in the write path. Everything else (pip installs, /etc writes)
# still goes through the whole-root overlay. mkdir writes the overlay upper,
# guaranteeing the mountpoint exists regardless of the base image.
mkdir -p /scratch/newroot/workspace
mount --bind /scratch/workspace /scratch/newroot/workspace
```

(The rest of the script — pivot_root, old-root detach, exec of ix-init — is unchanged. The bind mount holds its own reference to the scratch superblock, so the later `umount -l /old_root` stays safe, same as the overlay's references.)

- [ ] **Step 6: Shell-syntax check**

Run: `sh -n go-sdk/scripts/ix-stage0.sh`
Expected: no output.

---

### Task 17: Rebuild guest artifacts

**Files:** none (build/ops step)

- [ ] **Step 1: Full daemon test + release build**

Run: `cd daemon && cargo test --all && cargo build --release --target x86_64-unknown-linux-musl -p ix-server`
Expected: green; static `ixd` produced.

- [ ] **Step 2: Rebuild the rootfs images**

Follow the header of `go-sdk/scripts/build-rootfs-ext4.sh` (it drives the Docker build that bakes ixd, repl.py, and ix-stage0 into `base.ext4`; sudo may be required for loop mounts). Rebuild the `browser-vm` image too if browser benchmarks will run.
Expected: fresh `base.ext4` (and `browser-vm.ext4`) at the paths your `IX_ROOTFS_IMAGE` / `IX_BROWSER_VM_IMAGE` env points to.

- [ ] **Step 3: Invalidate the stale golden snapshot**

Run: `rm -rf /tmp/ix-golden-snapshot`
(The golden snapshot bakes the OLD rootfs + daemon; `Ready()` only checks file existence and would happily restore stale ixd without this.)

- [ ] **Step 4: Smoke the stack**

Run: `cd go-sdk && IX_ROOTFS_IMAGE=... IX_KERNEL_PATH=... IX_FC_BINARY=... go test -tags integration -run 'TestShellSessionPersistence|TestRootfsImmutableUnderConcurrentWrites|TestWorkspaceIsolation|TestSnapshotCloneIsolation' ./... -count=1 -timeout 600s`
Expected: ALL PASS — the new session test and all three v0.6 isolation regression tests.

---

## Phase 6 — Final measurement + documentation

### Task 18: Re-measure, attribute, document v0.7

**Files:**
- Modify: `daemon/BENCHMARKS.md`
- Modify: `docs/handbook/05-operations.md` (destroy semantics note)

- [ ] **Step 1: Full suite, same parameters as the baseline**

Run: `cd go-sdk && COUNT=5 IX_TRACE=1 IX_ROOTFS_IMAGE=... IX_KERNEL_PATH=... IX_FC_BINARY=... IX_BROWSER_VM_IMAGE=... ./scripts/run-benchmarks.sh 10`
Expected: completes; results file saved.

- [ ] **Step 2: benchstat comparison against the Task 6 baseline**

Run: `./scripts/run-benchmarks.sh compare bench-results/<v0.6.1-file>.txt bench-results/<v0.7-file>.txt`
Expected: statistically significant deltas per benchmark.

- [ ] **Step 3: Write the v0.7 section in BENCHMARKS.md**

Add a new tracker row + section. Required content (fill numbers from the run):

```markdown
| **v0.7** | **Firecracker** | **vsock UDS** | <n>ms snapshot | <n>ms | <n>ms | <n>ms | <n>ms |

### v0.7 — hot-path overhaul (2026-06-XX)

Per-fix attribution (benchstat v0.6.1 → v0.7, COUNT=5):

| Fix | Benchmarks affected | Delta |
|---|---|---|
| Persistent shell sessions (daemon, session_id was silently ignored) | ShellPersistent, E2E | ... |
| stderr sentinel (removed hardcoded 10 ms drain per exec) | CodeExec* | ... |
| SSE drain-to-EOF + keep-alive (was: 1 vsock dial per request) | all per-op benches | ... |
| Pollers: immediate first probe (was: ≥10 ms before first health check) | CreateFromSnapshot | ... |
| Pre-copied scratch pool (rename vs cp fork+exec) | CreateFromSnapshot | ... |
| Two-phase destroy (tombstone + async delete) | Destroy, E2E cycles | ... |
| Quiet console + RUST_LOG=warn | per-op benches, CreateCold | ... |
| /workspace bind-mount + journal-less noatime scratch | FileReadWriteWorkspace | ... |

**Destroy semantics note:** Destroy now returns once the VM is dead, the TAP
is released, and the dir is renamed to a tombstone; file deletion happens on
a background goroutine. Same total IO — cleanup moved OFF the measured path,
it did not vanish. Sustained-churn throughput is unchanged. Disk reclaim can
lag by seconds under burst churn (ENOSPC window on a nearly-full host; the
reaper's disk-pressure eviction is the backstop). A manager crash can orphan
`ix-*.deleting.*` tombstones; recover() sweeps them at startup.

**Creation floor analysis:** of the <n> ms snapshot-restore time, <n> ms is
Firecracker process spawn + /snapshot/load (not ours to remove). The product
hot path is the pre-warmed pool (BenchmarkCreateFromPool: <n> ms).

**10x scorecard vs v0.6 (strict):**

| Metric | v0.6 | v0.7 | Speedup | 10x? |
|---|---|---|---|---|
| ShellPersistent | 30.2ms | ... | ... | ... |
| FileReadWrite | 19.4ms | ... | ... | ... |
| CodeExec (warm) | 26.1ms | ... | ... | ... |
| E2E snapshot cycle | 130.6ms | ... | ... | ... |
| Creation (snapshot) | 75.1ms | ... | ... | ... |

New browser-path coverage (no prior baseline — these ARE the baseline):
BrowserNavigate ..., BrowserSnapshot ..., BrowserScreenshot ...,
BrowserAction ..., BrowserEval ..., BrowserFirstUse ..., BrowserE2E ...
```

Honesty requirements: where a metric misses 10x, name the floor and its owner. Do not move a number into the table without a benchstat-backed run behind it.

- [ ] **Step 4: Handbook note**

In `docs/handbook/05-operations.md`, add a short "Destroy is two-phase" paragraph mirroring the BENCHMARKS note (sync: kill + TAP release + tombstone rename; async: file deletion; startup sweep; disk-reclaim caveat).

- [ ] **Step 5: Verification-before-completion sweep**

Run, and paste outputs into the final report:
- `cd daemon && cargo test --all`
- `cd go-sdk && go test ./... -count=1`
- `cd go-sdk && go vet -tags=integration ./...`
- the integration smoke from Task 17 Step 4
Expected: everything green. Only then is the work claimable as done.

---

## Self-review checklist (done at plan-writing time)

- **Spec coverage:** §3 harness → Tasks 1-6; §4.1 sessions → Tasks 9-11; §4.2 sentinel → Task 7; §4.3 SSE termination → verified server-side already terminates (sender drop closes stream), client side → Task 12; §4.4 logging → Task 8; §5.1 → Task 12; §5.2 → Task 13; §5.3 → Task 14; §5.4 → Task 15; §6.1/6.2 → Task 16; §7 testing → embedded per task + Task 17/18; §5.5 stretch (netlink TAP, parallel PUTs) intentionally not planned — cold-path only, revisit after v0.7 numbers.
- **Known deviations from spec, accepted:** per-tombstone goroutine instead of a single deleter goroutine (same immediacy, less machinery); session busy-marker (`Option<Session>`) added beyond spec to handle concurrent same-id calls safely; default 300 s session command timeout added (hang protection the spec missed).
- **Type consistency:** `SessionManager::new_shared() -> Arc<Self>` used in Tasks 10/11; `takePooledScratch/startScratchPool/StopScratchPool` consistent across Tasks 14; `buildKernelBootArgs(envSlice, net, withConsole)` consistent across Task 8 tests/impl/callsite; `poolHits` defined in Task 1 and used only there.
