package ix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// passtArgs returns the argument list for passt.
func passtArgs(socketPath string) []string {
	return []string{"--socket", socketPath, "--foreground", "--quiet"}
}

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

// startPasst starts the passt networking daemon and waits for its socket to appear.
// It returns the process ID of the passt daemon.
func startPasst(ctx context.Context, socketDir string) (int, error) {
	passtPath, err := exec.LookPath("passt")
	if err != nil {
		return 0, fmt.Errorf("passt not found in PATH: %w", err)
	}

	socketPath := socketDir + "/passt.sock"
	args := passtArgs(socketPath)

	cmd := exec.CommandContext(ctx, passtPath, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start passt: %w", err)
	}

	// Wait for socket to appear
	if err := waitForFile(socketPath, 5*time.Second); err != nil {
		// Kill the process if socket didn't appear
		cmd.Process.Kill()
		return 0, fmt.Errorf("passt socket did not appear: %w", err)
	}

	return cmd.Process.Pid, nil
}

// stopPasst kills a passt process by its PID.
// It's a no-op if pid <= 0.
func stopPasst(pid int) {
	if pid <= 0 {
		return
	}

	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	_ = p.Kill()
}
