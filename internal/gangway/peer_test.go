package gangway_test

import (
	"errors"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

// TestPeerPolicyDecisions pins the rule itself, independently of any socket.
func TestPeerPolicyDecisions(t *testing.T) {
	const self = 1000

	for name, tc := range map[string]struct {
		policy   gangway.PeerPolicy
		uid, gid uint32
		want     bool
	}{
		"empty policy allows anyone":        {gangway.PeerPolicy{}, 4242, 4242, true},
		"matching uid":                      {gangway.PeerPolicy{UIDs: []uint32{1001}}, 1001, 50, true},
		"other uid":                         {gangway.PeerPolicy{UIDs: []uint32{1001}}, 1002, 50, false},
		"one of several uids":               {gangway.PeerPolicy{UIDs: []uint32{1001, 1002}}, 1002, 50, true},
		"matching gid":                      {gangway.PeerPolicy{GIDs: []uint32{50}}, 4242, 50, true},
		"other gid":                         {gangway.PeerPolicy{GIDs: []uint32{50}}, 4242, 51, false},
		"uid and gid must both match":       {gangway.PeerPolicy{UIDs: []uint32{1001}, GIDs: []uint32{50}}, 1001, 51, false},
		"uid and gid both matching":         {gangway.PeerPolicy{UIDs: []uint32{1001}, GIDs: []uint32{50}}, 1001, 50, true},
		"the proxy's own user is exempt":    {gangway.PeerPolicy{UIDs: []uint32{1001}}, self, 51, true},
		"the proxy's own user beats a gid":  {gangway.PeerPolicy{GIDs: []uint32{50}}, self, 51, true},
		"root is not exempt without a rule": {gangway.PeerPolicy{UIDs: []uint32{1001}}, 0, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.policy.AllowsPeer(tc.uid, tc.gid, self); got != tc.want {
				t.Errorf("allows(uid=%d gid=%d) = %v, want %v", tc.uid, tc.gid, got, tc.want)
			}
		})
	}
}

// TestPeerPolicyIsExemptForHealthcheck states the reason the proxy's own user is
// always allowed: --healthcheck reaches the proxy through the same socket, and a
// policy naming only the client would otherwise report a healthy proxy as down.
func TestPeerPolicyIsExemptForHealthcheck(t *testing.T) {
	const proxyUID = 1000

	policy := gangway.PeerPolicy{UIDs: []uint32{1001}}
	if !policy.AllowsPeer(proxyUID, 0, proxyUID) {
		t.Fatal("the proxy cannot probe its own socket under a policy naming only the client")
	}
}

// TestPeerPolicyString keeps the start-up record readable.
func TestPeerPolicyString(t *testing.T) {
	for _, tc := range []struct {
		policy gangway.PeerPolicy
		want   string
	}{
		{gangway.PeerPolicy{}, "any"},
		{gangway.PeerPolicy{UIDs: []uint32{1001}}, "uids=1001 gids=any"},
		{gangway.PeerPolicy{GIDs: []uint32{50, 60}}, "uids=any gids=50,60"},
		{gangway.PeerPolicy{UIDs: []uint32{0, 1001}, GIDs: []uint32{50}}, "uids=0,1001 gids=50"},
	} {
		if got := tc.policy.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// TestAuthorizePeersLeavesAnEmptyPolicyAlone keeps the default path free of a
// wrapper that would only ever say yes.
func TestAuthorizePeersLeavesAnEmptyPolicyAlone(t *testing.T) {
	listener := startListener(t)

	wrapped, err := gangway.AuthorizePeers(listener, gangway.PeerPolicy{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if wrapped != listener {
		t.Errorf("empty policy wrapped the listener: %T", wrapped)
	}
}

// TestAuthorizePeersRefusesAnUnenforceablePolicy is the fail-closed case: a
// check that cannot run must stop the proxy, not be skipped quietly.
func TestAuthorizePeersRefusesAnUnenforceablePolicy(t *testing.T) {
	if gangway.PeerCredentialsSupported() {
		t.Skip("this platform can enforce a peer policy")
	}

	listener := startListener(t)

	if _, err := gangway.AuthorizePeers(listener, gangway.PeerPolicy{UIDs: []uint32{1000}}, discardLogger()); !errors.Is(err, gangway.ErrPeerCredentialsUnsupported) {
		t.Fatalf("error = %v, want ErrPeerCredentialsUnsupported", err)
	}
}

// TestParseRefusesAnUnenforceablePolicy makes the same refusal a usage error, so
// it is reported before any socket is created.
func TestParseRefusesAnUnenforceablePolicy(t *testing.T) {
	if gangway.PeerCredentialsSupported() {
		t.Skip("this platform can enforce a peer policy")
	}

	if _, err := gangway.Parse([]string{"--allow-uid=1000"}, discardOutput()); !errors.Is(err, gangway.ErrPeerCredentialsUnsupported) {
		t.Fatalf("error = %v, want ErrPeerCredentialsUnsupported", err)
	}
}
