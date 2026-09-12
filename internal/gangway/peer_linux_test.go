//go:build linux

package gangway_test

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

// recordLogger writes debug records into records as they happen. A test whose
// records are produced by another goroutine cannot read a buffer the logger is
// still writing to, so they arrive one at a time instead; the send never blocks
// the goroutine being observed, which for a listener is its accept loop.
func recordLogger(records chan<- string) *slog.Logger {
	return jsonLogger(writerFunc(func(p []byte) {
		select {
		case records <- string(p):
		default:
		}
	}))
}

// awaitRecord waits for a record containing want, failing if none arrives.
func awaitRecord(t *testing.T, records <-chan string, want string) {
	t.Helper()

	deadline := time.After(awaitTimeout)

	for {
		select {
		case record := <-records:
			if strings.Contains(record, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no log record containing %q", want)
		}
	}
}

// TestPeerCredentialsReportTheConnectingProcess checks the syscall wiring, which
// is the part of the peer check that can break without any test noticing: a
// lookup that silently failed would reject every client, and one that reported
// the wrong process would admit the wrong one.
func TestPeerCredentialsReportTheConnectingProcess(t *testing.T) {
	listener := startListener(t)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	client, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	conn, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer conn.Close()

	pid, uid, gid, err := gangway.PeerCredentialsOf(conn)
	if err != nil {
		t.Fatal(err)
	}
	if int(pid) != os.Getpid() || int(uid) != os.Getuid() || int(gid) != os.Getgid() {
		t.Errorf("peer = pid %d uid %d gid %d, want pid %d uid %d gid %d",
			pid, uid, gid, os.Getpid(), os.Getuid(), os.Getgid())
	}
}

// TestAuthorizedListenerServesTheProxysOwnUser exercises the whole path with a
// policy in force, through the exemption the proxy's own user has: the
// healthcheck reaches the proxy over this socket, so a policy naming only the
// SPIRE agent must not report a healthy proxy as down.
func TestAuthorizedListenerServesTheProxysOwnUser(t *testing.T) {
	listener := startListener(t)

	// A policy that names only a user this process is not, so the connection can
	// only succeed through the exemption.
	policy := gangway.PeerPolicy{UIDs: []uint32{uint32(os.Getuid()) + 1}}

	authorized, err := gangway.AuthorizePeers(listener, policy, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(authorized) }()
	t.Cleanup(func() { _ = server.Close() })

	resp, err := unixClient(t, listener.Addr().String()).Get("http://proxy/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// TestAuthorizedListenerRejectsAnUnlistedPeer is the enforcement itself, end to
// end: a client the policy does not name has its connection closed and never
// reaches the HTTP handler.
//
// A test process can only connect as itself, and its own user is the one the
// policy always exempts, so the exempt user is named explicitly here as someone
// else. That leaves this process an ordinary client the policy has not allowed,
// which is the only way a test can stand where a rejected client stands.
// Without it the check could be deleted from the listener and every test on
// this platform would still pass.
func TestAuthorizedListenerRejectsAnUnlistedPeer(t *testing.T) {
	listener := startListener(t)

	// Someone this process is not, both as the proxy's own user and as the one
	// user the policy admits.
	other := uint32(os.Getuid()) + 1

	records := make(chan string, 8)
	authorized, err := gangway.AuthorizePeersAs(listener, gangway.PeerPolicy{UIDs: []uint32{other}}, other, recordLogger(records))
	if err != nil {
		t.Fatal(err)
	}

	var served atomic.Int64
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(authorized) }()
	t.Cleanup(func() { _ = server.Close() })

	resp, err := unixClient(t, listener.Addr().String()).Get("http://proxy/")
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("an unlisted peer was served: status %d", resp.StatusCode)
	}

	if got := served.Load(); got != 0 {
		t.Errorf("the handler ran %d times for a rejected peer", got)
	}

	awaitRecord(t, records, "peer not allowed")
}

// TestAuthorizedListenerServesAnAllowedPeerAfterARejection covers the other
// half of enforcement: a rejection must cost the next client nothing. The
// accept loop has to close the connection it turned down and go back to
// accepting, rather than returning the rejection as an error and taking the
// listener down with it.
//
// The rejected connection here is one whose peer cannot be identified at all,
// which is the other way Accept turns a client away, and which no socket a test
// can open would produce on its own.
func TestAuthorizedListenerServesAnAllowedPeerAfterARejection(t *testing.T) {
	listener := startListener(t)

	unidentifiable, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	queued := make(chan net.Conn, 1)
	queued <- unidentifiable

	records := make(chan string, 8)
	// This process by UID, and not through the exemption, so the connection
	// after the rejected one is served on the policy's own terms.
	policy := gangway.PeerPolicy{UIDs: []uint32{uint32(os.Getuid())}}
	authorized, err := gangway.AuthorizePeersAs(&queuedListener{Listener: listener, queued: queued}, policy, uint32(os.Getuid())+1, recordLogger(records))
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(authorized) }()
	t.Cleanup(func() { _ = server.Close() })

	awaitRecord(t, records, "unidentified")

	// The far end of a closed pipe reports it, which is how a test sees that
	// the connection was dropped rather than handed to the server.
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Error("the rejected connection is still open")
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("read from the rejected connection: %v", err)
	}

	resp, err := unixClient(t, listener.Addr().String()).Get("http://proxy/")
	if err != nil {
		t.Fatalf("an allowed peer was refused after a rejection: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// queuedListener hands out prepared connections before those of the listener
// underneath, so a test can decide what the authorizing listener accepts first.
type queuedListener struct {
	net.Listener
	queued chan net.Conn
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.queued:
		return conn, nil
	default:
	}

	return l.Listener.Accept()
}
