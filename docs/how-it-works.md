# How inhouse works

inhouse is one Go daemon, `inhoused`, on one Linux host. It runs your
services as rootless Podman pods and gives each one its own node on your
Tailscale tailnet. This page explains the moving parts and why they fit
together the way they do.

## The core idea

Tailscale already solves the hard parts of hosting private software:

| A platform usually needs | inhouse uses |
| --- | --- |
| An edge / load balancer | One embedded tailnet node per service |
| TLS certificates | Each node's `*.ts.net` HTTPS certificate |
| User accounts and API tokens | The caller's tailnet identity (WhoIs) |
| A permission system | App capability grants in your tailnet policy file |
| Firewall rules and a VPN | WireGuard; the host has no public ports |

That leaves inhouse one job: **keep containers running behind those nodes,
and keep reality matching a SQLite database.**

## The picture

```
 laptop ─┐                    ┌─ deploy.<tailnet>.ts.net   control: console, /v1 API, /mcp
 agent  ─┼── tailnet (WireGuard) ─┤
 phone  ─┘                    └─ blog.<tailnet>.ts.net     one node per service
                                       │
 ┌─────────────────── inhoused (one process, user "inhouse") ────────────────────┐
 │  tsnet nodes ── edge proxy ──▶ 127.0.0.1:20007 ──▶ pod svc-blog-r7 (rootless)  │
 │  engine: requests + reconciler        store: SQLite (desired state, history)   │
 └────────────────────────────────────────────────────────────────────────────────┘
   btrfs /var/lib/inhouse: database, node identities, volumes, snapshots, age key
```

Every service is reachable at `https://<service>.<tailnet>.ts.net` with a real
certificate. Nothing listens on a public address.

## Desired state and the reconciler

SQLite is the source of truth for what *should* exist. Podman and the tailnet
nodes are what *does* exist. The reconciler closes the gap.

A request never does slow work itself. `deploy` validates the spec, checks
permissions, and records three things in one transaction: a **pending
revision**, a reserved **host port**, and a running **operation**. Then it
returns the operation ID and nudges the reconciler. Callers wait on the
operation (`wait_for_operation`, `inhouse wait`).

The reconciler runs on start, whenever nudged, and every 30 seconds. Each
pass handles every service in parallel, one pass per service at a time:

1. **Deleted** (tombstoned): stop and remove every pod, delete volumes, delete
   the tailnet device, then forget the service.
2. **Expired** (ephemeral, TTL passed): tombstone it, then as above.
3. **Stopped**: keep the live revision stopped and the edge answering 503
   (see [Restart, stop and start](#restart-stop-and-start)).
4. **Live revision down** (reboot, crash, fresh restore): restart it as
   described in [Keeping live services healthy](#keeping-live-services-healthy).
5. **Operation running**: move it forward one step at a time (below).
6. **Leftovers**: stop replaced revisions after their drain window, remove
   failed revisions' containers after 24 hours, and remove labelled pods that
   have no revision after a 10-minute grace.

State is always written before the action it describes, and every action is
idempotent. A crash or reboot at any point is just picked up by the next
pass. There is no separate recovery procedure.

## A deploy, step by step

```
pending ──▶ starting ──▶ live ──▶ draining ──▶ stopped
   │           │
   └───────────┴──▶ failed   (traffic never moved)
```

1. **Pin.** Pull every image and replace its tag with the exact
   `@sha256:` digest the registry serves. Pin each referenced secret to the
   hash of its current encrypted value. If the result is identical to the
   live revision, the deploy is a no-op and reports the live revision.
2. **Node.** On a service's first deploy, mint a single-use auth key and
   bring up its tailnet node. Record its actual DNS name (Tailscale may have
   suffixed a taken name; inhouse reports the real one).
3. **Start.** Snapshot volumes, create the pod, start sidecars in spec order
   and the ingress last.
4. **Health.** Every container's check must pass three times in a row, two
   seconds apart, within the stack's health timeout.
5. **Cut over.** Make the candidate the live revision in the database, then
   swap the edge's upstream pointer. New requests go to the new pod; requests
   in flight finish on the old one.
6. **Drain.** After ten seconds, stop the old pod, remove it, release its port.

If pulling, starting or health checking fails, the candidate is stopped, its
last 20 log lines are saved as an event, and the operation fails. The live
revision never stopped serving.

**Rollback** creates a *new* revision copied from an old one (r9 = "rollback
to r7"), and runs the same steps. History stays linear, and because r7 is
pinned, r9 runs exactly the bytes r7 ran.

**Recreate updates.** Two revisions must never write the same volume, so a
stack with volumes uses `update_strategy: recreate`: the old revision stops
first, then the volumes are snapshotted, then the candidate starts. Expect a
few seconds of downtime per update. If the candidate fails, the old revision
is started again.

**One operation per service.** A second deploy while one is running is
refused with the running operation's ID, so callers wait instead of racing.
Every mutating request accepts an idempotency key: retrying with the same key
returns the same operation.

## Keeping live services healthy

Between passes, every live revision is probed every 10 seconds: each
container's health check runs once, the same check a deploy waits on. A
service reports how its live revision is doing in `list_services` and
`get_service`:

```json
"health": "degraded",
"health_reason": "3 health checks failed in a row, last: web: HTTP request failed",
"restarts": 2,
"restarted_at": 1791390884
```

A live revision is restarted when a container stops, or when three probes in
a row fail, which catches an app that is still running but has hung:

1. **Degrade.** Mark the service `degraded` with the reason, and point the
   edge at nothing, so callers get a quick 503 instead of a hung request.
2. **Restart.** Record the restart, stop the pod, start it again from its
   pinned images, and wait for health exactly as a deploy does.
3. **Recover.** Once healthy, mark the service `healthy` and point the edge
   back at the pod. If the restart fails, the service stays degraded with
   the failure as its reason.

The first restart is immediate. A revision that keeps failing waits 10
seconds before its second restart in a row, then twice as long each time, up
to 5 minutes, so a crash-looping app doesn't hammer the host or the
timeline. Once it stays healthy for 10 minutes, `restarts` returns to 0 and
the next restart is immediate again. Each step is a `degraded`,
`restarting` or `recovered` event.

A restart only ever reruns the same revision. It never rolls back, and an
operation in progress is unaffected: a deploy still cuts over only to a
healthy candidate. While a recreate update has stopped the live revision on
purpose, it is not probed or restarted.

## Restart, stop and start

These run as operations, one per service at a time like a deploy, and are
allowed to anyone who may deploy the live revision.

- **Restart** (`inhouse restart`, `restart_service`) deploys an exact copy
  of the live revision as a new revision, as a rollback to it would: the
  same image digests and the same secret versions. It follows the service's
  update strategy, so a rolling service keeps serving until the copy is
  healthy; a recreate service is down while the copy starts. Its volumes are
  snapshotted, as on every deploy.
- **Redeploy** (`inhouse redeploy`, `redeploy_service`) deploys the live
  revision's spec again with every secret pinned to its current value. It
  is how a rotated secret reaches a running service. Images keep their
  digests; to pull a newer image, deploy the spec with its tag. If no
  secret changed, it is a no-op.
- **Stop** (`inhouse stop`, `stop_service`) stops the live revision and
  leaves everything else: revisions, volumes, and the tailnet node, which
  answers 503. A stopped service is not probed or restarted, and deploys,
  rollbacks, restarts and redeploys are refused until it is started. An
  ephemeral service still expires on time, and a stopped service can be
  deleted.
- **Start** (`inhouse start`, `start_service`) starts the live revision
  again from its pinned images and gives it traffic once it is healthy, as
  after a reboot. If it doesn't come up healthy, the start fails and the
  service is `degraded`, so it is restarted with backoff like any live
  revision that went down. Stop it again, or start and then deploy a fix.

**Extend** (`inhouse extend`, `extend_service`) restarts an ephemeral
service's TTL from now, as deploying it unchanged would. It takes effect at
once and is recorded as an `extended` event.

## The edge

Each service node runs inside `inhoused` (via `tsnet`) and serves HTTPS on
port 443 with a reverse proxy whose upstream is one atomic pointer. Cutover
is a single pointer store; there is no restart and no dropped connection.
WebSockets and streaming responses pass through.

Before forwarding, the edge deletes every client-sent `Tailscale-User-*`
header, asks Tailscale who the caller is, and sets:

```
Tailscale-User-Login: alice@example.com
Tailscale-User-Name:  Alice Example
```

Your apps can trust these headers and get authentication for free. Tagged
devices (servers, agents) get no user headers. **Funnel** traffic, from the
public Internet, never gets identity headers: Funnel connections are
recognised by their transport, not by anything the client sends.

All nodes live in one process. Upstream swaps never interrupt traffic, but if
`inhoused` itself restarts, every service's HTTPS address is briefly
unavailable even though the pods keep running.

If a service's HTTPS listener stops, the next pass or probe, within 10
seconds, starts it again without a daemon restart. If the service's node
stopped too, a new node takes over from its saved state; one that rejoins
under a different name is refused and retried, since callers know the
service by its name. The live revision keeps running and is still probed
while its listener is down; only the edge restarts. A listener that
keeps stopping waits as a restarted revision does: 10 seconds, then twice as
long each time up to 5 minutes, counting from the first again once it stays
up for 10 minutes. Each restart is a `listener_stopped` event, then
`listener_restarted` once it serves again; a restart that fails is a
`live_unavailable` event.

## Identity and permissions

There are no API tokens. The control node, `deploy.<tailnet>.ts.net`, asks
Tailscale who sent each request and reads that caller's values for your
instance's **capability** (e.g. `example.com/cap/inhouse`) from the policy
file. Each value is a grant with a role: `viewer`, `deployer` or `admin`.
Reaching the control node without a grant gets a 403, recorded as a `denied`
event.

People are identified by login; tagged devices by `node:<stable id>`. That is
how agents get *their own* narrower permissions instead of yours. See
[Access, agents and MCP](access.md).

## Runtime and isolation

Each revision is one pod, `svc-<service>-r<N>`, owned by the unprivileged
`inhouse` user. Containers in a pod share `localhost`, so a web app reaches
its database sidecar without any networking. Only the ingress port is
published, on `127.0.0.1:<host port>`.

Every pod, container and Podman secret carries `inhouse=1` labels naming its
service, revision and spec hash, and inhouse verifies them before touching
anything. Objects without matching labels are never modified.

Every service gets its own fixed slice of 65,536 subordinate user IDs, shared
by all its revisions. Container root maps into that slice, never to the
`inhouse` user that owns the database, keys and node state, and never to
another service's slice. A container escape lands as an unprivileged,
per-service ID. A persistent service's slice is never reused, even after it
is deleted, because its trashed volumes and backups keep their numeric
ownership. An ephemeral service's data is deleted with it, so its slice is
freed for the next service. The default range holds 64 slices: persistent
services ever created plus ephemeral services alive at once.

## Data

**Volumes** belong to the service, not the revision: `blog`'s `data` volume
carries across deploys and rollbacks. Each is a btrfs subvolume, owned by
`inhouse`, with an ACL granting the service's mapped group access. Before a
revision starts, each volume gets a read-only snapshot; the newest five are
kept. An admin rollback with `restore_volumes` replaces live data with the
target revision's snapshots (the previous data moves to trash).

**Secrets** are encrypted with an age key generated on first start
(`keys/age.key`). Values are write-only: no API returns them, plans show only
names, and logs returned by the API have every current and past value
replaced with `[REDACTED]`. A revision pins the exact version it was deployed
with, so changing a secret affects the next deploy, not running revisions.
Values reach containers as revision-scoped Podman secrets, injected as
environment variables. Secrets named `registry-auth/<host>` are image-pull
credentials, sent only to that registry.

**Backups** (optional, see [Operating](operating.md)): Litestream streams the
database to S3-compatible storage continuously; restic backs up nightly
snapshots of every volume plus node identities. The age key is backed up by
you, separately.

## What lives where

| Path | Contents |
| --- | --- |
| `/var/lib/inhouse/state/inhouse.db` | Services, revisions, operations, events, encrypted secrets |
| `/var/lib/inhouse/ts/<node>/` | Each tailnet node's identity and certificate |
| `/var/lib/inhouse/volumes/<service>/<volume>` | Volume data, one btrfs subvolume each |
| `/var/lib/inhouse/snapshots/<service>/<volume>@r<N>` | Read-only pre-deploy snapshots |
| `/var/lib/inhouse/keys/age.key` | Decrypts secrets. Back it up separately |
| `/var/lib/inhouse/containers/` | Podman image and container storage |
| `/etc/inhouse/` | Root-only OAuth secrets, daemon settings, firewall rules |

## Code map

| Package | Role |
| --- | --- |
| `cmd/inhoused` | The daemon: wiring and flags |
| `cmd/inhouse` | The CLI |
| `internal/engine` | Requests and the reconciler: the deploy lifecycle |
| `internal/store` | SQLite schema and every state transition |
| `internal/spec` | Stack spec parsing, validation and JSON Schema |
| `internal/authz` | WhoIs → principal → grants |
| `internal/podman` | Pods over Podman's REST API, label-checked |
| `internal/volumes` | btrfs volumes, snapshots and restore |
| `internal/userns` | Per-service subordinate ID slices |
| `internal/vault` | age-encrypted secrets, pinning and redaction |
| `internal/tailnet` | tsnet nodes and the Tailscale API (keys, device deletion) |
| `internal/edge` | Per-service HTTPS listeners and the swapping proxy |
| `internal/api` | `/v1` JSON API and `/mcp`, over the same engine |
| `internal/console` | Serves the embedded web console build |
| `console/` | The web console's source (TanStack Start, single-page); calls `/v1` |
| `internal/mcpbridge` | stdio MCP bridge, as a device or as a tagged agent |

Invariants worth protecting when changing the code: write state before acting;
keep every reconciler step idempotent; never touch an unlabelled Podman
object; never return a secret value; never move traffic to an unhealthy
revision.
