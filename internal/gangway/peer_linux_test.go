//go:build linux

package gangway_test

import (
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

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
// policy in force. A test cannot check the rejection end to end, because it
// connects as the very user the policy exempts — that exemption is the thing
// under test here, and the decision it rests on is covered by
// TestPeerPolicyDecisions.
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
