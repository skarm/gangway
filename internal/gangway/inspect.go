package gangway

import (
	"errors"
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
// A nil map stays nil, so "Docker reported no labels" is still distinguishable
// from "every label was filtered out".
func (h *Handler) filterLabels(labels map[string]string) map[string]string {
	if len(h.labelPrefixes) == 0 || labels == nil {
		return labels
	}

	kept := make(map[string]string)

	for key, value := range labels {
		for _, prefix := range h.labelPrefixes {
			if strings.HasPrefix(key, prefix) {
				kept[key] = value
				break
			}
		}
	}

	return kept
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

	if err := decodeJSON(resp.Body, h.maxResponseBytes, dst); err != nil {
		h.writeUpstreamInvalid(w, r, err)
		return false
	}

	return true
}
