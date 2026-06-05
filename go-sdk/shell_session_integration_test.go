//go:build integration

package ix

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nevindra/oasis/sandbox"
)

// TestShellSessionPersistence: Shell() shares one bash session per sandbox;
// ShellOneShot() must NOT see session state. Requires a rootfs rebuilt with
// the session-aware ixd (Task 17).
func TestShellSessionPersistence(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(ctx, ManagerConfig{
		RootfsImage: rootfsImage(),
		KernelPath:  kernelPath(),
		FCBinary:    fcBinary(),
		DefaultTTL:  5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-shell-session"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Destroy(ctx, sb.(*IXSandbox).id) //nolint:errcheck

	if _, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "export IX_SESSION_PROBE=hello"}); err != nil {
		t.Fatalf("set var: %v", err)
	}
	res, err := sb.Shell(ctx, sandbox.ShellRequest{Command: "echo $IX_SESSION_PROBE"})
	if err != nil {
		t.Fatalf("read var: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("session state lost: %q", res.Output)
	}

	one, err := sb.(*IXSandbox).ShellOneShot(ctx, sandbox.ShellRequest{Command: "echo [$IX_SESSION_PROBE]"})
	if err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	if strings.Contains(one.Output, "hello") {
		t.Errorf("one-shot leaked session state: %q", one.Output)
	}
}
