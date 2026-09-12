package gangway

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Defaults applied to a zero-valued Config. They are also the documented
// command-line defaults, and their product is chosen so that
// WorstCaseMemoryBytes fits the memory the documented deployment gives the
// process several times over. Both are far above any real inspect response and
// any attestation rate a single node produces.
const (
	DefaultMaxResponseBytes = 1 << 20 // 1 MiB
	DefaultMaxConcurrent    = 8
	DefaultUpstreamTimeout  = 5 * time.Second
	// DefaultMaxRate is the command-line default only; it is not applied by
	// WithDefaults, where zero means "no rate limit" rather than "unset". Two
	// inspects per workload attestation puts it around twenty-five attestations
	// a second sustained, which is far above what a single node produces and
	// far below what a loop can ask of the daemon.
	DefaultMaxRate = 50
)

// responseMemoryFactor is how much live heap one request in flight costs per
// byte of Docker response it is allowed to read. The JSON itself is the small
// part: what a container inspect decodes to is a map, and the shape that costs
// the most per byte is Labels made of many short distinct keys, which alone is
// nine to twelve times the body it came from. The read buffer holding that body
// and the re-encoded response built beside it account for the rest. The label
// allowlist adds nothing: it deletes from the decoded map instead of building a
// second one, which at this factor is the difference between fitting and not.
//
// Measured between 7 and 14 in total, across response limits from 512 bytes to
// 16 MiB, peaking between 8 and 16 KiB and falling above that as a bigger map
// needs longer keys to fill it. Rounded up from that peak: a bound that only
// holds for the average response shape, or for the one limit that happened to
// be measured, is not a bound. The per-connection cost of the HTTP server is
// not in this figure, being a few KiB against a limit counted in MiB.
const responseMemoryFactor = 16

// Accepted range for each configurable limit.
const (
	maxResponseBytesLimit = 64 << 20
	maxConcurrentLimit    = 4096
	maxRateLimit          = 100_000
	minUpstreamTimeout    = 100 * time.Millisecond
	maxUpstreamTimeout    = time.Minute
	// maxLabelPrefixes bounds the allowlist so a filter cannot cost more per
	// label than reading the label did.
	maxLabelPrefixes = 64
)

// Config describes one proxy handler. The zero value is usable through
// WithDefaults; every limit exists to keep a single Docker request, and the
// memory it needs, bounded.
type Config struct {
	// DockerSocket is the absolute path of the upstream Docker Engine socket.
	DockerSocket string
	// MaxResponseBytes caps the JSON the proxy will read from Docker.
	MaxResponseBytes int64
	// MaxConcurrent caps requests in flight to Docker; the rest receive 503.
	MaxConcurrent int
	// UpstreamTimeout bounds a single Docker request, dial through body.
	UpstreamTimeout time.Duration
	// MaxRate caps how many requests a second may be forwarded to Docker; the
	// rest receive 429. Zero means no limit. It bounds sustained load on the
	// daemon, which MaxConcurrent does not: slots that turn over quickly are
	// unbounded work over time. The bucket holds four seconds of requests, so a
	// burst of attestations is absorbed rather than refused.
	MaxRate int
	// LabelPrefixes, when set, keeps only the container labels whose key starts
	// with one of them. Empty relays every label Docker reports. Labels are
	// preserved for selectors, but they routinely carry orchestrator
	// annotations, and an allowlist is the only thing that keeps a label added
	// somewhere else from reaching this proxy's clients.
	LabelPrefixes []string
	// Logger receives denial and failure records. Defaults to slog.Default.
	Logger *slog.Logger
}

// WithDefaults returns cfg with unset limits replaced by their defaults.
// Command-line flags carry the same defaults, so a caller that parses flags
// only needs Validate: there, a zero is an out-of-range value rather than a
// request for the default.
//
// MaxRate is the exception and is left alone: zero is a meaningful value there,
// so a Config built in code is unlimited unless it says otherwise, while the
// command line applies DefaultMaxRate itself.
func (c Config) WithDefaults() Config {
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = DefaultMaxResponseBytes
	}

	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}

	if c.UpstreamTimeout == 0 {
		c.UpstreamTimeout = DefaultUpstreamTimeout
	}

	return c
}

// WorstCaseMemoryBytes reports the live heap the handler can hold when every
// concurrency slot is decoding a response at the size limit. It is the number
// the process must be given memory for, and raising either limit raises it
// proportionally.
//
// GOMEMLIMIT is not a substitute for this bound. It makes the collector work
// harder as the heap grows; it cannot reclaim data a request still needs, so
// a configuration whose worst case does not fit gets an OOM kill instead of
// back pressure.
func (c Config) WorstCaseMemoryBytes() int64 {
	return int64(c.MaxConcurrent) * c.MaxResponseBytes * responseMemoryFactor
}

// Validate reports whether every limit is within its documented range.
func (c Config) Validate() error {
	if c.DockerSocket == "" {
		return errors.New("docker socket path is required")
	}

	if c.MaxResponseBytes < 1 || c.MaxResponseBytes > maxResponseBytesLimit {
		return fmt.Errorf("max response bytes must be between 1 and %d", maxResponseBytesLimit)
	}

	if c.MaxConcurrent < 1 || c.MaxConcurrent > maxConcurrentLimit {
		return fmt.Errorf("max concurrent requests must be between 1 and %d", maxConcurrentLimit)
	}

	if c.UpstreamTimeout < minUpstreamTimeout || c.UpstreamTimeout > maxUpstreamTimeout {
		return fmt.Errorf("upstream timeout must be between %s and %s", minUpstreamTimeout, maxUpstreamTimeout)
	}

	if c.MaxRate < 0 || c.MaxRate > maxRateLimit {
		return fmt.Errorf("max rate must be between 0 and %d", maxRateLimit)
	}

	if len(c.LabelPrefixes) > maxLabelPrefixes {
		return fmt.Errorf("at most %d label prefixes may be configured", maxLabelPrefixes)
	}

	if slices.Contains(c.LabelPrefixes, "") {
		return errors.New("label prefixes must not be empty; omit the option to keep every label")
	}

	return nil
}
