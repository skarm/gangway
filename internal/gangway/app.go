package gangway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

const (
	// healthcheckTimeout bounds the --healthcheck probe. It is deliberately
	// independent of the upstream timeout, so a supervisor's liveness check
	// cannot be stretched by proxy configuration.
	healthcheckTimeout = 2 * time.Second
	// connectionsPerRequest is how many accepted connections each concurrency
	// slot allows. Excess requests already receive 503; this bound keeps a
	// client that leaks sockets from exhausting the file-descriptor limit.
	connectionsPerRequest = 4
	// memoryHeadroomFactor is how much room over Config.WorstCaseMemoryBytes
	// the collector needs for the garbage a saturated proxy produces while
	// holding that much live. Below it, saturation stops being GC pressure and
	// becomes an OOM kill.
	memoryHeadroomFactor = 2
)

// Exit codes. Usage errors are separated from runtime failures so a supervisor
// can tell a misconfiguration from an outage.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// ConfigError marks a failure as a mistake in the configuration rather than a
// fault in the environment. It is what the two exit codes rest on: a supervisor
// running with RestartPreventExitStatus=2 has to stop rather than restart a
// proxy whose flags cannot work in any environment, and has to keep restarting
// one whose Docker socket merely was not there yet.
//
// Only checks whose verdict cannot change on the next start belong here. A path
// that cannot be resolved, a socket that cannot be created and an unreachable
// daemon are all runtime failures, however they were spelled in the unit file.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return e.Err.Error() }

func (e *ConfigError) Unwrap() error { return e.Err }

// configErrorf builds a ConfigError from a format string, the way fmt.Errorf
// builds an error. Wrapping one in turn keeps it a ConfigError, so a caller may
// add its own context without deciding the exit code again.
func configErrorf(format string, args ...any) error {
	return &ConfigError{Err: fmt.Errorf(format, args...)}
}

// Main runs the command and returns the process exit code. Structured logs go
// to stdout; usage errors go to stderr as plain text, before any logger exists.
func Main(args []string, stdout, stderr io.Writer) int {
	opts, err := Parse(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}

		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}

	log := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: opts.LogLevel}))
	if opts.Healthcheck {
		ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
		defer cancel()

		if err := Probe(ctx, opts.ListenSocket); err != nil {
			log.Error("healthcheck failed", "error", err)
			return exitFailure
		}

		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := Run(ctx, opts, log); err != nil {
		// Everything Parse can decide has already exited 2 by here, but the
		// checks that need the filesystem — a listen socket that resolves onto
		// the Docker socket, a docker-socket path that is not a socket at all —
		// can only run in Run, and are configuration mistakes just the same.
		if errors.As(err, new(*ConfigError)) {
			log.Error("invalid configuration", "error", err)
			return exitUsage
		}

		log.Error("proxy stopped with an error", "error", err)

		return exitFailure
	}

	return exitOK
}

// Run serves the proxy until ctx is done. Everything that can fail without side
// effects is checked before the socket is created, so a rejected configuration
// never leaves a socket behind.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	proxy := opts.Proxy.WithDefaults()

	if err := proxy.Validate(); err != nil {
		return configErrorf("invalid proxy configuration: %w", err)
	}

	if err := ValidatePaths(opts.ListenSocket, proxy.DockerSocket); err != nil {
		return fmt.Errorf("invalid socket configuration: %w", err)
	}

	proxy.Logger = log

	handler, err := NewHandler(proxy)
	if err != nil {
		return configErrorf("invalid proxy configuration: %w", err)
	}
	defer handler.Close()

	socket, err := Listen(opts.ListenSocket, opts.SocketMode)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Peer authorization sits directly on the socket, inside the connection
	// limiter, so a client being turned away does not hold a connection slot
	// that a permitted one needs.
	authorized, err := AuthorizePeers(socket, opts.Peers, log)
	if err != nil {
		// A policy this platform cannot enforce is a configuration that can
		// never work here, not an outage: Parse refuses it already, and this is
		// the same refusal for a Run called directly.
		return errors.Join(configErrorf("authorize socket peers: %w", err), socket.Close())
	}

	maxConnections := connectionsPerRequest * proxy.MaxConcurrent
	listener := LimitConnections(authorized, maxConnections)
	defer listener.Close()

	shutdownTimeout := opts.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = proxy.UpstreamTimeout + writeTimeoutHeadroom
	}

	server := NewServer(handler, proxy.UpstreamTimeout, log)
	// Serve already closes the server on every path it returns from; this keeps
	// the ownership visible here and is a no-op after a graceful shutdown.
	defer server.Close()

	log.Info("gangway started",
		"listen_socket", opts.ListenSocket,
		"docker_socket", proxy.DockerSocket,
		"max_concurrent", proxy.MaxConcurrent,
		"max_connections", maxConnections,
		"max_rate", proxy.MaxRate,
		"upstream_timeout", proxy.UpstreamTimeout.String(),
		"worst_case_memory_bytes", proxy.WorstCaseMemoryBytes(),
		"peer_policy", opts.Peers.String(),
		"label_prefixes", len(proxy.LabelPrefixes),
	)
	warnOnMemoryLimit(log, proxy)

	err = Serve(ctx, server, listener, shutdownTimeout)
	if err == nil {
		log.Info("gangway stopped")
	}

	return err
}

// warnOnMemoryLimit reports limits the process does not have the memory for.
// It is the one place the size and concurrency flags are checked against what
// the runtime was actually given, which is what keeps the two from drifting:
// raising either flag without raising GOMEMLIMIT is how a saturated proxy gets
// killed instead of merely slowed down. It is a warning rather than a refusal
// because the worst case needs every slot to hold a maximal response at once,
// which a given deployment may never see.
func warnOnMemoryLimit(log *slog.Logger, proxy Config) {
	// A negative argument reads the limit without setting it, and reports
	// math.MaxInt64 when GOMEMLIMIT is unset. An operator who has not capped
	// the heap has not told us anything to check against.
	limit := debug.SetMemoryLimit(-1)
	if limit == math.MaxInt64 {
		return
	}

	worstCase := proxy.WorstCaseMemoryBytes()
	if worstCase*memoryHeadroomFactor <= limit {
		return
	}

	log.Warn("configured limits exceed the memory this process was given",
		"memory_limit_bytes", limit,
		"worst_case_memory_bytes", worstCase,
		"required_memory_bytes", worstCase*memoryHeadroomFactor,
		"max_concurrent", proxy.MaxConcurrent,
		"max_response_bytes", proxy.MaxResponseBytes,
	)
}
