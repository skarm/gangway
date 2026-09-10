package gangway

import (
	"io"
	"net/http"
)

// pingHeaders are the response headers the Moby client reads while negotiating
// the API version. Every other upstream header, including the server banner and
// any cookie, is dropped.
var pingHeaders = [...]string{"Api-Version", "Ostype", "Docker-Experimental", "Builder-Version", "Swarm"}

// handlePing answers the negotiation probe. The upstream body is never
// forwarded: Docker's reply is a fixed "OK", so the proxy writes its own.
func (h *Handler) handlePing(w http.ResponseWriter, r *http.Request, path string) {
	resp, err := h.request(r.Context(), r.Method, path)
	if err != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.writeUpstreamStatus(w, r, resp)
		return
	}

	for _, header := range pingHeaders {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, "OK")
	}
}
