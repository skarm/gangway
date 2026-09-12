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

// TestDockerServerErrorsAreWarnings separates the daemon failing from the
// daemon answering. A 404 is an answer about the container that was asked for
// and stays a debug record; a 5xx is the daemon reporting that it could not
// serve the request, which means attestation is failing for a reason nothing
// else in this proxy will show. It is the client that decides how often either
// happens, so the warning is rate limited and the records that the limit drops
// are still there at debug.
func TestDockerServerErrorsAreWarnings(t *testing.T) {
	const requests = 20

	status := http.StatusServiceUnavailable
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"message":"daemon is shutting down"}`)
	}))

	var atInfo, atDebug strings.Builder
	newHandlerFor := func(records *strings.Builder, level slog.Level) *gangway.Handler {
		return newHandlerWithConfig(t, gangway.Config{
			DockerSocket:    socket,
			UpstreamTimeout: time.Second,
			Logger:          slog.New(slog.NewJSONHandler(records, &slog.HandlerOptions{Level: level})),
		})
	}

	h := newHandlerFor(&atInfo, slog.LevelInfo)
	for range requests {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want the daemon's own 503", rr.Code)
		}
	}

	if !strings.Contains(atInfo.String(), `"level":"WARN"`) ||
		!strings.Contains(atInfo.String(), "docker daemon reported a server error") {
		t.Errorf("a Docker server error was not reported at warn: %s", atInfo.String())
	}
	// One record for the burst. The proxy cannot keep a client from provoking
	// the daemon, only from setting the pace of the log while it does.
	if got := strings.Count(atInfo.String(), `"level":"WARN"`); got != 1 {
		t.Errorf("%d warnings for %d requests within %s, want 1", got, requests, gangway.UpstreamWarnInterval)
	}

	// The same failures at debug, where the ones the limit dropped still are.
	h = newHandlerFor(&atDebug, slog.LevelDebug)
	for range requests {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_ping", nil))
	}

	if got := strings.Count(atDebug.String(), "docker API returned an error status"); got != requests-1 {
		t.Errorf("%d server errors at debug, want the %d the warning limit dropped", got, requests-1)
	}

	// A status the daemon chose about the object asked for is not a fault here.
	status = http.StatusNotFound

	var clientError strings.Builder
	h = newHandlerFor(&clientError, slog.LevelInfo)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_ping", nil))

	if clientError.Len() != 0 {
		t.Errorf("a 404 from Docker was logged above debug: %s", clientError.String())
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

// TestTruncatedResponseIsAnUnreachableDaemon separates a daemon that stopped
// answering from one that answered badly. Both reach the decoder as an error,
// and to an operator they are opposite things: a body that ends part way
// through is the daemon restarting or being killed, not a response shape this
// proxy cannot parse, and reporting it as the latter sends the reader looking
// for a Docker version mismatch that is not there.
func TestTruncatedResponseIsAnUnreachableDaemon(t *testing.T) {
	var logged strings.Builder
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A length that promises more than what follows, flushed so the client
		// really receives a response and starts reading a body it will never
		// see the end of. Without the flush the connection would drop before
		// any of it left the server, and the failure would be the round trip
		// rather than the read this test is about.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"Id":"sha256:`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		UpstreamTimeout: time.Second,
		Logger:          slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/images/app/json", nil))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(logged.String(), "docker daemon is unreachable") {
		t.Errorf("a truncated response was not reported as an unreachable daemon: %s", logged.String())
	}
	// The failure has to be the body read rather than the request, or this
	// would pass without the response ever having started.
	if !strings.Contains(logged.String(), "docker response could not be read") {
		t.Errorf("the fault did not come from reading the body: %s", logged.String())
	}
	if strings.Contains(logged.String(), "invalid docker API response") {
		t.Errorf("a truncated response was reported as an unparseable one: %s", logged.String())
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
