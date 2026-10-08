// The /v1 API on this origin. The browser's tailnet identity is the
// credential: there are no tokens to store.

import type {
  Event,
  LogPage,
  Operation,
  Plan,
  Principal,
  Problem,
  SecretInfo,
  Service,
  ServiceDetail,
} from './types'

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly hint: string
  readonly operationId: string

  constructor(status: number, problem: Partial<Problem> = {}) {
    super(problem.message || `HTTP ${status}`)
    this.status = status
    this.code = problem.code || `http_${status}`
    this.hint = problem.hint || ''
    this.operationId = problem.operation_id || ''
  }

  // unreachable reports errors that mean the control node is down or
  // unreachable, as opposed to a refused or invalid request.
  get unreachable() {
    return this.status === 0 || this.status >= 500
  }
}

type Options = { text?: string; json?: unknown; key?: string; signal?: AbortSignal }

async function call<T>(method: string, path: string, { text, json, key, signal }: Options = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  let body: string | undefined
  if (json !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(json)
  } else if (text !== undefined) {
    headers['Content-Type'] = 'text/plain; charset=utf-8'
    body = text
  }
  if (key) headers['Idempotency-Key'] = key
  let resp: Response
  try {
    resp = await fetch(path, { method, headers, body, signal, cache: 'no-store' })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, {
      code: 'network',
      message: 'Cannot reach the control node.',
      hint: 'Check that this device is connected to the tailnet.',
    })
  }
  const raw = await resp.text()
  let data: unknown = null
  try {
    data = raw ? JSON.parse(raw) : null
  } catch {
    data = null
  }
  if (!resp.ok) {
    const problem = (data as { error?: Problem } | null)?.error
    throw new ApiError(resp.status, problem ?? { message: raw.trim() || resp.statusText })
  }
  return data as T
}

const seg = encodeURIComponent
const secretPath = (name: string) => '/v1/secrets/' + name.split('/').map(seg).join('/')

function query(params: Record<string, string | number | undefined>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '' && v !== 0) q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

export type EventQuery = { service?: string; since?: number; limit?: number }
export type LogQuery = { rev?: number; container?: string; tail?: number; cursor?: string }

export const api = {
  whoami: (signal?: AbortSignal) => call<Principal>('GET', '/v1/whoami', { signal }),
  services: (signal?: AbortSignal) => call<Service[]>('GET', '/v1/services', { signal }),
  service: (name: string, signal?: AbortSignal) => call<ServiceDetail>('GET', `/v1/services/${seg(name)}`, { signal }),
  events: ({ service, since, limit }: EventQuery, signal?: AbortSignal) =>
    call<Event[]>('GET', '/v1/events' + query({ service, since_id: since, limit }), { signal }),
  logs: (name: string, { rev, container, tail, cursor }: LogQuery, signal?: AbortSignal) =>
    call<LogPage>('GET', `/v1/services/${seg(name)}/logs` + query({ rev, container, tail, cursor }), { signal }),
  operation: (id: string, wait: number, signal?: AbortSignal) =>
    call<Operation>('GET', `/v1/operations/${seg(id)}` + query({ wait }), { signal }),
  plan: (spec: string) => call<Plan>('POST', '/v1/plan', { text: spec }),
  deploy: (spec: string, key: string) => call<Operation>('POST', '/v1/deploy', { text: spec, key }),
  rollback: (name: string, to: number, restore: boolean, key: string) =>
    call<Operation>('POST', `/v1/services/${seg(name)}/rollback`, { json: { to_rev: to, restore_volumes: restore }, key }),
  remove: (name: string, key: string) => call<Operation>('DELETE', `/v1/services/${seg(name)}`, { key }),
  secrets: (signal?: AbortSignal) => call<SecretInfo[]>('GET', '/v1/secrets', { signal }),
  setSecret: (name: string, value: string) => call<unknown>('PUT', secretPath(name), { json: { value } }),
  deleteSecret: (name: string) => call<unknown>('DELETE', secretPath(name)),
}

// Keys make retries safe: one per intended action, reused when it's retried.
export function newKey() {
  return crypto.randomUUID()
}
