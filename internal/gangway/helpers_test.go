package gangway_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

// awaitTimeout bounds every wait in these tests, so a lost signal fails the
// test instead of hanging the run.
const awaitTimeout = 3 * time.Second

// newHandler builds a handler with test-sized limits against dockerSocket.
func newHandler(t *testing.T, dockerSocket string) *gangway.Handler {
	t.Helper()
	return newHandlerWithConfig(t, gangway.Config{
		DockerSocket:     dockerSocket,
		MaxResponseBytes: 1 << 20,
		MaxConcurrent:    8,
		UpstreamTimeout:  time.Second,
	})
}

func newHandlerWithConfig(t *testing.T, cfg gangway.Config) *gangway.Handler {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	h, err := gangway.NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startDocker serves handler on a Unix socket standing in for the Docker
// Engine, and returns its path.
func startDocker(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return path
}

// socketDir returns a short-lived directory for Unix sockets. macOS limits
// socket paths to 104 bytes, which testing.T.TempDir and the platform's default
// temporary directory can exceed together.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gangway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove socket directory: %v", err)
		}
	})
	return dir
}

func unixClient(t *testing.T, path string) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: awaitTimeout}
}

func awaitSignal(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(awaitTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func awaitError(t *testing.T, event <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-event:
		return err
	case <-time.After(awaitTimeout):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

// requireQuiet fails if event fires within a short grace period, which is how
// these tests assert that something must not happen yet.
func requireQuiet[T any](t *testing.T, event <-chan T, name string) {
	t.Helper()
	select {
	case <-event:
		t.Fatalf("%s happened too early", name)
	case <-time.After(100 * time.Millisecond):
	}
}

// newWireReader wraps a raw HTTP request for http.ReadRequest.
func newWireReader(wire string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(wire))
}

// jsonLogger writes structured records into w, for tests that inspect logs.
// Debug is enabled because everything a request can produce is a debug record.
func jsonLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// timeAfterAwait is the shared deadline for selects that wait on a value.
func timeAfterAwait() <-chan time.Time { return time.After(awaitTimeout) }

// writerFunc adapts a function to io.Writer for log inspection.
type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

// awaitSocketRemoved waits until path no longer exists, which is how shutdown
// announces that it has stopped accepting connections.
func awaitSocketRemoved(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(awaitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("socket remains after shutdown starts")
		case <-tick.C:
		}
	}
}
