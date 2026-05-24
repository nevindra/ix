//go:build integration

package ix

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/nevindra/oasis/sandbox"
)

// imageExists reports whether the named Docker image is present on the local daemon.
func imageExists(ctx context.Context, imageName string) bool {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return false
	}
	defer cli.Close()
	_, _, err = cli.ImageInspectWithRaw(ctx, imageName)
	return err == nil
}

// waitPoolFill blocks until the manager pool has at least minReady entries or
// the deadline (20 s) is exceeded.
func waitPoolFill(m *IXManager, minReady int) {
	deadline := time.Now().Add(20 * time.Second)
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

const benchImage = "ix:base"

// BenchmarkCreateCold measures cold container creation latency (no pool).
func BenchmarkCreateCold(b *testing.B) {
	ctx := context.Background()
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		PoolSize:   0,
		DefaultTTL: 5 * time.Minute,
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
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
	}
}

// BenchmarkCreateFromPool measures pool-warmed container creation latency.
func BenchmarkCreateFromPool(b *testing.B) {
	ctx := context.Background()
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		PoolSize:   poolSize,
		DefaultTTL: 5 * time.Minute,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer mgr.Close()

	// Wait for pool to fill before benchmarking.
	waitPoolFill(mgr, 1)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sid := fmt.Sprintf("bench-pool-%d", i)
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: sid})
		if err != nil {
			b.Fatalf("Create: %v", err)
		}
		if err := mgr.Destroy(ctx, sb.(*IXSandbox).id); err != nil {
			b.Fatalf("Destroy: %v", err)
		}
		// Give the pool replenisher a moment to refill between iterations.
		time.Sleep(100 * time.Millisecond)
	}
}

// BenchmarkShellEcho measures end-to-end shell command latency on a single sandbox.
func BenchmarkShellEcho(b *testing.B) {
	ctx := context.Background()
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		DefaultTTL: 10 * time.Minute,
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
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		DefaultTTL: 10 * time.Minute,
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
	// Use a longer timeout — kernel boot can take 30s+ on first call.
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

// BenchmarkCodeExecFirstCall measures the combined latency of container + kernel
// pool for a fresh sandbox's first code execution.
func BenchmarkCodeExecFirstCall(b *testing.B) {
	ctx := context.Background()
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		DefaultTTL: 5 * time.Minute,
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
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		DefaultTTL: 10 * time.Minute,
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

// BenchmarkEndToEnd simulates a full agent workflow: create sandbox, write a
// Python file, execute it, read the output, then destroy.
func BenchmarkEndToEnd(b *testing.B) {
	ctx := context.Background()
	if !imageExists(ctx, benchImage) {
		b.Skipf("image %s not available", benchImage)
	}

	const poolSize = 3
	mgr, err := NewManager(ctx, ManagerConfig{
		Image:      benchImage,
		PoolSize:   poolSize,
		DefaultTTL: 5 * time.Minute,
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
