package gangway

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Test-only access to internals the external test package exercises directly.
// Keeping this bridge in one file leaves the package's own API surface minimal.

const (
	MaxLoggedPathBytes  = maxLoggedPathBytes
	DefaultListenSocket = defaultListenSocket
	DefaultDockerSocket = defaultDockerSocket
)

// DecodeJSON exposes the bounded JSON decoder. hint is the response's declared
// length; tests that are not about buffer sizing pass 0 for "none".
func DecodeJSON(r io.Reader, maxBytes, hint int64, dst any) error {
	return decodeJSON(r, maxBytes, hint, dst)
}

// WriteJSON exposes the reply writer, so the path taken when a value cannot be
// encoded can be exercised with a value that cannot be.
func WriteJSON(w http.ResponseWriter, status int, value any) { writeJSON(w, status, value) }

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

// AuthorizePeersAs is AuthorizePeers with the exempt user given explicitly, so
// the rejection path can be reached by a test that connects as itself.
func AuthorizePeersAs(listener net.Listener, policy PeerPolicy, self uint32, log *slog.Logger) (net.Listener, error) {
	return authorizePeers(listener, policy, self, log)
}

// Bucket exposes the token bucket behind the rate limit. Its clock is an
// argument, so the limit can be tested at exact instants rather than by
// sleeping for long enough that it probably refilled.
type Bucket struct{ inner *bucket }

// NewBucket builds a bucket the way the handler does.
func NewBucket(rate, capacity float64, now time.Time) *Bucket {
	return &Bucket{inner: newBucket(rate, capacity, now)}
}

// Allow takes a token as of now, reporting whether one was available.
func (b *Bucket) Allow(now time.Time) bool { return b.inner.allow(now) }

// Unlimited reports whether the configuration asked for no rate limit at all,
// which the handler represents as the absent bucket Allow always admits.
func (b *Bucket) Unlimited() bool { return b.inner == nil }

// Tokens reports what the bucket holds as of now, without taking any. It is how
// a test names the boundary it is checking when a refill lands mid-request.
func (b *Bucket) Tokens(now time.Time) float64 {
	if b.inner == nil {
		return 0
	}

	b.inner.mu.Lock()
	defer b.inner.mu.Unlock()

	tokens := b.inner.tokens
	if elapsed := now.Sub(b.inner.last); elapsed > 0 {
		tokens = min(b.inner.capacity, tokens+elapsed.Seconds()*b.inner.rate)
	}

	return tokens
}

// FilterLabels exposes the label allowlist as the handler applies it.
func (h *Handler) FilterLabels(labels map[string]string) map[string]string {
	return h.filterLabels(labels)
}

// BurstSeconds is how many seconds of tokens the rate limiter holds, so a test
// can work out the burst a configuration buys instead of restating it.
const BurstSeconds = burstSeconds

// UpstreamWarnInterval is the shortest gap between warnings about a Docker
// server error.
const UpstreamWarnInterval = upstreamWarnInterval
