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
	seen := make(map[int]bool, len(m.Taps))
	for i, te := range m.Taps {
		if te.Name == "" || te.HostIP == "" || te.GuestIP == "" || te.GuestMAC == "" {
			return nil, fmt.Errorf("network manifest tap %d is incomplete: %+v", i, te)
		}
		if te.Idx < 0 || te.Idx >= maxTapIndex {
			return nil, fmt.Errorf("network manifest tap %d has out-of-range idx %d (must be 0..%d); re-run ix-host-setup", i, te.Idx, maxTapIndex-1)
		}
		if seen[te.Idx] {
			return nil, fmt.Errorf("network manifest has duplicate idx %d; re-run ix-host-setup", te.Idx)
		}
		seen[te.Idx] = true
		// idx is authoritative: the address fields must match the derived /30
		// scheme, so a hand-edited or drifted manifest fails loudly here instead
		// of silently handing a VM the wrong MAC/IP.
		if want := deriveVMNet(te.Idx); te.Name != want.tapName ||
			te.HostIP != want.hostIP || te.GuestIP != want.guestIP || te.GuestMAC != want.guestMAC {
			return nil, fmt.Errorf("network manifest tap %d (idx %d) addressing diverges from the derived scheme; re-run ix-host-setup", i, te.Idx)
		}
	}
	return &m, nil
}

// tapExists reports whether a network interface named `name` is present on the
// host. /sys/class/net/<name> exists for every live interface; reading it needs
// no privilege.
func tapExists(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name)
	return err == nil
}

// filterPresentTaps returns only the nets whose TAP device currently exists on
// the host, per the supplied existence check. Missing TAPs are dropped so the
// remaining pool slots keep working (a partially-broken host setup degrades
// instead of failing wholesale).
func filterPresentTaps(nets []vmNet, exists func(name string) bool) []vmNet {
	out := make([]vmNet, 0, len(nets))
	for _, vn := range nets {
		if exists(vn.tapName) {
			out = append(out, vn)
		}
	}
	return out
}

// toVMNets converts the manifest's tap entries into runtime vmNet form. idx is
// authoritative — addressing is derived, not copied from the JSON, so there is a
// single source of truth (loadNetworkManifest already verified the stored fields
// agree with the derivation).
func (m *networkManifest) toVMNets() []vmNet {
	out := make([]vmNet, 0, len(m.Taps))
	for _, te := range m.Taps {
		out = append(out, deriveVMNet(te.Idx))
	}
	return out
}
