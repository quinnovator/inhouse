import { describe, expect, it } from 'vitest'
import { ago, duration, health, image } from './format'
import { deployable, eligible } from './spec'
import type { Revision, Service, Stack } from './types'

const digest = 'a'.repeat(64)

const stack: Stack = {
  name: 'notes',
  expose: 'tailnet',
  update_strategy: 'recreate',
  order: ['db', 'web'],
  containers: {
    web: { image: `ghcr.io/example/notes@sha256:${digest}`, port: 8080, health: { path: '/healthz' }, resources: { memory: '256Mi', cpus: 0.5 } },
    db: {
      image: `docker.io/library/postgres@sha256:${digest}`,
      secrets: { POSTGRES_PASSWORD: 'notes-db-password' },
      secret_versions: { 'notes-db-password': 'hash' },
      health: {},
      resources: { memory: '512Mi', cpus: 1 },
    },
  },
}

const service: Service = { name: 'notes', kind: 'persistent', current_rev: 2, created_by: 'a', created_at: 0 }
const rev = (n: number, state: Revision['state'], spec = stack): Revision => ({
  service: 'notes',
  rev: n,
  spec,
  spec_hash: '',
  state,
  created_by: 'a',
  created_at: 0,
})

describe('deployable', () => {
  it('drops platform-set fields and keeps container order', () => {
    const doc = JSON.parse(deployable(stack))
    expect(Object.keys(doc.containers)).toEqual(['db', 'web'])
    expect(doc.containers.db.secret_versions).toBeUndefined()
    expect(doc.order).toBeUndefined()
    expect(doc.update_strategy).toBe('recreate')
  })
})

describe('eligible', () => {
  it('allows pinned, settled revisions other than the live one', () => {
    expect(eligible(service, rev(1, 'stopped'))).toBe(true)
    expect(eligible(service, rev(1, 'failed'))).toBe(true)
    expect(eligible(service, rev(2, 'live'))).toBe(false)
    expect(eligible(service, rev(3, 'starting'))).toBe(false)
    const unpinned = { ...stack, containers: { web: { ...stack.containers.web!, image: 'ghcr.io/example/notes:1' } } }
    expect(eligible(service, rev(1, 'failed', unpinned))).toBe(false)
  })
})

describe('format', () => {
  it('shortens pinned images', () => {
    expect(image(`ghcr.io/x@sha256:${digest}`)).toEqual({ name: 'ghcr.io/x', digest: 'a'.repeat(12) })
    expect(image('ghcr.io/x:1')).toEqual({ name: 'ghcr.io/x:1', digest: '' })
  })

  it('formats durations and relative times', () => {
    expect(duration(38)).toBe('38s')
    expect(duration(5 * 3600 + 12 * 60)).toBe('5h 12m')
    expect(duration(3 * 86400 + 4 * 3600)).toBe('3d 4h')
    expect(ago(1000, 1000 + 120)).toBe('2 min ago')
  })

  it('summarizes health', () => {
    expect(health({ ...service, health: 'healthy' }).tone).toBe('ok')
    expect(health({ ...service, health: 'degraded', restarts: 3, health_reason: 'x' })).toMatchObject({
      tone: 'warn',
      detail: '3 restarts in a row',
      reason: 'x',
    })
    expect(health({ ...service, current_rev: 0 }).label).toBe('Not live')
    expect(health({ ...service, deleted_at: 5 }).label).toBe('Deleting')
  })
})
