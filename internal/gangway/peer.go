package gangway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
)

// PeerPolicy names the local peers allowed to use the proxy socket. It is
// defence in depth behind the socket permissions: those are a property of the
// file, which an operator can widen and a bind mount can carry somewhere
// unexpected, while this is a property of the process on the other end that the
// kernel reports and a client cannot forge.
//
// An empty policy allows every peer the socket permissions already let through.
type PeerPolicy struct {
	// UIDs are the accepted peer user IDs. Empty means any user.
	UIDs []uint32
	// GIDs are the accepted peer *primary* group IDs. Empty means any group.
	// This is not the group the socket mode grants: a process whose
	// supplementary groups include the socket's group still has its own primary
	// group here, so the two are configured separately and on purpose.
	GIDs []uint32
}

// Empty reports whether the policy constrains nothing.
func (p PeerPolicy) Empty() bool { return len(p.UIDs) == 0 && len(p.GIDs) == 0 }

// String renders the policy for the start-up record.
func (p PeerPolicy) String() string {
	if p.Empty() {
		return "any"
	}

	return fmt.Sprintf("uids=%s gids=%s", formatIDs(p.UIDs), formatIDs(p.GIDs))
}

func formatIDs(ids []uint32) string {
	if len(ids) == 0 {
		return "any"
	}

	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}

	return strings.Join(parts, ",")
}

// allows reports whether a peer may proceed. The proxy's own user is always
// allowed: it owns the socket already, and --healthcheck reaches the proxy
// through it, so a policy naming only the client would otherwise report a
// healthy proxy as down.
func (p PeerPolicy) allows(cred peerCredentials, self uint32) bool {
	if cred.uid == self {
		return true
	}

	if len(p.UIDs) != 0 && !containsID(p.UIDs, cred.uid) {
		return false
	}

	if len(p.GIDs) != 0 && !containsID(p.GIDs, cred.gid) {
		return false
	}

	return true
}

func containsID(ids []uint32, id uint32) bool {
	return slices.Contains(ids, id)
}

// peerCredentials is what the kernel reports about the process on the other end
// of a Unix socket.
type peerCredentials struct {
	pid int32
	uid uint32
	gid uint32
}

// ErrPeerCredentialsUnsupported reports that this platform cannot identify the
// process behind a Unix socket connection.
var ErrPeerCredentialsUnsupported = errors.New("peer credentials are not available on this platform")

// authorizedListener drops connections whose peer the policy does not allow,
// before the HTTP server ever reads from them. It sits directly on the Unix
// listener rather than outside the connection limiter, so a client that is
// being rejected does not occupy a connection slot while it is.
type authorizedListener struct {
	net.Listener
	policy PeerPolicy
	self   uint32
	log    *slog.Logger
}

// AuthorizePeers wraps listener so only peers matching policy are served. An
// empty policy returns listener unchanged. A policy that cannot be enforced on
// this platform is an error rather than a warning: a check configured and
// silently skipped is worse than no check at all.
func AuthorizePeers(listener net.Listener, policy PeerPolicy, log *slog.Logger) (net.Listener, error) {
	if policy.Empty() {
		return listener, nil
	}

	if err := supportPeerCredentials(); err != nil {
		return nil, err
	}

	if log == nil {
		log = slog.Default()
	}

	return &authorizedListener{
		Listener: listener,
		policy:   policy,
		self:     uint32(os.Geteuid()),
		log:      log,
	}, nil
}

func (l *authorizedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		cred, err := peerCredentialsOf(conn)
		if err != nil {
			// A peer that cannot be identified is not a peer that may proceed.
			// The usual cause is a connection that went away between accept and
			// this call, which is why it is a debug record.
			l.reject(conn, "unidentified", err, peerCredentials{})
			continue
		}

		if !l.policy.allows(cred, l.self) {
			l.reject(conn, "peer not allowed", nil, cred)
			continue
		}

		return conn, nil
	}
}

// reject closes a connection the policy turned down. Like every other record a
// client can provoke, it is debug only, so a rejected client cannot turn its own
// reconnection loop into log volume.
func (l *authorizedListener) reject(conn net.Conn, reason string, err error, cred peerCredentials) {
	_ = conn.Close()

	if !l.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}

	l.log.Debug("rejected socket peer",
		"reason", reason, "error", err, "peer_uid", cred.uid, "peer_gid", cred.gid, "peer_pid", cred.pid)
}
