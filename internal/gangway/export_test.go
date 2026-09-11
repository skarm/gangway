package gangway

import (
	"io"
	"net"
	"net/http"
)

// Test-only access to internals the external test package exercises directly.
// Keeping this bridge in one file leaves the package's own API surface minimal.

const (
	MaxLoggedPathBytes  = maxLoggedPathBytes
	DefaultListenSocket = defaultListenSocket
	DefaultDockerSocket = defaultDockerSocket
)

// DecodeJSON exposes the bounded JSON decoder.
func DecodeJSON(r io.Reader, maxBytes int64, dst any) error { return decodeJSON(r, maxBytes, dst) }

// PathForLog exposes the log-path truncation.
func PathForLog(path string) string { return pathForLog(path) }

// ResolvePath exposes the symlink resolver, so it can be held against the
// standard library's for paths that exist in full.
func ResolvePath(path string) (string, error) { return resolvePath(path) }

// InFlight reports how many upstream requests currently hold a slot.
func (h *Handler) InFlight() int { return len(h.inFlight) }

// Capacity reports the configured concurrency limit.
func (h *Handler) Capacity() int { return cap(h.inFlight) }

// MaxResponseBytes reports the configured response limit.
func (h *Handler) MaxResponseBytes() int64 { return h.maxResponseBytes }

// Transport exposes the upstream transport so its limits can be asserted.
func (h *Handler) Transport() *http.Transport { return h.transport }

// HasLogger reports whether a logger was resolved during construction.
func (h *Handler) HasLogger() bool { return h.log != nil }

// PeerCredentialsSupported reports whether this platform can identify the
// process behind a Unix socket connection.
func PeerCredentialsSupported() bool { return supportPeerCredentials() == nil }

// PeerCredentialsOf exposes the kernel's view of a connection's peer.
func PeerCredentialsOf(conn net.Conn) (pid int32, uid, gid uint32, err error) {
	cred, err := peerCredentialsOf(conn)
	if err != nil {
		return 0, 0, 0, err
	}
	return cred.pid, cred.uid, cred.gid, nil
}

// AllowsPeer exposes the policy decision, so the rule can be tested without a
// socket and on platforms that have no SO_PEERCRED.
func (p PeerPolicy) AllowsPeer(uid, gid, self uint32) bool {
	return p.allows(peerCredentials{uid: uid, gid: gid}, self)
}
