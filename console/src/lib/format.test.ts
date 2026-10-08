import { describe, expect, it } from 'vitest'
import { health } from './format'
import type { Service } from './types'

const svc = (s: Partial<Service>): Service => ({ name: 'x', kind: 'persistent', current_rev: 1, created_by: 'me', created_at: 0, ...s })

describe('health', () => {
  it('shows a stopped service as stopped', () => {
    expect(health(svc({ stopped_at: 100 }), 200).label).toBe('Stopped')
  })
  it('shows a started service as starting until it is healthy', () => {
    expect(health(svc({})).label).toBe('Starting')
    expect(health(svc({ health: 'healthy' })).label).toBe('Healthy')
    expect(health(svc({ health: 'degraded', health_reason: 'start failed' })).label).toBe('Degraded')
  })
})
