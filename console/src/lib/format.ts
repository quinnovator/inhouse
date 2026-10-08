// Formatting for times, durations, images and service state.

import type { RevState, Service } from './types'

export type Tone = 'ok' | 'warn' | 'busy' | 'bad' | 'none'

export function plural(n: number, one: string, many = one + 's') {
  return `${n} ${n === 1 ? one : many}`
}

export function ago(ts: number | undefined, now = Date.now() / 1000) {
  if (!ts) return ''
  const s = Math.max(0, Math.round(now - ts))
  if (s < 45) return 'just now'
  const m = Math.round(s / 60)
  if (m < 60) return `${m} min ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.round(h / 24)
  if (d < 45) return `${plural(d, 'day')} ago`
  return date(ts)
}

export function duration(seconds: number) {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d}d ${h % 24}h` : `${d}d`
}

export function until(ts: number, now = Date.now() / 1000) {
  const left = ts - now
  return left <= 0 ? 'expiring now' : `in ${duration(left)}`
}

const timeOnly = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' })
const dayTime = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
const dayOnly = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
const shortDay = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
const full = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'medium' })

const today = (d: Date) => d.toDateString() === new Date().toDateString()

// clock is the time for today, else the day and time.
export function clock(ts: number) {
  const d = new Date(ts * 1000)
  return today(d) ? timeOnly.format(d) : dayTime.format(d)
}

// brief is the time for today, else just the day.
export function brief(ts: number) {
  const d = new Date(ts * 1000)
  return today(d) ? timeOnly.format(d) : shortDay.format(d)
}

export function date(ts: number) {
  return dayOnly.format(new Date(ts * 1000))
}

export function exact(ts: number | undefined) {
  return ts ? full.format(new Date(ts * 1000)) : ''
}

// image splits a reference into its name and a short digest, if pinned.
export function image(ref: string) {
  const at = ref.indexOf('@sha256:')
  if (at < 0) return { name: ref, digest: '' }
  return { name: ref.slice(0, at), digest: ref.slice(at + 8, at + 20) }
}

export function pinned(ref: string) {
  return /@sha256:[a-f0-9]{64}$/.test(ref)
}

export type Health = { tone: Tone; label: string; detail: string; reason?: string }

// health summarizes a service for the list and the detail header.
export function health(s: Service, now = Date.now() / 1000): Health {
  if (s.deleted_at) return { tone: 'none', label: 'Deleting', detail: 'removing pods, volumes and node' }
  if (!s.current_rev) return { tone: 'none', label: 'Not live', detail: 'no revision is serving' }
  if (s.health === 'degraded') {
    return {
      tone: 'warn',
      label: 'Degraded',
      detail: s.restarts ? `${plural(s.restarts, 'restart')} in a row` : 'restart pending',
      reason: s.health_reason,
    }
  }
  return {
    tone: 'ok',
    label: 'Healthy',
    detail: s.restarts ? `${plural(s.restarts, 'restart')}, last ${ago(s.restarted_at, now)}` : '',
  }
}

const tones: Record<Exclude<Tone, 'none'>, string[]> = {
  ok: ['succeeded', 'cutover', 'recovered', 'node_ready', 'noop'],
  warn: ['degraded', 'restarting', 'stop_error', 'reconcile_error', 'live_unavailable'],
  bad: ['failed', 'denied', 'failure_logs'],
  busy: ['deploy', 'rollback', 'delete', 'normalized'],
}

export function eventTone(kind: string): Tone {
  for (const [tone, kinds] of Object.entries(tones)) if (kinds.includes(kind)) return tone as Tone
  return 'none'
}

export function revTone(state: RevState): Tone {
  const map: Record<RevState, Tone> = { live: 'ok', pending: 'busy', starting: 'busy', draining: 'none', stopped: 'none', failed: 'bad' }
  return map[state] ?? 'none'
}

// actor names the reconciler plainly; everyone else by principal.
export function actor(id: string) {
  return id === 'reconciler' ? 'inhouse' : id
}

// safeURL returns href only for https URLs, so a value from the API can
// never become a javascript: or data: link.
export function safeURL(href: string | undefined) {
  if (!href) return undefined
  try {
    return new URL(href).protocol === 'https:' ? href : undefined
  } catch {
    return undefined
  }
}
