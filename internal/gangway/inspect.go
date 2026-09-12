package gangway

import (
	"errors"
	"maps"
	"net/http"
	"strings"
)

// containerConfig is the only part of a container inspect response that leaves
// the proxy. Config.Env in particular is absent by construction rather than by
// an explicit filter, so a new upstream field cannot leak by default.
type containerConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// containerInspect is the subset decoded from Docker. Config is a pointer so a
// response missing it is distinguishable from one that sent an empty object,
// which would otherwise be relayed as a container with no image.
type containerInspect struct {
	Config *containerConfig `json:"Config"`
}

// containerResponse is what the proxy emits, keeping Docker's nesting so that
// clients decode it with their usual types.
type containerResponse struct {
	Config containerConfig `json:"Config"`
}

func (h *Handler) handleContainerInspect(w http.ResponseWriter, r *http.Request, path string) {
	var decoded containerInspect
	if !h.fetchJSON(w, r, path, &decoded) {
		return
	}

	if decoded.Config == nil {
		h.writeUpstreamInvalid(w, r, errors.New("container inspect response has no Config"))
		return
	}

	config := *decoded.Config
	config.Labels = h.filterLabels(config.Labels)

	writeJSON(w, http.StatusOK, containerResponse{Config: config})
}

// filterLabels keeps only the labels an operator asked for. With no allowlist
// configured every label is relayed, which is what label selectors need and
// what the proxy has always done; with one, a label added to a container
// somewhere else cannot reach a client just because it exists.
//
// The map is edited in place. It was decoded for this request, nothing else has
// a reference to it, and the alternative is holding the whole of it twice while
// a copy is built: the memory this proxy is sized for is one decoded response
// per slot, and a filter that doubles the largest allocation on the path would
// be the one thing that makes the bound wrong. What a Go map does not give back
// is the space the deleted entries left, which is bounded by the map that was
// already paid for.
//
// A nil map stays nil, so "Docker reported no labels" is still distinguishable
// from "every label was filtered out".
func (h *Handler) filterLabels(labels map[string]string) map[string]string {
	if len(h.labelPrefixes) == 0 || labels == nil {
		return labels
	}

	maps.DeleteFunc(labels, func(key, _ string) bool { return !h.allowsLabel(key) })

	return labels
}

// allowsLabel reports whether a label key matches the configured allowlist.
func (h *Handler) allowsLabel(key string) bool {
	for _, prefix := range h.labelPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}

// imageInspect is both the subset decoded from Docker and the response body,
// which are identical for images.
type imageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

func (h *Handler) handleImageInspect(w http.ResponseWriter, r *http.Request, path string) {
	var decoded imageInspect

	if !h.fetchJSON(w, r, path, &decoded) {
		return
	}

	if decoded.ID == "" {
		h.writeUpstreamInvalid(w, r, errors.New("image inspect response has no Id"))
		return
	}

	writeJSON(w, http.StatusOK, decoded)
}

// fetchJSON performs the upstream inspect request and decodes only the fields
// dst declares. On any failure it writes the client response itself and reports
// false, so no upstream body can reach the client by accident.
func (h *Handler) fetchJSON(w http.ResponseWriter, r *http.Request, path string, dst any) bool {
	resp, err := h.request(r.Context(), http.MethodGet, path)
	if err != nil {
		h.writeUpstreamUnavailable(w, r, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.writeUpstreamStatus(w, r, resp)
		return false
	}

	// ContentLength is -1 when the daemon sent no length or framed the response
	// in chunks, which decodeJSON reads as "no hint" rather than as a size.
	if err := decodeJSON(resp.Body, h.maxResponseBytes, resp.ContentLength, dst); err != nil {
		h.writeUpstreamInvalid(w, r, err)
		return false
	}

	return true
}
