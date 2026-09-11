package gangway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
)

// maxUpstreamErrorBytes caps how much of a Docker error body the proxy drains
// before giving up on reusing the connection. Nothing in it is read: the body is
// consumed only so the idle connection can be pooled again.
const maxUpstreamErrorBytes = 64 << 10

// upstreamURL is the parsed form of upstreamHost. Classify has already checked
// the request target by the time it gets here, so the target is filled in
// directly rather than reparsed out of a string for every request.
var upstreamURL = url.URL{Scheme: "http", Host: upstreamAuthority}

// request issues a request to Docker carrying none of the client's headers,
// credentials or host.
func (h *Handler) request(ctx context.Context, method, path string) (*http.Response, error) {
	target := upstreamURL
	target.Path = path

	req := (&http.Request{
		Method:     method,
		URL:        &target,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"User-Agent": {userAgent}},
	}).WithContext(ctx)

	return h.client.Do(req)
}

// writeUpstreamStatus relays a Docker failure as a status code and nothing else.
// The body Docker sent is drained and discarded rather than parsed: its
// "message" is the last remaining channel through which the daemon's own text
// reaches a client, and no client of this proxy needs it — SPIRE acts on the
// status. Dropping it also means a message quoting an image reference, which the
// client itself supplied, cannot be reflected back out of the proxy.
//
// A status Docker chose is not a fault here, and asking about a container that
// no longer exists is ordinary traffic, so this is the one upstream outcome that
// stays a debug record: the client decides how often it happens.
func (h *Handler) writeUpstreamStatus(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	// A body that stalls or is cut short mid-drain means the daemon failed to
	// answer, not that it answered with this status.
	err := h.drain(resp)
	if isTimeout(err) || r.Context().Err() != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return
	}

	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}

	h.log.DebugContext(r.Context(), "docker API returned an error status",
		"upstream_status", resp.StatusCode, "status", status)

	writeError(w, status, bodyUpstreamFail)
}

// drain consumes a bounded amount of a response body so its connection can go
// back to the idle pool. A body left unread costs the next request a new dial.
func (h *Handler) drain(resp *http.Response) error {
	_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxUpstreamErrorBytes))
	return err
}

// writeUpstreamUnavailable reports that Docker could not be reached in time. A
// client that has already gone away gets no response at all.
//
// This is a fault rather than an answer — the daemon is down, too slow, or gone
// — so it is logged as it happens and at a level the default configuration
// shows. An operator should not have to turn on debug logging to find out that
// the proxy has stopped being able to reach Docker. The rate limit is what keeps
// a client retrying into a dead daemon from setting the pace of the log.
func (h *Handler) writeUpstreamUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		h.log.DebugContext(r.Context(), "docker API request canceled", "error", err)
		return
	}

	timedOut := isTimeout(err) || errors.Is(r.Context().Err(), context.DeadlineExceeded)

	h.log.WarnContext(r.Context(), "docker daemon is unreachable", "error", err, "timed_out", timedOut)

	if timedOut {
		writeError(w, http.StatusGatewayTimeout, bodyTimedOut)
		return
	}

	writeError(w, http.StatusBadGateway, bodyUnavailable)
}

// writeUpstreamInvalid reports a response the proxy could not make sense of. A
// body truncated by a timeout also reads as invalid, so the timeout case is
// separated out first.
//
// Whatever remains is a daemon that answered 200 with something this proxy
// cannot parse: a Docker version whose response shape changed, a response past
// the size limit, or something on the socket that is not the daemon at all. None
// of those are things a client did, and all of them mean attestation is broken
// right now, so it is logged immediately at error level.
func (h *Handler) writeUpstreamInvalid(w http.ResponseWriter, r *http.Request, err error) {
	if isTimeout(err) || r.Context().Err() != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return
	}

	h.log.ErrorContext(r.Context(), "invalid docker API response", "error", err)
	writeError(w, http.StatusBadGateway, bodyInvalid)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}
