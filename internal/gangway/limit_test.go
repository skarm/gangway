package gangway_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

func TestLimitConnectionsBoundsAcceptedConnections(t *testing.T) {
	path := filepath.Join(socketDir(t), "limited.sock")
	socket, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	listener := gangway.LimitConnections(socket, 1)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 2)
	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			accepted <- conn
		}
	}()

	first, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	var served net.Conn
	select {
	case served = <-accepted:
	case <-timeAfterAwait():
		t.Fatal("first connection was not accepted")
	}

	// The kernel completes this handshake, but the limiter must not hand the
	// connection to the server while the single slot is occupied.
	second, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	requireQuiet(t, accepted, "accept beyond the connection limit")

	if err := served.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-timeAfterAwait():
		t.Fatal("closing a connection did not release its slot")
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, acceptErr, "accept after close"); err == nil {
		t.Fatal("accept after close reported success")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closing the limiter did not close the underlying socket: %v", err)
	}
}

// TestLimitConnectionsUnblocksAcceptOnClose keeps Close from deadlocking behind
// a blocked Accept, which would leave shutdown waiting forever.
func TestLimitConnectionsUnblocksAcceptOnClose(t *testing.T) {
	path := filepath.Join(socketDir(t), "blocked.sock")
	socket, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	listener := gangway.LimitConnections(socket, 1)
	t.Cleanup(func() { _ = listener.Close() })

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := listener.Accept(); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		_, err := listener.Accept()
		blocked <- err
	}()
	requireQuiet(t, blocked, "accept while the limit is reached")

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, blocked, "unblocked accept"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept error = %v, want %v", err, net.ErrClosed)
	}
}
