// What the caller's grants allow, mirrored from internal/authz so the
// console only offers actions that can succeed. The API still decides.

import type { Grant, Principal, Service } from './types'

// glob compiles a path.Match pattern (*, ?, [...] and \ escapes) to a RegExp.
export function glob(pattern: string): RegExp {
  let re = ''
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i]!
    if (c === '*') re += '[^/]*'
    else if (c === '?') re += '[^/]'
    else if (c === '\\' && i + 1 < pattern.length) re += '\\' + pattern[++i]
    else if (c === '[') {
      const end = pattern.indexOf(']', i + 2)
      if (end < 0) return /$^/
      re += pattern.slice(i, end + 1)
      i = end
    } else re += c.replace(/[.+^${}()|\]\\/]/g, '\\$&')
  }
  try {
    return new RegExp(`^${re}$`)
  } catch {
    return /$^/
  }
}

export type Access = {
  principal: string
  grants: Grant[]
  admin: boolean
  role: Grant['role']
  canDeployAny: boolean
  canDeploy: (service: string) => boolean
  canDelete: (service: Service) => boolean
}

export function access(me: Principal): Access {
  const grants = me.grants ?? []
  const admin = grants.some((g) => g.role === 'admin')
  const matches = (g: Grant, name: string) => g.role === 'admin' || (g.services ?? []).some((p) => glob(p).test(name))
  const deployers = grants.filter((g) => g.role === 'deployer' && (g.expose ?? []).length > 0)
  return {
    principal: me.principal,
    grants,
    admin,
    role: admin ? 'admin' : deployers.length ? 'deployer' : 'viewer',
    canDeployAny: admin || deployers.length > 0,
    canDeploy: (name) => admin || deployers.some((g) => matches(g, name)),
    canDelete: (svc) =>
      admin ||
      (svc.kind === 'ephemeral' &&
        svc.created_by === me.principal &&
        grants.some((g) => g.role === 'deployer' && matches(g, svc.name))),
  }
}
