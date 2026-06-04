//go:build integration

package ix

import (
	"context"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestEgressProbe is a DIAGNOSTIC for TestColdBootNetworking failures. It
// boots one networked VM and reports, step by step, where the egress path
// breaks: in-guest resolv.conf → guest→host TAP hop → NAT'd UDP DNS →
// NAT'd TCP. Logs everything; only fails on boot errors, so the full picture
// always prints.
func TestEgressProbe(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "egress-probe", TTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer sb.Close()

	probe := `
import socket, struct, sys

print("--- resolv.conf:")
try:
    print(open("/etc/resolv.conf").read().strip())
except Exception as e:
    print("READ FAILED:", e)

print("--- route/gateway:")
gw = None
try:
    for line in open("/proc/net/route").readlines()[1:]:
        f = line.split()
        if f[1] == "00000000":
            gw = socket.inet_ntoa(struct.pack("<I", int(f[2], 16)))
            print("default via", gw, "dev", f[0])
except Exception as e:
    print("ROUTE READ FAILED:", e)

def tcp(host, port, label):
    s = socket.socket(); s.settimeout(4)
    try:
        s.connect((host, port)); print(label, "TCP", host, port, "OK")
    except ConnectionRefusedError:
        print(label, "TCP", host, port, "REFUSED (host reachable)")
    except Exception as e:
        print(label, "TCP", host, port, "FAILED:", repr(e))
    finally:
        s.close()

if gw:
    tcp(gw, 9, "guest->TAP-host:")  # refused == the TAP hop works

q = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(4)
try:
    s.sendto(q, ("8.8.8.8", 53)); data, _ = s.recvfrom(512)
    print("NAT UDP 8.8.8.8:53 OK,", len(data), "bytes")
except Exception as e:
    print("NAT UDP 8.8.8.8:53 FAILED:", repr(e))

tcp("8.8.8.8", 53, "NAT")
tcp("1.1.1.1", 443, "NAT")
`
	res, err := sb.ExecCode(ctx, sandbox.CodeRequest{Language: "python", Code: probe, Timeout: 60})
	if err != nil {
		t.Fatalf("probe exec: %v", err)
	}
	t.Logf("egress probe:\n%s\nstderr: %s", res.Stdout, res.Stderr)
}
