package gangway

import (
	"bytes"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
)

// labelAlphabet is 64 characters that need no JSON escaping, which is what lets
// the keys below stay distinct while staying as short as they can.
const labelAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_"

// labelKey returns the i-th distinct key in the shortest spelling available.
func labelKey(i int) string {
	if i == 0 {
		return labelAlphabet[:1]
	}

	var key []byte
	for i > 0 {
		key = append(key, labelAlphabet[i%len(labelAlphabet)])
		i /= len(labelAlphabet)
	}

	return string(key)
}

// denseLabelsBody builds the container inspect response of at most size bytes
// that costs the most to decode: the JSON is nearly all Labels, and every entry
// is the shortest distinct key with an empty value, so each byte of body buys
// as much map as it can. A single large label value, or a response padded with
// fields the proxy drops, costs a fraction of this.
func denseLabelsBody(size int) []byte {
	const open, tail = `{"Config":{"Image":"i","Labels":{`, `}}}`

	body := make([]byte, 0, size)
	body = append(body, open...)

	for i := 0; ; i++ {
		entry := `"` + labelKey(i) + `":""`
		if i > 0 {
			entry = "," + entry
		}

		if len(body)+len(entry)+len(tail) > size {
			break
		}

		body = append(body, entry...)
	}

	return append(body, tail...)
}

// liveHeap reports the heap that survives collection. Two cycles, because the
// first can leave finalizable objects behind for the second to reclaim.
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	return int64(stats.HeapAlloc)
}

// TestWorstCaseMemoryBoundsASaturatedProxy pins the load case the sizing
// formula exists for, and the one an OOM kill comes out of: every concurrency
// slot holding a maximal, worst-shaped response at the same time, which is the
// state a burst of attestations puts the proxy in. Every response is decoded
// concurrently and all of them are kept alive together, because what the
// process has to fit is their sum and not the largest of them.
//
// Each case carries the same total of upstream JSON, so the rows differ only in
// how that total is split between response size and concurrency. The 1 MiB row
// is the shipped configuration.
func TestWorstCaseMemoryBoundsASaturatedProxy(t *testing.T) {
	for _, tc := range []struct {
		maxResponseBytes int64
		maxConcurrent    int
	}{
		{maxResponseBytes: 4 << 10, maxConcurrent: 2048},
		// Where the cost per byte peaks, so the row with the least margin
		// against the factor is the one that runs.
		{maxResponseBytes: 16 << 10, maxConcurrent: 512},
		{maxResponseBytes: 64 << 10, maxConcurrent: 128},
		{maxResponseBytes: DefaultMaxResponseBytes, maxConcurrent: DefaultMaxConcurrent},
		{maxResponseBytes: 4 << 20, maxConcurrent: 2},
	} {
		cfg := Config{
			DockerSocket:     "/unused.sock",
			MaxResponseBytes: tc.maxResponseBytes,
			MaxConcurrent:    tc.maxConcurrent,
		}.WithDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}

		body := denseLabelsBody(int(cfg.MaxResponseBytes))
		if int64(len(body)) > cfg.MaxResponseBytes {
			t.Fatalf("test body of %d bytes exceeds the limit it was built for", len(body))
		}

		base := liveHeap()

		// One slot's worth of state each: what the response decoded to, and the
		// response the proxy encoded from it.
		decoded := make([]containerInspect, cfg.MaxConcurrent)
		encoded := make([]*bytes.Buffer, cfg.MaxConcurrent)

		var slots sync.WaitGroup
		for i := range decoded {
			slots.Go(func() {
				if err := decodeJSON(bytes.NewReader(body), cfg.MaxResponseBytes, &decoded[i]); err != nil {
					t.Error(err)
					return
				}

				buf := getBuffer()
				if err := encodeJSON(buf, containerResponse{Config: *decoded[i].Config}); err != nil {
					t.Error(err)
					return
				}

				encoded[i] = buf
			})
		}
		slots.Wait()
		if t.Failed() {
			return
		}

		peak := liveHeap() - base
		runtime.KeepAlive(decoded)
		runtime.KeepAlive(encoded)

		budget := cfg.WorstCaseMemoryBytes()
		t.Logf("%d x %d B: %d bytes live, %d budgeted (x%.1f of the JSON, factor %d)",
			cfg.MaxConcurrent, cfg.MaxResponseBytes, peak, budget,
			float64(peak)/float64(int64(cfg.MaxConcurrent)*cfg.MaxResponseBytes), responseMemoryFactor)

		if peak > budget {
			t.Errorf("%d requests of %d bytes hold %d bytes of live heap, over the %d bytes WorstCaseMemoryBytes promises",
				cfg.MaxConcurrent, cfg.MaxResponseBytes, peak, budget)
		}
	}
}

// TestMemoryLimitWarningTracksTheConfiguredLimits covers the one place the
// limits are held against the memory the runtime was actually given. A
// configuration that does not fit has to say so at start-up; the alternative is
// an operator learning it from an exit code during the first burst.
func TestMemoryLimitWarningTracksTheConfiguredLimits(t *testing.T) {
	defaults := Config{DockerSocket: "/unused.sock"}.WithDefaults()
	worstCase := defaults.WorstCaseMemoryBytes()

	// The limit is process-wide, so it goes back to whatever this binary was
	// started with before the next test runs.
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })

	for name, tc := range map[string]struct {
		limit int64
		warn  bool
	}{
		"the documented deployment": {limit: 448 << 20},
		"exactly the headroom":      {limit: worstCase * memoryHeadroomFactor},
		"a byte under it":           {limit: worstCase*memoryHeadroomFactor - 1, warn: true},
		"only the worst case":       {limit: worstCase, warn: true},
		"no limit to check against": {limit: math.MaxInt64},
	} {
		t.Run(name, func(t *testing.T) {
			var records bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&records, &slog.HandlerOptions{Level: slog.LevelWarn}))

			debug.SetMemoryLimit(tc.limit)
			warnOnMemoryLimit(log, defaults)

			if warned := records.Len() > 0; warned != tc.warn {
				t.Errorf("warned = %v, want %v (limit %d, worst case %d): %s",
					warned, tc.warn, tc.limit, worstCase, records.String())
			}
		})
	}
}
