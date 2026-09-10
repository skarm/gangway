package gangway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	// userAgent identifies the proxy to Docker; no client header is forwarded.
	userAgent = "gangway"
	// upstreamAuthority is a placeholder authority for the Unix-socket
	// transport, which resolves the address from the socket path instead.
	upstreamAuthority = "docker"
	upstreamHost      = "http://" + upstreamAuthority

	maxResponseHeaderBytes = 64 << 10
	upstreamIdleTimeout    = 30 * time.Second
)

// Handler serves the allowed Docker API surface. It is safe for concurrent use
// and owns the single upstream connection pool.
type Handler struct {
	client           *http.Client
	transport        *http.Transport
	maxResponseBytes int64
	// inFlight holds one token per request being forwarded to Docker, so
	// len(inFlight) is the number of upstream requests in progress.
	inFlight chan struct{}
	log      *slog.Logger
}

// NewHandler builds a Handler from cfg, applying defaults before validating.
func NewHandler(cfg Config) (*Handler, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	dialer := &net.Dialer{Timeout: cfg.UpstreamTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", cfg.DockerSocket)
		},
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		MaxIdleConns:           cfg.MaxConcurrent,
		MaxIdleConnsPerHost:    cfg.MaxConcurrent,
		MaxConnsPerHost:        cfg.MaxConcurrent,
		IdleConnTimeout:        upstreamIdleTimeout,
		ResponseHeaderTimeout:  cfg.UpstreamTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}

	return &Handler{
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.UpstreamTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		transport:        transport,
		maxResponseBytes: cfg.MaxResponseBytes,
		inFlight:         make(chan struct{}, cfg.MaxConcurrent),
		log:              cfg.Logger,
	}, nil
}

// UpstreamTimeout reports the resolved per-request timeout, which the server
// needs in order to size its write and shutdown deadlines around it.
func (h *Handler) UpstreamTimeout() time.Duration { return h.client.Timeout }

// Close releases idle upstream connections. It does not cancel active requests;
// stop serving first if that matters.
func (h *Handler) Close() { h.transport.CloseIdleConnections() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	route, ok := Classify(r)
	if !ok {
		h.logDenied(r)
		writeError(w, http.StatusForbidden, bodyNotAllowed)
		return
	}

	if !h.acquire() {
		writeError(w, http.StatusServiceUnavailable, bodyBusy)
		return
	}
	defer h.release()

	switch route.Kind {
	case RoutePing:
		h.handlePing(w, r, route.Path)
	case RouteContainerInspect:
		h.handleContainerInspect(w, r, route.Path)
	case RouteImageInspect:
		h.handleImageInspect(w, r, route.Path)
	default:
		// Unreachable: Classify reports only the routes handled above.
		writeError(w, http.StatusForbidden, bodyNotAllowed)
	}
}

// logDenied records a rejection. Rejections are debug records: they are the
// one thing an untrusted client can ask for without limit, and at any higher
// level a flood would turn its own traffic into log volume and into serialized
// writes on the logging handler's mutex. The level is checked before the path
// is measured or the arguments are boxed, so a proxy at the default level does
// no work here at all.
func (h *Handler) logDenied(r *http.Request) {
	ctx := r.Context()
	if !h.log.Enabled(ctx, slog.LevelDebug) {
		return
	}

	h.log.DebugContext(ctx, "denied docker API request",
		"method", r.Method, "path", pathForLog(r.URL.Path))
}

// acquire takes an upstream slot without blocking, so a saturated proxy answers
// immediately instead of queueing work it cannot start.
func (h *Handler) acquire() bool {
	select {
	case h.inFlight <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Handler) release() { <-h.inFlight }
