// Helpers over stored stack specs and revisions.

import { pinned } from './format'
import type { Revision, Service, Stack } from './types'

export function order(spec: Stack) {
  return spec.order?.length ? spec.order : Object.keys(spec.containers ?? {})
}

export function hasVolumes(spec: Stack | undefined) {
  return !!spec && Object.values(spec.containers).some((c) => c.volumes && Object.keys(c.volumes).length > 0)
}

// eligible reports whether a revision can be the target of a rollback.
export function eligible(svc: Service, r: Revision) {
  return (
    r.rev !== svc.current_rev &&
    r.state !== 'pending' &&
    r.state !== 'starting' &&
    Object.values(r.spec.containers).every((c) => pinned(c.image))
  )
}

// shown is the revision whose spec describes the service: the live one,
// else the newest.
export function shown(svc: Service, revs: Revision[]) {
  return revs.find((r) => r.rev === svc.current_rev) ?? revs[0]
}

// deployable turns a revision's stored spec back into a document the
// deploy API accepts: platform-set fields removed, container order kept.
export function deployable(spec: Stack) {
  const out: Record<string, unknown> = { name: spec.name, expose: spec.expose }
  if (spec.ttl) out.ttl = spec.ttl
  if (spec.update_strategy) out.update_strategy = spec.update_strategy
  const containers: Record<string, unknown> = {}
  for (const n of order(spec)) {
    const { secret_versions: _, ...c } = spec.containers[n]!
    containers[n] = c
  }
  out.containers = containers
  return JSON.stringify(out, null, 2) + '\n'
}
