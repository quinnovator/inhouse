import { Link, createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useAccess } from '~/components/shell'
import { EventFeed, Pill, ProblemAlert, Segmented, Time } from '~/components/ui'
import { health, plural, safeURL, type Tone, until } from '~/lib/format'
import { useServices } from '~/lib/queries'
import { useNow } from '~/lib/stores'
import type { Service } from '~/lib/types'

export const Route = createFileRoute('/')({
  head: () => ({ meta: [{ title: 'Services · inhouse' }] }),
  component: Services,
})

type Kind = 'all' | Service['kind']
const attention: Record<Tone, number> = { warn: 0, bad: 0, busy: 1, none: 1, ok: 2 }

function Services() {
  const can = useAccess()
  const services = useServices()
  const now = useNow()
  const [kind, setKind] = useState<Kind>('all')
  const [search, setSearch] = useState('')

  const all = services.data
  const states = all?.map((s) => health(s, now).tone) ?? []
  const count = (t: Tone) => states.filter((x) => x === t).length
  const summary = all
    ? [
        plural(all.length, 'service'),
        count('ok') && `${count('ok')} healthy`,
        count('warn') && `${count('warn')} degraded`,
        count('none') && `${count('none')} not serving`,
      ]
        .filter(Boolean)
        .join(' · ')
    : 'Loading…'

  const rows = (all ?? [])
    .filter((s) => kind === 'all' || s.kind === kind)
    .filter((s) => !search || s.name.includes(search))
    .sort((a, b) => attention[health(a, now).tone] - attention[health(b, now).tone] || a.name.localeCompare(b.name))

  return (
    <div className="split">
      <section className="card main" aria-labelledby="services-h">
        <div className="card-head">
          <div>
            <h1 id="services-h">Services</h1>
            <p className="sub">{summary}</p>
          </div>
          <div className="row">
            <label className="sr-only" htmlFor="service-filter">
              Filter services by name
            </label>
            <input
              id="service-filter"
              type="search"
              placeholder="Filter by name"
              value={search}
              onChange={(e) => setSearch(e.target.value.trim().toLowerCase())}
            />
            <Segmented<Kind>
              label="Kind"
              options={[
                ['all', 'All'],
                ['persistent', 'Persistent'],
                ['ephemeral', 'Ephemeral'],
              ]}
              value={kind}
              onChange={setKind}
            />
            {can.canDeployAny && (
              <Link className="btn primary" to="/deploy">
                Deploy
              </Link>
            )}
          </div>
        </div>
        <div className="card-body flush">
          {!all ? (
            services.error ? (
              <div className="card-body">
                <ProblemAlert error={services.error} />
              </div>
            ) : null
          ) : !all.length ? (
            <div className="empty">
              <strong>No services yet</strong>
              <span>
                Nothing is deployed that your grants cover. Deploy a stack spec with <code>inhouse deploy</code>
                {can.canDeployAny && (
                  <>
                    {' '}
                    or on the <Link to="/deploy">Deploy</Link> page
                  </>
                )}
                .
              </span>
            </div>
          ) : !rows.length ? (
            <div className="empty">No services match.</div>
          ) : (
            <div className="table-wrap">
              <table className="grid services">
                <thead>
                  <tr>
                    <th scope="col">Service</th>
                    <th scope="col">Health</th>
                    <th scope="col">Live</th>
                    <th scope="col">Kind</th>
                    <th scope="col">Created</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((s) => (
                    <Row key={s.name} s={s} now={now} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>
      <aside className="card side" aria-labelledby="recent-h">
        <div className="card-head">
          <h2 id="recent-h">Recent events</h2>
          <Link className="small" to="/events">
            All events
          </Link>
        </div>
        <div className="card-body">
          <EventFeed size={15} />
        </div>
      </aside>
    </div>
  )
}

function Row({ s, now }: { s: Service; now: number }) {
  const state = health(s, now)
  const url = safeURL(s.url)
  return (
    <tr>
      <td>
        <Link className="svc-name" to="/services/$name" params={{ name: s.name }}>
          {s.name}
        </Link>
        <div>
          {url ? (
            <a className="svc-url" href={url} target="_blank" rel="noopener noreferrer">
              {s.node_dns}
            </a>
          ) : (
            <span className="svc-url">no tailnet node yet</span>
          )}
        </div>
        {state.reason && <div className="reason">{state.reason}</div>}
      </td>
      <td className="nowrap">
        <Pill tone={state.tone}>{state.label}</Pill>
        {state.detail && <div className="sub">{state.detail}</div>}
      </td>
      <td className="mono nowrap">{s.current_rev ? `r${s.current_rev}` : '—'}</td>
      <td className="nowrap">
        {s.kind}
        {s.kind === 'ephemeral' && s.expires_at ? <div className="sub">expires {until(s.expires_at, now)}</div> : null}
      </td>
      <td>
        <Time ts={s.created_at} as="ago" />
        <div className="sub wrap-anywhere">{s.created_by}</div>
      </td>
    </tr>
  )
}
