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

func TestFilterPresentTaps(t *testing.T) {
	nets := []vmNet{deriveVMNet(0), deriveVMNet(1), deriveVMNet(2)}
	// only ixtap1 "exists"
	exists := func(name string) bool { return name == "ixtap1" }
	got := filterPresentTaps(nets, exists)
	if len(got) != 1 || got[0].tapName != "ixtap1" {
		t.Fatalf("filterPresentTaps = %+v, want only ixtap1", got)
	}
}

func TestFilterPresentTapsAllPresent(t *testing.T) {
	nets := []vmNet{deriveVMNet(0), deriveVMNet(1)}
	got := filterPresentTaps(nets, func(string) bool { return true })
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
}

func TestFilterPresentTapsNonePresent(t *testing.T) {
	nets := []vmNet{deriveVMNet(0)}
	got := filterPresentTaps(nets, func(string) bool { return false })
	if len(got) != 0 {
		t.Fatalf("want 0, got %d", len(got))
	}
}

func TestLoadManifestRejectsDuplicateIdx(t *testing.T) {
	body := `{"version":1,"cidr":"172.16.0.0/16","taps":[
		{"idx":0,"name":"ixtap0","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","guest_mac":"06:00:AC:10:00:02","mask":"255.255.255.252"},
		{"idx":0,"name":"ixtap0","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","guest_mac":"06:00:AC:10:00:02","mask":"255.255.255.252"}
	]}`
	if _, err := loadNetworkManifest(writeTemp(t, body)); err == nil {
		t.Error("expected error for duplicate idx")
	}
}

func TestLoadManifestRejectsOutOfRangeIdx(t *testing.T) {
	body := `{"version":1,"cidr":"172.16.0.0/16","taps":[
		{"idx":99999,"name":"ixtap99999","host_ip":"1.2.3.4","guest_ip":"1.2.3.5","guest_mac":"06:00:00:00:00:00","mask":"255.255.255.252"}
	]}`
	if _, err := loadNetworkManifest(writeTemp(t, body)); err == nil {
		t.Error("expected error for out-of-range idx")
	}
}

func TestLoadManifestRejectsDivergentAddressing(t *testing.T) {
	// idx 0 with a wrong guest_mac must be rejected (idx is authoritative).
	body := `{"version":1,"cidr":"172.16.0.0/16","taps":[
		{"idx":0,"name":"ixtap0","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","guest_mac":"06:00:DE:AD:BE:EF","mask":"255.255.255.252"}
	]}`
	if _, err := loadNetworkManifest(writeTemp(t, body)); err == nil {
		t.Error("expected error for manifest addressing diverging from deriveVMNet")
	}
}

func TestToVMNetsDerivesFromIdx(t *testing.T) {
	// Even a (hypothetical) manifest that passed load must yield derived addressing.
	m, err := loadNetworkManifest(writeTemp(t, sampleManifest))
	if err != nil {
		t.Fatal(err)
	}
	for _, vn := range m.toVMNets() {
		want := deriveVMNet(vn.idx)
		if vn != want {
			t.Errorf("toVMNets idx %d = %+v, want derived %+v", vn.idx, vn, want)
		}
	}
}
