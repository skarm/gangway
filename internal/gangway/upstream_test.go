package gangway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

// TestDockerErrorsCarryNoUpstreamText asserts that a Docker failure reaches the
// client as a status code and the proxy's own fixed message, with nothing the
// daemon wrote in it — not the message field, not a header, not a redirect
// target.
func TestDockerErrorsCarryNoUpstreamText(t *testing.T) {
	const upstreamText = "No such image: hidden-by-the-proxy"

	for _, tc := range []struct {
		status     int
		body       string
		wantStatus int
	}{
		{404, `{"message":"` + upstreamText + `","secret":"hidden"}`, 404},
		{500, `not JSON`, 500},
		{503, `{}`, 503},
		{401, `{"message":"` + strings.Repeat("x", 64<<10) + `"}`, 401},
		// Redirects are never followed, and a non-error status becomes 502.
		{302, `{"message":"` + upstreamText + `"}`, 502},
		{204, ``, 502},
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
			want := map[string]string{"message": "docker API request failed"}
			if rr.Code != tc.wantStatus || !reflect.DeepEqual(got, want) {
				t.Errorf("response = %d %#v, want %d %#v", rr.Code, got, tc.wantStatus, want)
			}
			if strings.Contains(rr.Body.String(), "hidden") || strings.Contains(rr.Body.String(), "No such image") {
				t.Errorf("upstream text reached the client: %s", rr.Body.String())
			}
			if calls.Load() != 1 || rr.Header().Get("Location") != "" || rr.Header().Get("X-Secret") != "" {
				t.Errorf("redirect followed or headers leaked: calls = %d headers = %#v", calls.Load(), rr.Header())
			}
		})
	}
}

// TestDockerErrorStatusIsNotAProxyFault keeps a status Docker chose out of the
// default log: a client asking about a container that has gone is ordinary, and
// only a daemon the proxy cannot reach or parse is worth a record of its own.
func TestDockerErrorStatusIsNotAProxyFault(t *testing.T) {
	var logged strings.Builder
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"No such container"}`)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		UpstreamTimeout: time.Second,
		Logger:          slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if logged.Len() != 0 {
		t.Errorf("upstream status logged above debug: %s", logged.String())
	}
}

// TestUpstreamFaultsAreLoggedImmediately covers the other half: a daemon that
// cannot be reached, or answers with something unparseable, is a fault an
// operator must see without turning on debug logging, and it is recorded as it
// happens rather than counted up for later.
func TestUpstreamFaultsAreLoggedImmediately(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		var logged strings.Builder
		h := newHandlerWithConfig(t, gangway.Config{
			DockerSocket:    filepath.Join(socketDir(t), "missing.sock"),
			UpstreamTimeout: time.Second,
			Logger:          slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})),
		})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rr.Code)
		}
		if !strings.Contains(logged.String(), `"level":"WARN"`) ||
			!strings.Contains(logged.String(), "docker daemon is unreachable") {
			t.Errorf("unreachable daemon not logged at warn: %s", logged.String())
		}
	})

	t.Run("invalid response", func(t *testing.T) {
		var logged strings.Builder
		socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"Config":`)
		}))
		h := newHandlerWithConfig(t, gangway.Config{
			DockerSocket:    socket,
			UpstreamTimeout: time.Second,
			Logger:          slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})),
		})

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))

		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rr.Code)
		}
		if !strings.Contains(logged.String(), `"level":"ERROR"`) ||
			!strings.Contains(logged.String(), "invalid docker API response") {
			t.Errorf("invalid response not logged at error: %s", logged.String())
		}
	})
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
