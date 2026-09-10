package gangway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	readHeaderTimeout = 2 * time.Second
	readTimeout       = 5 * time.Second
	idleTimeout       = 30 * time.Second
	maxHeaderBytes    = 8 << 10
	// writeTimeoutHeadroom is the slack added to the upstream timeout so a
	// response arriving just before the deadline can still be written, and so a
	// graceful shutdown outlives the requests it is draining.
	writeTimeoutHeadroom = 5 * time.Second
)

// NewServer builds the HTTP server for handler. Its deadlines are derived from
// upstreamTimeout so that a slow but legitimate Docker response is never cut
// off by the server itself.
func NewServer(handler http.Handler, upstreamTimeout time.Duration, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.Default()
	}

	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      upstreamTimeout + writeTimeoutHeadroom,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		// Keep net/http's own diagnostics in the same JSON stream as the rest
		// of the service, at error level. Everything it writes for a server
		// like this one reports a fault here rather than a misbehaving client:
		// a recovered handler panic, a malformed header this code set, or an
		// Accept failure. A malformed request produces no record at all.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
}

// Serve runs server until ctx is done, then drains in-flight requests within
// timeout. Cancelling ctx must stop new connections without cancelling active
// requests, so the caller is kept alive until Shutdown has drained them or the
// deadline expires.
func Serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	select {
	case err := <-served:
		_ = server.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := server.Shutdown(shutdownCtx)
	if err != nil {
		// The deadline passed with requests still running; close them out
		// rather than leaving the process alive around them.
		_ = server.Close()
	}

	serveErr := <-served

	if err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}

	return nil
}
