//go:build integration

package ix

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// R7 — measure what a sandbox actually costs the host.
//
// Admission control (ADR 0001) divides host memory by a per-sandbox allowance,
// but that allowance is a configured ceiling, not observed usage: Firecracker
// faults guest RAM in lazily, so a VM configured for 512 MB may hold far less
// until the guest touches it. The number admission must be sized against is the
// HIGH-WATER MARK — once a guest page is faulted in, the host does not get it
// back when the workload ends.
//
// This test reports host RSS per Firecracker process across the phases of a
// sandbox's life, with every sandbox bursting at once (the case the reserve has
// to survive). It asserts almost nothing; its output is the deliverable.
//
//	go test -tags=integration -run TestSandboxRSSProfile -v -timeout 30m ./...
//
// Tunables: IX_RSS_SANDBOXES (default 6), IX_RSS_MEMORY_MB (default 512, matching
// Athena's setting), IX_RSS_BURST_MB (default 256, how much the guest allocates).

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// procRSS reads VmRSS for a pid. Returns 0 once the process is gone, which is a
// valid reading during teardown rather than an error worth failing a run over.
func procRSS(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// meminfoField reads one /proc/meminfo value in bytes (e.g. "MemAvailable").
func meminfoField(name string) int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	prefix := name + ":"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

type rssSample struct {
	phase     string
	note      string
	perVM     []int64
	hostAvail int64
}

func sampleRSS(phase, note string, pids []int) rssSample {
	s := rssSample{phase: phase, note: note, hostAvail: meminfoField("MemAvailable")}
	for _, pid := range pids {
		s.perVM = append(s.perVM, procRSS(pid))
	}
	return s
}

func (s rssSample) stats() (minv, median, maxv, total int64) {
	if len(s.perVM) == 0 {
		return 0, 0, 0, 0
	}
	sorted := append([]int64(nil), s.perVM...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, v := range sorted {
		total += v
	}
	return sorted[0], sorted[len(sorted)/2], sorted[len(sorted)-1], total
}

func mib(b int64) string { return fmt.Sprintf("%.0f", float64(b)/(1<<20)) }

func TestSandboxRSSProfile(t *testing.T) {
	n := envInt("IX_RSS_SANDBOXES", 6)
	memMB := envInt("IX_RSS_MEMORY_MB", 512)
	burstMB := envInt("IX_RSS_BURST_MB", 256)

	ctx := context.Background()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  30 * time.Minute,
		PerSandbox:  ResourceSpec{VCPUs: 1, Memory: int64(memMB) << 20},
		// Explicit: auto-detect would cap this run below n on a small host, and
		// the point of the run is to measure n sandboxes side by side.
		MaxConcurrent: n + 2,
		// Per-VM TAP setup needs root for nft, which a measurement should not
		// require. A TAP device costs the guest no memory, so vsock-only VMs
		// report the same footprint. Set IX_RSS_NETWORK=1 on a host where
		// ix-host-setup has run to confirm that on real hardware.
		DisableNetworking: os.Getenv("IX_RSS_NETWORK") != "1",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	baseline := meminfoField("MemAvailable")

	var (
		sandboxes []*IXSandbox
		pids      []int
	)
	for i := 0; i < n; i++ {
		sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: fmt.Sprintf("rss-%d", i)})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ix := sb.(*IXSandbox)
		sandboxes = append(sandboxes, ix)
		pids = append(pids, ix.vmm.Process.Pid)
	}
	defer func() {
		for _, sb := range sandboxes {
			_ = mgr.Destroy(context.Background(), sb.id)
		}
	}()

	samples := []rssSample{sampleRSS("born", "immediately after Create returned", pids)}

	// Settle: a freshly booted guest is still finishing its own startup work, so
	// an immediate reading understates a genuinely idle sandbox.
	time.Sleep(30 * time.Second)
	samples = append(samples, sampleRSS("idle", "30s after boot, no requests", pids))

	// Does the rootfs carry Athena's heavy toolchain? The answer changes how far
	// these numbers can be carried over to production.
	probe, err := sandboxes[0].Shell(ctx, sandbox.ShellRequest{
		Command: "python3 -c 'import pandas, matplotlib; print(pandas.__version__)' 2>&1 | tail -1; command -v typst || echo 'typst: absent'",
	})
	toolchain := "probe failed"
	if err == nil {
		toolchain = strings.TrimSpace(probe.Output)
	}

	// Warm the Python kernel on every sandbox — the state the pool pre-warms to.
	var wg sync.WaitGroup
	for _, sb := range sandboxes {
		wg.Add(1)
		go func(sb *IXSandbox) {
			defer wg.Done()
			warmCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			if _, err := sb.ExecCode(warmCtx, sandbox.CodeRequest{Language: "python", Code: "pass", Timeout: 150}); err != nil {
				t.Logf("warm %s: %v", sb.id, err)
			}
		}(sb)
	}
	wg.Wait()
	samples = append(samples, sampleRSS("warm", "Python kernel booted, idle again", pids))

	// Burst, all at once. Sequential bursts would let the host recover page cache
	// between them and understate the peak that admission has to survive.
	burstCode := fmt.Sprintf(`
import array, hashlib
mb = %d
# array('b') is a real contiguous allocation the guest kernel must back with
# pages, unlike a list of small ints that may share cached objects.
buf = array.array('b', bytes(1024 * 1024))
chunks = [array.array('b', bytes(1024 * 1024)) for _ in range(mb)]
h = hashlib.sha256()
for c in chunks:
    h.update(c[:4096])
print('touched', len(chunks), 'MiB', h.hexdigest()[:8])
`, burstMB)

	burstStart := time.Now()
	for _, sb := range sandboxes {
		wg.Add(1)
		go func(sb *IXSandbox) {
			defer wg.Done()
			burstCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if _, err := sb.ExecCode(burstCtx, sandbox.CodeRequest{Language: "python", Code: burstCode, Timeout: 240}); err != nil {
				t.Logf("burst %s: %v", sb.id, err)
			}
		}(sb)
	}
	wg.Wait()
	burstDur := time.Since(burstStart)
	samples = append(samples, sampleRSS("burst", fmt.Sprintf("all %d allocated %d MiB concurrently", n, burstMB), pids))

	// The reading that decides the reserve: does the host get the memory back
	// when the workload finishes? For lazily-faulted guest RAM it does not.
	time.Sleep(60 * time.Second)
	samples = append(samples, sampleRSS("settled", "60s after burst, guests idle again", pids))

	t.Logf("\n=== R7: sandbox resident memory ===")
	t.Logf("host: %d logical CPUs, MemTotal %s MiB", runtime.NumCPU(), mib(meminfoField("MemTotal")))
	t.Logf("config: %d sandboxes, %d MiB guest each, burst %d MiB", n, memMB, burstMB)
	t.Logf("rootfs toolchain probe: %s", toolchain)
	t.Logf("concurrent burst wall time: %s", burstDur.Round(time.Millisecond))
	t.Logf("")
	t.Logf("%-9s %8s %8s %8s %9s %11s  %s", "phase", "min", "median", "max", "total", "host avail", "note")
	for _, s := range samples {
		mn, md, mx, tot := s.stats()
		t.Logf("%-9s %8s %8s %8s %9s %11s  %s", s.phase, mib(mn), mib(md), mib(mx), mib(tot), mib(s.hostAvail), s.note)
	}
	t.Logf("")

	var warm, burst, settled rssSample
	for _, s := range samples {
		switch s.phase {
		case "warm":
			warm = s
		case "burst":
			burst = s
		case "settled":
			settled = s
		}
	}
	_, warmMed, _, _ := warm.stats()
	_, burstMed, _, _ := burst.stats()
	_, settledMed, settledMax, _ := settled.stats()

	t.Logf("MemAvailable consumed end-to-end: %s MiB (baseline %s → %s MiB)",
		mib(baseline-settled.hostAvail), mib(baseline), mib(settled.hostAvail))
	t.Logf("idle-to-peak ratchet: %s MiB warm → %s MiB after a %d MiB burst",
		mib(warmMed), mib(burstMed), burstMB)
	t.Logf("reclaimed 60s after the burst: %s MiB", mib(burstMed-settledMed))

	// The reading that matters is not the peak number — that is just idle plus
	// whatever this burst happened to allocate, and a larger burst moves it. It
	// is whether the peak comes back down. If it does not, a sandbox's cost is a
	// ratchet that climbs toward its configured ceiling over a long session, and
	// admission cannot be sized against observed averages.
	if settledMed >= burstMed {
		t.Logf("→ guest memory is NOT returned to the host after use. Per-sandbox cost")
		t.Logf("  ratchets toward the %d MiB configured ceiling and stays there, so", memMB)
		t.Logf("  admission must be sized against the ceiling and bounded by a real limit.")
	} else {
		t.Logf("→ the host reclaimed %s MiB; admission may be sized below the ceiling",
			mib(burstMed-settledMed))
	}

	if settledMax == 0 {
		t.Fatal("every Firecracker process reported zero RSS — the measurement, not the runtime, is broken")
	}
}
