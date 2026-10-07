# Access, agents and MCP

Two things in your tailnet policy file decide what a caller can do:

1. **Network grants** decide who can reach the control node and each service.
2. **Capability grants** on the control node decide what a caller may *do*.

Reaching the control node without a capability grant gets a 403.

## Who is calling

The control node asks Tailscale who sent each request:

- A **person's device** is the person's login, e.g. `alice@example.com`.
- A **tagged device** (server, CI runner, agent) is `node:<stable node ID>`,
  regardless of who created it. Tagged callers never inherit a person's grants.

Every mutation is recorded as an event attributed to that principal, so
`inhouse events` is also the audit log.

## Grants

Each value under your instance's capability is one grant:

```json
{"role": "deployer", "services": ["preview-*"], "expose": ["tailnet"], "max_ttl": "24h"}
```

| Field | Meaning |
| --- | --- |
| `role` | `viewer`, `deployer` or `admin`. An admin grant contains nothing else. |
| `services` | 1–64 shell patterns on service names (`blog`, `preview-*`). Required for viewer and deployer. |
| `expose` | Exposure modes a deployer may use: `tailnet`, `funnel`. A deployer without it can't deploy. |
| `max_ttl` | If set, the deployer may only deploy ephemeral services with a TTL at or below this. |

A deploy is allowed when **one** grant covers the name, exposure and TTL
together. A grant with an unknown field or invalid value is ignored entirely.

| Action | viewer | deployer | admin |
| --- | --- | --- | --- |
| List, get, plan, logs, events, wait | matching services | matching services | all |
| Deploy, roll back | | matching, within `expose` and `max_ttl` | all |
| Delete | | ephemeral services it created | all |
| Roll back with volume restore | | | yes |
| Set, list, delete secrets | | | yes |

## A policy

[`deploy/policy/policy.hujson`](../deploy/policy/policy.hujson) is a complete
example to merge into your policy. The essential grant:

```json
{"src": ["you@example.com"], "dst": ["tag:inhouse-control"], "ip": ["tcp:443"],
 "app": {"example.com/cap/inhouse": [{"role": "admin"}]}}
```

The capability name is yours to choose (use a domain you control); pass the
same name to `inhoused -capability`.

## Agents and MCP

The control node serves an MCP server at `https://deploy.<tailnet>.ts.net/mcp`
(Streamable HTTP). Its tools call the same engine as the CLI with the same
permission checks:

| Tool | Does |
| --- | --- |
| `whoami` | Your identity and exact grants |
| `list_services`, `get_service` | What exists and whether it is healthy; revisions with pinned specs; recent events |
| `plan_deploy` | Diff a spec against the live revision, with warnings; changes nothing |
| `deploy` | Start a deploy; returns an operation |
| `wait_for_operation` | Block up to 120 s for the result, instead of polling |
| `rollback` | Redeploy an earlier revision (optionally restoring volumes, admin) |
| `get_logs` | A page of a container's output, secrets redacted, with a cursor for more |
| `get_events` | The attributed timeline, including failed containers' last log lines |
| `delete_service` | Delete a service and its node |
| `list_secrets`, `set_secret`, `delete_secret` | Write-only secrets (admin) |

Tools carry honest read-only and destructive annotations, return compact
JSON, and fail with a `code`, `message` and a `hint` naming the next call
(for example, which operation to wait on when a service is busy).

### Connect as yourself

The agent gets your permissions, because its requests come from your device:

```sh
claude mcp add --transport http inhouse https://deploy.<tailnet>.ts.net/mcp
```

For agents that only launch stdio servers, `inhouse mcp-stdio` bridges stdio
to the remote server from this device:

```json
{"mcpServers": {"inhouse": {"command": "inhouse",
  "args": ["-url", "https://deploy.<tailnet>.ts.net", "mcp-stdio"]}}}
```

### Connect as a scoped agent (recommended)

`inhouse mcp-agent` joins the tailnet as a fresh, ephemeral device tagged
`tag:inhouse-agent`, connects to the MCP server *from that device*, and offers
the same tools over stdio. The platform sees `node:<id>` with the agent's
grant, not you. The device logs out when the agent exits.

```json
{"mcpServers": {"inhouse": {"command": "inhouse",
  "args": ["-url", "https://deploy.<tailnet>.ts.net", "mcp-agent"]}}}
```

One-time setup:

1. Create a Tailscale OAuth client with only the `auth_keys` write scope,
   allowed to assign `tag:inhouse-agent`.
2. Save its ID to `~/.config/inhouse/agent/client-id` and its secret to
   `~/.config/inhouse/agent/client-secret`, both mode 0600 (or pass
   `-client-id` and `-secret-file`).
3. Grant `tag:inhouse-agent` a narrow role, such as the `preview-*` deployer
   grant in the example policy, and let it reach the services it deploys if it
   should test them.

An agent with shell access on your laptop could still use your identity
directly; the tagged route limits well-behaved agents, not hostile ones.

## HTTP API

The CLI uses a small JSON API on the same node. Every route needs a grant;
errors return `{"error": {"code", "message", "hint", "operation_id"}}`.
Mutating routes accept an `Idempotency-Key` header.

| Route | |
| --- | --- |
| `GET /v1/whoami` | Identity and grants |
| `GET /v1/services`, `GET /v1/services/{name}` | List; detail with revisions and events |
| `POST /v1/plan`, `POST /v1/deploy` | Body: the spec (YAML or JSON) |
| `POST /v1/services/{name}/rollback` | `{"to_rev": 7, "restore_volumes": false}` |
| `DELETE /v1/services/{name}` | Delete |
| `GET /v1/operations/{id}?wait=60` | Operation state, waiting up to 120 s |
| `GET /v1/services/{name}/logs?rev=&container=&tail=&cursor=` | A page of logs |
| `GET /v1/events?service=&since_id=&limit=` | Events |
| `GET /v1/secrets`, `PUT /v1/secrets/{name}`, `DELETE /v1/secrets/{name}` | `PUT` body: `{"value": "..."}` |
