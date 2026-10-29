# Stack spec

A stack spec is one YAML (or JSON) document describing a service. People and
agents write the same format; `inhouse schema` prints its JSON Schema, and the
MCP `deploy` tool uses it as its input schema.

```yaml
version: 1                   # the spec format; see Versions below
name: notes                  # becomes https://notes.<tailnet>.ts.net
expose: tailnet              # tailnet (default) | funnel
ttl: 6h                      # optional: makes the service ephemeral
update_strategy: recreate    # rolling (default) | recreate (required with volumes)
containers:
  db:                        # sidecars start first, in this order
    image: docker.io/library/postgres:17
    env:
      POSTGRES_USER: notes
    secrets:
      POSTGRES_PASSWORD: notes-db-password   # env var ← secret name
    volumes:
      data: /var/lib/postgresql/data         # volume name → mount path
    health:
      command: ["pg_isready", "-U", "notes"]
  web:                       # the ingress starts last
    image: ghcr.io/example/notes:1.0.0
    port: 8080               # exactly one container sets port
    command: ["/app/server"] # optional: replaces the image entrypoint
    args: ["--listen", ":8080"]
    health:
      path: /healthz
      timeout: 90s
    resources:
      memory: 256Mi
      cpus: 0.5
```

More in [`examples/`](../examples).

## Top level

| Field | Required | Meaning |
| --- | --- | --- |
| `version` | no | The spec format, `1`. Without it the spec is read as version 1, and `plan` and `inhouse deploy` say so. See [Versions](#versions). |
| `name` | yes | 1–40 lowercase letters, digits or hyphens; starts and ends with a letter or digit. Becomes the hostname. `deploy` is reserved for the control node. |
| `expose` | no | `tailnet`: only your tailnet can reach it. `funnel`: also the public Internet, through Tailscale Funnel, without identity headers. Fixed once deployed. |
| `ttl` | no | A duration from `1s` to `720h`. The service is **ephemeral**: it is deleted automatically, node and volumes included, that long after its last successful deploy. Ephemeral services cannot use Funnel. Whether a service is ephemeral is fixed once deployed. |
| `update_strategy` | no | `rolling` starts the new revision beside the old one, so there is no downtime. `recreate` stops the old revision first; it is the default, and required, when any container has volumes. |
| `drain` | no | A duration from `1s` to `24h`: how long a replaced revision keeps serving the requests, WebSockets, streams, raw connections and UDP flows it already has. Defaults to the daemon's 10s. Only rolling updates drain; see [Drain](#drain). |
| `routes` | no | Up to 16 path prefixes served by a container other than the ingress, on the same HTTPS address. See [Routes](#routes). |
| `containers` | yes | 1–16 containers, keyed by name. Names follow the same rule as `name`. |

## Containers

| Field | Meaning |
| --- | --- |
| `image` | Fully qualified reference (`docker.io/library/nginx:1`, not `nginx`). Tags are resolved to digests at deploy time. |
| `port` | The container port that receives HTTPS traffic. Exactly one container, the **ingress**, sets it. |
| `protocol` | Ingress only: what it speaks on `port`. `http` (default) is HTTP/1.1; `h2c` is HTTP/2 without TLS, which gRPC needs. See [Protocols](#protocols). |
| `ports` | Raw TCP or UDP ports served on the service's tailnet addresses. See [Raw TCP and UDP ports](#raw-tcp-and-udp-ports). |
| `command`, `args` | Override the image's entrypoint and arguments. |
| `env` | Environment variables. Values are visible to anyone who can read the service. |
| `secrets` | Environment variable → secret name. Values are never shown. Set them with `inhouse secrets set` first. |
| `volumes` | Volume name → absolute mount path. Volumes belong to the service and survive deploys and rollbacks. |
| `health` | See below. |
| `resources` | `memory` (`4Mi`–`64Gi`, default `512Mi`) and `cpus` (`0.01`–`64`, default `1`). |

## Health checks

A deploy cuts over only after every container passes its check three times in
a row, two seconds apart, before `timeout` (`5s`–`120s`, default `60s`; the
longest in the stack applies).

| You set | Check |
| --- | --- |
| `path: /healthz` (ingress only) | `GET` on the ingress returns 2xx |
| `command: [...]` | The command exits 0 inside the container |
| nothing, on the ingress | A TCP connection to the port succeeds |
| nothing, on a sidecar | The container is still running |

The same checks keep running after cutover, once every 10 seconds. If a
container stops, or the checks fail three times in a row, the service is
marked `degraded` and its pod is restarted, which must pass the checks again
before it gets traffic back. A revision that keeps failing is restarted
less and less often: the second restart in a row waits 10 seconds after the
first, and each one after that waits twice as long, up to 5 minutes. The
count starts over once the revision stays healthy for 10 minutes.

## Protocols

Callers reach every service over HTTPS on port 443, with HTTP/2 or
HTTP/1.1, whichever they negotiate. The edge then speaks to the ingress in
its `protocol`:

- `http` (the default): HTTP/1.1. WebSockets and streaming responses such as
  server-sent events pass through.
- `h2c`: HTTP/2 without TLS, for every request. gRPC needs it, for its
  trailers and bidirectional streams; gRPC-Web and Connect work too. An
  `h2c` upstream can't accept WebSockets, which need an HTTP/1.1 upgrade.

```yaml
containers:
  api:
    image: ghcr.io/example/grpc-api:1.0.0
    port: 9000
    protocol: h2c
```

## Routes

A route sends HTTP requests whose path is `path`, or below it, to another
container port in the stack, instead of to the ingress. The path reaches
the container unchanged, and the longest matching path wins. Requests
through a route are handled exactly like the ingress's: the same identity
headers, the same HTTP/2 front, and the route's own `protocol`.

```yaml
routes:
  - {path: /rtc, container: livekit, port: 7880}              # WebSocket signaling
  - {path: /pkg.Echo, container: grpc, port: 9000, protocol: h2c}
```

| Field | Meaning |
| --- | --- |
| `path` | `/` followed by letters, digits, `.`, `_`, `~`, `-` and further `/`-separated segments, with no trailing slash. `/rtc` matches `/rtc` and `/rtc/v1`, not `/rtcx`. |
| `container` | The container that listens on `port`. |
| `port` | A container port other than the ingress's. Routes may share a port, with one protocol. |
| `protocol` | `http` (default) or `h2c`. |

Routes are for one service's own parts. A routed container has no path
health check; give it a `command` check if it matters.

## Raw TCP and UDP ports

`ports` publishes raw ports on the service's own tailnet addresses, beside
its HTTPS address, each forwarded to the same port in the pod. Use them for
protocols that aren't HTTP: WebRTC media, game and voice servers, SIP, MQTT,
databases.

```yaml
containers:
  media:
    image: docker.io/livekit/livekit-server:v1.9.1
    ports:
      - {port: 7882, protocol: udp}              # one port
      - {port: 7881, protocol: tcp}
      - {port: 50000, protocol: udp, count: 20}  # 50000–50019
```

| Field | Meaning |
| --- | --- |
| `port` | The port on the tailnet, and in the pod. |
| `protocol` | `tcp` or `udp`. |
| `count` | Consecutive ports from `port`, default 1. |

A stack publishes at most 64 raw ports in all. Each `tcp` or `udp` port can
be published once; TCP 443 is the HTTPS address, and a TCP port can't also
be the ingress or a routed port.

How they behave:

- **Tailnet only.** Raw ports are reachable from your tailnet, as your
  policy allows, and never through Funnel, even on a Funnel service.
- **No identity.** There are no headers to add, so the pod sees the edge,
  not the caller. Authenticate callers over HTTPS, where identity headers
  arrive; a WebRTC server does this in signaling, and ICE credentials then
  tie the media to that session.
- **Each UDP peer is its own flow.** Every remote address and port reaches
  the pod from its own loopback address and port, so a server can tell
  peers apart, as a WebRTC server multiplexing every peer on one port must.
  A flow silent both ways for 2 minutes is forgotten, and one listener
  tracks at most 512 flows.
- **Deploys don't move what is open.** New connections and flows go to the
  live revision; open ones stay on the revision they started on until its
  [drain](#drain) ends.
- **Advertise the tailnet address.** A WebRTC server must offer its ICE
  candidates on the address callers reach: `INHOUSE_TAILNET_IPV4` (see
  [Platform environment](#platform-environment)). Peers on a tailnet need no
  STUN or TURN server, and a phone keeps its tailnet address when it moves
  between Wi-Fi and cellular. Keep media packets at WebRTC's usual 1200
  bytes; the tailnet MTU is 1280.

[`examples/livekit.yaml`](../examples/livekit.yaml) runs a LiveKit server
this way.

## Drain

When a rolling deploy cuts over, the replaced revision keeps running for
`drain`, so what it has open can finish: requests, WebSockets, server-sent
events, raw TCP connections and UDP flows. New ones go to the new revision
at once. Once the drain ends, the old revision stops, and anything still
open to it is closed.

The default, 10 seconds, suits requests. Set a longer drain for long-lived
connections: calls, game sessions, streams. A deploy finishes as soon as the
new revision is live; it doesn't wait for the old one's drain. The
replacing revision's `drain` applies.

A `recreate` update stops the old revision before the new one starts, so it
has nothing to drain.

## Platform environment

Every container gets these variables. A container's own `env` wins over
them.

| Variable | Value |
| --- | --- |
| `INHOUSE_SERVICE` | The service name |
| `INHOUSE_REVISION` | The revision number |
| `INHOUSE_DNS_NAME` | The service's MagicDNS name, e.g. `notes.tail1234.ts.net` |
| `INHOUSE_URL` | `https://` and the DNS name |
| `INHOUSE_TAILNET_IPV4` | The service's tailnet IPv4 address, where its raw ports listen |
| `INHOUSE_TAILNET_IPV6` | The service's tailnet IPv6 address |

## What a deploy does with your spec

The stored revision is your spec plus defaults, with every image pinned to an
exact digest and every secret pinned to an exact version. `get_service`
returns it in full. Deploying a spec that pins to the same result as the live
revision changes nothing and reports the live revision.

## Versions

`version` names the format a spec is written in, so a spec keeps meaning the
same thing as inhouse changes. This page describes version 1, the only one
so far.

Changes to the format either only add optional fields, which every spec
written before them keeps working without, or come with a new version.
`protocol`, `ports`, `routes` and `drain` were added this way: a spec
without them deploys, and hashes, exactly as before. A
daemon that reads a new version still accepts every older one, so specs you
have already written keep deploying after an upgrade.

A spec with a newer `version` than the daemon reads is refused with a
message to upgrade it. The CLI, including `inhouse mcp-stdio` and
`mcp-agent`, warns on stderr when it and the daemon are different versions;
upgrade whichever is older.

The version describes the document, not the service: it is not part of the
stored revision, and adding `version: 1` to a spec that was deployed without
one changes nothing.

## Tunnel device

An administrator may set `devices: [/dev/net/tun]` on a container for a userspace VPN. This is the only permitted device. It does not grant host networking or extra capabilities. Preview/deployer identities cannot request devices. The host must provide the device and, on SELinux hosts, permit the container to open it.
