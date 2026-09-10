package gangway

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Defaults applied to a zero-valued Config. They are also the documented
// command-line defaults.
const (
	DefaultMaxResponseBytes = 4 << 20 // 4 MiB
	DefaultMaxConcurrent    = 64
	DefaultUpstreamTimeout  = 5 * time.Second
)

// Accepted range for each configurable limit.
const (
	maxResponseBytesLimit = 64 << 20
	maxConcurrentLimit    = 4096
	minUpstreamTimeout    = 100 * time.Millisecond
	maxUpstreamTimeout    = time.Minute
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
	// Logger receives denial and failure records. Defaults to slog.Default.
	Logger *slog.Logger
}

// WithDefaults returns cfg with unset limits replaced by their defaults.
// Command-line flags carry the same defaults, so a caller that parses flags
// only needs Validate: there, a zero is an out-of-range value rather than a
// request for the default.
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

	return nil
}
