# Operating a host

inhouse runs on one machine with Fedora CoreOS, two disks (OS and data), and
Tailscale. Everything host-specific comes from a Butane template and a few
values you keep in a **private instance directory**, separate from this repo:

```
my-instance/          # private; never commit credentials here either
├── instance.env      # HOSTNAME, SSH_PUBKEY, DATA_DISK
├── host.bu, host.ign # rendered by deploy/host/render.sh
├── policy.hujson     # your tailnet policy
└── stacks/           # your service specs
```

Credentials (OAuth secrets, the age key, backup keys) live in your password
manager and are copied to the host as files. They never go into Butane,
Ignition, a repo, or a command line.

## 1. Tailnet

1. Enable MagicDNS and HTTPS certificates for your tailnet.
2. Merge [`deploy/policy/policy.hujson`](../deploy/policy/policy.hujson) into
   your policy, with your login and capability name.
3. Create two OAuth clients:
   - **enroll**: scope `auth_keys` (write), tag `tag:inhouse-enroll`. It mints
     one single-use key per node.
   - **lifecycle**: scope `devices:core` (write), tags `tag:inhouse-svc` and
     `tag:inhouse-eph`. It deletes a service's device when the service is
     deleted. This scope can manage any device in the tailnet; inhouse only
     deletes a device whose ID, node ID, name and sole tag match what it
     recorded when creating it.

Check that your plan's device limit covers one device per service, plus
ephemeral services at their peak, plus the control node and host.

## 2. Host

Render and validate the Ignition config (needs Podman or Docker locally):

```sh
cat > my-instance/instance.env <<'EOF'
HOSTNAME=inhouse
SSH_PUBKEY=ssh-ed25519 AAAA... you@laptop
DATA_DISK=/dev/disk/by-id/nvme-EXAMPLE_SERIAL
EOF
deploy/host/render.sh my-instance
```

Install Fedora CoreOS on the OS disk with `coreos-installer` (typically from
your provider's rescue system), passing `host.ign`. **This erases the disks.**
On first boot the config:

- formats the data disk as btrfs at `/var/lib/inhouse`
  (`compress=zstd,user_subvol_rm_allowed`),
- creates the `inhouse` user (UID 1500) with lingering and a subordinate ID
  range of 4,194,304 starting at 1,000,000 (64 service slots),
- configures rootless Podman storage and a mode-0600 API socket,
- layers Tailscale, masks Docker, and sets OS updates to Sunday 04:00,
- loads a bootstrap nftables firewall that drops everything except loopback,
  `tailscale0`, WireGuard (UDP 41641), ICMP and, for now, public SSH.

Then, over public SSH: `sudo tailscale up --ssh --advertise-tags=tag:inhouse-host`.

Once tailnet SSH works from **two separate sessions**, close public SSH:

```sh
sudo bash lockdown.sh --approved   # deploy/host/lockdown.sh, through tailnet SSH
sudo bash verify.sh --locked       # deploy/host/verify.sh
```

Scan the host's public address from another machine: nothing should answer.

## 3. inhoused

Each [release](https://github.com/quinnovator/inhouse/releases) has an
`inhoused_<version>_linux_<arch>.tar.gz` archive holding `inhoused`,
`inhoused.service` and `install.sh`, plus a `checksums.sha512` covering
every archive. Download both onto the host into an empty directory, verify
and unpack the archive, and record the binary's checksum, which `install.sh`
checks:

```sh
sha512sum -c --ignore-missing checksums.sha512
tar -xzf inhoused_<version>_linux_<arch>.tar.gz
sha512sum inhoused > inhoused.sha512
```

Every archive also carries a GitHub build provenance attestation; from a
machine with the `gh` CLI, `gh attestation verify <archive> -R
quinnovator/inhouse` checks that it was built from this repo. To build from
source instead, stage your own `inhoused` with `deploy/host/inhoused.service`
and `deploy/host/install.sh`.

Copy the OAuth secrets as root-only files:

```
/etc/inhouse/enroll-secret      root 0600
/etc/inhouse/lifecycle-secret   root 0600
```

Then:

```sh
sudo bash install.sh example.com/cap/inhouse <enroll-client-id> <lifecycle-client-id>
journalctl -fu inhoused   # "control ready at https://deploy.<tailnet>.ts.net"
```

From your laptop:

```sh
export INHOUSE_URL=https://deploy.<tailnet>.ts.net
inhouse whoami
inhouse deploy examples/hello.yaml
```

**Upgrades** are the same `install.sh` with a new binary. The database
migrates itself forward; the reconciler restores every service on start.
HTTPS for all services pauses for the few seconds the daemon is restarting.

### Daemon flags

| Flag | Default | |
| --- | --- | --- |
| `-capability` | (required) | Capability name in your policy |
| `-enroll-client-id`, `-enroll-secret-file` | (required), `$CREDENTIALS_DIRECTORY/enroll-secret` | Node enrollment |
| `-lifecycle-client-id`, `-lifecycle-secret-file` | , `$CREDENTIALS_DIRECTORY/lifecycle-secret` | Device deletion (persistent services need it) |
| `-data-dir` | `/var/lib/inhouse` | State root, on btrfs |
| `-podman-socket` | `$XDG_RUNTIME_DIR/podman/podman.sock` | Rootless Podman API |
| `-podman-network` | `pasta` | Or `slirp4netns` |
| `-control-hostname` | `deploy` | The control node's name |
| `-port-range` | `20000-29999` | Loopback ports for pod ingress |
| `-subid-base`, `-subid-count` | `1000000`, `4194304` | Must match `/etc/subuid` and `/etc/subgid` |

## 4. Backups

A dead host is rebuilt from four things: this repo, your instance directory,
the age key, and the backup bucket. Tools in [`deploy/backup`](../deploy/backup):

| What | How | Loss window |
| --- | --- | --- |
| Database | Litestream, continuous, to S3-compatible storage | seconds |
| Volumes and node identities | restic, nightly at 03:00, encrypted; keeps 7 daily, 4 weekly, 6 monthly | up to 24 h |
| `keys/age.key` | You, once, into your password manager | |

Setup, as root on the host:

1. `install-tools.sh` installs checksum-pinned Litestream and restic.
2. Render `litestream.yml.tmpl` with your bucket, endpoint and region to
   `/etc/inhouse/litestream.yml`. Put the AWS credentials and
   `RESTIC_REPOSITORY=s3:https://<endpoint>/<bucket>/inhouse/restic` in
   `/etc/inhouse/backup.env`, and a random password in
   `/etc/inhouse/restic-password` (both 0600; keep a copy of the password
   off the host). Run `restic init` once.
3. `install.sh` installs and starts `inhouse-litestream.service` and the
   nightly `inhouse-backup.timer`.

Volume snapshots are crash-consistent, which Postgres and SQLite recover
from like a power cut.

### Restore onto a new host

1. Provision the new host (sections 1–2), but don't start `inhoused`. Stop the
   old host if it is still running: **never run two copies of a node identity.**
2. `litestream restore` the database to a scratch path and check it with
   `sqlite3 <db> 'PRAGMA integrity_check'`. `restic restore latest` into a
   scratch directory.
3. `restore.sh <db> <restic-restore-dir> <age-key>` places the database,
   node identities, age key and volumes, preserving numeric ownership and
   ACLs. It refuses to overwrite anything, and skips (with a warning) volume
   data of services that aren't live in the restored database or whose ID
   slot has since passed to another service.
4. Install the OAuth secrets and run `install.sh`. Every service comes back
   with the same name, revision, data and secrets; images are pulled by
   their pinned digests. Services whose images come from your own registry
   service retry every 30 seconds until the registry is back.

## Troubleshooting

| Symptom | Look at |
| --- | --- |
| A deploy failed | `inhouse events SERVICE`: the reason, and the failing container's last log lines |
| `service_busy` | Another operation is running; `inhouse wait <id>` |
| 403 on everything | `inhouse whoami`; the grant's `dst` must be `tag:inhouse-control` and the capability name must match `-capability` |
| Service shows `blog-1.<tailnet>` | A device named `blog` already existed; delete the stale one in the admin console, then delete and redeploy the service |
| Delete is stuck | `reconcile_error` events; usually the lifecycle OAuth client |
| 503 "no live revision" | The live revision is degraded and being restarted: `inhouse get SERVICE` shows `health_reason` and `restarts`; `degraded`, `restarting` and `live_unavailable` events say what failed |
| Anything else | `journalctl -u inhoused` on the host |
