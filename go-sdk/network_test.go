//go:build !integration

package ix

import (
	"testing"
)

// TestPasstArgsConstruction verifies that passtArgs returns the correct argument list.
func TestPasstArgsConstruction(t *testing.T) {
	socketPath := "/tmp/passt.sock"
	args := passtArgs(socketPath)

	// Verify length
	if len(args) != 4 {
		t.Errorf("expected 4 arguments, got %d", len(args))
	}

	// Verify --socket and socketPath
	if args[0] != "--socket" {
		t.Errorf("expected args[0] to be '--socket', got %q", args[0])
	}
	if args[1] != socketPath {
		t.Errorf("expected args[1] to be %q, got %q", socketPath, args[1])
	}

	// Verify --foreground
	found := false
	for _, arg := range args {
		if arg == "--foreground" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '--foreground' in arguments")
	}

	// Verify --quiet
	found = false
	for _, arg := range args {
		if arg == "--quiet" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '--quiet' in arguments")
	}
}

// TestPasstArgsWithDifferentPaths verifies that socket path is correctly embedded for various paths.
func TestPasstArgsWithDifferentPaths(t *testing.T) {
	tests := []struct {
		name       string
		socketPath string
	}{
		{"simple path", "/tmp/passt.sock"},
		{"nested path", "/var/run/sandbox/network/passt.sock"},
		{"relative path", "./passt.sock"},
		{"home path", "~/sandbox/passt.sock"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := passtArgs(tc.socketPath)

			// Verify the socket path is at index 1 (after --socket flag)
			if args[0] != "--socket" {
				t.Errorf("expected args[0] to be '--socket', got %q", args[0])
			}
			if args[1] != tc.socketPath {
				t.Errorf("expected socket path %q at args[1], got %q", tc.socketPath, args[1])
			}
		})
	}
}

// TestStopPasstNoOp verifies that stopPasst doesn't panic with invalid PIDs.
func TestStopPasstNoOp(t *testing.T) {
	// These should not panic
	stopPasst(0)
	stopPasst(-1)
	stopPasst(-999)

	// If we get here without panic, the test passes
	t.Log("stopPasst handled invalid PIDs gracefully")
}
