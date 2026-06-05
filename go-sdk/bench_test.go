//go:build integration

package ix

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// rootfsImage returns the ext4 rootfs image path from IX_ROOTFS_IMAGE or a default.
func rootfsImage() string {
	if p := os.Getenv("IX_ROOTFS_IMAGE"); p != "" {
		return p
	}
	return "/opt/ix/rootfs/base.ext4"
}

// kernelPath returns the vmlinux kernel path from IX_KERNEL_PATH or a default.
func kernelPath() string {
	if p := os.Getenv("IX_KERNEL_PATH"); p != "" {
		return p
	}
	return "/opt/ix/firecracker/vmlinux.bin"
}

// fcBinary returns the firecracker binary path from IX_FC_BINARY or empty (PATH lookup).
func fcBinary() string {
	return os.Getenv("IX_FC_BINARY")
}

// waitPoolFill blocks until the manager pool has at least minReady entries or
// the deadline (30 s) is exceeded.
func waitPoolFill(m *IXManager, minReady int) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m.poolMu.Lock()
		n := len(m.pool)
		m.poolMu.Unlock()
		if n >= minReady {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

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

// BenchmarkCreateCold measures cold VM creation latency (no pool).
func BenchmarkCreateCold(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		PoolSize:    0,
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sid := fmt.Sprintf("bench-cold-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkCreateFromPool measures pool-warmed VM creation latency.
func BenchmarkCreateFromPool(b *testing.B) {
	ctx := context.Background()

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		PoolSize:    poolSize,
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

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
}

// BenchmarkShellEcho measures end-to-end shell command latency on a single sandbox.
func BenchmarkShellEcho(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-shell"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo hello"})
		if err != nil {
			b.Fatalf("Shell: %v", err)
		}
	}
}

// BenchmarkCodeExecPython measures Python code execution after the kernel is warm.
func BenchmarkCodeExecPython(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-python"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	// Warmup: boot the Python kernel so subsequent calls are steady-state.
	warmupCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if _, err := sb.ExecCode(warmupCtx, sandbox.CodeRequest{Language: "python", Code: "pass", Timeout: 90}); err != nil {
		b.Fatalf("warmup ExecCode: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := sb.ExecCode(ctx, sandbox.CodeRequest{Language: "python", Code: "x = 42"})
		if err != nil {
			b.Fatalf("ExecCode: %v", err)
		}
	}
}

// BenchmarkCodeExecFirstCall measures the combined latency of VM creation + kernel
// boot for a fresh sandbox's first code execution.
func BenchmarkCodeExecFirstCall(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sid := fmt.Sprintf("bench-firstcall-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		if _, err := sb.ExecCode(ctx, sandbox.CodeRequest{Language: "python", Code: "x = 1"}); err != nil {
			b.Fatalf("ExecCode: %v", err)
		}
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkFileReadWrite measures a file write + read round-trip latency.
func BenchmarkFileReadWrite(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-file"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    "/tmp/bench.txt",
			Content: "hello",
		}); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}
		if _, err := sb.ReadFile(ctx, sandbox.ReadFileRequest{
			Path: "/tmp/bench.txt",
		}); err != nil {
			b.Fatalf("ReadFile: %v", err)
		}
	}
}

// BenchmarkShellPersistent measures shell latency with a persistent bash session
// (the default Shell() behavior). Contrasts with BenchmarkShellOneShot to
// demonstrate session-reuse savings after the first call.
func BenchmarkShellPersistent(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-shell-persistent"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	// Prime the persistent session so the first measured iteration is steady-state.
	if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "true"}); err != nil {
		b.Fatalf("prime Shell: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo hello"})
		if err != nil {
			b.Fatalf("Shell: %v", err)
		}
	}
}

// BenchmarkShellOneShot measures shell latency without session reuse.
// Each call fork+execs a fresh shell process inside the VM, exposing the
// full fork+exec overhead per invocation (~12 ms target).
func BenchmarkShellOneShot(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  10 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-shell-oneshot"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := sb.(*IXSandbox).ShellOneShot(ctx, sandbox.ShellRequest{Command: "echo hello"})
		if err != nil {
			b.Fatalf("ShellOneShot: %v", err)
		}
	}
}

// BenchmarkCreatePoolPreWarmed measures pool-grab + first code execution latency
// when the pool was created with PreWarmKernels: ["python"]. The kernel is
// already booted inside the VM so the first ExecCode call should be ~10 ms.
func BenchmarkCreatePoolPreWarmed(b *testing.B) {
	ctx := context.Background()

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:    rootfsImage(),
		KernelPath:     kernelPath(),
		FCBinary:       fcBinary(),
		PoolSize:       poolSize,
		DefaultTTL:     5 * time.Minute,
		PreWarmKernels: []string{"python"},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	// Wait for at least one pre-warmed entry to be ready before measuring.
	waitPoolFill(mgr, 1)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		waitPoolFill(mgr, 1)
		b.StartTimer()

		sid := fmt.Sprintf("bench-prewarmed-%d", i)

		// Pool grab: VM already running with Python kernel booted.
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}

		// First code exec: kernel is pre-warmed so this should be near-instant.
		if _, err := sb.ExecCode(ctx, sandbox.CodeRequest{Language: "python", Code: "x = 1"}); err != nil {
			b.Fatalf("ExecCode: %v", err)
		}

		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}

	if got := mgr.poolHits.Load(); got < int64(b.N) {
		b.Fatalf("pool misses: only %d/%d creates were pool hits", got, b.N)
	}
}

// BenchmarkE2EAgentCycle measures a full agent interaction cycle matching the
// spec's definition: create → shell → file write → code exec → destroy.
// Uses a pre-warmed pool to exclude VM boot time from the measurement.
func BenchmarkE2EAgentCycle(b *testing.B) {
	ctx := context.Background()

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:    rootfsImage(),
		KernelPath:     kernelPath(),
		FCBinary:       fcBinary(),
		PoolSize:       poolSize,
		DefaultTTL:     5 * time.Minute,
		PreWarmKernels: []string{"python"},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	waitPoolFill(mgr, 1)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sid := fmt.Sprintf("bench-agent-cycle-%d", i)

		// 1. Create sandbox from pool.
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		ix := sb.(*IXSandbox)

		// 2. Shell command (persistent session — ~3 ms target).
		if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo agent-ready"}); err != nil {
			b.Fatalf("Shell: %v", err)
		}

		// 3. File write.
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    "/tmp/agent_input.txt",
			Content: "input data",
		}); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}

		// 4. Code execution (kernel pre-warmed — ~10 ms target).
		if _, err := sb.ExecCode(ctx, sandbox.CodeRequest{
			Language: "python",
			Code:     `print(open("/tmp/agent_input.txt").read())`,
		}); err != nil {
			b.Fatalf("ExecCode: %v", err)
		}

		// 5. Destroy sandbox.
		if err := mgr.Destroy(ctx, ix.id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkEndToEnd simulates a full agent workflow: create sandbox, write a
// Python file, execute it, read the output, then destroy.
func BenchmarkEndToEnd(b *testing.B) {
	ctx := context.Background()

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		PoolSize:    poolSize,
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	// Pre-warm the pool before measuring.
	waitPoolFill(mgr, 1)

	const script = `
	result = 6 * 7
	print(result)
	`
	const outFile = "/tmp/bench_result.txt"

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sid := fmt.Sprintf("bench-e2e-%d", i)

		// 1. Create sandbox (from pool when available).
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		ix := sb.(*IXSandbox)

		// 2. Write a Python script.
		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    "/tmp/bench_script.py",
			Content: script,
		}); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}

		// 3. Execute the Python script, redirecting output to a file.
		if _, err := sb.Shell(ctx, sandbox.ShellRequest{
			Command: "python3 /tmp/bench_script.py > " + outFile,
		}); err != nil {
			b.Fatalf("Shell exec: %v", err)
		}

		// 4. Read the output file.
		if _, err := sb.ReadFile(ctx, sandbox.ReadFileRequest{Path: outFile}); err != nil {
			b.Fatalf("ReadFile: %v", err)
		}

		// 5. Destroy sandbox.
		if err := mgr.Destroy(ctx, ix.id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkCreateFromSnapshot measures VM creation latency when restoring from
// a golden snapshot. This is the primary performance measurement for snapshot/restore.
func BenchmarkCreateFromSnapshot(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		UseSnapshot: true,
		PoolSize:    0,
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	// Wait for golden snapshot to be ready.
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
		sid := fmt.Sprintf("bench-snap-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		b.StopTimer()
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkE2ESnapshotCycle measures end-to-end cycle latency (create → shell → file I/O → destroy)
// when using snapshot/restore. This compares against BenchmarkE2EAgentCycle to show
// the E2E performance improvement snapshot brings.
func BenchmarkE2ESnapshotCycle(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		UseSnapshot: true,
		PoolSize:    0,
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
		sid := fmt.Sprintf("bench-snap-e2e-%d", i)

		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}

		if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo agent-ready"}); err != nil {
			b.Fatalf("Shell: %v", err)
		}

		if err := sb.WriteFile(ctx, sandbox.WriteFileRequest{
			Path:    "/tmp/agent_input.txt",
			Content: "input data",
		}); err != nil {
			b.Fatalf("WriteFile: %v", err)
		}

		if _, err := sb.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/tmp/agent_input.txt"}); err != nil {
			b.Fatalf("ReadFile: %v", err)
		}

		// Code execution (kernel pre-warmed in snapshot).
		if _, err := sb.ExecCode(ctx, sandbox.CodeRequest{
			Language: "python",
			Code:     `print(open("/tmp/agent_input.txt").read())`,
		}); err != nil {
			b.Fatalf("ExecCode: %v", err)
		}

		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkCodeExecSnapshot measures Python code execution latency on a
// snapshot-restored sandbox with a pre-warmed kernel.
func BenchmarkCodeExecSnapshot(b *testing.B) {
	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		UseSnapshot: true,
		PoolSize:    0,
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

	// Create one sandbox and keep it for all iterations.
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "bench-code-snap"})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := sb.ExecCode(ctx, sandbox.CodeRequest{Language: "python", Code: "x = 42"})
		if err != nil {
			b.Fatalf("ExecCode: %v", err)
		}
	}
}

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
