# Security

## Reporting

Please report vulnerabilities privately through GitHub's security advisory
form for this repository. Never include credentials or secret values in an
issue, log or reproduction.

## Threat model

inhouse trusts the host and the tailnet identities your policy file
describes. It does not trust Funnel traffic, client-supplied headers, or what
an agent intends beyond its grants. The largest residual risk is a
compromised device of yours: it carries your admin identity. Use Tailscale
device approval and Tailnet Lock.

| Threat | Mitigation | Residual |
| --- | --- | --- |
| Forged `Tailscale-User-*` headers | The edge deletes them and sets its own from WhoIs, after hop-by-hop processing | None for tailnet traffic |
| Public traffic reaching control | Control runs only on `deploy`, which never listens on Funnel; Funnel is granted only to `tag:inhouse-svc` | Funnel-exposed apps must authenticate their own users |
| Unauthorized control calls | Destination-scoped capability grants from WhoIs; no tokens; refused calls are recorded | Anyone your policy grants |
| An agent doing more than intended | Tagged agent identity with narrow grants; secrets, volume restore and other services' deletion need admin | An agent can do anything its grant allows |
| An agent on your laptop acting as you | `inhouse mcp-agent` gives it a tagged identity | An agent with shell access can bypass it |
| Container escape | Rootless Podman; each service's container root maps to its own subordinate ID slice, never to the `inhouse` user that owns keys and state | A kernel exploit that escapes the user namespace |
| Touching unrelated containers | Every Podman mutation verifies inhouse labels on the pod and each member first | |
| Stolen enroll secret | Root-only file, passed via systemd credentials; can only mint keys for inhouse tags | Attacker can join tagged nodes until revoked |
| Stolen lifecycle secret | Root-only file; inhouse deletes only devices matching their recorded identity | The credential itself can manage any device in the tailnet |
| Secret disclosure | age encryption at rest; no API returns values; logs from the API redact every current and past value | Apps can still log transformed or encoded secrets |
| Swapped images | Tags pinned to registry-served digests at deploy time; rollbacks reuse them | You run what you deploy |
| Host on the internet | No public ports after lockdown; SSH only over the tailnet | Provider console access |
| Data loss | Pre-deploy snapshots, continuous database replication, nightly encrypted backups | Up to 24 h of volume data |

Protect the age key and restic password out-of-band: without them, secrets
and backups cannot be recovered. Never run a restored host and its original
at the same time; they would share node identities.
