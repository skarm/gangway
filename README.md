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

Everything else is rejected, including request bodies and query strings, and
including the collection endpoints (`/images/json`, `/containers/json`) whose
path is an inspect prefix and suffix with nothing between them. All other
response fields, `Config.Env` among them, are dropped. Client headers,
credentials, redirects, and Docker error bodies are never forwarded: a Docker
failure reaches the client as a status code and the proxy's own fixed message,
with none of the daemon's text in it.

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
- `--max-response-bytes=1048576` (1 byte to 64 MiB of upstream JSON)
- `--max-concurrent=8` (1 to 4096; the listener accepts at most four
  connections per slot)
- `--max-rate=50` (Docker API requests per second, 0 to 100000; `0` disables the
  limit)
- `--allow-uid=`, `--allow-gid=` (comma-separated peer IDs; empty allows anyone
  the socket permissions admit)
- `--label-prefix=` (comma-separated container label key prefixes to relay;
  empty relays every label)
- `--shutdown-timeout=0` (0 means upstream timeout plus 5s; explicit up to 2m)
- `--log-level=info` (`debug`, `info`, `warn` or `error`)

`--max-concurrent` bounds how many requests are open at once; `--max-rate`
bounds how many Docker is asked to serve over time, which the first does not:
eight slots that each turn over in a millisecond are eight thousand inspects a
second against the daemon. Excess requests receive HTTP 429 immediately rather
than queueing. The bucket holds four seconds of requests, so an agent that
restarts and re-attests every workload at once is absorbed rather than refused;
raise the rate for a node that sustains more than twenty-five attestations a
second, which is two inspects each.

`--allow-uid` and `--allow-gid` check the peer's credentials (`SO_PEERCRED`) as
the kernel reports them, which a client cannot forge and which holds even if the
socket's permissions are widened later. `--allow-gid` matches the peer's
**primary** group, not the group the socket mode grants, so the two are set
separately. The proxy's own user is always allowed, because `--healthcheck`
reaches the proxy through the same socket. A policy that cannot be enforced —
anywhere without `SO_PEERCRED` — exits with status 2 rather than running with
the check silently skipped.

`--label-prefix` is data minimisation, not access control: labels are relayed
for selectors, and they routinely carry whatever the orchestrator put on the
container. With the option set, only labels whose key starts with one of the
prefixes leave the proxy — set it to the prefixes your registration entries
actually select on, and a label added somewhere else cannot reach a client just
because it exists.

`--max-response-bytes` and `--max-concurrent` together decide how much memory
the process needs. A response costs up to sixteen times its own size in live
heap once decoded — the JSON is the small part, the map its labels decode into
is not — so the worst case in flight is their product times 16, logged as
`worst_case_memory_bytes` at start-up: 128 MiB at the defaults. Give the process
at least twice that, and expect a warning in the log when `GOMEMLIMIT` is set
below it. `GOMEMLIMIT` on its own only makes the collector work harder; it
cannot reclaim a response a request is still decoding.

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
- `mem_limit` and `GOMEMLIMIT` are sized for the default response and
  concurrency limits. Raise them along with those flags, never the flags alone.

The socket defaults to mode `0600`, so SPIRE must run as UID 1000 or root. For
a separate SPIRE UID, use `--socket-mode=0660` and give SPIRE GID 1000; only
trusted clients should belong to that group. Add `--allow-uid=<spire-uid>` once
you know it — with the containers sharing the host's user namespace, that is the
UID the SPIRE container runs as — so that widening the mode later does not widen
who may connect. The socket's directory has to let
that group through as well: the proxy creates one as `0750` whatever the umask
says, and refuses to serve a `0660` socket from a directory prepared without
group access, rather than running where no client can reach it.

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
`docker:env` selectors do not, because `Config.Env` is removed. If you set
`--label-prefix`, every label your entries select on must match one of the
prefixes, or those entries stop matching. SPIRE still
needs its normal host process/cgroup access — this proxy only replaces its
Docker API connection. See the
[SPIRE Docker Workload Attestor documentation](https://github.com/spiffe/spire/blob/main/doc/plugin_agent_workloadattestor_docker.md).

No released SPIRE version has been validated end to end with this proxy (SPIRE
`main` was inspected on 2026-09-11). Check your registration entries and
selectors against your own SPIRE and Docker versions before rollout.

## Security boundary

What this proxy is worth depends entirely on what the client would otherwise
have. Read this section before deploying it.

- The proxy does not make the Docker socket safe; it moves who holds it. Access
  to the real socket stays root-equivalent for a rootful daemon, and a
  compromised proxy is one `POST /containers/create` away from a privileged
  container, so `cap_drop`, `read_only` and `network_mode: none` raise the bar
  to reaching that point and bound nothing after it. What actually keeps the bar
  high is the size of the attack surface: no dependencies, no cgo, a memory-safe
  language, and a parser surface that is `net/http` and `encoding/json` rather
  than this code. Keep `govulncheck` in CI — it is the control that matters most.
- **The gain is the blast radius of a compromised client, not of the proxy.** A
  SPIRE agent that already runs as root on the host, with host PID namespace,
  for its process and cgroup attestors gains almost nothing from having the
  Docker socket taken away, and this proxy buys little there. It pays off when
  the agent is containerized and unprivileged, which the Compose setup above
  assumes. Check which one you are actually deploying.
- The single largest improvement available is outside this code: run a
  **rootless** Docker daemon, and the socket stops being root-equivalent at all.
- Socket permissions decide which local clients may use the proxy, and
  `--allow-uid`/`--allow-gid` add a check the kernel makes and a client cannot
  forge. Neither filters the metadata per workload: any client that reaches the
  socket can inspect any container whose 64-hex ID it knows. Labels are
  preserved intentionally and can contain sensitive data; `--label-prefix`
  narrows that to the labels your entries select on.
- Upstream timeouts, response-size limits, bounded concurrency and `--max-rate`
  keep a single request from growing without bounds and keep a client from
  turning a bounded number of slots into unbounded work against the daemon.
  Saturation returns HTTP 503, an exceeded rate returns HTTP 429. Idle accepted
  connections are reaped after two seconds without a request header, or a
  30-second keep-alive idle timeout.
- The response-size and concurrency limits are the memory bound described under
  the flags, and the response that reaches it is an ordinary one: nothing about
  a container whose labels are many short keys is hostile. Sizing them past the
  memory the process has turns a burst of attestations into an OOM kill.
- Docker failures return a status code and a fixed message. Nothing the daemon
  wrote — no message field, no header, no redirect target — is forwarded, and
  logs are JSON on stdout that never contain Docker response bodies.
- **Faults are logged as they happen, at a level the default configuration
  shows**: a daemon the proxy cannot reach or that times out is a `WARN`, a
  response it cannot parse is an `ERROR`. An operator should not have to enable
  debug logging to learn that attestation has stopped working. The trade-off is
  that a daemon that stays down produces a record per attempt; `--max-rate` is
  what bounds how fast that can be.
- What a *client* provokes stays a debug record — a denied request, a throttled
  one, a rejected socket peer, and a status Docker chose, such as the 404 for a
  container that has already exited. None of those are faults here, and keeping
  them at debug is what stops a client from turning its own traffic into log
  volume, or into contention on the logging handler. Run with
  `--log-level=debug` to see them, and expect a client that is being rejected to
  set the pace of the log while it is on.
- Start-up, shutdown, and faults in the proxy itself, including a recovered
  handler panic, are at the default level.
