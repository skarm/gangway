// Package gangway implements the proxy: which Docker Engine API requests are
// allowed, how their responses are reduced, how the Unix socket is published,
// and how the process is configured and shut down.
package gangway

import (
	"net/http"
	"strings"
)

// RouteKind identifies an allowed endpoint. The zero value is deliberately
// invalid so an unset Route cannot be mistaken for a permitted one.
type RouteKind uint8

const (
	RouteInvalid RouteKind = iota
	RoutePing
	RouteContainerInspect
	RouteImageInspect
)

// Route is a request that may be forwarded. Path is the verified request
// target, which callers use instead of re-reading it from the request.
type Route struct {
	Kind RouteKind
	Path string
}

const (
	pingPath        = "/_ping"
	containerPrefix = "/containers/"
	imagePrefix     = "/images/"
	inspectSuffix   = "/json"
)

// Classify reports whether r may be forwarded, and under which route. This is
// the security boundary: whatever it rejects never reaches the Docker socket,
// so every rule here fails closed. Requests carrying a body, a query string, or
// any percent-encoding in the path are rejected outright.
func Classify(r *http.Request) (Route, bool) {
	if r == nil || r.URL == nil {
		return Route{}, false
	}

	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return Route{}, false
	}

	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		return Route{}, false
	}

	path := r.URL.Path
	// Requiring the raw target to equal the parsed path rules out any escaped
	// spelling of a path that would otherwise pass the checks below.
	if path == "" || r.RequestURI != path || !strings.HasPrefix(path, "/") {
		return Route{}, false
	}

	if strings.ContainsAny(path, "%\\\x00\r\n") || strings.Contains(path, "//") {
		return Route{}, false
	}

	// Moby negotiates the API version against the unversioned /_ping endpoint.
	// SPIRE's inspect calls are versioned once that negotiation has finished.
	if path == pingPath {
		if r.Method == http.MethodHead || r.Method == http.MethodGet {
			return Route{Kind: RoutePing, Path: path}, true
		}

		return Route{}, false
	}

	if r.Method != http.MethodGet {
		return Route{}, false
	}

	rest, ok := trimAPIVersion(path)
	if !ok {
		return Route{}, false
	}

	if id, ok := trimAround(rest, containerPrefix, inspectSuffix); ok {
		if validContainerID(id) {
			return Route{Kind: RouteContainerInspect, Path: path}, true
		}

		return Route{}, false
	}

	if ref, ok := trimAround(rest, imagePrefix, inspectSuffix); ok {
		if validImageReference(ref) {
			return Route{Kind: RouteImageInspect, Path: path}, true
		}

		return Route{}, false
	}

	return Route{}, false
}

// trimAround returns the part of s between prefix and suffix.
func trimAround(s, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return "", false
	}

	return strings.TrimSuffix(strings.TrimPrefix(s, prefix), suffix), true
}
