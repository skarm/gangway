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
than queueing, and a request refused for want of a slot receives 503 without
spending a token — neither limit is charged for work Docker never did, so a
client retrying into a busy proxy cannot exhaust the budget the requests being
served need. The bucket holds four seconds of requests, and never fewer than
`--max-concurrent` of them, so an agent that restarts and re-attests every
workload at once is absorbed rather than refused; raise the rate for a node that
sustains more than twenty-five attestations a second, which is two inspects
each.

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
is not — measured across everything a request holds at once: the bytes read from
the daemon, the map they decoded to, the label filter applied to it, and the
reply encoded beside them. `--label-prefix` adds nothing to that, because it
edits the decoded map rather than building a second one. The worst case in
flight is the product of the two limits times 16, logged as
`worst_case_memory_bytes` at start-up: 128 MiB at the defaults. Give the process
at least twice that, and expect a warning in the log when `GOMEMLIMIT` is set
below it. `GOMEMLIMIT` on its own only makes the collector work harder; it
cannot reclaim a response a request is still decoding.

Exit status 2 is a configuration mistake and status 1 is a runtime failure, a
distinction `RestartPreventExitStatus=2` rests on. An out-of-range value, a
relative path, a `--listen-socket` that is not already clean, and one path
written for both sockets are all refused before either socket is touched; so are
the mistakes that can only be seen once the filesystem has been read, such as a
listen path that resolves onto the Docker socket through a symlink, or a
`--docker-socket` that is not a socket. A path that is spelled correctly and
fails on what happens to be on disk — a socket directory that is a regular file,
a listen socket another instance still holds — exits 1, because the next start
may well find it fixed.

On `SIGTERM`/`SIGINT` the service stops accepting connections and drains within
the shutdown timeout; the service manager's stop timeout must exceed it.

## Deployment options

Where the SPIRE agent runs, and whether it goes through this proxy, decide what
the proxy is worth. Two questions settle it before any configuration matters:

1. **Is the daemon rootful?** For a rootless daemon the socket is not
   root-equivalent, and none of this is needed — give the agent the real socket.
2. **Is the agent root?** A root agent opens `/var/run/docker.sock` whatever you
   configure, so a proxy in front of it changes nothing.

This proxy is for one case: an agent that is **not** root and needs
`docker:label` or `docker:image` selectors. The rest of this section is here so
you can tell whether that is the case you have.

| Agent runs | Docker access | What a compromised agent gets |
| --- | --- | --- |
| Host, root | real socket | host root — it already had it |
| Host, root | this proxy | host root; the proxy changes nothing |
| Host, unprivileged, group `docker` | real socket | **host root** |
| Host, unprivileged | this proxy | container labels and images |
| Container, `pid: host`, root | real socket mounted in | host root |
| Container, `pid: host`, unprivileged | this proxy's socket mounted in | container labels and images |

In every row the agent can also issue an SVID for any workload registered
against that node: it performs attestation itself, so a compromised agent means
compromised identities on that node regardless. This proxy bounds escalation to
host root, not the damage inside the trust domain. Nothing here substitutes for
protecting the agent.

The agent must run in the **host PID namespace** in every row. It reads the
caller's PID from the connection, then resolves it to a container through
`/proc/<pid>/cgroup`; a PID from any other namespace resolves to the wrong
process or to none. For a containerized agent that means `pid: host`.

### Agent on the host, no proxy

Simplest, and correct if you have decided the agent is a trusted component of
that machine. As root it reads any `/proc/<pid>` and reaches Docker directly.

What does **not** work here is making a non-root agent safe by giving it the
socket some other way. Membership in group `docker` is root-equivalent; so is an
ACL on the socket file (`setfacl -m u:spire-agent:rw`), which grants one user
instead of a group but the same full API. A read-only bind mount does not make
the API read-only either. Docker's authorization plugins identify a client by
its TLS certificate, which a Unix socket connection does not present, so they
cannot express "this local client may only inspect" — a second TLS listener with
client certificates can, at considerably more cost than this proxy.

A root agent has one option a non-root agent does not: **drop the Docker
attestor entirely.** Under root the `unix` attestor reads `/proc/<pid>/exe`,
which resolves inside the container's mount namespace, so `unix:sha256` and
`unix:path` identify the workload's real binary with no Docker socket in the
picture at all. That beats both direct access and this proxy. The cost is that
one image is one hash: two containers from the same image are
indistinguishable, and every rebuild changes the selector. Worth it when the
workloads are few and stable.

### Agent on the host, behind the proxy

The case this proxy exists for. The agent runs as its own unprivileged user and
belongs to no privileged group; `gangway` is the only account in group `docker`.
A compromised agent can then read container labels and image references and
nothing else. See [Run on the host](#run-on-the-host).

The cost is the `unix:sha256` and `unix:path` selectors: reading
`/proc/<pid>/exe` needs root or the workload's own UID. `unix:uid`, `unix:gid`
and every `docker:` selector still work, because `/proc/<pid>/cgroup` is
world-readable — unless `/proc` is mounted with `hidepid`, which breaks the
Docker attestor for a non-root agent outright. Check that first.

### Agent in a container, no proxy

Needs `pid: host` like every other row, plus the real socket mounted in. The
container then holds a root-equivalent socket, so its `cap_drop`, `read_only`
and user settings decorate a process that can start a privileged container
whenever it likes. This is the configuration that looks hardened and is not.

### Agent in a container, behind the proxy

What [Run with Docker Compose](#run-with-docker-compose) sets up: the proxy
holds the real socket in one container, and the agent gets a socket that answers
the three endpoints at the top of this file. Mount the proxy's socket **directory** into the agent, give the
agent `pid: host`, and keep the real socket out of it.

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
who may connect. The socket's directory has to let that group through as well:
the proxy creates one as `0750` whatever the umask says, and refuses to serve a
`0660` socket from a directory prepared without group access, rather than
running where no client can reach it.

## Run on the host

Two accounts, neither of which is root: `gangway` is the only member of group
`docker`, and the agent reaches the proxy through a group they share.

```sh
sudo groupadd --system spire
sudo useradd --system --gid spire --no-create-home --shell /usr/sbin/nologin gangway
sudo useradd --system --gid spire --no-create-home --shell /usr/sbin/nologin spire-agent
sudo usermod --append --groups docker gangway
```

`Group=spire` below is the proxy's *primary* group, so the socket it creates
belongs to `spire` and the agent can open it at mode `0660`. `--allow-uid` then
pins it to the agent's UID, so widening the mode later does not widen who may
connect; the proxy's own user is always allowed, which is what keeps
`--healthcheck` working.

```ini
# /etc/systemd/system/gangway.service
[Unit]
Description=Docker API proxy for the SPIRE agent
Wants=docker.service
After=docker.service

[Service]
User=gangway
Group=spire
SupplementaryGroups=docker
RuntimeDirectory=gangway
RuntimeDirectoryMode=0750
Environment=GOMEMLIMIT=448MiB
ExecStart=/usr/local/bin/gangway \
    --listen-socket=/run/gangway/docker.sock \
    --docker-socket=/var/run/docker.sock \
    --socket-mode=0660 \
    --allow-uid=SPIRE_AGENT_UID
Restart=always
RestartPreventExitStatus=2
TimeoutStopSec=70
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
PrivateNetwork=yes
MemoryMax=512M
TasksMax=128
LimitNOFILE=1024

[Install]
WantedBy=multi-user.target
```

`RuntimeDirectory=gangway` creates `/run/gangway` as `gangway:spire` mode
`0750`, which is what the proxy requires: owned by its own user, not writable by
group or others, and traversable by the group the `0660` socket is for. It is
removed on stop, taking the socket and lock file with it.

The rest mirrors the Compose file. `PrivateNetwork=yes` is the equivalent of
`network_mode: none` and costs nothing, because the proxy speaks only to Unix
sockets; `ProtectSystem=strict` still permits connecting to one, since a
read-only mount blocks writes to files and directories but not to sockets.
`MemoryMax`, `GOMEMLIMIT`, `TasksMax` and `LimitNOFILE` are sized for the
default response and concurrency limits — raise them along with those flags,
never the flags alone — and `TimeoutStopSec` must exceed the shutdown timeout.
`RestartPreventExitStatus=2` is what stops a misconfiguration from becoming a
restart loop: the proxy exits 2 for a usage error and 1 for a runtime failure.

`Wants=`, not `Requires=`, in both directions. The proxy answers 502 while the
daemon is away and recovers on its own, so it need not be torn down with a
Docker restart, and the agent has no reason to stop when the proxy does.

The agent's unit needs `Wants=gangway.service` and `After=gangway.service`, or
its first attestations arrive before the socket exists. Two things about its own
Workload API socket are worth setting deliberately:

- Put it under `/run`, not the default below `/tmp`. Any unit with
  `PrivateTmp=yes` gets a private `/tmp`, and a socket there is invisible to
  every workload — a failure that looks like the agent never started.
- Workloads reach it by opening that socket, so the path must be readable from
  wherever they run. For containerized workloads, bind-mount the **directory**
  into each container and point `SPIFFE_ENDPOINT_SOCKET` at it; mounting the
  socket file pins an inode the agent replaces on restart. SPIRE's boundary at
  that socket is attestation, not file permissions — check the mode your version
  creates rather than assuming it restricts anything.

```hcl
agent {
    data_dir     = "/var/lib/spire/agent"
    socket_path  = "/run/spire/agent/api.sock"
    trust_domain = "example.org"
}
```

## Connect SPIRE

Point the Docker attestor at the proxy's socket instead of Docker's. On the
host that is the `--listen-socket` path directly:

```hcl
WorkloadAttestor "docker" {
    plugin_data {
        docker_socket_path = "unix:///run/gangway/docker.sock"
    }
}
```

For a containerized agent, mount the proxy's socket **directory** — not the
socket file, which pins an inode the proxy replaces on restart — read-only into
the agent (`proxy-socket:/run/gangway:ro` in the same Compose project, with
`depends_on: {gangway: {condition: service_healthy}}`), and use the same path
inside it. That agent also needs `pid: host`, as every deployment does; see
[Deployment options](#deployment-options).

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
  than this code. Keep `govulncheck` in CI — it is the control that matters
  most.
- **The gain is the blast radius of a compromised client, not of the proxy.** An
  agent that is already root gains nothing from having the Docker socket taken
  away, and a compromised agent can issue every SVID on its node either way.
  [Deployment options](#deployment-options) has the cases side by side; read it
  before assuming this proxy is buying you something.
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
  Saturation returns HTTP 503, an exceeded rate returns HTTP 429, and a request
  answered with either costs the other limit nothing. Idle accepted
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
  response it cannot parse is an `ERROR`, and a daemon answering `5xx` is a
  `WARN` at most once every 30 seconds. An operator should not have to enable
  debug logging to learn that attestation has stopped working. The trade-off is
  that a daemon that stays down produces a record per attempt; `--max-rate` is
  what bounds how fast that can be.
- What a *client* provokes stays a debug record — a denied request, a throttled
  one, a rejected socket peer, and a `4xx` Docker chose, such as the 404 for a
  container that has already exited. None of those are faults here, and keeping
  them at debug is what stops a client from turning its own traffic into log
  volume, or into contention on the logging handler. Run with
  `--log-level=debug` to see them, and expect a client that is being rejected to
  set the pace of the log while it is on. The `5xx` warnings the rate limit
  drops are there too.
- Start-up, shutdown, and faults in the proxy itself, including a recovered
  handler panic, are at the default level.
