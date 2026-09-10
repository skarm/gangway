package gangway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
)

// maxUpstreamErrorBytes caps the Docker error body the proxy will parse. Only
// its "message" field is ever relayed.
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

// writeUpstreamStatus relays a Docker failure status with its message only,
// dropping every other field, header and redirect target Docker may have sent.
func (h *Handler) writeUpstreamStatus(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}

	var upstream errorMessage

	err := decodeJSON(resp.Body, min(h.maxResponseBytes, maxUpstreamErrorBytes), &upstream)
	if isTimeout(err) || r.Context().Err() != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return
	}
	if err != nil || upstream.Message == "" {
		writeError(w, status, bodyUpstreamFail)
		return
	}

	writeJSON(w, status, upstream)
}

// writeUpstreamUnavailable reports that Docker could not be reached in time. A
// client that has already gone away gets no response at all.
func (h *Handler) writeUpstreamUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		h.log.DebugContext(r.Context(), "docker API request canceled", "error", err)
		return
	}

	h.log.DebugContext(r.Context(), "docker API request failed", "error", err)

	if isTimeout(err) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, bodyTimedOut)
		return
	}

	writeError(w, http.StatusBadGateway, bodyUnavailable)
}

// writeUpstreamInvalid reports a response the proxy could not make sense of. A
// body truncated by a timeout also reads as invalid, so the timeout case is
// separated out first.
func (h *Handler) writeUpstreamInvalid(w http.ResponseWriter, r *http.Request, err error) {
	if isTimeout(err) || r.Context().Err() != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return
	}

	h.log.DebugContext(r.Context(), "invalid docker API response", "error", err)
	writeError(w, http.StatusBadGateway, bodyInvalid)
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}
