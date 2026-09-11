package gangway_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

// TestRateLimitBoundsUpstreamWork is the point of the limit: concurrency alone
// bounds how many requests are open at once, not how many Docker is asked to
// serve. A client that never exceeds one in flight can still drive the daemon
// without bound unless the rate is capped too.
func TestRateLimitBoundsUpstreamWork(t *testing.T) {
	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		MaxConcurrent:   1,
		MaxRate:         1,
		UpstreamTimeout: time.Second,
	})

	// The bucket starts full at burstSeconds of tokens, so the first few
	// requests pass and everything after them is refused within the same second.
	var throttled int

	for range 50 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code == http.StatusTooManyRequests {
			throttled++
			continue
		}

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 or 429", rr.Code)
		}
	}

	if throttled == 0 {
		t.Fatal("rate limit never engaged")
	}
	if got := calls.Load(); got > 50-int64(throttled) {
		t.Errorf("throttled requests still reached Docker: %d calls, %d throttled", got, throttled)
	}
	if int64(throttled)+calls.Load() != 50 {
		t.Errorf("requests unaccounted for: %d throttled, %d forwarded", throttled, calls.Load())
	}
}

// TestRateLimitRefillsOverTime keeps the limit a rate rather than a quota.
func TestRateLimitRefillsOverTime(t *testing.T) {
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		MaxConcurrent:   1,
		MaxRate:         100,
		UpstreamTimeout: time.Second,
	})

	// Drain the bucket, then wait for it to refill and assert it serves again.
	for range 500 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	}

	deadline := time.Now().Add(awaitTimeout)
	for {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("bucket never refilled")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestRateLimitIsOffByDefaultInCode records that a Config built in code, unlike
// the command line, is unlimited unless it says otherwise.
func TestRateLimitIsOffByDefaultInCode(t *testing.T) {
	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h := newHandler(t, socket)

	for range 200 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d with no rate limit configured", rr.Code)
		}
	}

	if calls.Load() != 200 {
		t.Errorf("forwarded %d of 200 requests", calls.Load())
	}
}

// TestThrottledRequestsAreDebugRecords keeps a refused client from setting the
// pace of the log, the same way a denied one cannot.
func TestThrottledRequestsAreDebugRecords(t *testing.T) {
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var atInfo, atDebug strings.Builder
	for _, tc := range []struct {
		into  *strings.Builder
		level int
	}{{&atInfo, 0}, {&atDebug, -4}} {
		h := newHandlerWithConfig(t, gangway.Config{
			DockerSocket:    socket,
			MaxConcurrent:   1,
			MaxRate:         1,
			UpstreamTimeout: time.Second,
			Logger:          levelLogger(tc.into, tc.level),
		})
		for range 20 {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_ping", nil))
		}
	}

	if atInfo.Len() != 0 {
		t.Errorf("throttling logged at info: %s", atInfo.String())
	}
	if !strings.Contains(atDebug.String(), "throttled docker API request") {
		t.Errorf("throttling missing from the debug log: %s", atDebug.String())
	}
}
