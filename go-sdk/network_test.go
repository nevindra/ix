//go:build !integration

package ix

import (
	"strings"
	"testing"
)

func TestDeriveVMNet(t *testing.T) {
	tests := []struct {
		n                     int
		host, guest, mac, tap string
	}{
		{0, "172.16.0.1", "172.16.0.2", "06:00:AC:10:00:02", "ixtap0"},
		{1, "172.16.0.5", "172.16.0.6", "06:00:AC:10:00:06", "ixtap1"},
		{63, "172.16.0.253", "172.16.0.254", "06:00:AC:10:00:FE", "ixtap63"},
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

func TestForwardRule(t *testing.T) {
	in := forwardRule("DOCKER-USER", "-i")
	if got, want := strings.Join(in, " "), "DOCKER-USER -i ixtap+ -j ACCEPT"; got != want {
		t.Errorf("forwardRule(-i) = %q, want %q", got, want)
	}
	out := forwardRule("FORWARD", "-o")
	if got, want := strings.Join(out, " "), "FORWARD -o ixtap+ -j ACCEPT"; got != want {
		t.Errorf("forwardRule(-o) = %q, want %q", got, want)
	}
}

func TestForwardChain(t *testing.T) {
	if got := forwardChain(true); got != "DOCKER-USER" {
		t.Errorf("forwardChain(true) = %q, want DOCKER-USER", got)
	}
	if got := forwardChain(false); got != "FORWARD" {
		t.Errorf("forwardChain(false) = %q, want FORWARD", got)
	}
}

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

func TestTapAllocatorDoubleRelease(t *testing.T) {
	a := newTapAllocator(3)
	idx, _ := a.alloc()
	a.release(idx)
	a.release(idx) // double-release must be a no-op
	got1, _ := a.alloc()
	got2, _ := a.alloc()
	if got1 == got2 {
		t.Errorf("double-release caused duplicate alloc: both returned %d", got1)
	}
}

func TestTapArgs(t *testing.T) {
	if got := tapAddArgs("ixtap7"); strings.Join(got, " ") != "tuntap add ixtap7 mode tap" {
		t.Errorf("tapAddArgs = %v", got)
	}
	if got := tapAddrArgs("ixtap7", "172.16.0.29"); strings.Join(got, " ") != "addr add 172.16.0.29/30 dev ixtap7" {
		t.Errorf("tapAddrArgs = %v", got)
	}
	if got := linkUpArgs("ixtap7"); strings.Join(got, " ") != "link set ixtap7 up" {
		t.Errorf("linkUpArgs = %v", got)
	}
	if got := linkDelArgs("ixtap7"); strings.Join(got, " ") != "link del ixtap7" {
		t.Errorf("linkDelArgs = %v", got)
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
		`oifname "ixtap*" accept`,
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
