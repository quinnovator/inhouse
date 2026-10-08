import { Link, createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { useAccess } from '~/components/shell'
import { ProblemAlert, Time } from '~/components/ui'
import { actor, eventTone } from '~/lib/format'
import { useEventFeed, useServices } from '~/lib/queries'
import type { Event } from '~/lib/types'

// Events without a service (secrets, refusals) are "platform" events. The
// sentinel can't collide with a service name, which is a DNS label.
const PLATFORM = '@platform'

export const Route = createFileRoute('/events')({
  validateSearch: (search: Record<string, unknown>): { service?: string } =>
    typeof search.service === 'string' && search.service ? { service: search.service } : {},
  head: () => ({ meta: [{ title: 'Events · inhouse' }] }),
  component: Events,
})

function Events() {
  const can = useAccess()
  const navigate = useNavigate({ from: '/events' })
  const { service = '' } = Route.useSearch()
  const services = useServices()
  const feed = useEventFeed(service === PLATFORM ? undefined : service || undefined, 100, 500)
  const [kind, setKind] = useState('')
  const [search, setSearch] = useState('')

  const items = feed.data?.items ?? []
  const kinds = [...new Set(items.map((e) => e.kind))].sort()
  const names = [...new Set([...(services.data ?? []).map((s) => s.name), ...(service && service !== PLATFORM ? [service] : [])])].sort()
  const rows = items.filter(
    (e) =>
      (service !== PLATFORM || !e.service) &&
      (!kind || e.kind === kind) &&
      (!search ||
        e.message.toLowerCase().includes(search) ||
        e.actor.toLowerCase().includes(search) ||
        (e.service ?? '').includes(search)),
  )
  const fresh = feed.data?.fresh ?? []

  return (
    <section className="card" aria-labelledby="events-h">
      <div className="card-head">
        <div>
          <h1 id="events-h">Events</h1>
          <p className="sub">The newest 100 you can see, updating live. Every line names who or what acted.</p>
        </div>
        <div className="row">
          <label className="sr-only" htmlFor="ev-service">
            Service
          </label>
          <select
            id="ev-service"
            value={service}
            onChange={(e) => navigate({ search: e.target.value ? { service: e.target.value } : {}, replace: true })}
          >
            <option value="">All services</option>
            {can.admin && <option value={PLATFORM}>Platform (secrets, refusals)</option>}
            {names.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
          <label className="sr-only" htmlFor="ev-kind">
            Kind
          </label>
          <select id="ev-kind" value={kinds.includes(kind) ? kind : ''} onChange={(e) => setKind(e.target.value)}>
            <option value="">All kinds</option>
            {kinds.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
          <label className="sr-only" htmlFor="ev-search">
            Search messages
          </label>
          <input
            id="ev-search"
            type="search"
            placeholder="Search messages"
            value={search}
            onChange={(e) => setSearch(e.target.value.trim().toLowerCase())}
          />
        </div>
      </div>
      <div className="card-body flush">
        {!feed.data ? (
          feed.error ? (
            <div className="card-body">
              <ProblemAlert error={feed.error} />
            </div>
          ) : (
            <div className="empty">Loading…</div>
          )
        ) : !rows.length ? (
          <div className="empty">{items.length ? 'No events match.' : 'No events yet.'}</div>
        ) : (
          <div className="table-wrap">
            <table className="grid events">
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Service</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Message</th>
                  <th scope="col">Actor</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => (
                  <tr key={e.id} className={fresh.includes(e.id) ? 'new' : undefined}>
                    <td className="mono small nowrap">
                      <Time ts={e.ts} />
                    </td>
                    <td className="nowrap">
                      {e.service ? (
                        <Link to="/services/$name" params={{ name: e.service }}>
                          {e.service}
                        </Link>
                      ) : (
                        <span className="muted">platform</span>
                      )}
                      {e.rev ? <span className="kind"> r{e.rev}</span> : null}
                    </td>
                    <td className="nowrap">
                      <span className={`kind ${eventTone(e.kind)}`}>{e.kind}</span>
                    </td>
                    <td className="ev-msg">
                      <Message e={e} />
                    </td>
                    <td className="small wrap-anywhere">{actor(e.actor)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  )
}

// Message shows multi-line messages (failed deploys' last log lines)
// collapsed. Rows are keyed by event id, so an expanded one stays open
// while new events arrive.
function Message({ e }: { e: Event }) {
  if (!e.message.includes('\n')) return <>{e.message}</>
  return (
    <details className="logs-inline">
      <summary>{e.kind === 'failure_logs' ? 'Last log lines of the failed revision' : e.message.split('\n')[0]}</summary>
      <pre className="block">{e.message}</pre>
    </details>
  )
}
