package gangway_test

import (
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

func TestConfigDefaultsAndBoundaries(t *testing.T) {
	defaults := newHandlerWithConfig(t, gangway.Config{DockerSocket: "/unused.sock"})
	if defaults.MaxResponseBytes() != gangway.DefaultMaxResponseBytes ||
		defaults.Capacity() != gangway.DefaultMaxConcurrent ||
		defaults.UpstreamTimeout() != gangway.DefaultUpstreamTimeout {
		t.Fatalf("unexpected defaults: bytes = %d concurrent = %d timeout = %s",
			defaults.MaxResponseBytes(), defaults.Capacity(), defaults.UpstreamTimeout())
	}

	// The documented extremes must survive construction unchanged, including
	// the transport limits derived from them.
	for _, cfg := range []gangway.Config{
		{DockerSocket: "/unused.sock", MaxResponseBytes: 1, MaxConcurrent: 1, UpstreamTimeout: 100 * time.Millisecond},
		{DockerSocket: "/unused.sock", MaxResponseBytes: 64 << 20, MaxConcurrent: 4096, UpstreamTimeout: time.Minute},
	} {
		h := newHandlerWithConfig(t, cfg)
		if h.MaxResponseBytes() != cfg.MaxResponseBytes || h.Capacity() != cfg.MaxConcurrent || h.UpstreamTimeout() != cfg.UpstreamTimeout {
			t.Errorf("configuration not preserved: %#v", cfg)
		}
		transport := h.Transport()
		if transport.MaxConnsPerHost != cfg.MaxConcurrent ||
			transport.MaxIdleConnsPerHost != cfg.MaxConcurrent ||
			transport.MaxResponseHeaderBytes != 64<<10 {
			t.Errorf("transport resource limits not configured: %#v", transport)
		}
	}

	for name, cfg := range map[string]gangway.Config{
		"missing socket":       {},
		"negative bytes":       {DockerSocket: "/unused.sock", MaxResponseBytes: -1},
		"large bytes":          {DockerSocket: "/unused.sock", MaxResponseBytes: 64<<20 + 1},
		"negative concurrency": {DockerSocket: "/unused.sock", MaxConcurrent: -1},
		"large concurrency":    {DockerSocket: "/unused.sock", MaxConcurrent: 4097},
		"negative timeout":     {DockerSocket: "/unused.sock", UpstreamTimeout: -time.Second},
		"short timeout":        {DockerSocket: "/unused.sock", UpstreamTimeout: 100*time.Millisecond - 1},
		"long timeout":         {DockerSocket: "/unused.sock", UpstreamTimeout: time.Minute + 1},
		"negative rate":        {DockerSocket: "/unused.sock", MaxRate: -1},
		"large rate":           {DockerSocket: "/unused.sock", MaxRate: 100_001},
		"empty label prefix":   {DockerSocket: "/unused.sock", LabelPrefixes: []string{"spiffe.io/", ""}},
		"too many prefixes":    {DockerSocket: "/unused.sock", LabelPrefixes: make([]string, 65)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := cfg.WithDefaults().Validate(); err == nil {
				t.Error("invalid configuration passed validation")
			}
			if h, err := gangway.NewHandler(cfg); err == nil {
				h.Close()
				t.Fatal("invalid configuration accepted")
			}
		})
	}

	// Omitting Logger must work independently of the test helper.
	h, err := gangway.NewHandler(gangway.Config{DockerSocket: "/unused.sock"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.HasLogger() {
		t.Fatal("default logger is nil")
	}
}
