package gangway

import (
	"bytes"
	"io"
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
//
// keyPrefix goes in front of every key, which is what lets a case configure a
// label allowlist that keeps all of them or none. It makes each key one byte
// longer and so the body very slightly less dense, which is why the rows
// measuring the decoder itself leave it empty.
func denseLabelsBody(size int, keyPrefix string) []byte {
	const open, tail = `{"Config":{"Image":"i","Labels":{`, `}}}`

	body := make([]byte, 0, size)
	body = append(body, open...)

	for i := 0; ; i++ {
		entry := `"` + keyPrefix + labelKey(i) + `":""`
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
// What a slot holds is measured across the whole path a response takes, not
// only its ends: the bytes read from Docker, the map they decoded to, the label
// filter applied to that map, and the response encoded beside it. The filter is
// on this path because it is the one step that could hold a second copy of the
// most expensive object in the process, and a bound measured without it would
// be a bound on a different program.
//
// The first rows carry the same total of upstream JSON and differ only in how
// it is split between response size and concurrency; the 1 MiB row is the
// shipped configuration. The last two add an allowlist at the size where the
// cost per byte peaks, keeping every label and then none of them.
func TestWorstCaseMemoryBoundsASaturatedProxy(t *testing.T) {
	for _, tc := range []struct {
		name             string
		maxResponseBytes int64
		maxConcurrent    int
		keyPrefix        string
		labelPrefixes    []string
	}{
		{name: "4 KiB x 2048", maxResponseBytes: 4 << 10, maxConcurrent: 2048},
		// Where the cost per byte peaks, so the row with the least margin
		// against the factor is the one that runs.
		{name: "16 KiB x 512", maxResponseBytes: 16 << 10, maxConcurrent: 512},
		{name: "64 KiB x 128", maxResponseBytes: 64 << 10, maxConcurrent: 128},
		{name: "the defaults", maxResponseBytes: DefaultMaxResponseBytes, maxConcurrent: DefaultMaxConcurrent},
		{name: "4 MiB x 2", maxResponseBytes: 4 << 20, maxConcurrent: 2},
		{
			name: "16 KiB x 512 keeping every label", maxResponseBytes: 16 << 10, maxConcurrent: 512,
			keyPrefix: "z", labelPrefixes: []string{"z"},
		},
		{
			name: "16 KiB x 512 keeping no label", maxResponseBytes: 16 << 10, maxConcurrent: 512,
			keyPrefix: "z", labelPrefixes: []string{"q"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				DockerSocket:     "/unused.sock",
				MaxResponseBytes: tc.maxResponseBytes,
				MaxConcurrent:    tc.maxConcurrent,
				LabelPrefixes:    tc.labelPrefixes,
				Logger:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
			}.WithDefaults()

			h, err := NewHandler(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()

			body := denseLabelsBody(int(cfg.MaxResponseBytes), tc.keyPrefix)
			if int64(len(body)) > cfg.MaxResponseBytes {
				t.Fatalf("test body of %d bytes exceeds the limit it was built for", len(body))
			}

			base := liveHeap()

			// One slot's worth of state each: the bytes read off the socket,
			// what they decoded to, and the response encoded from it.
			raw := make([][]byte, cfg.MaxConcurrent)
			decoded := make([]containerInspect, cfg.MaxConcurrent)
			filtered := make([]containerConfig, cfg.MaxConcurrent)
			encoded := make([]*bytes.Buffer, cfg.MaxConcurrent)

			var slots sync.WaitGroup
			for i := range decoded {
				slots.Go(func() {
					// A private copy, because every slot reads its own response
					// off its own connection and no two share the bytes.
					raw[i] = append([]byte(nil), body...)

					if err := decodeJSON(bytes.NewReader(raw[i]), cfg.MaxResponseBytes, int64(len(raw[i])), &decoded[i]); err != nil {
						t.Error(err)
						return
					}

					config := *decoded[i].Config
					config.Labels = h.filterLabels(config.Labels)
					// Both the decoded response and the filtered one are held,
					// because in the handler both are reachable while the reply
					// is encoded: the decoded value is a live local until the
					// request returns. A filter that builds a second map is a
					// second copy of the most expensive object in the process,
					// and this is the moment at which it exists.
					filtered[i] = config

					buf := getBuffer()
					if err := encodeJSON(buf, containerResponse{Config: config}); err != nil {
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
			runtime.KeepAlive(raw)
			runtime.KeepAlive(decoded)
			runtime.KeepAlive(filtered)
			runtime.KeepAlive(encoded)

			budget := cfg.WorstCaseMemoryBytes()
			t.Logf("%d x %d B: %d bytes live, %d budgeted (x%.1f of the JSON, factor %d)",
				cfg.MaxConcurrent, cfg.MaxResponseBytes, peak, budget,
				float64(peak)/float64(int64(cfg.MaxConcurrent)*cfg.MaxResponseBytes), responseMemoryFactor)

			if peak > budget {
				t.Errorf("%d requests of %d bytes hold %d bytes of live heap, over the %d bytes WorstCaseMemoryBytes promises",
					cfg.MaxConcurrent, cfg.MaxResponseBytes, peak, budget)
			}
		})
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
