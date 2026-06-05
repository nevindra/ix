package ix

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// waitReady listens on the Firecracker vsock UDS for the daemon's READY signal.
//
// Firecracker vsock guest→host flow: the guest connects to VMADDR_CID_HOST:1025,
// and Firecracker proxies it to a Unix socket at "{uds_path}_1025" on the host.
// We create a listener there before the VM boots, then accept one connection
// and read "READY\n".
func (fb *firecrackerBackend) waitReady(ctx context.Context, handle *VMMHandle) error {
	readyPath := handle.VsockPath + "_1025"

	// Clean up stale socket if it exists.
	_ = os.Remove(readyPath)

	ln, err := net.Listen("unix", readyPath)
	if err != nil {
		return fmt.Errorf("listen ready socket %s: %w", readyPath, err)
	}
	defer ln.Close()
	defer os.Remove(readyPath)

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- acceptResult{conn, err}
	}()

	deadline := time.After(60 * time.Second)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline:
		return fmt.Errorf("ready signal timeout after 60s: %s", readyPath)
	case result := <-ch:
		if result.err != nil {
			return fmt.Errorf("accept ready connection: %w", result.err)
		}
		defer result.conn.Close()
		result.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		buf := make([]byte, 6)
		if _, err := io.ReadFull(result.conn, buf); err != nil {
			return fmt.Errorf("read ready signal: %w", err)
		}
		if string(buf) != "READY\n" {
			return fmt.Errorf("unexpected ready signal: %q", buf)
		}
		return nil
	}
}

// vsockTransport returns an http.RoundTripper that connects to the guest daemon
// via Firecracker's vsock UDS proxy.
//
// Firecracker host→guest flow: the host connects to the vsock UDS, sends
// "CONNECT <port>\n", reads "OK <host_port>\n", then the connection is
// a raw TCP-like stream to the guest port.
func vsockTransport(vsockUDS string) http.RoundTripper {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialCount.Add(1)
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", vsockUDS)
			if err != nil {
				return nil, fmt.Errorf("dial vsock UDS: %w", err)
			}

			// Send CONNECT command for port 1024 (daemon HTTP).
			if _, err := fmt.Fprintf(conn, "CONNECT 1024\n"); err != nil {
				conn.Close()
				return nil, fmt.Errorf("vsock CONNECT: %w", err)
			}

			// Read "OK <port>\n" response.
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("vsock CONNECT response: %w", err)
			}
			if len(line) < 3 || line[:2] != "OK" {
				conn.Close()
				return nil, fmt.Errorf("vsock CONNECT rejected: %s", line)
			}

			// Return a conn that uses the buffered reader for remaining data.
			return &vsockConn{Conn: conn, reader: reader}, nil
		},
		// The daemon terminates SSE streams after the final event, so
		// connections come back to this pool instead of re-dialing the UDS
		// + CONNECT handshake per request.
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
}

// vsockConn wraps a net.Conn with a buffered reader so bytes already consumed
// from the CONNECT handshake response aren't lost.
type vsockConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *vsockConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
