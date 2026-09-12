package gangway_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
		opts.Proxy.MaxRate != gangway.DefaultMaxRate ||
		len(opts.Proxy.LabelPrefixes) != 0 ||
		!opts.Peers.Empty() ||
		opts.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", opts)
	}

	opts, err = gangway.Parse([]string{
		"--socket-mode=0660", "--upstream-timeout=30s", "--shutdown-timeout=1m", "--healthcheck",
		"--log-level=DEBUG", "--max-rate=0", "--label-prefix=spiffe.io/, com.example/",
	}, io.Discard)
	if err != nil || opts.SocketMode != 0o660 || opts.Proxy.UpstreamTimeout != 30*time.Second ||
		opts.ShutdownTimeout != time.Minute || !opts.Healthcheck || opts.LogLevel != slog.LevelDebug ||
		opts.Proxy.MaxRate != 0 ||
		!slices.Equal(opts.Proxy.LabelPrefixes, []string{"spiffe.io/", "com.example/"}) {
		t.Fatalf("custom options = %+v, error = %v", opts, err)
	}

	// A peer policy is only parsed here; whether it can be enforced is decided
	// by the platform, and refused at parse time where it cannot be.
	if gangway.PeerCredentialsSupported() {
		opts, err = gangway.Parse([]string{"--allow-uid=1001,0", "--allow-gid=50"}, io.Discard)
		if err != nil || !slices.Equal(opts.Peers.UIDs, []uint32{1001, 0}) ||
			!slices.Equal(opts.Peers.GIDs, []uint32{50}) {
			t.Fatalf("peer policy = %+v, error = %v", opts.Peers, err)
		}
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
		{"--max-rate=-1"}, {"--max-rate=100001"}, {"--max-rate=abc"},
		{"--allow-uid=nobody"}, {"--allow-uid=-1"}, {"--allow-uid=1000,"}, {"--allow-uid=,"},
		{"--allow-gid=wheel"}, {"--allow-gid=4294967296"},
		{"--label-prefix=a,,b"}, {"--label-prefix=,"},
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

// TestMainExitsTwoForEveryConfigurationMistake holds the exit codes to what the
// README promises a service manager, which is the whole point of having two of
// them: RestartPreventExitStatus=2 stops a misconfigured proxy rather than
// restarting it into the same refusal until the start limit runs out. Half of
// these are only decided once the filesystem has been read, well past the point
// where the command line has had its say, so asserting that Run returns *an*
// error would not tell an operator which of the two codes they get.
func TestMainExitsTwoForEveryConfigurationMistake(t *testing.T) {
	dir := socketDir(t)
	dockerSocket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", dockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// A name that resolves onto the Docker socket only once the link in it has
	// been expanded, which is the aliasing ValidatePaths exists for.
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(dockerSocket, alias); err != nil {
		t.Fatal(err)
	}

	regularFile := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(regularFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	listen := "--listen-socket=" + filepath.Join(dir, "gangway.sock")

	// Every case below has to be refused before the proxy starts serving. One
	// that is not would block until the whole test binary timed out, which is a
	// failure nobody can read, so Main is given a deadline of its own.
	mainExit := func(t *testing.T, args []string) int {
		t.Helper()

		exit := make(chan int, 1)
		go func() { exit <- gangway.Main(args, io.Discard, io.Discard) }()

		select {
		case code := <-exit:
			return code
		case <-timeAfterAwait():
			t.Fatal("Main never returned: the configuration was accepted and the proxy is serving")
			return 0
		}
	}

	for name, args := range map[string][]string{
		"a relative listen socket":     {"--listen-socket=gangway.sock"},
		"a relative docker socket":     {"--docker-socket=docker.sock"},
		"one path spelled for both":    {"--listen-socket=" + dockerSocket, "--docker-socket=" + dockerSocket},
		"an unclean listen socket":     {"--listen-socket=" + dir + "/./gangway.sock", "--docker-socket=" + dockerSocket},
		"a listen socket aliasing it":  {"--listen-socket=" + alias, "--docker-socket=" + dockerSocket},
		"a docker socket that is not":  {listen, "--docker-socket=" + regularFile},
		"a limit outside its range":    {listen, "--max-concurrent=0"},
		"an unparseable socket mode":   {listen, "--socket-mode=0777"},
		"an empty label prefix":        {listen, "--label-prefix=a,,b"},
		"a shutdown timeout too short": {listen, "--shutdown-timeout=1ms"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mainExit(t, args); got != 2 {
				t.Errorf("exit = %d, want 2, for %v", got, args)
			}
		})
	}

	// The other side of the contract: a path that is spelled correctly and
	// fails on what the filesystem happens to hold is an outage rather than a
	// mistake, and a supervisor has to keep restarting it. Here the socket
	// directory is a regular file, which the next start may well find fixed.
	unusable := []string{"--listen-socket=" + filepath.Join(regularFile, "gangway.sock"), "--docker-socket=" + dockerSocket}
	if got := mainExit(t, unusable); got != 1 {
		t.Errorf("exit = %d for a socket directory that is a regular file, want 1", got)
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
