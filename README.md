# gangway

Small Unix-socket proxy that exposes only the Docker Engine requests needed by
SPIRE's Docker Workload Attestor and strips everything else from the responses.
One Go executable, standard library only. Targets Linux.

## Allowed API

| Request | Response contains |
| --- | --- |
| `HEAD /_ping`, `GET /_ping` | status only (unversioned, for client version negotiation) |
| `GET /vX.Y/containers/<64-hex-id>/json` | `Config.Image`, `Config.Labels` |
| `GET /vX.Y/images/<image-ref>/json` | `Id`, `RepoDigests` |

Everything else is rejected, including request bodies and query strings. All
other response fields, `Config.Env` among them, are dropped. Client headers,
credentials, redirects, and raw Docker error bodies are never forwarded.

## Build and test

Go 1.27 or newer.

```sh
go build -trimpath -o bin/gangway .
go vet ./...
go test -race ./...
```

`main.go` only wires up the process; everything else lives in
`internal/gangway`, one file per concern.

## Run the binary

The user needs permission to connect to Docker and to create the output socket.

```sh
./bin/gangway --listen-socket="$PWD/.run/docker.sock" --docker-socket=/var/run/docker.sock
```

From a second terminal:

```sh
./bin/gangway --healthcheck --listen-socket="$PWD/.run/docker.sock"
```

`--healthcheck` sends `HEAD /_ping` through the listening proxy with a fixed
two-second deadline and exits non-zero if the proxy or Docker is unavailable.

Flags (`--help` for the full list):

- `--listen-socket=/run/gangway/docker.sock`
- `--docker-socket=/var/run/docker.sock`
- `--socket-mode=0600` (or `0660`)
- `--upstream-timeout=5s` (100ms to 1m)
- `--max-response-bytes=4194304` (1 byte to 64 MiB of upstream JSON)
- `--max-concurrent=64` (1 to 4096; the listener accepts at most four
  connections per slot)
- `--shutdown-timeout=0` (0 means upstream timeout plus 5s; explicit up to 2m)
- `--log-level=info` (`debug`, `info`, `warn` or `error`)

Out-of-range values exit with status 2 without touching either socket. On
`SIGTERM`/`SIGINT` the service stops accepting connections and drains within the
shutdown timeout; the service manager's stop timeout must exceed it.

## Run with Docker Compose

```sh
export DOCKER_GID="$(stat -c '%g' /var/run/docker.sock)"
docker compose up -d --build --wait
docker compose exec gangway /gangway --healthcheck
```

The container runs as `1000:1000` in a read-only `scratch` image with no
network, dropped capabilities, and its own health check.

- `/var/run` is bind-mounted read-only at `/run/docker-host`. Mount the
  **parent directory**, not the socket file, so the proxy can reconnect after
  Docker replaces the socket on restart. For Docker elsewhere, set
  `DOCKER_SOCKET_DIR` and read `DOCKER_GID` from that socket.
- `proxy-socket` is a named volume at `/run/gangway`. Docker initializes it
  from the image directory (`1000:1000`, mode `0750`) on first use, so start
  the proxy before mounting the volume into SPIRE. For a bind mount instead,
  prepare the directory first with
  `sudo install -d -o 1000 -g 1000 -m 0750 /run/gangway`; it must be owned by
  the proxy's UID and never writable by others.
- `stop_grace_period` is 70s, which covers the automatic shutdown timeout for
  all supported upstream settings. Raise it for a longer explicit timeout.

The socket defaults to mode `0600`, so SPIRE must run as UID 1000 or root. For
a separate SPIRE UID, use `--socket-mode=0660` and give SPIRE GID 1000; only
trusted clients should belong to that group.

## Connect SPIRE

Mount the output **directory** into the SPIRE container at `/run/gangway`
read-only (`proxy-socket:/run/gangway:ro` in the same Compose project, with
`depends_on: {gangway: {condition: service_healthy}}`).

```hcl
WorkloadAttestor "docker" {
    plugin_data {
        docker_socket_path = "unix:///run/gangway/docker.sock"
    }
}
```

Leave `docker_version` unset to negotiate the API version. Labels, image
references, and image digests are preserved, so label selectors work;
`docker:env` selectors do not, because `Config.Env` is removed. SPIRE still
needs its normal host process/cgroup access — this proxy only replaces its
Docker API connection. See the
[SPIRE Docker Workload Attestor documentation](https://github.com/spiffe/spire/blob/main/doc/plugin_agent_workloadattestor_docker.md).

No released SPIRE version has been validated end to end with this proxy (SPIRE
`main` was inspected on 2026-09-11). Check your registration entries and
selectors against your own SPIRE and Docker versions before rollout.

## Security boundary

- Access to the real Docker socket stays root-equivalent for a rootful daemon,
  and a read-only bind mount does **not** make the Docker API read-only. The
  proxy process is trusted; keep the real socket out of the SPIRE container.
- Socket permissions decide which local clients may use the proxy, but the
  allowed metadata is not filtered per workload. Labels are preserved
  intentionally and can contain sensitive data.
- Upstream timeouts, response-size limits, and bounded concurrency keep a
  single request from growing without bounds; saturation returns HTTP 503.
  Idle accepted connections are reaped after two seconds without a request
  header, or a 30-second keep-alive idle timeout.
- Docker failures and invalid responses return sanitized errors. Logs are JSON
  on stdout and never contain Docker response bodies.
- At the default level the log carries start-up, shutdown, and faults in the
  proxy itself, including a recovered handler panic. Everything a request can
  produce — rejections and upstream failures — is a debug record, so a client
  cannot turn its own traffic into log volume or into contention on the logging
  handler. Run with `--log-level=debug` to see those, and expect a client that
  is being rejected to set the pace of the log while it is on.
