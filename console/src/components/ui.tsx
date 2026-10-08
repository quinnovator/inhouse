// Small shared pieces: pills, alerts, times, toggles, dialogs, operation
// status and event lists.

import { Link } from '@tanstack/react-router'
import { type ReactNode, useEffect, useLayoutEffect, useRef } from 'react'
import { ApiError } from '~/lib/api'
import { actor, ago, brief, clock, eventTone, exact, type Tone } from '~/lib/format'
import { type Feed, useEventFeed } from '~/lib/queries'
import { useNow } from '~/lib/stores'
import type { Event, Operation } from '~/lib/types'

export function Pill({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={`pill ${tone}`}>{children}</span>
}

export function ProblemAlert({ error }: { error: unknown }) {
  const message = error instanceof Error ? error.message : String(error)
  const hint = error instanceof ApiError ? error.hint : ''
  return (
    <div className="alert bad" role="alert">
      <strong>{message}</strong>
      {hint && <span className="hint">{hint}</span>}
    </div>
  )
}

export function Time({ ts, as = 'clock' }: { ts: number; as?: 'clock' | 'ago' | 'brief' }) {
  const now = useNow()
  const text = as === 'ago' ? ago(ts, now) : as === 'brief' ? brief(ts) : clock(ts)
  return (
    <time dateTime={new Date(ts * 1000).toISOString()} title={exact(ts)}>
      {text}
    </time>
  )
}

export function Segmented<T extends string>(props: {
  label: string
  options: [T, string][]
  value: T
  onChange: (v: T) => void
}) {
  return (
    <div className="seg" role="group" aria-label={props.label}>
      {props.options.map(([v, text]) => (
        <button key={v} type="button" aria-pressed={v === props.value} onClick={() => props.onChange(v)}>
          {text}
        </button>
      ))}
    </div>
  )
}

export function Spinner() {
  return <span className="spinner" aria-hidden="true" />
}

// Modal is a native dialog: focus, Escape and the backdrop come free. It
// opens on mount; closing it any way calls onClose.
export function Modal(props: { title: string; onClose: () => void; children: ReactNode; actions: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null)
  const onClose = useRef(props.onClose)
  useLayoutEffect(() => {
    onClose.current = props.onClose
  })
  useEffect(() => {
    const d = ref.current!
    if (!d.open) d.showModal()
    const close = () => onClose.current()
    d.addEventListener('close', close)
    return () => d.removeEventListener('close', close)
  }, [])
  return (
    <dialog ref={ref} aria-label={props.title}>
      <div className="dialog-body">
        <h2>{props.title}</h2>
        {props.children}
      </div>
      <div className="dialog-actions">{props.actions}</div>
    </dialog>
  )
}

const running: Record<Operation['kind'], string> = {
  deploy: 'Pinning images, starting the new revision and waiting for its health checks. The live revision keeps serving until then.',
  rollback: 'Starting a copy of the target revision and waiting for its health checks. The live revision keeps serving until then.',
  delete: 'Stopping pods and removing the tailnet node and volumes.',
}

export function OperationStatus({ op }: { op: Operation }) {
  const tone: Tone = op.state === 'succeeded' ? 'ok' : op.state === 'failed' ? 'bad' : 'busy'
  const label = op.state === 'succeeded' ? 'Succeeded' : op.state === 'failed' ? 'Failed' : 'Running'
  return (
    <div className="progress" role="status">
      <div className="row">
        {op.state === 'running' && <Spinner />}
        <Pill tone={tone}>{label}</Pill>
        <span>
          {op.kind} {op.service}
          {op.rev ? <span className="mono"> r{op.rev}</span> : null}
        </span>
      </div>
      {op.reason && <div className={op.state === 'failed' ? 'alert bad' : 'sub'}>{op.reason}</div>}
      {op.state === 'running' && <div className="sub">{running[op.kind]}</div>}
    </div>
  )
}

function firstLine(s: string) {
  const i = s.indexOf('\n')
  return i < 0 ? s : s.slice(0, i) + ' …'
}

export function FeedItem({ ev, service = true, fresh = false }: { ev: Event; service?: boolean; fresh?: boolean }) {
  return (
    <li className={fresh ? 'new' : undefined}>
      <span className="t">
        <Time ts={ev.ts} as="brief" />
      </span>
      <div>
        <div>
          {service &&
            (ev.service ? (
              <Link to="/services/$name" params={{ name: ev.service }}>
                {ev.service}
              </Link>
            ) : (
              <strong>platform</strong>
            ))}
          {service && ' '}
          <span className={`kind ${eventTone(ev.kind)}`}>{ev.kind}</span>
          {ev.rev ? <span className="kind"> r{ev.rev}</span> : null}
        </div>
        <div className="msg">{firstLine(ev.message)}</div>
        <div className="actor">{actor(ev.actor)}</div>
      </div>
    </li>
  )
}

// EventFeed is a compact, live list of the newest events.
export function EventFeed({ service, size }: { service?: string; size: number }) {
  const feed = useEventFeed(service, size)
  return <FeedList feed={feed.data} error={feed.error} service={!service} />
}

function FeedList({ feed, error, service }: { feed?: Feed; error: unknown; service: boolean }) {
  if (!feed) return error ? <ProblemAlert error={error} /> : <p className="sub">Loading…</p>
  if (!feed.items.length) return <p className="sub">Nothing has happened yet.</p>
  return (
    <ol className="feed">
      {feed.items.map((ev) => (
        <FeedItem key={ev.id} ev={ev} service={service} fresh={feed.fresh.includes(ev.id)} />
      ))}
    </ol>
  )
}
