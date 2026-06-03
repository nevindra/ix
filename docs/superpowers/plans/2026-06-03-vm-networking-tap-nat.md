# VM Networking — per-VM TAP + NAT Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Commit policy (project override):** Do NOT `git commit`. Each task ends with a **Stage** step (`git add`) and leaves the working tree dirty. The user reviews batched changes and runs commits themselves.

**Goal:** Give every cold-booted Firecracker VM outbound IPv4 (per-VM TAP + kernel `ip=` + one-time host NAT) and make the shared browser Gateway reachable from per-chat guests, replacing the orphaned `passt` code.

**Architecture:** A `go-sdk` host-side change. `network.go` is rewritten: pure helpers derive a per-VM `/30` and build `ip`/`nft` command args (unit-tested), thin exec wrappers run them (boot-tested). `startVMCold` creates a TAP, wires it via `PUT /network-interfaces`, and adds an `ip=` boot arg. The manager applies host NAT + pins the Gateway's link-local IP once at startup. Snapshot-restored VMs stay vsock-only (unchanged).

**Tech Stack:** Go 1.x, Firecracker REST API (Unix socket), Linux `ip`/`nft`/`sysctl` (manager holds `CAP_NET_ADMIN`).

**Reference spec:** `docs/superpowers/specs/2026-06-03-vm-networking-tap-nat-design.md`

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `go-sdk/network.go` | **Rewrite** | Address derivation, `tapAllocator`, pure `ip`/`nft` arg builders, exec wrappers (`setupTap`/`teardownTap`/`ensureHostNAT`/`detectEgressInterface`/`ensureGatewayAddr`/`teardownGatewayAddr`), `waitForFile` (moved here). Removes all `passt` code. |
| `go-sdk/network_test.go` | **Rewrite** | Unit tests for derivation, allocator, arg/ruleset builders, parsers. Removes passt tests. |
| `go-sdk/vmm.go` | Modify | `VMMHandle.Net *vmNet` (replaces `PasstPID`); `firecrackerBackend` gains tap allocator + net config; `startVMCold` wires the TAP; `buildKernelBootArgs` gains `ip=`; `cleanup` tears down the TAP; remove `killPID`. |
| `go-sdk/vmm_test.go` | Modify | Update `buildKernelBootArgs` call sites + `VMMHandle` test; add `ip=` boot-arg tests. |
| `go-sdk/snapshot.go` | Modify | Drop `PasstPID: 0` from the restored `VMMHandle` literal (leaves `Net` nil = no tap). |
| `go-sdk/manager.go` | Modify | `ManagerConfig` net fields + defaults; construct backend with allocator/egress; call `ensureHostNAT`+`ensureGatewayAddr` before `startBrowserTier`; `teardownGatewayAddr` in `Close`. |
| `go-sdk/manager_test.go` | Modify | Tests for new `applyDefaults` fields + custom-CIDR rejection. |
| `daemon/cmd/browser-vm-init.sh` | Modify | Write `/etc/resolv.conf` (DNS belt-and-suspenders for Chrome). |

**Constant:** `172.16.0.0` = `0xAC100000`. `/30` mask = `255.255.255.252`. Max index = `16384` (`n ∈ [0,16383]`).

---

## Task 1: Address derivation (`vmNet` + `deriveVMNet`)

**Files:**
- Modify: `go-sdk/network.go` (rewrite begins here)
- Test: `go-sdk/network_test.go`

- [ ] **Step 1: Replace `network.go` contents with the package decl + derivation code**

Overwrite `go-sdk/network.go` with:

```go
package ix

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// netBase is 172.16.0.0 as a uint32 (0xAC100000). VM subnets are carved from
// 172.16.0.0/16 as /30 blocks: index n -> base = netBase + n*4.
const netBase uint32 = 0xAC100000

// netMask30 is the dotted-quad netmask for every per-VM /30.
const netMask30 = "255.255.255.252"

// maxTapIndex bounds the free-list: n in [0, maxTapIndex-1] => 16384 VMs.
const maxTapIndex = 16384

// vmNet describes the host/guest addressing for one VM's TAP device.
// A nil *vmNet means "no tap" (snapshot-restored or networking-disabled VM).
type vmNet struct {
	idx      int
	tapName  string
	hostIP   string // host side of the /30 (== guest's gateway)
	guestIP  string
	guestMAC string
	mask     string
}

// ipString formats a uint32 as a dotted-quad IPv4 string.
func ipString(u uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}

// guestMAC derives a deterministic, unique MAC from the guest IP, matching the
// Firecracker docs convention 06:00:<ip-as-4-hex-octets>.
func guestMAC(guest uint32) string {
	return fmt.Sprintf("06:00:%02X:%02X:%02X:%02X",
		byte(guest>>24), byte(guest>>16), byte(guest>>8), byte(guest))
}

// deriveVMNet computes the addressing for VM index n (pure, no side effects).
func deriveVMNet(n int) vmNet {
	base := netBase + uint32(n)*4
	return vmNet{
		idx:      n,
		tapName:  fmt.Sprintf("ixtap%d", n),
		hostIP:   ipString(base + 1),
		guestIP:  ipString(base + 2),
		guestMAC: guestMAC(base + 2),
		mask:     netMask30,
	}
}

// waitForFile polls for the existence of a file with a timeout. Shared by
// startVMCold and snapshot Restore to wait for the Firecracker API socket.
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return fmt.Errorf("file not found after %v: %s", timeout, path)
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
}
```

- [ ] **Step 2: Replace `network_test.go` with derivation table tests**

Overwrite `go-sdk/network_test.go` with:

```go
//go:build !integration

package ix

import "testing"

func TestDeriveVMNet(t *testing.T) {
	tests := []struct {
		n                                  int
		host, guest, mac, tap string
	}{
		{0, "172.16.0.1", "172.16.0.2", "06:00:AC:10:00:02", "ixtap0"},
		{1, "172.16.0.5", "172.16.0.6", "06:00:AC:10:00:06", "ixtap1"},
		{63, "172.16.0.253", "172.16.0.254", "06:00:AC:10:00:FE", "ixtap63"},
		// n=64 rolls into the third octet (64*4 = 256).
		{64, "172.16.1.1", "172.16.1.2", "06:00:AC:10:01:02", "ixtap64"},
		{16383, "172.16.255.253", "172.16.255.254", "06:00:AC:10:FF:FE", "ixtap16383"},
	}
	for _, tc := range tests {
		got := deriveVMNet(tc.n)
		if got.hostIP != tc.host {
			t.Errorf("n=%d hostIP = %q, want %q", tc.n, got.hostIP, tc.host)
		}
		if got.guestIP != tc.guest {
			t.Errorf("n=%d guestIP = %q, want %q", tc.n, got.guestIP, tc.guest)
		}
		if got.guestMAC != tc.mac {
			t.Errorf("n=%d guestMAC = %q, want %q", tc.n, got.guestMAC, tc.mac)
		}
		if got.tapName != tc.tap {
			t.Errorf("n=%d tapName = %q, want %q", tc.n, got.tapName, tc.tap)
		}
		if got.mask != "255.255.255.252" {
			t.Errorf("n=%d mask = %q, want 255.255.255.252", tc.n, got.mask)
		}
	}
}
```

- [ ] **Step 3: Run the tests — expect FAIL then PASS**

Run: `cd go-sdk && go test -run TestDeriveVMNet -count=1 .`
Expected: PASS. (If the package doesn't compile because `startPasst`/`passtArgs` are still referenced from `vmm.go`, that is expected until Task 6 — to verify Task 1 in isolation, the file rewrite above already removed those; the package will not compile until `vmm.go` stops calling them. Proceed to Task 2–5 which only touch `network.go`/`network_test.go`, then Task 6 fixes `vmm.go`. The compile is green again at the end of Task 6.)

> **Note for the executor:** Tasks 1–5 rewrite `network.go`. The package will **not compile** until Task 6 updates `vmm.go` (it still calls the deleted `startPasst`/`killPID`). Run `go vet`/`go build` only after Task 6. Within Tasks 1–5, verify each pure function by `go test -run <Name>` once Task 6 lands, or temporarily with a throwaway `_test` build. Each task below states its own post-Task-6 verification command.

- [ ] **Step 4: Stage (do not commit)**

```bash
git add go-sdk/network.go go-sdk/network_test.go
```

---

## Task 2: TAP index allocator (free-list)

**Files:**
- Modify: `go-sdk/network.go`
- Test: `go-sdk/network_test.go`

- [ ] **Step 1: Add the allocator to `network.go`** (append after `deriveVMNet`)

```go
// tapAllocator hands out TAP indices and reuses freed ones (LIFO). Unlike the
// monotonic allocateCID counter, freed indices are recycled so long-running
// managers do not exhaust the space.
type tapAllocator struct {
	mu   sync.Mutex
	next int
	free []int
	max  int
}

func newTapAllocator(max int) *tapAllocator {
	if max <= 0 {
		max = maxTapIndex
	}
	return &tapAllocator{max: max}
}

// alloc returns the next free index, reusing released ones first.
func (a *tapAllocator) alloc() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.free); n > 0 {
		idx := a.free[n-1]
		a.free = a.free[:n-1]
		return idx, nil
	}
	if a.next >= a.max {
		return 0, fmt.Errorf("tap allocator exhausted (max %d indices)", a.max)
	}
	idx := a.next
	a.next++
	return idx, nil
}

// release returns an index to the free list for reuse.
func (a *tapAllocator) release(idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.free = append(a.free, idx)
}
```

- [ ] **Step 2: Add allocator tests to `network_test.go`**

```go
func TestTapAllocatorSequential(t *testing.T) {
	a := newTapAllocator(0)
	for want := 0; want < 5; want++ {
		got, err := a.alloc()
		if err != nil {
			t.Fatalf("alloc: %v", err)
		}
		if got != want {
			t.Fatalf("alloc = %d, want %d", got, want)
		}
	}
}

func TestTapAllocatorReuse(t *testing.T) {
	a := newTapAllocator(0)
	_, _ = a.alloc() // 0
	one, _ := a.alloc()
	_, _ = a.alloc() // 2
	a.release(one)
	got, err := a.alloc()
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if got != one {
		t.Fatalf("alloc after release = %d, want reused %d", got, one)
	}
}

func TestTapAllocatorExhaustion(t *testing.T) {
	a := newTapAllocator(2)
	if _, err := a.alloc(); err != nil {
		t.Fatalf("alloc 0: %v", err)
	}
	if _, err := a.alloc(); err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	if _, err := a.alloc(); err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
}
```

- [ ] **Step 3: Verify (after Task 6 compiles)**

Run: `cd go-sdk && go test -run TestTapAllocator -count=1 .`
Expected: PASS (3 tests).

- [ ] **Step 4: Stage**

```bash
git add go-sdk/network.go go-sdk/network_test.go
```

---

## Task 3: `ip`/`nft`/parse builders (pure)

**Files:**
- Modify: `go-sdk/network.go`
- Test: `go-sdk/network_test.go`

- [ ] **Step 1: Add pure builders to `network.go`** (append the functions below; imports are fixed in Step 2)

```go
// --- pure command/ruleset builders (unit-tested; no exec) ---

// tapAddArgs / tapAddrArgs / tapUpArgs / tapDelArgs build `ip` argument lists.
func tapAddArgs(name string) []string  { return []string{"tuntap", "add", name, "mode", "tap"} }
func tapUpArgs(name string) []string   { return []string{"link", "set", name, "up"} }
func tapDelArgs(name string) []string  { return []string{"link", "del", name} }
func tapAddrArgs(name, hostIP string) []string {
	return []string{"addr", "add", hostIP + "/30", "dev", name}
}

// dummyAddArgs / gwAddrArgs build `ip` args for the Gateway dummy interface.
func dummyAddArgs(iface string) []string { return []string{"link", "add", iface, "type", "dummy"} }
func gwAddrArgs(iface, ip string) []string {
	return []string{"addr", "add", ip + "/32", "dev", iface}
}

// nftRuleset returns an idempotent nft script for the ix-nat table: create the
// table (no-op if present), flush it, then re-add the masquerade + forward
// rules. Loaded via `nft -f -`.
func nftRuleset(cidr, egressIface string) string {
	return fmt.Sprintf(`add table ip ix-nat
flush table ip ix-nat
add chain ip ix-nat postrouting { type nat hook postrouting priority 100 ; }
add rule ip ix-nat postrouting ip saddr %s oifname "%s" masquerade
add chain ip ix-nat forward { type filter hook forward priority 0 ; }
add rule ip ix-nat forward iifname "ixtap*" accept
add rule ip ix-nat forward oifname "ixtap*" accept
`, cidr, egressIface)
}

// parseEgressInterface extracts the first `dev <iface>` from `ip route show
// default` output.
func parseEgressInterface(routeOutput string) (string, error) {
	fields := strings.Fields(routeOutput)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("no default-route interface in %q", routeOutput)
}

// parseGatewayIP returns the host portion of a host:port listen address, e.g.
// "169.254.0.1:9100" -> "169.254.0.1".
func parseGatewayIP(listenAddr string) (string, error) {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse gateway addr %q: %w", listenAddr, err)
	}
	if host == "" {
		return "", fmt.Errorf("gateway addr %q has no host", listenAddr)
	}
	return host, nil
}
```

- [ ] **Step 2: Fix imports in `network.go`**

Update the import block at the top of `network.go` to the full set (some are first used by the Task 4 exec wrappers; the package does not compile until Task 6 regardless, so temporarily-unused imports here are moot):

```go
import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)
```

(`context`, `os/exec` are used by the exec wrappers in Task 4; `net`, `strings` by the parsers above.)

- [ ] **Step 3: Add builder tests to `network_test.go`**

```go
func TestTapArgs(t *testing.T) {
	if got := tapAddArgs("ixtap7"); strings.Join(got, " ") != "tuntap add ixtap7 mode tap" {
		t.Errorf("tapAddArgs = %v", got)
	}
	if got := tapAddrArgs("ixtap7", "172.16.0.29"); strings.Join(got, " ") != "addr add 172.16.0.29/30 dev ixtap7" {
		t.Errorf("tapAddrArgs = %v", got)
	}
	if got := tapUpArgs("ixtap7"); strings.Join(got, " ") != "link set ixtap7 up" {
		t.Errorf("tapUpArgs = %v", got)
	}
	if got := tapDelArgs("ixtap7"); strings.Join(got, " ") != "link del ixtap7" {
		t.Errorf("tapDelArgs = %v", got)
	}
}

func TestGatewayArgs(t *testing.T) {
	if got := dummyAddArgs("ixgw0"); strings.Join(got, " ") != "link add ixgw0 type dummy" {
		t.Errorf("dummyAddArgs = %v", got)
	}
	if got := gwAddrArgs("ixgw0", "169.254.0.1"); strings.Join(got, " ") != "addr add 169.254.0.1/32 dev ixgw0" {
		t.Errorf("gwAddrArgs = %v", got)
	}
}

func TestNftRuleset(t *testing.T) {
	rs := nftRuleset("172.16.0.0/16", "enp6s0")
	for _, want := range []string{
		"add table ip ix-nat",
		"flush table ip ix-nat",
		`ip saddr 172.16.0.0/16 oifname "enp6s0" masquerade`,
		`iifname "ixtap*" accept`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("nftRuleset missing %q\n%s", want, rs)
		}
	}
}

func TestParseEgressInterface(t *testing.T) {
	out := "default via 192.168.1.1 dev enp6s0 proto dhcp src 192.168.1.50 metric 100"
	got, err := parseEgressInterface(out)
	if err != nil || got != "enp6s0" {
		t.Fatalf("parseEgressInterface = %q, %v; want enp6s0", got, err)
	}
	if _, err := parseEgressInterface("blackhole default"); err == nil {
		t.Error("expected error when no dev present")
	}
}

func TestParseGatewayIP(t *testing.T) {
	got, err := parseGatewayIP("169.254.0.1:9100")
	if err != nil || got != "169.254.0.1" {
		t.Fatalf("parseGatewayIP = %q, %v; want 169.254.0.1", got, err)
	}
	if _, err := parseGatewayIP("garbage"); err == nil {
		t.Error("expected error on malformed addr")
	}
}
```

- [ ] **Step 4: Verify (after Task 6 compiles)**

Run: `cd go-sdk && go test -run 'TestTapArgs|TestGatewayArgs|TestNftRuleset|TestParse' -count=1 .`
Expected: PASS (5 tests).

- [ ] **Step 5: Stage**

```bash
git add go-sdk/network.go go-sdk/network_test.go
```

---

## Task 4: Exec wrappers (TAP / NAT / Gateway)

**Files:**
- Modify: `go-sdk/network.go`
- Test: verified by Task 10 boot-test (needs root/`CAP_NET_ADMIN`; no unit test).

- [ ] **Step 1: Append the exec wrappers to `network.go`**

```go
// --- exec wrappers (run real `ip`/`nft`/`sysctl`; require CAP_NET_ADMIN) ---

// runIP runs `ip <args...>` and returns combined output on error.
func runIP(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %v: %w: %s", args, err, out)
	}
	return nil
}

// runIPIgnoreExists is like runIP but treats "File exists" as success, for
// idempotent create operations.
func runIPIgnoreExists(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return fmt.Errorf("ip %v: %w: %s", args, err, out)
	}
	return nil
}

// setupTap derives the addressing for index n, removes any stale same-name tap,
// then creates + addresses + brings up the TAP. Returns the vmNet on success.
func setupTap(ctx context.Context, n int) (vmNet, error) {
	vn := deriveVMNet(n)
	// Best-effort delete a leaked same-name tap so re-create never fails.
	_ = runIP(ctx, tapDelArgs(vn.tapName)...)
	if err := runIP(ctx, tapAddArgs(vn.tapName)...); err != nil {
		return vmNet{}, fmt.Errorf("create tap: %w", err)
	}
	if err := runIP(ctx, tapAddrArgs(vn.tapName, vn.hostIP)...); err != nil {
		_ = runIP(ctx, tapDelArgs(vn.tapName)...)
		return vmNet{}, fmt.Errorf("addr tap: %w", err)
	}
	if err := runIP(ctx, tapUpArgs(vn.tapName)...); err != nil {
		_ = runIP(ctx, tapDelArgs(vn.tapName)...)
		return vmNet{}, fmt.Errorf("up tap: %w", err)
	}
	return vn, nil
}

// teardownTap deletes the TAP. Best-effort (callers log; do not block).
func teardownTap(ctx context.Context, vn vmNet) error {
	return runIP(ctx, tapDelArgs(vn.tapName)...)
}

// ensureHostNAT enables IP forwarding and installs the idempotent ix-nat table.
func ensureHostNAT(ctx context.Context, cidr, egressIface string) error {
	if out, err := exec.CommandContext(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1").CombinedOutput(); err != nil {
		return fmt.Errorf("enable ip_forward: %w: %s", err, out)
	}
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(nftRuleset(cidr, egressIface))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply nft ix-nat: %w: %s", err, out)
	}
	return nil
}

// detectEgressInterface returns the interface of the default route.
func detectEgressInterface(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w: %s", err, out)
	}
	return parseEgressInterface(string(out))
}

// gatewayDummyIface is the host dummy interface that owns the Gateway IP.
const gatewayDummyIface = "ixgw0"

// ensureGatewayAddr pins gatewayIP on a host dummy interface so the browser
// Gateway can bind it and guests can route to it. Idempotent.
func ensureGatewayAddr(ctx context.Context, gatewayIP string) error {
	if err := runIPIgnoreExists(ctx, dummyAddArgs(gatewayDummyIface)...); err != nil {
		return fmt.Errorf("create %s: %w", gatewayDummyIface, err)
	}
	if err := runIPIgnoreExists(ctx, gwAddrArgs(gatewayDummyIface, gatewayIP)...); err != nil {
		return fmt.Errorf("addr %s: %w", gatewayDummyIface, err)
	}
	if err := runIP(ctx, tapUpArgs(gatewayDummyIface)...); err != nil {
		return fmt.Errorf("up %s: %w", gatewayDummyIface, err)
	}
	return nil
}

// teardownGatewayAddr removes the Gateway dummy interface. Best-effort.
func teardownGatewayAddr(ctx context.Context) error {
	return runIP(ctx, tapDelArgs(gatewayDummyIface)...)
}
```

- [ ] **Step 2: Verify it compiles after Task 6**

Run (after Task 6): `cd go-sdk && go build ./...`
Expected: builds clean. No unit test here — behavior is covered by Task 10.

- [ ] **Step 3: Stage**

```bash
git add go-sdk/network.go
```

---

## Task 5: `buildKernelBootArgs` gains `ip=`

**Files:**
- Modify: `go-sdk/vmm.go:75-98`
- Test: `go-sdk/vmm_test.go`

- [ ] **Step 1: Add the failing test** to `go-sdk/vmm_test.go`

```go
func TestBuildKernelBootArgsWithNet(t *testing.T) {
	vn := deriveVMNet(0) // host 172.16.0.1, guest 172.16.0.2
	args := buildKernelBootArgs(nil, &vn)
	want := "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off:8.8.8.8"
	if !strings.Contains(args, want) {
		t.Errorf("boot args missing %q\n%s", want, args)
	}
}

func TestBuildKernelBootArgsNoNet(t *testing.T) {
	args := buildKernelBootArgs(nil, nil)
	if strings.Contains(args, "ip=") {
		t.Errorf("boot args should have no ip= when net is nil: %s", args)
	}
}
```

- [ ] **Step 2: Update the existing 3 call sites** in `go-sdk/vmm_test.go`

Change `buildKernelBootArgs(env)` → `buildKernelBootArgs(env, nil)` at lines ~48 and ~171, and `buildKernelBootArgs(nil)` → `buildKernelBootArgs(nil, nil)` at line ~139. (Existing assertions still hold — `nil` net adds no `ip=`.)

- [ ] **Step 3: Run — expect compile FAIL** (signature mismatch)

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs -count=1 .`
Expected: build error `too many arguments` / `not enough arguments` until Step 4.

- [ ] **Step 4: Change the signature + body** in `go-sdk/vmm.go`

Replace the `buildKernelBootArgs` function (lines ~75-98) with:

```go
// buildKernelBootArgs constructs the kernel command line for the Firecracker VM.
// Environment variables from envSlice are injected as ix.env.KEY=VALUE entries
// so the ix-init script can read them from /proc/cmdline. When net is non-nil,
// an ip= argument autoconfigures eth0 (IP, gateway, /30 mask, DNS) at boot.
func buildKernelBootArgs(envSlice []string, net *vmNet) string {
	parts := []string{
		"console=ttyS0",
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
		"rw",
		"init=/sbin/ix-init",
	}
	if net != nil {
		// ip=<client>:<server>:<gw>:<mask>:<host>:<dev>:<autoconf>:<dns>
		parts = append(parts, fmt.Sprintf(
			"ip=%s::%s:%s::eth0:off:8.8.8.8", net.guestIP, net.hostIP, net.mask))
	}
	for _, e := range envSlice {
		parts = append(parts, "ix.env."+e)
	}
	return strings.Join(parts, " ")
}
```

- [ ] **Step 5: Run — expect PASS**

Run: `cd go-sdk && go test -run TestBuildKernelBootArgs -count=1 .`
Expected: PASS (all `TestBuildKernelBootArgs*` tests). (Other package files may still fail to compile until Task 6 — run the focused test; full build is green after Task 6.)

- [ ] **Step 6: Stage**

```bash
git add go-sdk/vmm.go go-sdk/vmm_test.go
```

---

## Task 6: Wire the TAP into `startVMCold` / `cleanup` / `VMMHandle`

**Files:**
- Modify: `go-sdk/vmm.go` (`VMMHandle`, `firecrackerBackend`, `startVMCold`, `cleanup`; remove `killPID`)
- Modify: `go-sdk/snapshot.go:248-255`
- Modify: `go-sdk/vmm_test.go` (`TestVMMHandleFields`)

- [ ] **Step 1: Replace `PasstPID` with `Net` in `VMMHandle`** (`go-sdk/vmm.go:20-28`)

```go
// VMMHandle holds all state for a running VM managed by the firecrackerBackend.
type VMMHandle struct {
	Process   *os.Process
	SocketDir string
	VsockPath string // path to vsock UDS file (used by Firecracker vsock device)
	APISocket string // Firecracker API socket path
	CID       uint32 // AF_VSOCK context ID assigned to this VM
	Net       *vmNet // per-VM TAP networking; nil = vsock-only (snapshot/disabled)
}
```

- [ ] **Step 2: Add networking fields to `firecrackerBackend`** (`go-sdk/vmm.go:100-107`)

```go
// firecrackerBackend implements VM lifecycle management using Firecracker.
type firecrackerBackend struct {
	fcBinary    string
	kernelPath  string
	rootfsImage string
	logger      *slog.Logger
	snapshot    *SnapshotManager // optional; when set, startVM uses snapshot restore

	tapAlloc    *tapAllocator // TAP index allocator (nil only in tests that never cold-boot)
	egressIface string        // host uplink for MASQUERADE (resolved at manager start)
	disableNet  bool          // skip TAP setup entirely (vsock-only VM)
}
```

- [ ] **Step 3: Rewrite the networking portion of `startVMCold`** (`go-sdk/vmm.go:135-256`)

Replace the block from the `// Start passt...` comment through the `return &VMMHandle{...}` with the version below. Key changes: TAP setup replaces passt; a `cleanupOnErr` closure replaces the repeated `killPID(passtPID)` lines; `PUT /network-interfaces` is added; boot args get the net; the handle stores `Net`.

```go
func (fb *firecrackerBackend) startVMCold(ctx context.Context, sandboxID string, vcpus int, memMB int64, rootfsImage string, envSlice []string, extraDrives []driveSpec) (*VMMHandle, error) {
	cid := allocateCID()

	socketDir := filepath.Join(os.TempDir(), "ix-"+sandboxID)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	apiSocket := filepath.Join(socketDir, "fc.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.uds")

	// Per-VM TAP networking (skipped when disabled). On any later error we must
	// tear the tap down and release its index, so track them for the closure.
	var vn *vmNet
	if !fb.disableNet {
		idx, err := fb.tapAlloc.alloc()
		if err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("allocate tap index: %w", err)
		}
		net, err := setupTap(ctx, idx)
		if err != nil {
			fb.tapAlloc.release(idx)
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("setup tap: %w", err)
		}
		vn = &net
	}

	// cleanupOnErr undoes everything created so far. Call on every error path
	// after this point.
	cleanupOnErr := func(proc *os.Process) {
		if proc != nil {
			_ = proc.Kill()
		}
		if vn != nil {
			_ = teardownTap(ctx, *vn)
			fb.tapAlloc.release(vn.idx)
		}
		_ = os.RemoveAll(socketDir)
	}

	cmd := exec.CommandContext(ctx, fb.fcBinary, "--api-sock", apiSocket)
	cmd.Dir = socketDir
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cleanupOnErr(nil)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	if err := waitForFile(apiSocket, 5*time.Second); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("firecracker API socket not ready: %w", err)
	}

	apiClient := fcAPIClient(apiSocket)
	bootArgs := buildKernelBootArgs(envSlice, vn)

	if err := fcPut(ctx, apiClient, "/boot-source", map[string]any{
		"kernel_image_path": fb.kernelPath,
		"boot_args":         bootArgs,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set boot source: %w", err)
	}

	if err := fcPut(ctx, apiClient, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsImage,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set rootfs drive: %w", err)
	}

	for _, d := range extraDrives {
		id, _ := d["drive_id"].(string)
		if err := fcPut(ctx, apiClient, "/drives/"+id, d); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set drive %s: %w", id, err)
		}
	}

	// Network interface (TAP). Must precede InstanceStart. Skipped when disabled.
	if vn != nil {
		if err := fcPut(ctx, apiClient, "/network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     vn.guestMAC,
			"host_dev_name": vn.tapName,
		}); err != nil {
			cleanupOnErr(cmd.Process)
			return nil, fmt.Errorf("set network interface: %w", err)
		}
	}

	if err := fcPut(ctx, apiClient, "/machine-config", map[string]any{
		"vcpu_count":   vcpus,
		"mem_size_mib": memMB,
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set machine config: %w", err)
	}

	if err := fcPut(ctx, apiClient, "/vsock", map[string]any{
		"guest_cid": cid,
		"uds_path":  "vsock.uds",
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("set vsock: %w", err)
	}

	if err := fcPut(ctx, apiClient, "/actions", map[string]any{
		"action_type": "InstanceStart",
	}); err != nil {
		cleanupOnErr(cmd.Process)
		return nil, fmt.Errorf("start VM instance: %w", err)
	}

	return &VMMHandle{
		Process:   cmd.Process,
		SocketDir: socketDir,
		VsockPath: vsockUDS,
		APISocket: apiSocket,
		CID:       cid,
		Net:       vn,
	}, nil
}
```

Also update the doc comment above `startVMCold` (lines ~123-134): replace the "Start passt for user-mode networking" bullet with "Create a per-VM TAP and wire it via PUT /network-interfaces", and the closing sentence "the process and passt are killed" with "the process and TAP are torn down".

- [ ] **Step 4: Update `cleanup`** (`go-sdk/vmm.go:258-272`)

```go
// cleanup kills the Firecracker process, tears down the VM's TAP (if any), and
// removes the socket directory.
func (fb *firecrackerBackend) cleanup(handle *VMMHandle) {
	if handle == nil {
		return
	}
	if handle.Process != nil {
		_ = handle.Process.Kill()
		_, _ = handle.Process.Wait()
	}
	if handle.Net != nil {
		if err := teardownTap(context.Background(), *handle.Net); err != nil && fb.logger != nil {
			fb.logger.Warn("teardown tap", "tap", handle.Net.tapName, "error", err)
		}
		if fb.tapAlloc != nil {
			fb.tapAlloc.release(handle.Net.idx)
		}
	}
	if handle.SocketDir != "" {
		_ = os.RemoveAll(handle.SocketDir)
	}
}
```

- [ ] **Step 5: Remove the now-unused `killPID`** (`go-sdk/vmm.go:311-321`)

Delete the entire `killPID` function. (It was only used for `passtPID`.)

- [ ] **Step 6: Fix the `VMMHandle` literal in `snapshot.go`** (`go-sdk/snapshot.go:248-255`)

Remove the `PasstPID: 0,` line (leave `Net` unset → nil = vsock-only). Update the comment at `snapshot.go:168-169` from "CID and PasstPID are 0" to "CID is 0 and Net is nil — snapshot-restored VMs use neither a vsock CID nor a host TAP (networking is baked into the snapshot)."

- [ ] **Step 7: Fix `TestVMMHandleFields`** in `go-sdk/vmm_test.go` (no change needed unless it references `PasstPID`)

The current test (lines ~73-87) does not set `PasstPID`, so it still compiles. Verify by reading; if any test in the package references `PasstPID`, replace with `Net`. (Grep: `grep -rn PasstPID go-sdk` must return **zero** matches after this task.)

- [ ] **Step 8: Compile + run the unit suite (TAP allocator etc. now reachable)**

Run:
```bash
cd go-sdk && go build ./... && go test -count=1 .
```
Expected: build clean; all non-integration tests PASS. `grep -rn 'PasstPID\|startPasst\|passtArgs\|killPID' go-sdk` returns nothing.

- [ ] **Step 9: Stage**

```bash
git add go-sdk/vmm.go go-sdk/snapshot.go go-sdk/vmm_test.go
```

---

## Task 7: ManagerConfig fields + manager wiring

**Files:**
- Modify: `go-sdk/manager.go` (`ManagerConfig` ~29-54, `applyDefaults` ~57+, `NewManager` backend construction ~172-202, `Close` ~428)
- Test: `go-sdk/manager_test.go`

- [ ] **Step 1: Add fields to `ManagerConfig`** (after `GatewayToken`, `go-sdk/manager.go:53`)

```go
	// Networking (per-VM TAP + host NAT).
	EgressInterface   string // host uplink for NAT MASQUERADE; auto-detected if empty
	NetworkCIDR       string // base address space; default "172.16.0.0/16"
	DisableNetworking bool   // skip TAP setup (vsock-only VMs)
```

- [ ] **Step 2: Default `NetworkCIDR` in `applyDefaults`** (inside `func (c *ManagerConfig) applyDefaults()`)

```go
	if c.NetworkCIDR == "" {
		c.NetworkCIDR = "172.16.0.0/16"
	}
```

- [ ] **Step 3: Add the failing defaults test** to `go-sdk/manager_test.go`

```go
func TestApplyDefaultsNetworkCIDR(t *testing.T) {
	c := ManagerConfig{}
	c.applyDefaults()
	if c.NetworkCIDR != "172.16.0.0/16" {
		t.Errorf("NetworkCIDR default = %q, want 172.16.0.0/16", c.NetworkCIDR)
	}
}
```

- [ ] **Step 4: Run — expect FAIL then PASS**

Run: `cd go-sdk && go test -run TestApplyDefaultsNetworkCIDR -count=1 .`
Expected: PASS once Steps 1–2 are in.

- [ ] **Step 5: Wire the backend + host setup in `NewManager`**

Find the `firecrackerBackend` literal (`go-sdk/manager.go:174-179`) and extend it, then add host-NAT/gateway setup. Replace the backend literal with:

```go
		vmm: &firecrackerBackend{
			fcBinary:    cfg.FCBinary,
			kernelPath:  cfg.KernelPath,
			rootfsImage: cfg.RootfsImage,
			logger:      cfg.Logger,
			tapAlloc:    newTapAllocator(0),
			disableNet:  cfg.DisableNetworking,
		},
```

Then, immediately **after** `m.accepting.Store(true)` (line ~187) and **before** the `if cfg.BrowserMode == "remote"` block (line ~189), insert:

```go
	// Reject unsupported custom CIDR up front (derivation assumes the /16 base).
	if cfg.NetworkCIDR != "172.16.0.0/16" {
		cancel()
		return nil, fmt.Errorf("custom NetworkCIDR %q not yet supported; leave empty for the default", cfg.NetworkCIDR)
	}

	if !cfg.DisableNetworking {
		egress := cfg.EgressInterface
		if egress == "" {
			detected, err := detectEgressInterface(mCtx)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("detect egress interface (set EgressInterface to override): %w", err)
			}
			egress = detected
		}
		m.vmm.egressIface = egress
		if err := ensureHostNAT(mCtx, cfg.NetworkCIDR, egress); err != nil {
			cancel()
			return nil, fmt.Errorf("ensure host NAT: %w", err)
		}
		if cfg.BrowserMode == "remote" {
			gwIP, err := parseGatewayIP(cfg.GatewayListenAddr)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("parse gateway addr: %w", err)
			}
			if err := ensureGatewayAddr(mCtx, gwIP); err != nil {
				cancel()
				return nil, fmt.Errorf("ensure gateway addr: %w", err)
			}
		}
	}
```

(`cfg.GatewayListenAddr` is already defaulted by `applyDefaults` when `BrowserMode=="remote"` — confirm at `manager.go:91-92`. This block runs **before** `startBrowserTier`, so the dummy interface owns `169.254.0.1` before the Gateway binds it.)

- [ ] **Step 6: Tear down the gateway interface in `Close`** (`go-sdk/manager.go:428`)

Immediately after `m.tier.stop(m.vmm)`, add:

```go
	if m.cfg.BrowserMode == "remote" && !m.cfg.DisableNetworking {
		if err := teardownGatewayAddr(context.Background()); err != nil {
			m.logger.Warn("teardown gateway addr", "error", err)
		}
	}
```

- [ ] **Step 7: Build + full unit suite**

Run: `cd go-sdk && go build ./... && go test -count=1 .`
Expected: build clean; all non-integration tests PASS.

- [ ] **Step 8: Stage**

```bash
git add go-sdk/manager.go go-sdk/manager_test.go
```

---

## Task 8: Guest DNS belt-and-suspenders

The kernel `ip=` arg seeds DNS into `/proc/net/pnp`, but Chrome/pinchtab resolve via `/etc/resolv.conf`. `ix-init` already writes it; `browser-vm-init.sh` brings up `lo` but does not, so the browser tier needs an explicit write.

**Files:**
- Modify: `daemon/cmd/browser-vm-init.sh`

- [ ] **Step 1: Confirm `ix-init` already writes `/etc/resolv.conf`**

Run: `grep -rn 'resolv.conf' daemon/`
Expected: a `nameserver` write exists in the base-tier `ix-init` (per spec). If present, no change there.

- [ ] **Step 2: Add the DNS write to `browser-vm-init.sh`**

Immediately after the loopback block (the `ip link set lo up ...` line, ~line 46), insert:

```sh
# Seed DNS for Chrome. The kernel ip= arg populates /proc/net/pnp, but Chrome
# resolves via /etc/resolv.conf, so write it explicitly (belt-and-suspenders).
echo "nameserver 8.8.8.8" > /etc/resolv.conf 2>/dev/null || true
```

- [ ] **Step 3: Lint the script**

Run: `sh -n daemon/cmd/browser-vm-init.sh`
Expected: no syntax errors. (Verified end-to-end by the Task 10 browser navigate.)

- [ ] **Step 4: Stage**

```bash
git add daemon/cmd/browser-vm-init.sh
```

---

## Task 9: Full regression (non-integration)

**Files:** none (verification only)

- [ ] **Step 1: Confirm no passt remnants anywhere**

Run: `cd go-sdk && grep -rn 'passt\|PasstPID\|killPID' . || echo CLEAN`
Expected: `CLEAN` (or only matches inside this plan/spec docs, not `.go` files).

- [ ] **Step 2: Run the whole non-integration suite + vet**

Run:
```bash
cd go-sdk && go vet ./... && go test -count=1 ./...
```
Expected: PASS, no vet warnings.

- [ ] **Step 3: Stage (nothing expected, but capture any incidental fmt)**

```bash
git add -A go-sdk
```

---

## Task 10: Boot-test (integration — real Firecracker, needs `CAP_NET_ADMIN`)

**Files:**
- Modify/Create: `go-sdk/integration_test.go` (or the existing FC boot-test harness)
- Test tag: `//go:build integration`

> This task needs a host with KVM, the kernel/rootfs images, and the manager binary granted `CAP_NET_ADMIN` (`sudo setcap cap_net_admin+ep $(go test -c -tags=integration -o /tmp/ix.test . && echo /tmp/ix.test)` or run the test under `sudo`). If the harness already has a cold-boot integration test, extend it rather than adding a new one.

- [ ] **Step 1: Add an end-to-end networking integration test**

```go
//go:build integration

package ix

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestColdBootNetworking boots a real cold VM, confirms eth0 came up from ip=,
// reaches the internet, and that the host TAP is removed on cleanup.
func TestColdBootNetworking(t *testing.T) {
	fb := newIntegrationBackend(t) // existing helper: wires fcBinary/kernel/rootfs
	fb.tapAlloc = newTapAllocator(0)
	if egress, err := detectEgressInterface(context.Background()); err == nil {
		fb.egressIface = egress
		if err := ensureHostNAT(context.Background(), "172.16.0.0/16", egress); err != nil {
			t.Fatalf("ensureHostNAT: %v", err)
		}
	} else {
		t.Fatalf("detectEgressInterface: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := fb.startVMCold(ctx, "nettest", 1, 256, fb.rootfsImage, nil, nil)
	if err != nil {
		t.Fatalf("startVMCold: %v", err)
	}
	tapName := h.Net.tapName
	defer fb.cleanup(h)

	client := newGuestClient(t, h) // existing helper: daemon HTTP over vsock

	// eth0 exists and has the derived guest IP.
	if out := execInGuest(t, client, "ip -o -4 addr show eth0"); !strings.Contains(out, h.Net.guestIP) {
		t.Errorf("eth0 missing guest IP %s: %s", h.Net.guestIP, out)
	}
	// Reaches the internet (egress firewall permitting).
	if out := execInGuest(t, client, "curl -sS -o /dev/null -w '%{http_code}' https://example.com"); !strings.HasPrefix(out, "2") && !strings.HasPrefix(out, "3") {
		t.Errorf("guest could not reach example.com: %q", out)
	}

	// Teardown removes the host tap.
	fb.cleanup(h)
	if out := hostCmd(t, "ip", "link", "show", tapName); !strings.Contains(out, "does not exist") {
		t.Errorf("tap %s still present after cleanup: %s", tapName, out)
	}
}
```

> `newIntegrationBackend`, `newGuestClient`, `execInGuest`, `hostCmd` are harness helpers — reuse the existing ones in the integration suite. If they don't exist, implement thin versions: `execInGuest` POSTs to the daemon `/v1` shell endpoint over the vsock transport (`vsockTransport(h.VsockPath)`); `hostCmd` runs a local command and returns combined output.

- [ ] **Step 2: Add the browser-tier reachability check**

Extend the existing browser-tier integration test (`go-sdk/browser_tier_integration_test.go`) so that, with `BrowserMode=remote`, after `NewManager` + creating a per-chat sandbox, a `BrowserNavigate` to `https://example.com` returns success — exercising guest → `169.254.0.1:9100` Gateway → vsock → browser VM → internet end-to-end.

- [ ] **Step 3: Run the integration tests**

Run (on a capable host, serial):
```bash
cd go-sdk && sudo -E go test -tags=integration -run 'TestColdBootNetworking|TestBrowserTier' -count=1 -v .
```
Expected: PASS — eth0 up from `ip=`, internet reachable, browser navigate succeeds, tap removed on cleanup.

- [ ] **Step 4: Stage**

```bash
git add go-sdk/integration_test.go go-sdk/browser_tier_integration_test.go
```

---

## Done criteria

- `go vet ./...` clean; `go test -count=1 ./...` green (non-integration).
- No `passt`/`PasstPID`/`killPID` references remain in `*.go`.
- Integration boot-test: guest reaches the internet and the browser Gateway; tap torn down on cleanup.
- Spec sections all covered: addressing (Task 1), allocator (Task 2), builders (Task 3), exec wrappers incl. `ensureGatewayAddr` (Task 4), `ip=` boot arg (Task 5), `PUT /network-interfaces` + teardown (Task 6), config + host setup ordering (Task 7), guest DNS (Task 8), regression + boot-test (Tasks 9–10).
