package gangway

import (
	"sync"
	"time"
)

// burstSeconds is how many seconds of tokens the bucket holds, so an agent that
// restarts and re-attests every workload at once is absorbed rather than
// refused, while the sustained rate still bounds what Docker can be made to do.
const burstSeconds = 4

// bucket is a token bucket refilled lazily, on the request that reads it. A
// background refill would be a goroutine and a timer per proxy for a value only
// ever read from one place.
//
// It bounds the *rate* of upstream work, which the concurrency limit does not:
// eight slots that each turn over in a millisecond are eight thousand inspects a
// second against the daemon. Concurrency bounds what the proxy holds at once,
// this bounds what it can ask Docker to do over time.
type bucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64
	last     time.Time
}

// newBucket returns a bucket refilling at rate tokens per second and holding
// capacity of them. A non-positive rate returns nil, which callers read as "no
// limit".
func newBucket(rate, capacity float64, now time.Time) *bucket {
	if rate <= 0 {
		return nil
	}

	if capacity < 1 {
		capacity = 1
	}

	return &bucket{tokens: capacity, capacity: capacity, rate: rate, last: now}
}

// allow takes one token if the bucket has one, and never blocks: a request that
// cannot be started now is refused rather than queued, the same way the
// concurrency limit answers saturation immediately.
func (b *bucket) allow(now time.Time) bool {
	if b == nil {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// A clock that went backwards must not mint tokens; it costs the caller at
	// most one refill interval.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.last = now
		b.tokens = min(b.capacity, b.tokens+elapsed.Seconds()*b.rate)
	}

	if b.tokens < 1 {
		return false
	}

	b.tokens--

	return true
}
