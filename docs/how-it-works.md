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
 laptop ─┐                    ┌─ deploy.<tailnet>.ts.net   control: /v1 API, /mcp
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
3. **Live revision down** (reboot, crash, fresh restore): pull its pinned
   images, start the pod, wait for health, then point the edge at it.
4. **Operation running**: move it forward one step at a time (below).
5. **Leftovers**: stop replaced revisions after their drain window, remove
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
per-service ID. Slices are never reused (old volumes and backups keep their
numeric ownership), so the default range supports 64 services over the
host's lifetime.

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
| `internal/mcpbridge` | stdio MCP bridge, as a device or as a tagged agent |

Invariants worth protecting when changing the code: write state before acting;
keep every reconciler step idempotent; never touch an unlabelled Podman
object; never return a secret value; never move traffic to an unhealthy
revision.
