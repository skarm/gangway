package gangway_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

const testContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestClassifyAcceptsTheAllowedSurface(t *testing.T) {
	for _, path := range []string{
		"/_ping",
		"/v1.55/containers/" + testContainerID + "/json",
		"/v1.24/containers/" + testContainerID + "/json",
		"/v1.55/images/sha256:" + testContainerID + "/json",
		"/v1.55/images/registry.example:5000/team/app@sha256:" + testContainerID + "/json",
	} {
		t.Run(path, func(t *testing.T) {
			route, ok := gangway.Classify(httptest.NewRequest(http.MethodGet, path, nil))
			if !ok {
				t.Fatalf("valid request rejected: %q", path)
			}
			if route.Path != path {
				t.Fatalf("route path = %q, want %q", route.Path, path)
			}
			if route.Kind == gangway.RouteInvalid {
				t.Fatal("accepted request classified as invalid")
			}
		})
	}
	if _, ok := gangway.Classify(nil); ok {
		t.Error("nil request accepted")
	}
	if _, ok := gangway.Classify(&http.Request{}); ok {
		t.Error("request without URL accepted")
	}
}

// TestDeniedRequestsNeverReachDocker is the core allowlist assertion: not only
// is the response a 403, no upstream call is made at all.
func TestDeniedRequestsNeverReachDocker(t *testing.T) {
	var calls atomic.Int64
	dockerSocket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h := newHandler(t, dockerSocket)

	paths := []string{
		// Endpoints outside the allowlist.
		"/v1.55/containers/create", "/v1.55/containers/json", "/version", "/info", "/events",
		// Ping variants.
		"/_ping?", "/_ping?x=1", "/v1.55/_ping", "/_ping/", "//_ping", "http://docker/_ping",
		// Missing or malformed API versions.
		"/containers/" + testContainerID + "/json", "/v1/containers/" + testContainerID + "/json",
		"/v1.x/images/app/json", "/v.1/images/app/json", "/v1./images/app/json",
		"/v1.2.3/images/app/json", "/v123456789012345.12/images/app/json", "/v1.55",
		// Container IDs that are not a full lowercase hex ID.
		"/v1.55/containers/abc/json", "/v1.55/containers/" + strings.ToUpper(testContainerID) + "/json",
		"/v1.55/containers/" + testContainerID + "a/json",
		// Image references with empty, dot or traversal segments.
		"/v1.55/images//json", "/v1.55/images/app//latest/json", "/v1.55/images/./app/json",
		"/v1.55/images/app/../json", "/v1.55/images/foo/../bar/json", "/v1.55/images/app/json/",
		// Escaped or otherwise out-of-charset targets.
		"/v1.55/images/app\\latest/json", "/v1.55/images/app%2Flatest/json", "/v1.55/images/foo%2fbar/json",
		"/v1.55/images/%61pp/json", "/v1.55/images/app%00/json", "/v1.55/images/app%0a/json",
		"/v1.55/images/app%25/json",
		// Query strings and oversized references.
		"/v1.55/images/app?size=1", "/v1.55/containers/" + testContainerID + "/json?size=1",
		"/v1.55/images/" + strings.Repeat("a", 1025) + "/json",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rr.Code)
			}
		})
	}

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions,
		http.MethodPatch, http.MethodConnect, http.MethodTrace, "get",
	} {
		t.Run("method "+method, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(method, "/_ping", nil))
			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rr.Code)
			}
		})
	}

	for name, mutate := range map[string]func(*http.Request){
		"unknown content length":   func(r *http.Request) { r.ContentLength = -1 },
		"chunked":                  func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
		"raw path":                 func(r *http.Request) { r.URL.RawPath = "/_ping" },
		"different request target": func(r *http.Request) { r.RequestURI = "/info" },
		"head inspect": func(r *http.Request) {
			r.Method = http.MethodHead
			r.URL.Path = "/v1.55/images/app/json"
			r.RequestURI = r.URL.Path
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/_ping", nil)
			mutate(r)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rr.Code)
			}
		})
	}

	t.Run("request body", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", strings.NewReader("x"))
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
	})

	if got := calls.Load(); got != 0 {
		t.Fatalf("denied requests reached Docker %d times", got)
	}
}

// TestRawHTTPRequestsWithBodiesAreDenied parses requests off the wire, so the
// framing checks are exercised the way a real client would trigger them.
func TestRawHTTPRequestsWithBodiesAreDenied(t *testing.T) {
	h := newHandler(t, "/unused.sock")
	for _, wire := range []string{
		"GET /_ping HTTP/1.1\r\nHost: proxy\r\nContent-Length: 1\r\n\r\nx",
		"GET /_ping HTTP/1.1\r\nHost: proxy\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		"GET http://docker/_ping HTTP/1.1\r\nHost: proxy\r\n\r\n",
	} {
		req, err := http.ReadRequest(newWireReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		_ = req.Body.Close()
		if rr.Code != http.StatusForbidden {
			t.Errorf("raw request accepted: %q, status = %d", wire, rr.Code)
		}
	}
}

// FuzzClassify asserts that anything Classify accepts still means exactly the
// same request once it is rebuilt as an upstream URL.
func FuzzClassify(f *testing.F) {
	for _, path := range []string{
		"/_ping",
		"/v1.55/containers/" + testContainerID + "/json",
		"/v1.55/images/registry.example/app:v1/json",
		"/v1.55/images/app/../json",
		"/v1.55/images/%2fapp/json",
	} {
		f.Add(http.MethodGet, path)
	}
	f.Fuzz(func(t *testing.T, method, path string) {
		req := &http.Request{Method: method, URL: &url.URL{Path: path}, RequestURI: path}
		route, ok := gangway.Classify(req)
		if !ok {
			return
		}
		upstream, err := http.NewRequest(method, "http://docker"+route.Path, nil)
		if err != nil {
			t.Fatalf("allowed target cannot be forwarded: %q: %v", path, err)
		}
		if upstream.URL.Host != "docker" || upstream.URL.Scheme != "http" || upstream.URL.User != nil ||
			upstream.URL.RawQuery != "" || upstream.URL.Fragment != "" || upstream.URL.RawPath != "" ||
			upstream.URL.RequestURI() != path {
			t.Fatalf("allowed target changes meaning when forwarded: %q -> %#v", path, upstream.URL)
		}
		if method != http.MethodGet && !(method == http.MethodHead && path == "/_ping") {
			t.Fatalf("allowed method = %q, path = %q", method, path)
		}
	})
}
