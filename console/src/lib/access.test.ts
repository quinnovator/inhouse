import { describe, expect, it } from 'vitest'
import { access, glob } from './access'
import type { Service } from './types'

describe('glob', () => {
  it.each([
    ['preview-*', 'preview-pr-1', true],
    ['preview-*', 'blog', false],
    ['blog', 'blog', true],
    ['b?og', 'blog', true],
    ['[ab]log', 'blog', true],
    ['[^ab]log', 'blog', false],
    ['a.b', 'axb', false],
    ['a.b', 'a.b', true],
  ])('%s matches %s: %s', (pattern, name, want) => {
    expect(glob(pattern).test(name)).toBe(want)
  })
})

const svc = (name: string, kind: Service['kind'], created_by: string): Service => ({
  name,
  kind,
  created_by,
  current_rev: 1,
  created_at: 0,
})

describe('access', () => {
  const agent = access({
    principal: 'node:agent',
    node_id: 'agent',
    grants: [{ role: 'deployer', services: ['preview-*'], expose: ['tailnet'], max_ttl: '24h' }],
  })

  it('lets a deployer deploy only matching services', () => {
    expect(agent.role).toBe('deployer')
    expect(agent.canDeployAny).toBe(true)
    expect(agent.canDeploy('preview-x')).toBe(true)
    expect(agent.canDeploy('blog')).toBe(false)
  })

  it('lets a deployer delete only ephemeral services it created', () => {
    expect(agent.canDelete(svc('preview-x', 'ephemeral', 'node:agent'))).toBe(true)
    expect(agent.canDelete(svc('preview-x', 'ephemeral', 'bob@example.com'))).toBe(false)
    expect(agent.canDelete(svc('preview-x', 'persistent', 'node:agent'))).toBe(false)
  })

  it('treats a deployer without exposure as a viewer', () => {
    const v = access({ principal: 'v', node_id: 'v', grants: [{ role: 'deployer', services: ['*'] }] })
    expect(v.role).toBe('viewer')
    expect(v.canDeployAny).toBe(false)
  })

  it('lets admins do anything', () => {
    const a = access({ principal: 'a', node_id: 'a', grants: [{ role: 'admin' }] })
    expect(a.canDeploy('anything')).toBe(true)
    expect(a.canDelete(svc('blog', 'persistent', 'someone'))).toBe(true)
  })
})
