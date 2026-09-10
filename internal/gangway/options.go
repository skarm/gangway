package gangway

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenSocket = "/run/gangway/docker.sock"
	defaultDockerSocket = "/var/run/docker.sock"

	minShutdownTimeout = 100 * time.Millisecond
	maxShutdownTimeout = 2 * time.Minute

	defaultLogLevel = "info"
)

// logLevels are the accepted --log-level values. Everything a request can
// produce is a debug record, so info reports only that the service started and
// stopped, and debug is what an operator turns on to see rejections and
// upstream failures.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Options is a fully validated command line.
type Options struct {
	// ListenSocket is the absolute path of the socket exposed to SPIRE.
	ListenSocket string
	// SocketMode is the permission bits of that socket: 0600 or 0660.
	SocketMode os.FileMode
	// ShutdownTimeout bounds request draining; zero derives it from the
	// upstream timeout.
	ShutdownTimeout time.Duration
	// LogLevel is the lowest record level written to stdout.
	LogLevel slog.Level
	// Healthcheck selects the probe mode instead of serving.
	Healthcheck bool
	// Proxy configures the request handler.
	Proxy Config
}

// Parse reads args, writing usage and flag errors to output. It returns
// flag.ErrHelp when help was requested.
func Parse(args []string, output io.Writer) (Options, error) {
	var opts Options
	var socketMode string
	var logLevel string

	flags := flag.NewFlagSet("gangway", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.ListenSocket, "listen-socket", defaultListenSocket, "absolute Unix socket path exposed to SPIRE")
	flags.StringVar(&opts.Proxy.DockerSocket, "docker-socket", defaultDockerSocket, "absolute Docker Engine Unix socket path")
	flags.Int64Var(&opts.Proxy.MaxResponseBytes, "max-response-bytes", DefaultMaxResponseBytes, "maximum Docker JSON response size in bytes")
	flags.IntVar(&opts.Proxy.MaxConcurrent, "max-concurrent", DefaultMaxConcurrent, "maximum concurrent Docker API requests")
	flags.DurationVar(&opts.Proxy.UpstreamTimeout, "upstream-timeout", DefaultUpstreamTimeout, "Docker API request timeout (100ms to 1m)")
	flags.DurationVar(&opts.ShutdownTimeout, "shutdown-timeout", 0, "graceful shutdown timeout (100ms to 2m; 0 uses upstream-timeout + 5s)")
	flags.StringVar(&socketMode, "socket-mode", "0600", "listen socket permissions: 0600 or 0660 (shared primary group)")
	flags.StringVar(&logLevel, "log-level", defaultLogLevel, "lowest level written to stdout: debug, info, warn or error")
	flags.BoolVar(&opts.Healthcheck, "healthcheck", false, "check Docker availability through the listen socket and exit")

	if err := flags.Parse(args); err != nil {
		return Options{}, err
	}

	if flags.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}

	mode, err := strconv.ParseUint(socketMode, 8, 32)
	if err != nil || (mode != 0o600 && mode != 0o660) {
		return Options{}, errors.New("socket-mode must be 0600 or 0660")
	}

	opts.SocketMode = os.FileMode(mode)

	level, ok := logLevels[strings.ToLower(logLevel)]
	if !ok {
		return Options{}, errors.New("log-level must be debug, info, warn or error")
	}

	opts.LogLevel = level

	if opts.ShutdownTimeout != 0 && (opts.ShutdownTimeout < minShutdownTimeout || opts.ShutdownTimeout > maxShutdownTimeout) {
		return Options{}, fmt.Errorf("shutdown-timeout must be between %s and %s, or 0 for automatic", minShutdownTimeout, maxShutdownTimeout)
	}
	// Every proxy flag carries a non-zero default, so an out-of-range value is
	// a usage error rather than a request for the default.
	if err := opts.Proxy.Validate(); err != nil {
		return Options{}, err
	}

	return opts, nil
}
