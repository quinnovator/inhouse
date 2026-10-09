# Roadmap

inhouse is alpha software. This page says what comes next, in order, without
dates. It changes as we learn from people running it, so tell us what you
need in [Discussions](https://github.com/quinnovator/inhouse/discussions).

## Next

- **The platform in the console.** The console shows every service you can
  see, with its revisions, events and logs. Next it shows the platform
  itself: the daemon, its hosts, and each host's available storage, CPU use
  and memory pressure, plus the operations running on each service.
- **Bundled agent skills.** Skills shipped with inhouse that teach agents to
  write stack specs, deploy, debug and roll back on the platform, so they
  don't have to work it out from the MCP tools alone.
- **A versioned stack spec.** Specs carry `version: 1`, and future changes
  are additive or come with a new version the daemon still accepts.
- **Boring upgrades.** The database is backed up before every migration,
  upgrades from each release are tested in CI, and redeploying an unchanged
  spec stays a no-op across upgrades.
- **Version checks.** The CLI warns when it and the daemon disagree.
- **An end-to-end test** in CI that deploys, crashes, recovers, rolls back and
  deletes a real service.
- **Any Linux with rootless Podman.** Fedora CoreOS stays the recommended
  host, but `inhoused` installs on an existing Debian, Ubuntu or Fedora
  machine.
- **btrfs optional.** Without it, volumes are plain directories with no
  snapshots or volume restore, and `plan` says so.
- **Try it in 15 minutes** on a disposable VM, without erasing a machine.

## Later

- **Stack spec:** pre-deploy jobs such as database migrations, secrets as
  files, tunable health checks with longer startup windows, and changing
  `expose` without deleting the service.
- **CLI:** readable output by default (`--json` for scripts), `logs -f`,
  `status`, `exec`, confirmation before `delete`, a config file and shell
  completion.
- **Observability:** Prometheus metrics, a daemon health endpoint,
  per-service CPU and memory use, and notifications when a deploy fails or a
  service degrades.
- **More services per host:** lift the lifetime limit of 64 persistent
  services.
- **More than one host.** Join several hosts into one platform, so services
  keep running when a host goes down. One host stays a complete, supported
  setup.

## Considering

Not committed. If you need one of these, say so in Discussions.

- Headscale as the control server
- TCP services and more than one port per service
- Scheduled jobs
- Rotating the age key

## Not planned

- Multi-tenancy, or sandboxing untrusted code beyond rootless containers
