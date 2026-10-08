// Shapes of the /v1 API, mirrored from the Go structs they serialize:
// internal/store (Service, Revision, Operation, Event, SecretInfo),
// internal/spec (Stack), internal/authz (Principal, Grant) and
// internal/engine (ServiceView, ServiceDetail, LogPage, Plan). Timestamps
// are Unix seconds.

export type Grant = {
  role: 'viewer' | 'deployer' | 'admin'
  services?: string[]
  expose?: string[]
  max_ttl?: string
}

export type Principal = {
  principal: string
  login?: string
  node_id: string
  tags?: string[]
  grants: Grant[]
}

export type Service = {
  name: string
  kind: 'persistent' | 'ephemeral'
  node_dns?: string
  current_rev: number
  expires_at?: number
  created_by: string
  created_at: number
  deleted_at?: number
  health?: 'healthy' | 'degraded'
  health_reason?: string
  restarts?: number
  restarted_at?: number
  url?: string
}

export type Container = {
  image: string
  port?: number
  command?: string[]
  args?: string[]
  env?: Record<string, string>
  secrets?: Record<string, string>
  volumes?: Record<string, string>
  health: { path?: string; command?: string[]; timeout?: string }
  resources: { memory: string; cpus: number }
  secret_versions?: Record<string, string>
}

export type Stack = {
  name: string
  expose: string
  ttl?: string
  update_strategy?: string
  containers: Record<string, Container>
  order: string[]
}

export type RevState = 'pending' | 'starting' | 'live' | 'draining' | 'stopped' | 'failed'

export type Revision = {
  service: string
  rev: number
  spec: Stack
  spec_hash: string
  host_port?: number
  state: RevState
  reason?: string
  created_by: string
  created_at: number
  health_started_at?: number
  drain_until?: number
  finished_at?: number
}

export type ServiceDetail = {
  service: Service
  revisions: Revision[]
  events: Event[]
}

export type Operation = {
  operation_id: string
  kind: 'deploy' | 'rollback' | 'delete'
  service: string
  rev?: number
  restore_from?: number
  state: 'running' | 'succeeded' | 'failed'
  idempotency_key?: string
  reason?: string
  created_by: string
  created_at: number
  finished_at?: number
}

export type Event = {
  id: number
  ts: number
  service?: string
  rev?: number
  actor: string
  kind: string
  message: string
}

export type LogPage = {
  service: string
  rev: number
  container: string
  lines: string[]
  next_cursor?: string
  truncated: boolean
}

export type SecretInfo = {
  name: string
  updated_by: string
  updated_at: number
}

export type ContainerPlan = {
  image: string
  port?: number
  env_keys: string[]
  secrets?: Record<string, string>
  volumes?: Record<string, string>
  resources: { memory: string; cpus: number }
}

export type Plan = {
  service: string
  exists: boolean
  live_rev?: number
  before: Record<string, ContainerPlan>
  after: Record<string, ContainerPlan>
  expose: string
  ttl?: string
  update_strategy: string
  warnings: string[]
  may_apply: boolean
}

export type Problem = {
  code: string
  message: string
  hint?: string
  operation_id?: string
}
