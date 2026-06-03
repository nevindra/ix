package ix

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

// waitForFile polls for the existence of a file with a timeout.
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

// release returns an index to the free list for reuse. Out-of-range indices
// and indices already free are ignored, so a double-release cannot hand the
// same subnet to two VMs.
func (a *tapAllocator) release(idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if idx < 0 || idx >= a.max {
		return
	}
	for _, f := range a.free {
		if f == idx {
			return
		}
	}
	a.free = append(a.free, idx)
}

// --- pure command/ruleset builders (unit-tested; no exec) ---

// tapAddArgs / tapAddrArgs / linkUpArgs / linkDelArgs build `ip` argument lists.
func tapAddArgs(name string) []string  { return []string{"tuntap", "add", name, "mode", "tap"} }
func linkUpArgs(name string) []string  { return []string{"link", "set", name, "up"} }
func linkDelArgs(name string) []string { return []string{"link", "del", name} }
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
	_ = runIP(ctx, linkDelArgs(vn.tapName)...)
	if err := runIP(ctx, tapAddArgs(vn.tapName)...); err != nil {
		return vmNet{}, fmt.Errorf("create tap: %w", err)
	}
	if err := runIP(ctx, tapAddrArgs(vn.tapName, vn.hostIP)...); err != nil {
		_ = runIP(ctx, linkDelArgs(vn.tapName)...)
		return vmNet{}, fmt.Errorf("addr tap: %w", err)
	}
	if err := runIP(ctx, linkUpArgs(vn.tapName)...); err != nil {
		_ = runIP(ctx, linkDelArgs(vn.tapName)...)
		return vmNet{}, fmt.Errorf("up tap: %w", err)
	}
	return vn, nil
}

// teardownTap deletes the TAP. Best-effort (callers log; do not block).
func teardownTap(ctx context.Context, vn vmNet) error {
	return runIP(ctx, linkDelArgs(vn.tapName)...)
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
	if err := runIP(ctx, linkUpArgs(gatewayDummyIface)...); err != nil {
		return fmt.Errorf("up %s: %w", gatewayDummyIface, err)
	}
	return nil
}

// teardownGatewayAddr removes the Gateway dummy interface. Best-effort.
func teardownGatewayAddr(ctx context.Context) error {
	return runIP(ctx, linkDelArgs(gatewayDummyIface)...)
}
