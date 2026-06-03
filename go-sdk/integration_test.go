//go:build integration

package ix

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

func TestIntegrationCreateAndShell(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{
		SessionID: "integration-test",
		TTL:       2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	// Shell
	result, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", result.ExitCode, result.Output)
	}
	t.Logf("shell output: %s", result.Output)

	// Code execution
	codeResult, err := sb.ExecCode(ctx, sandbox.CodeRequest{
		Language: "python",
		Code:     "print(1 + 1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if codeResult.Status != "ok" {
		t.Fatalf("expected ok, got %s: %s", codeResult.Status, codeResult.Stderr)
	}
	t.Logf("code output: %s", codeResult.Stdout)

	// File write + read roundtrip
	err = sb.WriteFile(ctx, sandbox.WriteFileRequest{
		Path:    "/tmp/test.txt",
		Content: "hello world",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := sb.ReadFile(ctx, sandbox.ReadFileRequest{Path: "/tmp/test.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Content, "hello world") {
		t.Fatalf("expected content containing 'hello world', got %q", content.Content)
	}

	// Get by session ID
	sb2, err := mgr.Get("integration-test")
	if err != nil {
		t.Fatal(err)
	}
	if sb2 == nil {
		t.Fatal("expected sandbox from Get")
	}
}

// TestColdBootNetworking verifies a cold-booted VM gets eth0 configured from the
// kernel ip= arg (a per-VM /30 in 172.16.0.0/16) and can reach the public
// internet through the host NAT. Requires IX_ROOTFS_IMAGE, IX_KERNEL_PATH,
// IX_FC_BINARY and a host with CAP_NET_ADMIN (the manager runs ip/nft at start).
func TestColdBootNetworking(t *testing.T) {
	if rootfsImage() == "" || kernelPath() == "" {
		t.Skip("set IX_ROOTFS_IMAGE, IX_KERNEL_PATH, IX_FC_BINARY to run")
	}
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

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-net", TTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer sb.Close()

	// eth0 came up from the kernel ip= arg with a per-VM /30 address. Read it via
	// a SIOCGIFADDR ioctl so the check doesn't depend on iproute2 in the guest.
	ethIP, err := sb.ExecCode(ctx, sandbox.CodeRequest{
		Language: "python",
		Code: `import socket,fcntl,struct
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
print(socket.inet_ntoa(fcntl.ioctl(s.fileno(),0x8915,struct.pack('256s',b'eth0'))[20:24]))`,
	})
	if err != nil {
		t.Fatalf("exec eth0 ip: %v", err)
	}
	if ethIP.Status != "ok" || !strings.Contains(ethIP.Stdout, "172.16.") {
		t.Errorf("eth0 has no 172.16.x address: status=%s stdout=%q stderr=%q",
			ethIP.Status, ethIP.Stdout, ethIP.Stderr)
	}

	// Reaches the public internet through host NAT (egress firewall permitting).
	web, err := sb.ExecCode(ctx, sandbox.CodeRequest{
		Language: "python",
		Code:     `import urllib.request; print(urllib.request.urlopen('https://example.com', timeout=20).status)`,
	})
	if err != nil {
		t.Fatalf("exec web fetch: %v", err)
	}
	if web.Status != "ok" || !strings.Contains(web.Stdout, "200") {
		t.Errorf("guest could not reach example.com: status=%s stdout=%q stderr=%q",
			web.Status, web.Stdout, web.Stderr)
	}
}
