package gangway

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
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
)

// Exit codes. Usage errors are separated from runtime failures so a supervisor
// can tell a misconfiguration from an outage.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

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
		return fmt.Errorf("invalid proxy configuration: %w", err)
	}

	if err := ValidatePaths(opts.ListenSocket, proxy.DockerSocket); err != nil {
		return fmt.Errorf("invalid socket configuration: %w", err)
	}

	proxy.Logger = log

	handler, err := NewHandler(proxy)
	if err != nil {
		return fmt.Errorf("invalid proxy configuration: %w", err)
	}
	defer handler.Close()

	socket, err := Listen(opts.ListenSocket, opts.SocketMode)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	maxConnections := connectionsPerRequest * proxy.MaxConcurrent
	listener := LimitConnections(socket, maxConnections)
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
		"upstream_timeout", proxy.UpstreamTimeout.String(),
	)

	err = Serve(ctx, server, listener, shutdownTimeout)
	if err == nil {
		log.Info("gangway stopped")
	}

	return err
}
