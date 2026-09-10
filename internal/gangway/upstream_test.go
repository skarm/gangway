package gangway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

func TestDockerErrorsAreReducedToAMessage(t *testing.T) {
	for _, tc := range []struct {
		status      int
		body        string
		wantStatus  int
		wantMessage string
	}{
		{404, `{"message":"No such image","secret":"hidden"}`, 404, "No such image"},
		{500, `not JSON`, 500, "docker API request failed"},
		{503, `{}`, 503, "docker API request failed"},
		// A message larger than the error budget is dropped rather than relayed.
		{401, `{"message":"` + strings.Repeat("x", 64<<10) + `"}`, 401, "docker API request failed"},
		// Redirects are never followed, and a non-error status becomes 502.
		{302, `{"message":"redirect"}`, 502, "redirect"},
		{204, ``, 502, "docker API request failed"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var calls atomic.Int64
			socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "http://docker/info")
				w.Header().Set("X-Secret", "hidden")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			rr := httptest.NewRecorder()
			newHandler(t, socket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/images/app/json", nil))

			var got map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if rr.Code != tc.wantStatus || !reflect.DeepEqual(got, map[string]string{"message": tc.wantMessage}) {
				t.Errorf("response = %d %#v, want %d %q", rr.Code, got, tc.wantStatus, tc.wantMessage)
			}
			if calls.Load() != 1 || rr.Header().Get("Location") != "" || rr.Header().Get("X-Secret") != "" {
				t.Errorf("redirect followed or headers leaked: calls = %d headers = %#v", calls.Load(), rr.Header())
			}
		})
	}
}

// TestUpstreamTimeouts covers every stage a Docker response can stall at, and
// asserts the upstream request is cancelled rather than left running.
func TestUpstreamTimeouts(t *testing.T) {
	for _, stage := range []string{"headers", "container body", "image body", "error body"} {
		t.Run(stage, func(t *testing.T) {
			canceled := make(chan struct{})
			socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage != "headers" {
					if stage == "error body" {
						w.WriteHeader(http.StatusNotFound)
					}
					_, _ = io.WriteString(w, "{")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(canceled)
			}))
			h := newHandlerWithConfig(t, gangway.Config{
				DockerSocket:    socket,
				MaxConcurrent:   1,
				UpstreamTimeout: 100 * time.Millisecond,
			})

			path := "/v1.55/images/app/json"
			if stage == "container body" {
				path = "/v1.55/containers/" + testContainerID + "/json"
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

			if rr.Code != http.StatusGatewayTimeout || h.InFlight() != 0 {
				t.Errorf("timeout: status = %d body = %s occupied = %d", rr.Code, rr.Body.String(), h.InFlight())
			}
			awaitSignal(t, canceled, "upstream cancellation")
		})
	}
}

func TestUnavailableDockerReturnsBadGateway(t *testing.T) {
	h := newHandler(t, filepath.Join(socketDir(t), "missing.sock"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if rr.Code != http.StatusBadGateway || h.InFlight() != 0 {
		t.Fatalf("unavailable upstream: status = %d occupied = %d", rr.Code, h.InFlight())
	}
}

func TestOversizedUpstreamHeadersAreRejected(t *testing.T) {
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Large", strings.Repeat("x", 65<<10))
		_, _ = io.WriteString(w, "OK")
	}))
	rr := httptest.NewRecorder()
	newHandler(t, socket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if rr.Code != http.StatusBadGateway || rr.Header().Get("X-Large") != "" {
		t.Fatalf("oversized headers: status = %d", rr.Code)
	}
}
