package gangway_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

// origin is the instant every bucket test starts from. The bucket reads its
// clock from the caller, so these tests name the moment they are checking
// instead of sleeping until it has probably arrived.
var origin = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// drain takes every token the bucket holds at now and reports how many that
// was, failing if the bucket never empties.
func drain(t *testing.T, b *gangway.Bucket, now time.Time) int {
	t.Helper()

	for taken := 0; taken <= 1000; taken++ {
		if !b.Allow(now) {
			return taken
		}
	}

	t.Fatal("bucket never emptied")

	return 0
}

// TestBucketHoldsExactlyItsCapacity pins the burst a configuration buys. A
// bucket that starts with more than it says would let the first moment of a
// restart cost the daemon more than the flags allow.
func TestBucketHoldsExactlyItsCapacity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate     float64
		capacity float64
		want     int
	}{
		{name: "four seconds at one a second", rate: 1, capacity: 4, want: 4},
		{name: "the shipped default", rate: 50, capacity: 50 * gangway.BurstSeconds, want: 200},
		// A capacity under one token would refuse every request, so it is
		// raised to a single token rather than making the limit a stop.
		{name: "less than one token", rate: 10, capacity: 0.5, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := gangway.NewBucket(tc.rate, tc.capacity, origin)

			if got := drain(t, b, origin); got != tc.want {
				t.Errorf("bucket allowed %d requests at once, want %d", got, tc.want)
			}
		})
	}
}

// TestBucketRefillsAtTheConfiguredRate is the limit itself: a token appears
// when the configured interval has passed and not before. Both edges are
// checked, because a bucket that refills early is a rate limit in name only.
func TestBucketRefillsAtTheConfiguredRate(t *testing.T) {
	const rate = 4 // one token every 250ms

	b := gangway.NewBucket(rate, 8, origin)
	drain(t, b, origin)

	interval := time.Duration(float64(time.Second) / rate)

	if b.Allow(origin.Add(interval - time.Nanosecond)) {
		t.Errorf("a token appeared %s early", time.Nanosecond)
	}

	if !b.Allow(origin.Add(interval)) {
		t.Errorf("no token after the full %s refill interval", interval)
	}

	if b.Allow(origin.Add(interval)) {
		t.Error("one refill interval produced more than one token")
	}

	// Two and a half intervals of credit is two tokens, and the half does not
	// round up into a third.
	if got := drain(t, b, origin.Add(interval*7/2)); got != 2 {
		t.Errorf("two and a half intervals produced %d tokens, want 2", got)
	}
}

// TestBucketCapsWhatIdleTimeCanAccumulate keeps the limit from turning into a
// quota: a proxy that has been quiet all day must not be able to hand a client
// a day's worth of Docker requests at once.
func TestBucketCapsWhatIdleTimeCanAccumulate(t *testing.T) {
	b := gangway.NewBucket(50, 50*gangway.BurstSeconds, origin)
	drain(t, b, origin)

	if got := drain(t, b, origin.Add(24*time.Hour)); got != 50*gangway.BurstSeconds {
		t.Errorf("a day of idle time allowed %d requests at once, want the %d-token capacity",
			got, 50*gangway.BurstSeconds)
	}
}

// TestBucketMintsNothingWhenTheClockGoesBackwards covers a clock stepped back
// under a running proxy. The bucket may not refill from a negative interval,
// and may not lose the refill it was owed either.
func TestBucketMintsNothingWhenTheClockGoesBackwards(t *testing.T) {
	b := gangway.NewBucket(1, 4, origin)
	drain(t, b, origin)

	if b.Allow(origin.Add(-time.Hour)) {
		t.Error("an hour backwards produced a token")
	}

	if got := b.Tokens(origin); got != 0 {
		t.Errorf("bucket holds %v tokens after the clock went backwards, want 0", got)
	}

	if !b.Allow(origin.Add(time.Second)) {
		t.Error("the refill after a backwards step was lost")
	}
}

// TestBucketWithoutARateIsUnlimited records how "no limit" is represented: an
// absent bucket that admits everything, rather than one with a huge capacity.
func TestBucketWithoutARateIsUnlimited(t *testing.T) {
	for _, rate := range []float64{0, -1} {
		b := gangway.NewBucket(rate, 4, origin)

		if !b.Unlimited() {
			t.Errorf("rate %v built a limiter", rate)
		}

		for i := range 1000 {
			if !b.Allow(origin) {
				t.Fatalf("rate %v refused request %d", rate, i)
			}
		}
	}
}

// TestRateLimitBoundsUpstreamWork is the point of the limit: concurrency alone
// bounds how many requests are open at once, not how many Docker is asked to
// serve. A client that never exceeds one in flight can still drive the daemon
// without bound unless the rate is capped too.
//
// The bounds are the ones the configuration buys — the burst, plus whatever the
// run itself was slow enough to earn back — so a handler wired to a different
// rate or burst than it was configured with fails here.
func TestRateLimitBoundsUpstreamWork(t *testing.T) {
	const rate = 1

	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		MaxConcurrent:   1,
		MaxRate:         rate,
		UpstreamTimeout: time.Second,
	})

	var throttled int

	start := time.Now()

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

	elapsed := time.Since(start)

	// The burst never drops below the concurrency limit, which is 1 here, so
	// this is the four seconds of tokens the rate itself buys.
	burst := int64(rate * gangway.BurstSeconds)
	refilled := int64(math.Ceil(elapsed.Seconds() * rate))

	if throttled == 0 {
		t.Fatal("rate limit never engaged")
	}
	if calls.Load() < burst {
		t.Errorf("only %d of a %d-token burst reached Docker", calls.Load(), burst)
	}
	if calls.Load() > burst+refilled {
		t.Errorf("%d requests reached Docker in %s, more than the %d-token burst and %d refilled",
			calls.Load(), elapsed, burst, refilled)
	}
	if int64(throttled)+calls.Load() != 50 {
		t.Errorf("requests unaccounted for: %d throttled, %d forwarded", throttled, calls.Load())
	}
}

// TestRefusedRequestsDoNotSpendRateTokens holds the two limits against each
// other. A request turned away for want of a slot never reaches Docker, so it
// must not cost a token either: otherwise a client retrying into a saturated
// proxy spends the budget that the requests Docker *is* serving need, and a
// moment of saturation becomes a refusal that outlives it.
func TestRefusedRequestsDoNotSpendRateTokens(t *testing.T) {
	const rate = 1

	var held atomic.Bool
	holding := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)

		if held.CompareAndSwap(false, true) {
			close(holding)
			<-release
		}

		w.WriteHeader(http.StatusOK)
	}))
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		MaxConcurrent:   1,
		MaxRate:         rate,
		UpstreamTimeout: awaitTimeout,
	})

	ping := func() int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		return rr.Code
	}

	start := time.Now()

	// One request occupies the single slot for as long as the test needs.
	go func() {
		defer close(done)

		if code := ping(); code != http.StatusOK {
			t.Errorf("held request status = %d, want 200", code)
		}
	}()
	awaitSignal(t, holding, "the upstream request to start")

	for i := range 3 {
		if code := ping(); code != http.StatusServiceUnavailable {
			t.Fatalf("request %d against a full proxy = %d, want 503", i, code)
		}
	}

	close(release)
	awaitSignal(t, done, "the held request to finish")

	// What the burst has left. Before the slot and the rate were checked in
	// this order, the three 503s above had spent it and this was a 429.
	served := int(calls.Load())

	for range 10 {
		code := ping()
		if code == http.StatusTooManyRequests {
			break
		}

		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 or 429", code)
		}

		served++
	}

	elapsed := time.Since(start)
	burst := rate * gangway.BurstSeconds
	refilled := int(math.Ceil(elapsed.Seconds() * rate))

	if served < burst {
		t.Errorf("Docker served %d requests out of a %d-token burst: refused requests spent tokens",
			served, burst)
	}
	if served > burst+refilled {
		t.Errorf("Docker served %d requests in %s, more than the %d-token burst and %d refilled",
			served, elapsed, burst, refilled)
	}
	if int(calls.Load()) != served {
		t.Errorf("%d requests reached Docker, %d were served", calls.Load(), served)
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

// TestBurstNeverFallsBelowTheConcurrencyLimit keeps the two flags from
// contradicting each other: a burst smaller than the number of requests that
// may be in flight would make the rate limit, not the concurrency limit, decide
// how much work the proxy can start.
func TestBurstNeverFallsBelowTheConcurrencyLimit(t *testing.T) {
	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	// A rate whose four seconds of tokens is far below the concurrency limit.
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket:    socket,
		MaxConcurrent:   32,
		MaxRate:         1,
		UpstreamTimeout: time.Second,
	})

	for i := range 32 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil))

		if rr.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: the burst is below the concurrency limit", i, rr.Code)
		}
	}

	if calls.Load() != 32 {
		t.Errorf("forwarded %d of 32 requests", calls.Load())
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
