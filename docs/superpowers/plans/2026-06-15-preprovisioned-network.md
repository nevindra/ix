# Preprovisioned Host Network Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `IXManager` run fully unprivileged after a one-time root `ix-host-setup`, by allocating per-VM TAP devices from a pre-provisioned pool described in a manifest instead of shelling out to `ip`/`nft`/`sysctl` at runtime.

**Architecture:** Introduce a `netProvider` interface with two implementations — the existing create-on-demand path (`dynamicNet`, root mode) and a new `preconfiguredNet` that hands out pre-derived `vmNet` entries loaded from `/etc/ix/network.json` with zero privileged syscalls. A root setup script renders the same NAT ruleset and `/30` address plan the Go code already uses, creates owned persistent TAPs, and writes the manifest. The manager skips all privileged host-network setup when preconfigured mode is on.

**Tech Stack:** Go 1.x (go-sdk), bash, `iproute2` (`ip tuntap ... user`), `nftables`, `sysctl`, JSON manifest.

**Spec:** `docs/superpowers/specs/2026-06-11-preprovisioned-network-design.md`

---

## Background facts (verified against the codebase)

- TAP addressing is pure + deterministic: `deriveVMNet(n)` (`network.go:67`) maps index `n` to `ixtap{n}`, host IP `netBase+4n+1`, guest IP `+2`, MAC `06:00:<guestIP hex>`, mask `255.255.255.252`. `netBase = 0xAC100000` (172.16.0.0). This is the source of truth the setup script must mirror.
- Root-mode TAP lifecycle lives in `vmm.go:206-242` (`startVMCold`: `fb.tapAlloc.alloc()` → `setupTap` → store `vn`; `cleanupOnErr` tears down) and `vmm.go:379-383` (`Destroy`). The allocator field is `firecrackerBackend.tapAlloc` (`vmm.go:135`), set in `manager.go:224`.
- Privileged host setup runs in `NewManager` under `if !cfg.DisableNetworking` (`manager.go:246-272`): `ensureHostNAT` (sysctl + nft), `ensureForwardAccept` (iptables), and — only for `BrowserMode=="remote"` — `ensureGatewayAddr` (dummy iface `ixgw0`).
- `ensureHostNAT` (`network.go:239`) runs `sysctl -w net.ipv4.ip_forward=1` + `nft -f -` with `nftRuleset(cidr, egressIface)` (`network.go:157`). The setup script must emit byte-identical nft rules.
- `ManagerConfig` (`manager.go:29-70`) is plain explicit config; `applyDefaults` (`manager.go:73`) fills zero values. Custom CIDRs are rejected (`manager.go:211`) — only `172.16.0.0/16`.
- No existing test constructs `firecrackerBackend` with `tapAlloc` set (literals at `snapshot_test.go:18,94,132` and `vmm_test.go:217,223,300` omit it), so renaming/replacing that field is compile-safe for tests.

---

## File Structure

- `go-sdk/manager.go` — MODIFY: add `PreconfiguredNetwork`/`NetworkManifest` config fields, env defaults, preconfigured branch in `NewManager`, concurrency cap from pool size.
- `go-sdk/netmanifest.go` — CREATE: manifest JSON type, loader, `[]vmNet` builder, tap presence verification.
- `go-sdk/netprovider.go` — CREATE: `netProvider` interface, `dynamicNet`, `preconfiguredNet`, `ErrNetworkPoolExhausted`.
- `go-sdk/vmm.go` — MODIFY: replace `tapAlloc *tapAllocator` with `net netProvider`; route acquire/release through it.
- `go-sdk/netmanifest_test.go` — CREATE: manifest parse + verify tests.
- `go-sdk/netprovider_test.go` — CREATE: provider alloc/release/exhaustion tests.
- `go-sdk/scripts/ix-host-setup.sh` — CREATE: root, idempotent host prep + manifest writer.
- `go-sdk/preconfigured_network_integration_test.go` — CREATE: sudo-gated acceptance test (build tag `integration`).
- `docs/handbook/preprovisioned-network.md` — CREATE: operator doc.

---

## Task 1: netProvider abstraction + refactor root-mode TAP lifecycle

Pure refactor. Behavior unchanged; existing tests stay green. This isolates TAP acquire/release behind an interface so Task 3 can add the preconfigured implementation without touching `vmm.go` again.

**Files:**
- Create: `go-sdk/netprovider.go`
- Modify: `go-sdk/vmm.go:135` (field), `go-sdk/vmm.go:206-242` (setup + cleanup), `go-sdk/vmm.go:379-383` (Destroy), `go-sdk/manager.go:224` (construction)

- [ ] **Step 1: Create `go-sdk/netprovider.go` with the interface + dynamic impl**

```go
package ix

import (
	"context"
	"errors"
	"log/slog"
)

// ErrNetworkPoolExhausted is returned by a netProvider when no TAP slot is
// free. The manager surfaces it so an embedding service can queue or report it.
var ErrNetworkPoolExhausted = errors.New("ix: network pool exhausted")

// netProvider abstracts per-VM TAP acquisition. The dynamic implementation
// creates/destroys TAPs at runtime (needs CAP_NET_ADMIN); the preconfigured
// implementation hands out pre-provisioned TAPs from a manifest (no privilege).
type netProvider interface {
	// acquire returns the addressing for one VM's TAP, ready to attach to
	// Firecracker. Returns ErrNetworkPoolExhausted when none are available.
	acquire(ctx context.Context) (*vmNet, error)
	// release returns a TAP to the provider. Best-effort; never returns after
	// logging its own errors via the supplied logger (may be nil).
	release(vn *vmNet, logger *slog.Logger)
}

// dynamicNet creates and tears down TAPs on demand. This is the root-mode
// provider — identical behavior to the pre-refactor inline code.
type dynamicNet struct {
	alloc *tapAllocator
}

func newDynamicNet() *dynamicNet { return &dynamicNet{alloc: newTapAllocator(0)} }

func (d *dynamicNet) acquire(ctx context.Context) (*vmNet, error) {
	idx, err := d.alloc.alloc()
	if err != nil {
		return nil, err
	}
	vn, err := setupTap(ctx, idx)
	if err != nil {
		d.alloc.release(idx)
		return nil, err
	}
	return &vn, nil
}

func (d *dynamicNet) release(vn *vmNet, logger *slog.Logger) {
	if err := teardownTap(context.Background(), *vn); err != nil && logger != nil {
		logger.Warn("teardown tap", "tap", vn.tapName, "error", err)
	}
	d.alloc.release(vn.idx)
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd go-sdk && go build ./...`
Expected: no output (success).

- [ ] **Step 3: Swap the `firecrackerBackend` field**

In `go-sdk/vmm.go:135`, replace:

```go
	tapAlloc        *tapAllocator    // TAP index allocator (set by the manager; nil only in tests that never cold-boot)
```

with:

```go
	net             netProvider      // per-VM TAP provider (set by the manager; nil only in tests that never cold-boot)
```

- [ ] **Step 4: Route setup through the provider**

In `go-sdk/vmm.go`, replace the TAP setup block (currently `vmm.go:206-225`):

```go
	var vn *vmNet
	if !fb.disableNet && !forceNoNet {
		tTap := time.Now()
		if fb.tapAlloc == nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("networking enabled but tap allocator not configured")
		}
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
		tracePhase(fb.logger, "coldboot", "tap", tTap)
	}
```

with:

```go
	var vn *vmNet
	if !fb.disableNet && !forceNoNet {
		tTap := time.Now()
		if fb.net == nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("networking enabled but net provider not configured")
		}
		acquired, err := fb.net.acquire(ctx)
		if err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("acquire tap: %w", err)
		}
		vn = acquired
		tracePhase(fb.logger, "coldboot", "tap", tTap)
	}
```

- [ ] **Step 5: Route cleanup-on-error through the provider**

In `go-sdk/vmm.go`, replace the `vn != nil` block inside `cleanupOnErr` (currently `vmm.go:233-240`):

```go
		if vn != nil {
			if err := teardownTap(context.Background(), *vn); err != nil && fb.logger != nil {
				fb.logger.Warn("teardown tap (error path)", "tap", vn.tapName, "error", err)
			}
			if fb.tapAlloc != nil {
				fb.tapAlloc.release(vn.idx)
			}
		}
```

with:

```go
		if vn != nil && fb.net != nil {
			fb.net.release(vn, fb.logger)
		}
```

- [ ] **Step 6: Route Destroy through the provider**

In `go-sdk/vmm.go`, replace the teardown block in `Destroy` (currently `vmm.go:378-384`):

```go
		if err := teardownTap(context.Background(), *handle.Net); err != nil && fb.logger != nil {
			fb.logger.Warn("teardown tap", "tap", handle.Net.tapName, "error", err)
		}
		if fb.tapAlloc != nil {
			fb.tapAlloc.release(handle.Net.idx)
		}
```

with:

```go
		if fb.net != nil {
			fb.net.release(handle.Net, fb.logger)
		}
```

> Confirm with `grep -n "handle.Net" go-sdk/vmm.go` before editing; match the exact surrounding lines. The block guards on `handle.Net != nil` above it — leave that guard intact.

- [ ] **Step 7: Update manager construction**

In `go-sdk/manager.go:224`, replace:

```go
			tapAlloc:        newTapAllocator(0),
```

with:

```go
			net:             newDynamicNet(),
```

- [ ] **Step 8: Build + run the full unit suite**

Run: `cd go-sdk && go build ./... && go test ./... -count=1`
Expected: PASS (no behavior change). If `teardownTap`/`setupTap` are now reported unused, they are still referenced by `dynamicNet` — they are not unused; do not delete them.

- [ ] **Step 9: Commit**

```bash
git add go-sdk/netprovider.go go-sdk/vmm.go go-sdk/manager.go
git commit -m "refactor(go-sdk): abstract per-VM TAP lifecycle behind netProvider"
```

---

## Task 2: Network manifest type + loader

**Files:**
- Create: `go-sdk/netmanifest.go`
- Create: `go-sdk/netmanifest_test.go`

- [ ] **Step 1: Write failing tests for the loader**

Create `go-sdk/netmanifest_test.go`:

```go
package ix

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleManifest = `{
  "version": 1,
  "cidr": "172.16.0.0/16",
  "egress_iface": "",
  "owner": "ixsvc",
  "gateway_ip": "169.254.0.1",
  "taps": [
    {"idx":0,"name":"ixtap0","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","guest_mac":"06:00:AC:10:00:02","mask":"255.255.255.252"},
    {"idx":1,"name":"ixtap1","host_ip":"172.16.0.5","guest_ip":"172.16.0.6","guest_mac":"06:00:AC:10:00:06","mask":"255.255.255.252"}
  ]
}`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "network.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadManifestParsesTaps(t *testing.T) {
	m, err := loadNetworkManifest(writeTemp(t, sampleManifest))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.CIDR != "172.16.0.0/16" || m.Owner != "ixsvc" || m.GatewayIP != "169.254.0.1" {
		t.Fatalf("header fields wrong: %+v", m)
	}
	if len(m.Taps) != 2 {
		t.Fatalf("want 2 taps, got %d", len(m.Taps))
	}
	if m.Taps[1].Name != "ixtap1" || m.Taps[1].GuestIP != "172.16.0.6" {
		t.Fatalf("tap[1] wrong: %+v", m.Taps[1])
	}
}

func TestManifestEntriesMatchDeriveVMNet(t *testing.T) {
	m, err := loadNetworkManifest(writeTemp(t, sampleManifest))
	if err != nil {
		t.Fatal(err)
	}
	for _, te := range m.Taps {
		want := deriveVMNet(te.Idx)
		if te.Name != want.tapName || te.HostIP != want.hostIP ||
			te.GuestIP != want.guestIP || te.GuestMAC != want.guestMAC {
			t.Errorf("manifest idx %d diverges from deriveVMNet: %+v vs %+v", te.Idx, te, want)
		}
	}
}

func TestManifestToVMNets(t *testing.T) {
	m, _ := loadNetworkManifest(writeTemp(t, sampleManifest))
	nets := m.toVMNets()
	if len(nets) != 2 || nets[0].tapName != "ixtap0" || nets[1].guestMAC != "06:00:AC:10:00:06" {
		t.Fatalf("toVMNets wrong: %+v", nets)
	}
}

func TestLoadManifestErrors(t *testing.T) {
	if _, err := loadNetworkManifest("/nonexistent/network.json"); err == nil {
		t.Error("expected error for missing file")
	}
	if _, err := loadNetworkManifest(writeTemp(t, `{"version":1,"taps":[]}`)); err == nil {
		t.Error("expected error for empty tap pool")
	}
	if _, err := loadNetworkManifest(writeTemp(t, `not json`)); err == nil {
		t.Error("expected error for malformed json")
	}
	if _, err := loadNetworkManifest(writeTemp(t, `{"version":99,"cidr":"172.16.0.0/16","taps":[{"idx":0,"name":"ixtap0","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","guest_mac":"06:00:AC:10:00:02","mask":"255.255.255.252"}]}`)); err == nil {
		t.Error("expected error for unsupported version")
	}
}
```

- [ ] **Step 2: Run tests, verify they fail to compile (loader undefined)**

Run: `cd go-sdk && go test ./... -run TestLoadManifest -count=1`
Expected: FAIL — `undefined: loadNetworkManifest`.

- [ ] **Step 3: Implement the loader**

Create `go-sdk/netmanifest.go`:

```go
package ix

import (
	"encoding/json"
	"fmt"
	"os"
)

// manifestVersion is the only network.json schema version this build accepts.
const manifestVersion = 1

// tapEntry is one pre-provisioned TAP described in the manifest. Fields mirror
// vmNet and MUST equal deriveVMNet(idx) — ix-host-setup derives them the same
// way so the manager never needs an `ip` call to learn a slot's addressing.
type tapEntry struct {
	Idx      int    `json:"idx"`
	Name     string `json:"name"`
	HostIP   string `json:"host_ip"`
	GuestIP  string `json:"guest_ip"`
	GuestMAC string `json:"guest_mac"`
	Mask     string `json:"mask"`
}

// networkManifest is the source of truth written by ix-host-setup and consumed
// by the manager in preconfigured mode.
type networkManifest struct {
	Version     int        `json:"version"`
	CIDR        string     `json:"cidr"`
	EgressIface string     `json:"egress_iface"`
	Owner       string     `json:"owner"`
	GatewayIP   string     `json:"gateway_ip"`
	Taps        []tapEntry `json:"taps"`
}

// loadNetworkManifest reads, parses, and validates the manifest at path.
func loadNetworkManifest(path string) (*networkManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read network manifest %q (run ix-host-setup?): %w", path, err)
	}
	var m networkManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse network manifest %q: %w", path, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("network manifest version %d unsupported (want %d); re-run ix-host-setup", m.Version, manifestVersion)
	}
	if len(m.Taps) == 0 {
		return nil, fmt.Errorf("network manifest %q has an empty tap pool; re-run ix-host-setup", path)
	}
	for i, te := range m.Taps {
		if te.Name == "" || te.HostIP == "" || te.GuestIP == "" || te.GuestMAC == "" {
			return nil, fmt.Errorf("network manifest tap %d is incomplete: %+v", i, te)
		}
	}
	return &m, nil
}

// toVMNets converts the manifest's tap entries into the runtime vmNet form.
func (m *networkManifest) toVMNets() []vmNet {
	out := make([]vmNet, 0, len(m.Taps))
	for _, te := range m.Taps {
		mask := te.Mask
		if mask == "" {
			mask = netMask30
		}
		out = append(out, vmNet{
			idx:      te.Idx,
			tapName:  te.Name,
			hostIP:   te.HostIP,
			guestIP:  te.GuestIP,
			guestMAC: te.GuestMAC,
			mask:     mask,
		})
	}
	return out
}
```

- [ ] **Step 4: Run tests, verify PASS**

Run: `cd go-sdk && go test ./... -run 'TestLoadManifest|TestManifest' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go-sdk/netmanifest.go go-sdk/netmanifest_test.go
git commit -m "feat(go-sdk): network manifest type + loader for preconfigured mode"
```

---

## Task 3: preconfiguredNet provider

**Files:**
- Modify: `go-sdk/netprovider.go`
- Create: `go-sdk/netprovider_test.go`

- [ ] **Step 1: Write failing tests**

Create `go-sdk/netprovider_test.go`:

```go
package ix

import (
	"context"
	"errors"
	"testing"
)

func threeNets() []vmNet {
	return []vmNet{deriveVMNet(0), deriveVMNet(1), deriveVMNet(2)}
}

func TestPreconfiguredNetAcquireRelease(t *testing.T) {
	p := newPreconfiguredNet(threeNets())
	if got := p.size(); got != 3 {
		t.Fatalf("size = %d, want 3", got)
	}
	a, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.tapName == b.tapName {
		t.Fatalf("acquired same tap twice: %s", a.tapName)
	}
	p.release(a, nil)
	c, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.tapName != a.tapName {
		t.Fatalf("released tap not reused: got %s want %s", c.tapName, a.tapName)
	}
}

func TestPreconfiguredNetExhaustion(t *testing.T) {
	p := newPreconfiguredNet([]vmNet{deriveVMNet(0)})
	if _, err := p.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := p.acquire(context.Background())
	if !errors.Is(err, ErrNetworkPoolExhausted) {
		t.Fatalf("want ErrNetworkPoolExhausted, got %v", err)
	}
}

func TestPreconfiguredNetDoubleReleaseSafe(t *testing.T) {
	p := newPreconfiguredNet([]vmNet{deriveVMNet(0)})
	a, _ := p.acquire(context.Background())
	p.release(a, nil)
	p.release(a, nil) // must not push a duplicate
	if got := p.freeCount(); got != 1 {
		t.Fatalf("freeCount = %d after double release, want 1", got)
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd go-sdk && go test ./... -run TestPreconfiguredNet -count=1`
Expected: FAIL — `undefined: newPreconfiguredNet`.

- [ ] **Step 3: Implement `preconfiguredNet`**

Append to `go-sdk/netprovider.go`:

```go
// preconfiguredNet hands out TAPs from a fixed pool created by ix-host-setup.
// acquire/release are pure bookkeeping — no `ip`/`nft` exec, no privilege.
type preconfiguredNet struct {
	mu    sync.Mutex
	all   []vmNet // immutable: every slot, for size reporting
	free  []vmNet // currently available
	inUse map[int]bool
}

func newPreconfiguredNet(nets []vmNet) *preconfiguredNet {
	free := make([]vmNet, len(nets))
	copy(free, nets)
	return &preconfiguredNet{all: nets, free: free, inUse: make(map[int]bool)}
}

func (p *preconfiguredNet) size() int { return len(p.all) }

func (p *preconfiguredNet) freeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free)
}

func (p *preconfiguredNet) acquire(ctx context.Context) (*vmNet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return nil, ErrNetworkPoolExhausted
	}
	vn := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	p.inUse[vn.idx] = true
	return &vn, nil
}

func (p *preconfiguredNet) release(vn *vmNet, logger *slog.Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.inUse[vn.idx] {
		return // double-release or never-acquired: ignore, don't duplicate the slot
	}
	delete(p.inUse, vn.idx)
	p.free = append(p.free, *vn)
}
```

Add `"sync"` to the import block in `go-sdk/netprovider.go` (it currently imports `context`, `errors`, `log/slog`).

- [ ] **Step 4: Run, verify PASS**

Run: `cd go-sdk && go test ./... -run TestPreconfiguredNet -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go-sdk/netprovider.go go-sdk/netprovider_test.go
git commit -m "feat(go-sdk): preconfiguredNet provider (pooled TAP, no privilege)"
```

---

## Task 4: ManagerConfig fields + env defaults

**Files:**
- Modify: `go-sdk/manager.go:61-70` (config fields), `go-sdk/manager.go:73-131` (applyDefaults)
- Modify: `go-sdk/manager_test.go` (add a defaults test)

- [ ] **Step 1: Write a failing defaults test**

Add to `go-sdk/manager_test.go`:

```go
func TestPreconfiguredNetworkDefaults(t *testing.T) {
	t.Setenv("IX_PRECONFIGURED_NETWORK", "1")
	c := ManagerConfig{}
	c.applyDefaults()
	if !c.PreconfiguredNetwork {
		t.Error("IX_PRECONFIGURED_NETWORK=1 should set PreconfiguredNetwork")
	}
	if c.NetworkManifest != "/etc/ix/network.json" {
		t.Errorf("NetworkManifest default = %q, want /etc/ix/network.json", c.NetworkManifest)
	}
}

func TestPreconfiguredNetworkExplicitOff(t *testing.T) {
	t.Setenv("IX_PRECONFIGURED_NETWORK", "")
	c := ManagerConfig{}
	c.applyDefaults()
	if c.PreconfiguredNetwork {
		t.Error("PreconfiguredNetwork should default false without the env var")
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd go-sdk && go test ./... -run TestPreconfiguredNetwork -count=1`
Expected: FAIL — `c.PreconfiguredNetwork undefined`.

- [ ] **Step 3: Add the config fields**

In `go-sdk/manager.go`, inside the `// Networking (per-VM TAP + host NAT).` group (after `DisableNetworking bool` at `manager.go:64`), add:

```go

	// Preconfigured (rootless) network mode. When true, the manager performs no
	// privileged host-network setup: it loads NetworkManifest (written by
	// ix-host-setup) and allocates per-VM TAPs from that pool. ip_forward/nft/TAP
	// creation must already be done by `sudo ix-host-setup`. Also enabled by env
	// IX_PRECONFIGURED_NETWORK=1.
	PreconfiguredNetwork bool
	// NetworkManifest is the path read in preconfigured mode; default
	// /etc/ix/network.json.
	NetworkManifest string
```

- [ ] **Step 4: Add env + path defaults in `applyDefaults`**

In `go-sdk/manager.go`, inside `applyDefaults` (after the `NetworkCIDR` default block at `manager.go:103-105`), add:

```go
	if !c.PreconfiguredNetwork && os.Getenv("IX_PRECONFIGURED_NETWORK") == "1" {
		c.PreconfiguredNetwork = true
	}
	if c.NetworkManifest == "" {
		c.NetworkManifest = "/etc/ix/network.json"
	}
```

(`os` is already imported in `manager.go`.)

- [ ] **Step 5: Run, verify PASS**

Run: `cd go-sdk && go test ./... -run TestPreconfiguredNetwork -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go-sdk/manager.go go-sdk/manager_test.go
git commit -m "feat(go-sdk): PreconfiguredNetwork config + IX_PRECONFIGURED_NETWORK env"
```

---

## Task 5: Wire preconfigured mode into NewManager

**Files:**
- Modify: `go-sdk/manager.go:217-272` (backend construction + host-setup branch)
- Create helper: `go-sdk/manager.go` (small `verifyIPForward` reader)

- [ ] **Step 1: Add an ip_forward read-check helper**

Add to `go-sdk/manager.go` (near the other free functions, e.g. above `applyDefaults`):

```go
// ipForwardEnabled reads /proc/sys/net/ipv4/ip_forward (no privilege needed).
// In preconfigured mode the manager cannot enable it, so it fails fast with an
// actionable error when ix-host-setup has not run.
func ipForwardEnabled() bool {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}
```

Confirm `strings` is imported in `manager.go` (`grep -n '"strings"' go-sdk/manager.go`); it is used at `manager.go:599`. If absent, add it.

- [ ] **Step 2: Build the provider before constructing the backend**

In `go-sdk/manager.go`, immediately before the `m := &IXManager{` literal (`manager.go:217`), add:

```go
	// Select the per-VM network provider. Preconfigured mode loads the manifest
	// written by ix-host-setup and never touches privileged host network state.
	var provider netProvider
	if cfg.DisableNetworking {
		provider = nil
	} else if cfg.PreconfiguredNetwork {
		if !ipForwardEnabled() {
			cancel()
			return nil, fmt.Errorf("preconfigured network: ip_forward is off; run `sudo ix-host-setup`")
		}
		manifest, err := loadNetworkManifest(cfg.NetworkManifest)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("preconfigured network: %w", err)
		}
		if manifest.CIDR != cfg.NetworkCIDR {
			cancel()
			return nil, fmt.Errorf("preconfigured network: manifest CIDR %q != config %q", manifest.CIDR, cfg.NetworkCIDR)
		}
		pool := newPreconfiguredNet(manifest.toVMNets())
		// The manifest pool is the hard host cap; never admit more VMs than TAPs.
		if pool.size() < maxConc {
			maxConc = pool.size()
		}
		provider = pool
		cfg.Logger.Info("preconfigured network mode",
			"manifest", cfg.NetworkManifest, "taps", pool.size(), "maxConcurrent", maxConc)
	} else {
		provider = newDynamicNet()
	}
```

> `mCtx, cancel := context.WithCancel(ctx)` already runs at `manager.go:215`, before this block — `cancel()` is valid here. Place this block after that line.

- [ ] **Step 3: Use the provider in the backend literal**

In `go-sdk/manager.go`, change the backend field (just edited in Task 1 to `net: newDynamicNet(),` at `manager.go:224`) to:

```go
			net:             provider,
```

- [ ] **Step 4: Skip privileged host setup in preconfigured mode**

In `go-sdk/manager.go`, change the guard at `manager.go:246` from:

```go
	if !cfg.DisableNetworking {
```

to:

```go
	if !cfg.DisableNetworking && !cfg.PreconfiguredNetwork {
```

This skips `ensureHostNAT`, `ensureForwardAccept`, and `ensureGatewayAddr` together. The browser-remote gateway address (`ixgw0`) must instead be pinned by `ix-host-setup --gateway-ip` (Task 6); the manager trusts the manifest's `gateway_ip`.

- [ ] **Step 5: Guard the browser-remote gateway requirement**

Still in `go-sdk/manager.go`, after the host-setup block, before `if cfg.BrowserMode == "remote" {` (`manager.go:274`), add a fail-fast for the unsupported combo this plan does not fully wire:

```go
	if cfg.PreconfiguredNetwork && cfg.BrowserMode == "remote" {
		// ix-host-setup must have pinned the gateway IP on ixgw0; we only verify
		// the manifest carries it so a misconfigured host fails loudly here.
		if m := cfg.NetworkManifest; m != "" {
			mani, err := loadNetworkManifest(m)
			if err != nil || mani.GatewayIP == "" {
				cancel()
				return nil, fmt.Errorf("preconfigured network + remote browser requires gateway_ip in manifest; re-run ix-host-setup --gateway-ip")
			}
		}
	}
```

> This re-reads the manifest for clarity; the cost is one file read at startup. If you prefer, hoist the `manifest` variable from Step 2 to reuse it — either is acceptable.

- [ ] **Step 6: Build + full unit suite**

Run: `cd go-sdk && go build ./... && go test ./... -count=1`
Expected: PASS. Root-mode path is unchanged (provider = `newDynamicNet()`).

- [ ] **Step 7: Commit**

```bash
git add go-sdk/manager.go
git commit -m "feat(go-sdk): NewManager preconfigured-network branch (no privileged setup)"
```

---

## Task 6: `ix-host-setup.sh` root setup script

**Files:**
- Create: `go-sdk/scripts/ix-host-setup.sh`

The script must mirror `deriveVMNet` and `nftRuleset` exactly (see Background facts). `netBase = 172.16.0.0`; slot `n` → host `.4n+1`, guest `.4n+2`, MAC `06:00:AC:10:<HH>:<LL>` where the last two octets are the guest's 3rd/4th octets.

- [ ] **Step 1: Create the script**

Create `go-sdk/scripts/ix-host-setup.sh`:

```bash
#!/usr/bin/env bash
# ix-host-setup: one-time, root, idempotent host prep for ix preconfigured
# (rootless-manager) networking. Enables ip_forward, installs the ix-nat
# nftables table, creates a pool of owned persistent TAPs, and writes the
# manifest the manager reads (IX_PRECONFIGURED_NETWORK=1).
#
# Addressing MUST match go-sdk deriveVMNet: 172.16.0.0/16 carved into /30s,
# slot n -> host 172.16.x.(4n+1), guest .(4n+2), tap ixtap{n}.
set -euo pipefail

TAPS=32
USERNAME=""
EGRESS_IFACE=""
GATEWAY_IP=""
CIDR="172.16.0.0/16"
MANIFEST="/etc/ix/network.json"

usage() {
  cat >&2 <<EOF
Usage: sudo ix-host-setup --taps N --user USER [--egress-iface IF] [--gateway-ip IP] [--manifest PATH]
  --taps N           number of TAP slots to pre-provision (default 32)
  --user USER        owner of the TAPs (the unprivileged service account) [required]
  --egress-iface IF  pin NAT masquerade to this uplink (default: any non-TAP iface)
  --gateway-ip IP    pin this IP on dummy ixgw0 (required for remote browser tier)
  --manifest PATH    manifest output path (default /etc/ix/network.json)
EOF
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --taps) TAPS="$2"; shift 2 ;;
    --user) USERNAME="$2"; shift 2 ;;
    --egress-iface) EGRESS_IFACE="$2"; shift 2 ;;
    --gateway-ip) GATEWAY_IP="$2"; shift 2 ;;
    --manifest) MANIFEST="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown arg: $1" >&2; usage ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "must run as root" >&2; exit 1; }
[[ -n "$USERNAME" ]] || { echo "--user is required" >&2; usage; }
id "$USERNAME" >/dev/null 2>&1 || { echo "user $USERNAME does not exist" >&2; exit 1; }
[[ "$TAPS" =~ ^[0-9]+$ && "$TAPS" -ge 1 ]] || { echo "--taps must be a positive integer" >&2; exit 1; }

echo "==> enable + persist ip_forward"
sysctl -w net.ipv4.ip_forward=1 >/dev/null
echo "net.ipv4.ip_forward=1" > /etc/sysctl.d/90-ix.conf

echo "==> install ix-nat nftables table (cidr=$CIDR egress=${EGRESS_IFACE:-any})"
if [[ -n "$EGRESS_IFACE" ]]; then
  MASQ="ip saddr $CIDR oifname \"$EGRESS_IFACE\" masquerade"
else
  MASQ="ip saddr $CIDR oifname != \"ixtap*\" masquerade"
fi
nft -f - <<EOF
add table ip ix-nat
flush table ip ix-nat
add chain ip ix-nat postrouting { type nat hook postrouting priority 100 ; }
add rule ip ix-nat postrouting $MASQ
add chain ip ix-nat forward { type filter hook forward priority 0 ; }
add rule ip ix-nat forward iifname "ixtap*" accept
add rule ip ix-nat forward oifname "ixtap*" accept
EOF

# Survive a DROP forward policy (Docker): mirror ensureForwardAccept. Prefer the
# DOCKER-USER chain when present, else FORWARD. Idempotent via -C check.
if command -v iptables >/dev/null 2>&1; then
  CHAIN="FORWARD"
  iptables -S DOCKER-USER >/dev/null 2>&1 && CHAIN="DOCKER-USER"
  for DIR in -i -o; do
    iptables -C "$CHAIN" "$DIR" ixtap+ -j ACCEPT 2>/dev/null \
      || iptables -I "$CHAIN" "$DIR" ixtap+ -j ACCEPT
  done
fi

if [[ -n "$GATEWAY_IP" ]]; then
  echo "==> pin gateway IP $GATEWAY_IP on ixgw0"
  ip link show ixgw0 >/dev/null 2>&1 || ip link add ixgw0 type dummy
  ip addr show dev ixgw0 | grep -q "$GATEWAY_IP" || ip addr add "$GATEWAY_IP/32" dev ixgw0
  ip link set ixgw0 up
fi

echo "==> create $TAPS owned persistent TAPs"
TAP_JSON=""
for ((n=0; n<TAPS; n++)); do
  base=$((0xAC100000 + n*4))         # netBase + 4n
  host=$((base + 1)); guest=$((base + 2))
  ho2=$(((host>>8)&255)); ho1=$((host&255))
  go2=$(((guest>>8)&255)); go1=$((guest&255))
  hip="172.16.$ho2.$ho1"
  gip="172.16.$go2.$go1"
  mac=$(printf "06:00:AC:10:%02X:%02X" "$go2" "$go1")
  tap="ixtap$n"

  # Idempotent: recreate cleanly so owner/addr are guaranteed correct.
  ip link del "$tap" 2>/dev/null || true
  ip tuntap add "$tap" mode tap user "$USERNAME"
  ip addr add "$hip/30" dev "$tap"
  ip link set "$tap" up

  TAP_JSON+=$(printf '{"idx":%d,"name":"%s","host_ip":"%s","guest_ip":"%s","guest_mac":"%s","mask":"255.255.255.252"}' \
    "$n" "$tap" "$hip" "$gip" "$mac")
  [[ $n -lt $((TAPS-1)) ]] && TAP_JSON+=","
done

echo "==> write manifest $MANIFEST (owner=$USERNAME)"
mkdir -p "$(dirname "$MANIFEST")"
cat > "$MANIFEST" <<EOF
{
  "version": 1,
  "cidr": "$CIDR",
  "egress_iface": "$EGRESS_IFACE",
  "owner": "$USERNAME",
  "gateway_ip": "$GATEWAY_IP",
  "taps": [$TAP_JSON]
}
EOF
chmod 644 "$MANIFEST"

echo "==> done. Run the service as $USERNAME with IX_PRECONFIGURED_NETWORK=1"
```

- [ ] **Step 2: Lint**

Run: `bash -n go-sdk/scripts/ix-host-setup.sh && echo "syntax OK"`
Expected: `syntax OK`

- [ ] **Step 3: Verify the manifest matches `deriveVMNet` (cross-check against Go)**

Run as root on a Linux host with `nft` + `iproute2`:

```bash
sudo bash go-sdk/scripts/ix-host-setup.sh --taps 4 --user "$USER" --manifest /tmp/ix-net.json
cat /tmp/ix-net.json
```

Then confirm the script's addressing equals the Go derivation with a throwaway test:

```bash
cd go-sdk && cat > /tmp/derive_check_test.go <<'EOF'
package ix
import ("encoding/json"; "os"; "testing")
func TestManifestMatchesDeriveCheck(t *testing.T) {
	m, err := loadNetworkManifest("/tmp/ix-net.json")
	if err != nil { t.Skip(err) }
	for _, te := range m.Taps {
		w := deriveVMNet(te.Idx)
		if te.Name != w.tapName || te.HostIP != w.hostIP || te.GuestIP != w.guestIP || te.GuestMAC != w.guestMAC {
			b, _ := json.Marshal(te); t.Fatalf("mismatch idx %d: %s vs %+v", te.Idx, b, w)
		}
	}
}
EOF
cp /tmp/derive_check_test.go ./zz_derive_check_test.go
go test -run TestManifestMatchesDeriveCheck -count=1 ./...
rm ./zz_derive_check_test.go
```
Expected: PASS (script addressing == `deriveVMNet`). Then clean up: `sudo ip link del ixtap0; sudo ip link del ixtap1; sudo ip link del ixtap2; sudo ip link del ixtap3`.

- [ ] **Step 4: Commit**

```bash
git add go-sdk/scripts/ix-host-setup.sh
git commit -m "feat(go-sdk): ix-host-setup — root prep + manifest for rootless network"
```

---

## Task 7: Sudo-gated acceptance test

**Files:**
- Create: `go-sdk/preconfigured_network_integration_test.go`

This proves the manager boots a real VM as a regular user in preconfigured mode. It is `//go:build integration`, skips unless `IX_PRECONFIGURED_NETWORK_TEST=1`, and assumes `sudo ix-host-setup` has already run (per the memory note: integration artifacts live in `~/ix`, host-NAT tests need sudo).

- [ ] **Step 1: Write the test**

Create `go-sdk/preconfigured_network_integration_test.go`:

```go
//go:build integration

package ix

import (
	"context"
	"os"
	"testing"
	"time"
)

// Preconditions (run once, as root):
//   sudo go-sdk/scripts/ix-host-setup.sh --taps 4 --user "$USER"
// Then export the rootfs/kernel paths used by the other integration tests and:
//   IX_PRECONFIGURED_NETWORK_TEST=1 go test -tags=integration -run TestPreconfiguredManagerBoots ./...
func TestPreconfiguredManagerBoots(t *testing.T) {
	if os.Getenv("IX_PRECONFIGURED_NETWORK_TEST") != "1" {
		t.Skip("set IX_PRECONFIGURED_NETWORK_TEST=1 after running ix-host-setup")
	}
	rootfs := os.Getenv("IX_TEST_ROOTFS")
	kernel := os.Getenv("IX_TEST_KERNEL")
	if rootfs == "" || kernel == "" {
		t.Skip("set IX_TEST_ROOTFS and IX_TEST_KERNEL")
	}
	if os.Geteuid() == 0 {
		t.Fatal("this test must run as a regular (non-root) user to prove rootless operation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage:          rootfs,
		KernelPath:           kernel,
		PreconfiguredNetwork: true,
		NetworkManifest:      "/etc/ix/network.json",
	})
	if err != nil {
		t.Fatalf("NewManager (preconfigured, non-root): %v", err)
	}
	defer mgr.Shutdown(context.Background())

	sb, err := mgr.Create(ctx, CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer sb.Destroy(context.Background())

	res, err := sb.Shell(ctx, "echo preconfigured-ok", nil)
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("shell exit %d, stderr=%s", res.ExitCode, res.Stderr)
	}
}
```

> Verify the exact `NewManager`, `Create`, `CreateOptions`, `Shell`, and result-field names against the current API before running (`grep -n "func NewManager\|func.*Create(\|type CreateOptions\|func.*Shell(" go-sdk/*.go`). Adjust signatures to match; the test's intent — boot + run a command as non-root in preconfigured mode — is what matters.

- [ ] **Step 2: Compile the integration build**

Run: `cd go-sdk && go vet -tags=integration ./...`
Expected: no errors (test compiles even though it will skip without the env var).

- [ ] **Step 3: Run on a prepared host (manual acceptance)**

```bash
sudo bash go-sdk/scripts/ix-host-setup.sh --taps 4 --user "$USER"
cd go-sdk && IX_PRECONFIGURED_NETWORK_TEST=1 IX_TEST_ROOTFS=~/ix/rootfs/base.ext4 \
  IX_TEST_KERNEL=~/ix/vmlinux.bin go test -tags=integration -run TestPreconfiguredManagerBoots -v ./...
```
Expected: PASS — a VM boots and `echo` returns exit 0, all as a regular user with no sudo on the Go process.

- [ ] **Step 4: Commit**

```bash
git add go-sdk/preconfigured_network_integration_test.go
git commit -m "test(go-sdk): sudo-gated acceptance for rootless preconfigured network"
```

---

## Task 8: Operator documentation

**Files:**
- Create: `docs/handbook/preprovisioned-network.md`

- [ ] **Step 1: Write the doc**

Create `docs/handbook/preprovisioned-network.md` covering:
- **Why:** the manager needs root today only because TAP create + NAT install are runtime ops; this mode moves all privileged work to a one-time `ix-host-setup`.
- **Setup:** `sudo go-sdk/scripts/ix-host-setup.sh --taps N --user <svc> [--egress-iface IF] [--gateway-ip IP]` — what each flag does, that it is idempotent, and that it must re-run after reboot (or be wrapped in a systemd unit — note `ip_forward` persists via `/etc/sysctl.d/90-ix.conf` but TAPs/nft do not survive reboot).
- **Run:** start the embedding service as `<svc>` with `IX_PRECONFIGURED_NETWORK=1` (or set `ManagerConfig.PreconfiguredNetwork = true`). No caps, no sudo.
- **Manifest:** the `/etc/ix/network.json` schema (version, cidr, egress_iface, owner, gateway_ip, taps[]), and that the pool size is the hard concurrency cap (`MaxConcurrent` is clamped down to it).
- **Failure modes:** missing/old manifest → "run ix-host-setup"; `ip_forward` off → same; pool exhausted → `ErrNetworkPoolExhausted` (queue or surface).
- **Limits (this iteration):** custom CIDR still unsupported (only `172.16.0.0/16`); remote browser tier needs `--gateway-ip`; reboot persistence of TAPs/nft is operator-managed.

Include a copy-paste quickstart:

```bash
# once, as root
sudo bash go-sdk/scripts/ix-host-setup.sh --taps 32 --user ixsvc
# then, as ixsvc
IX_PRECONFIGURED_NETWORK=1 ./your-service
```

- [ ] **Step 2: Commit**

```bash
git add docs/handbook/preprovisioned-network.md
git commit -m "docs: operator guide for preconfigured (rootless) network mode"
```

---

## Self-Review Notes

- **Spec coverage:**
  - Goal (privileged work once at setup, manager unprivileged after) → Tasks 5 + 6.
  - Design §1 `ix-host-setup` (sysctl persist, nft table, TAP pool, manifest) → Task 6.
  - Design §2 preconfigured manager (skip `ensureHostNAT`, read-only `ip_forward` check, allocate/release from pool, concurrency cap = pool size, typed exhaustion error) → Tasks 3, 4, 5 (`ipForwardEnabled`, `maxConc` clamp, `ErrNetworkPoolExhausted`).
  - Design §3 failure modes (manifest missing/unreadable, empty pool, version) → Task 2 loader errors + Task 5 fail-fast.
  - Design §4 egress base nft table installed by setup → Task 6 (`ix-nat` table identical to `nftRuleset`).
  - Acceptance (integration suite passes as regular user; root-mode unchanged when flag off) → Task 7 + the "root-mode path unchanged" checks in Tasks 1/5.
  - Open question 1 (passt/browser) → deferred; remote-browser gated to require `--gateway-ip`, documented as a limit (Task 8).
  - Open question 2 (scratch/RunDir chown) → out of scope here; `RunDir` defaults next to the rootfs and is operator-owned (noted in doc).
  - Open question 3 (pool size vs MaxConcurrent) → resolved: manifest size is the hard cap, clamps `MaxConcurrent` down (Task 5 Step 2).
  - The spec's "sibling fixes" (ix-init PATH, ix_repl sentinel, ixd-in-image, pivot_root leftovers) are **out of scope** — they are tracked by the bundle-publishing plan (`docs/superpowers/plans/2026-06-15-ix-bundle-publishing.md`), not here.
- **Placeholder scan:** no TBD/"add error handling"/"similar to Task N"; every code step is complete. The one intentional adapt-to-API note (Task 7 Step 1) is a verification instruction, not a placeholder — the test body is fully written.
- **Type consistency:** `netProvider.acquire(ctx)`/`release(vn, logger)` signatures are identical across `dynamicNet`, `preconfiguredNet`, and both call sites in `vmm.go`. `newPreconfiguredNet`, `newDynamicNet`, `loadNetworkManifest`, `(*networkManifest).toVMNets`, `ErrNetworkPoolExhausted`, `ipForwardEnabled`, `PreconfiguredNetwork`, `NetworkManifest` are each defined once and referenced with matching names. `tapEntry` field names match the JSON the script writes (`idx/name/host_ip/guest_ip/guest_mac/mask`).
- **Coupling risk (flagged):** the nft ruleset and `/30` addressing are duplicated in bash (`ix-host-setup.sh`) and Go (`nftRuleset`, `deriveVMNet`). Task 6 Step 3 adds a cross-check test that fails if the script's addressing drifts from `deriveVMNet`. The nft text has no automated cross-check — if `nftRuleset` changes, update the script by hand (comment in the script points at the Go source).
```
