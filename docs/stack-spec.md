# Stack spec

A stack spec is one YAML (or JSON) document describing a service. People and
agents write the same format; `inhouse schema` prints its JSON Schema, and the
MCP `deploy` tool uses it as its input schema.

```yaml
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
| `name` | yes | 1–40 lowercase letters, digits or hyphens; starts and ends with a letter or digit. Becomes the hostname. `deploy` is reserved for the control node. |
| `expose` | no | `tailnet`: only your tailnet can reach it. `funnel`: also the public Internet, through Tailscale Funnel, without identity headers. Fixed once deployed. |
| `ttl` | no | A duration from `1s` to `720h`. The service is **ephemeral**: it is deleted automatically, node and volumes included, that long after its last successful deploy. Ephemeral services cannot use Funnel. Whether a service is ephemeral is fixed once deployed. |
| `update_strategy` | no | `rolling` starts the new revision beside the old one, so there is no downtime. `recreate` stops the old revision first; it is the default, and required, when any container has volumes. |
| `containers` | yes | 1–16 containers, keyed by name. Names follow the same rule as `name`. |

## Containers

| Field | Meaning |
| --- | --- |
| `image` | Fully qualified reference (`docker.io/library/nginx:1`, not `nginx`). Tags are resolved to digests at deploy time. |
| `port` | The container port that receives HTTPS traffic. Exactly one container, the **ingress**, sets it. |
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

## What a deploy does with your spec

The stored revision is your spec plus defaults, with every image pinned to an
exact digest and every secret pinned to an exact version. `get_service`
returns it in full. Deploying a spec that pins to the same result as the live
revision changes nothing and reports the live revision.
