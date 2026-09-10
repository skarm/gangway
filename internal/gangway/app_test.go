package gangway_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

func TestParseDefaults(t *testing.T) {
	opts, err := gangway.Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ListenSocket != gangway.DefaultListenSocket ||
		opts.Proxy.DockerSocket != gangway.DefaultDockerSocket ||
		opts.SocketMode != 0o600 ||
		opts.Proxy.UpstreamTimeout != gangway.DefaultUpstreamTimeout ||
		opts.Proxy.MaxResponseBytes != gangway.DefaultMaxResponseBytes ||
		opts.Proxy.MaxConcurrent != gangway.DefaultMaxConcurrent ||
		opts.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", opts)
	}

	opts, err = gangway.Parse([]string{
		"--socket-mode=0660", "--upstream-timeout=30s", "--shutdown-timeout=1m", "--healthcheck",
		"--log-level=DEBUG",
	}, io.Discard)
	if err != nil || opts.SocketMode != 0o660 || opts.Proxy.UpstreamTimeout != 30*time.Second ||
		opts.ShutdownTimeout != time.Minute || !opts.Healthcheck || opts.LogLevel != slog.LevelDebug {
		t.Fatalf("custom options = %+v, error = %v", opts, err)
	}

	if _, err := gangway.Parse([]string{"--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v", err)
	}
}

// TestParseRejectsOutOfRangeFlags pins that a limit outside its documented
// range is a usage error. Every proxy flag has a non-zero default, so an
// explicit zero is a mistake rather than a request for the default.
func TestParseRejectsOutOfRangeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--socket-mode=0666"}, {"--socket-mode=garbage"},
		{"--shutdown-timeout=-1s"}, {"--shutdown-timeout=99ms"}, {"--shutdown-timeout=121s"},
		{"--upstream-timeout=invalid"}, {"unexpected"}, {"--unknown"},
		{"--docker-socket="},
		{"--max-response-bytes=0"}, {"--max-response-bytes=-1"}, {"--max-response-bytes=67108865"},
		{"--max-concurrent=0"}, {"--max-concurrent=-1"}, {"--max-concurrent=4097"},
		{"--upstream-timeout=0"}, {"--upstream-timeout=1ms"}, {"--upstream-timeout=61s"},
		{"--log-level="}, {"--log-level=trace"}, {"--log-level=INFO+1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := gangway.Parse(args, io.Discard); err == nil {
				t.Fatal("invalid arguments accepted")
			}
		})
	}
}

func TestMainExitCodes(t *testing.T) {
	var usage bytes.Buffer
	if got := gangway.Main([]string{"--help"}, io.Discard, &usage); got != 0 || !strings.Contains(usage.String(), "listen-socket") {
		t.Fatalf("help exit = %d, output = %q", got, usage.String())
	}
	if got := gangway.Main([]string{"--socket-mode=0777"}, io.Discard, io.Discard); got != 2 {
		t.Fatalf("invalid arguments exit = %d, want 2", got)
	}
	unavailable := []string{"--healthcheck", "--listen-socket=" + filepath.Join(socketDir(t), "absent.sock")}
	if got := gangway.Main(unavailable, io.Discard, io.Discard); got != 1 {
		t.Fatalf("unavailable healthcheck exit = %d, want 1", got)
	}
}

// TestRunDrainsActiveRequestOnCancellation is the graceful-shutdown contract:
// the socket disappears at once, but an in-flight request still completes.
func TestRunDrainsActiveRequestOnCancellation(t *testing.T) {
	upstreamStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)

	dockerSocket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		select {
		case <-release:
			_, _ = io.WriteString(w, "OK")
		case <-r.Context().Done():
			t.Error("active upstream request was canceled during graceful shutdown")
		}
	}))

	listenSocket := filepath.Join(socketDir(t), "proxy.sock")
	opts, err := gangway.Parse([]string{
		"--listen-socket=" + listenSocket, "--docker-socket=" + dockerSocket,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var startOnce sync.Once
	logger := jsonLogger(writerFunc(func(p []byte) {
		if bytes.Contains(p, []byte("gangway started")) {
			startOnce.Do(func() { close(started) })
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- gangway.Run(ctx, opts, logger) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("run exited before startup: %v", err)
	case <-timeAfterAwait():
		t.Fatal("server did not start")
	}

	client := unixClient(t, listenSocket)
	response := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://docker/_ping")
		if err == nil {
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			switch {
			case readErr != nil:
				err = readErr
			case resp.StatusCode != http.StatusOK || string(body) != "OK":
				err = errors.New("active request did not receive a complete successful response")
			}
		}
		response <- err
	}()

	awaitSignal(t, upstreamStarted, "active upstream request")
	cancel()
	awaitSocketRemoved(t, listenSocket)
	requireQuiet(t, done, "run returning before the active request finished")

	releaseRequest()
	if err := awaitError(t, response, "active response"); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, done, "graceful shutdown"); err != nil {
		t.Fatal(err)
	}
}

func TestServeForcesCloseAfterShutdownDeadline(t *testing.T) {
	path := filepath.Join(socketDir(t), "proxy.sock")
	listener, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	started, canceled := make(chan struct{}), make(chan struct{})
	server := gangway.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}), time.Second, discardLogger())
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- gangway.Serve(ctx, server, listener, 100*time.Millisecond) }()

	client := unixClient(t, path)
	requestDone := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://docker/_ping")
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()

	awaitSignal(t, started, "active request")
	cancel()
	if err := awaitError(t, done, "forced shutdown"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	awaitSignal(t, canceled, "canceled handler")
	_ = awaitError(t, requestDone, "closed client request")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listen socket remains after shutdown: %v", err)
	}
}

func TestRunRejectsInvalidConfigurationWithoutCreatingSocket(t *testing.T) {
	for name, proxy := range map[string]gangway.Config{
		"negative concurrency":   {DockerSocket: gangway.DefaultDockerSocket, MaxConcurrent: -1},
		"short timeout":          {DockerSocket: gangway.DefaultDockerSocket, UpstreamTimeout: time.Millisecond},
		"large response limit":   {DockerSocket: gangway.DefaultDockerSocket, MaxResponseBytes: 64<<20 + 1},
		"relative docker socket": {DockerSocket: "docker.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(socketDir(t), "proxy.sock")
			opts := gangway.Options{ListenSocket: path, SocketMode: 0o600, Proxy: proxy}
			if err := gangway.Run(context.Background(), opts, discardLogger()); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("socket created for invalid configuration: %v", err)
			}
		})
	}
}

// TestServerAccommodatesTheFullUpstreamTimeout keeps the write deadline from
// cutting off a Docker response that arrives just inside its own timeout.
func TestServerAccommodatesTheFullUpstreamTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{100 * time.Millisecond, gangway.DefaultUpstreamTimeout, time.Minute} {
		server := gangway.NewServer(http.NotFoundHandler(), timeout, nil)
		if server.WriteTimeout <= timeout {
			t.Errorf("write timeout %s cannot accommodate upstream timeout %s", server.WriteTimeout, timeout)
		}
		if server.ErrorLog == nil {
			t.Error("net/http diagnostics are not routed into the structured logger")
		}
	}
}

func TestProbe(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			path := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead || r.URL.Path != "/_ping" {
					t.Errorf("health request = %s %s", r.Method, r.URL.Path)
				}
				// A redirect must be reported, never followed.
				w.Header().Set("Location", "http://unreachable.invalid/")
				w.WriteHeader(status)
			}))
			ctx, cancel := context.WithTimeout(context.Background(), awaitTimeout)
			defer cancel()
			if err := gangway.Probe(ctx, path); (err == nil) != (status == http.StatusOK) {
				t.Fatalf("status %d: probe error = %v", status, err)
			}
		})
	}

	t.Run("unavailable socket", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), awaitTimeout)
		defer cancel()
		if err := gangway.Probe(ctx, filepath.Join(socketDir(t), "absent.sock")); err == nil {
			t.Fatal("unavailable socket reported healthy")
		}
	})
}

// TestServerDiagnosticsStayVisible pins that net/http's own error log is not
// demoted along with the request-driven records. It is where a recovered
// handler panic surfaces, and every record it carries for this server reports
// a fault in the proxy rather than a misbehaving client.
func TestServerDiagnosticsStayVisible(t *testing.T) {
	var logged strings.Builder
	// A default handler logs at info, so a debug-level bridge writes nothing.
	server := gangway.NewServer(http.NotFoundHandler(), time.Second,
		slog.New(slog.NewJSONHandler(&logged, nil)))

	server.ErrorLog.Printf("http: panic serving 127.0.0.1: boom")

	if !strings.Contains(logged.String(), `"level":"ERROR"`) ||
		!strings.Contains(logged.String(), "panic serving") {
		t.Fatalf("net/http diagnostics = %q", logged.String())
	}
}
