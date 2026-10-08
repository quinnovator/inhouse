import { useQueryClient } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { type ReactNode, useState } from 'react'
import { ActionDialog, DeleteDialog, RollbackDialog } from '~/components/dialogs'
import { LogView } from '~/components/LogView'
import { useAccess } from '~/components/shell'
import { EventFeed, Menu, Pill, ProblemAlert, Spinner, Time } from '~/components/ui'
import { ApiError, api, type ServiceAction } from '~/lib/api'
import { actor, ago, exact, health, image, plural, revTone, safeURL, until } from '~/lib/format'
import { useService } from '~/lib/queries'
import { deployable, eligible, hasSecrets, order, shown } from '~/lib/spec'
import { toast, useNow } from '~/lib/stores'
import type { Container, Revision, ServiceDetail, Stack } from '~/lib/types'

export const Route = createFileRoute('/services/$name')({
  head: ({ params }) => ({ meta: [{ title: `${params.name} · inhouse` }] }),
  component: ServicePage,
})

type Dialog = { kind: 'rollback'; rev?: number } | { kind: 'delete' } | { kind: 'action'; action: ServiceAction }

function ServicePage() {
  const { name } = Route.useParams()
  const service = useService(name)
  const [dialog, setDialog] = useState<Dialog | null>(null)

  if (service.error instanceof ApiError && service.error.status === 404) {
    return (
      <div className="card">
        <div className="empty">
          <strong>{name} no longer exists</strong>
          <span>It was deleted, or it never existed.</span>
          <Link to="/">Back to services</Link>
        </div>
      </div>
    )
  }
  const detail = service.data
  return (
    <>
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link to="/">Services</Link> / {name}
      </nav>
      {!detail ? (
        service.error ? <ProblemAlert error={service.error} /> : <p className="sub">Loading…</p>
      ) : (
        <Loaded detail={detail} open={setDialog} />
      )}
      {detail && dialog?.kind === 'rollback' && <RollbackDialog detail={detail} preselect={dialog.rev} onClose={() => setDialog(null)} />}
      {detail && dialog?.kind === 'delete' && <DeleteDialog detail={detail} onClose={() => setDialog(null)} />}
      {detail && dialog?.kind === 'action' && <ActionDialog detail={detail} action={dialog.action} onClose={() => setDialog(null)} />}
    </>
  )
}

function Loaded({ detail, open }: { detail: ServiceDetail; open: (d: Dialog) => void }) {
  const can = useAccess()
  const now = useNow()
  const { service: svc, revisions: revs } = detail
  const live = shown(svc, revs)
  const spec = live?.spec
  const state = health(svc, now)
  const url = safeURL(svc.url)
  const mayOperate = !svc.deleted_at && can.canDeploy(svc.name)
  // A stopped service only starts; deploys of any kind are refused.
  const mayDeploy = mayOperate && !svc.stopped_at
  const rollbackable = mayDeploy && revs.some((r) => eligible(svc, r))
  const working = revs.find((r) => r.state === 'pending' || r.state === 'starting')
  const hasLive = !!svc.current_rev
  const mayDelete = !svc.deleted_at && can.canDelete(svc)
  const client = useQueryClient()
  const [extending, setExtending] = useState(false)

  async function extend() {
    setExtending(true)
    try {
      await api.extend(svc.name)
      toast(live?.spec.ttl ? `${svc.name} now expires in ${live.spec.ttl}` : `${svc.name} extended`)
      void client.invalidateQueries({ queryKey: ['service', svc.name] })
    } catch (err) {
      toast(err instanceof Error ? err.message : 'Could not extend', 'bad')
    } finally {
      setExtending(false)
    }
  }

  async function copySpec(s: Stack) {
    try {
      await navigator.clipboard.writeText(deployable(s))
      toast('Spec copied. It deploys exactly this revision.')
    } catch {
      toast('Could not copy to the clipboard', 'bad')
    }
  }

  return (
    <>
      <div className="svc-head">
        <div className="svc-title">
          <div className="row">
            <h1>{svc.name}</h1>
            <Pill tone={state.tone}>{state.label}</Pill>
            <span className="sub">{[svc.kind, spec?.expose, spec?.update_strategy].filter(Boolean).join(' · ')}</span>
          </div>
          {url ? (
            <a className="mono" href={url} target="_blank" rel="noopener noreferrer">
              {url}
            </a>
          ) : (
            <span className="sub">No tailnet node yet</span>
          )}
        </div>
        <div className="svc-actions">
          {mayOperate && hasLive && svc.stopped_at ? (
            <button className="btn primary" type="button" onClick={() => open({ kind: 'action', action: 'start' })}>
              Start…
            </button>
          ) : null}
          {mayDeploy && hasLive && (
            <button className="btn" type="button" onClick={() => open({ kind: 'action', action: 'restart' })}>
              Restart…
            </button>
          )}
          {spec && mayDeploy && (
            <Link className="btn" to="/deploy" search={{ from: svc.name }}>
              Edit and deploy
            </Link>
          )}
          {rollbackable && (
            <button className="btn" type="button" onClick={() => open({ kind: 'rollback' })}>
              Roll back…
            </button>
          )}
          {(spec || mayDelete) && (
            <Menu label="More">
              {spec && (
                <button className="menu-item" type="button" onClick={() => copySpec(spec)}>
                  Copy spec
                  <span className="sub">A document that deploys exactly this revision</span>
                </button>
              )}
              {mayDeploy && hasLive && hasSecrets(live?.spec) && (
                <button className="menu-item" type="button" onClick={() => open({ kind: 'action', action: 'redeploy' })}>
                  Redeploy with current secrets…
                  <span className="sub">Picks up secrets changed since r{svc.current_rev}</span>
                </button>
              )}
              {mayDeploy && hasLive && (
                <button className="menu-item" type="button" onClick={() => open({ kind: 'action', action: 'stop' })}>
                  Stop…
                  <span className="sub">Take it offline and keep everything</span>
                </button>
              )}
              {mayDelete && (
                <>
                  {spec && <hr />}
                  <button className="menu-item danger" type="button" onClick={() => open({ kind: 'delete' })}>
                    Delete…
                  </button>
                </>
              )}
            </Menu>
          )}
        </div>
      </div>

      {svc.deleted_at ? (
        <div className="alert busy" role="status">
          <div className="row">
            <Spinner />
            <strong>Being deleted since {ago(svc.deleted_at, now)}</strong>
          </div>
          <span className="hint">Pods, volumes and the tailnet node are being removed.</span>
        </div>
      ) : svc.stopped_at ? (
        <div className="alert" role="status">
          <strong>Stopped {ago(svc.stopped_at, now)}</strong>
          <span className="hint">
            r{svc.current_rev} isn't running and its address answers 503. Revisions, volumes and the tailnet node are kept.
            {mayOperate ? ' Start it to serve again; deploys are refused until then.' : ''}
          </span>
        </div>
      ) : working ? (
        <div className="alert busy" role="status">
          <div className="row">
            <Spinner />
            <strong>
              {working.state === 'pending'
                ? `r${working.rev}: pulling images and pinning digests`
                : `r${working.rev}: starting and waiting for health checks`}
            </strong>
          </div>
          <span className="hint">
            Recorded by {actor(working.created_by)} {ago(working.created_at, now)}.{' '}
            {svc.current_rev ? `r${svc.current_rev} keeps serving until r${working.rev} is healthy.` : 'It gets traffic once it is healthy.'}
          </span>
        </div>
      ) : null}
      {state.tone === 'warn' && (
        <div className="alert warn" role="status">
          <strong>r{svc.current_rev} is degraded</strong>
          <span>{svc.health_reason}</span>
          <span className="hint">
            {svc.restarts
              ? `${plural(svc.restarts, 'restart')} in a row so far, the latest ${ago(svc.restarted_at, now)}. inhouse keeps restarting it, waiting longer each time.`
              : 'inhouse is restarting it.'}
            {rollbackable ? ' If a recent change caused this, roll back.' : ''}
          </span>
        </div>
      )}

      <div className="split">
        <div className="main stack">
          <LogView name={svc.name} detail={detail} />
          <section className="card" aria-labelledby="rev-h">
            <div className="card-head">
              <h2 id="rev-h">Revisions</h2>
              <span className="sub">The 10 newest. A rollback is a new revision that runs exactly what an old one ran.</span>
            </div>
            <div className="card-body flush">
              {revs.length ? (
                <div className="table-wrap">
                  <table className="grid revs">
                    <thead>
                      <tr>
                        <th scope="col">Revision</th>
                        <th scope="col">State</th>
                        <th scope="col">Created</th>
                        <th scope="col">Images</th>
                        <th scope="col">
                          <span className="sr-only">Actions</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {revs.map((r) => (
                        <RevisionRow
                          key={r.rev}
                          r={r}
                          current={r.rev === svc.current_rev}
                          onRollback={mayDeploy && eligible(svc, r) ? () => open({ kind: 'rollback', rev: r.rev }) : undefined}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <div className="empty">No revisions.</div>
              )}
            </div>
          </section>
          <section className="card" aria-labelledby="ctr-h">
            <div className="card-head">
              <h2 id="ctr-h">{live ? `Containers in r${live.rev}` : 'Containers'}</h2>
            </div>
            <div className="card-body">
              {spec ? (
                <div className="containers">
                  {order(spec).map((n) => (
                    <ContainerCard key={n} name={n} c={spec.containers[n]!} />
                  ))}
                </div>
              ) : (
                <p className="sub">No revisions.</p>
              )}
            </div>
          </section>
        </div>
        <div className="side stack">
          <section className="card" aria-labelledby="health-h">
            <div className="card-head">
              <h2 id="health-h">Health</h2>
            </div>
            <div className="card-body">
              <dl className="facts">
                <dt>Status</dt>
                <dd>
                  <Pill tone={state.tone}>{state.label}</Pill>
                </dd>
                {state.reason && (
                  <>
                    <dt>Reason</dt>
                    <dd>{state.reason}</dd>
                  </>
                )}
                {svc.current_rev ? (
                  <>
                    <dt>Live revision</dt>
                    <dd className="mono">r{svc.current_rev}</dd>
                  </>
                ) : null}
                <dt>Restarts in a row</dt>
                <dd>
                  {svc.restarts || 0}
                  {svc.restarted_at ? <span className="sub"> · last {ago(svc.restarted_at, now)}</span> : null}
                </dd>
                {svc.kind === 'ephemeral' && svc.expires_at ? (
                  <>
                    <dt>Expires</dt>
                    <dd>
                      <div className="row">
                        <span title={exact(svc.expires_at)}>{until(svc.expires_at, now)}</span>
                        {mayOperate && hasLive && (
                          <button className="btn small" type="button" disabled={extending} onClick={extend}>
                            Extend to {live?.spec.ttl ?? 'full TTL'}
                          </button>
                        )}
                      </div>
                    </dd>
                  </>
                ) : null}
                <dt>Created</dt>
                <dd>
                  <Time ts={svc.created_at} as="ago" />
                  <div className="sub wrap-anywhere">{svc.created_by}</div>
                </dd>
              </dl>
            </div>
          </section>
          <section className="card" aria-labelledby="svc-ev-h">
            <div className="card-head">
              <h2 id="svc-ev-h">Events</h2>
              <Link className="small" to="/events" search={{ service: svc.name }}>
                All events
              </Link>
            </div>
            <div className="card-body">
              <EventFeed service={svc.name} size={15} />
            </div>
          </section>
        </div>
      </div>
    </>
  )
}

function RevisionRow({ r, current, onRollback }: { r: Revision; current: boolean; onRollback?: () => void }) {
  return (
    <tr className={current ? 'current' : undefined}>
      <td className="mono nowrap">r{r.rev}</td>
      <td>
        <Pill tone={revTone(r.state)}>{r.state}</Pill>
        {r.reason && <div className="sub wrap-anywhere">{r.reason}</div>}
      </td>
      <td>
        <Time ts={r.created_at} as="ago" />
        <div className="sub wrap-anywhere">{actor(r.created_by)}</div>
      </td>
      <td>
        <div className="images">
          {order(r.spec).map((n) => {
            const img = image(r.spec.containers[n]!.image)
            return (
              <div className="image" key={n}>
                <span className="muted">{n}: </span>
                {img.name}
                {img.digest && <span className="digest"> @{img.digest}</span>}
              </div>
            )
          })}
        </div>
      </td>
      <td className="num">
        {onRollback && (
          <button className="btn small" type="button" onClick={onRollback}>
            Roll back to r{r.rev}
          </button>
        )}
      </td>
    </tr>
  )
}

function check(c: Container) {
  const hc = c.health ?? {}
  let what: string
  if (hc.path) what = `GET ${hc.path}`
  else if (hc.command?.length) what = hc.command.join(' ')
  else if (c.port) what = `TCP connect to :${c.port}`
  else what = 'container keeps running'
  return hc.timeout ? `${what} (timeout ${hc.timeout})` : what
}

function Pairs({ entries, sep }: { entries: [string, string][]; sep: string }) {
  if (!entries.length) return <span className="muted">none</span>
  return (
    <ul className="kv">
      {entries.map(([k, v]) => (
        <li key={k}>
          {k}
          {sep}
          {v}
        </li>
      ))}
    </ul>
  )
}

function ContainerCard({ name, c }: { name: string; c: Container }) {
  const img = image(c.image)
  const cmd = [...(c.command ?? []), ...(c.args ?? [])]
  return (
    <div className="ctr">
      <div className="ctr-head">
        <strong>{name}</strong>
        <span className="tag">{c.port ? `ingress :${c.port}` : 'sidecar'}</span>
        <span className="image">
          {img.name}
          {img.digest && <span className="digest"> @sha256:{img.digest}…</span>}
        </span>
      </div>
      <div className="ctr-grid">
        <Item label="Health check">
          <span className="mono">{check(c)}</span>
        </Item>
        <Item label="Resources">
          {c.resources?.memory || '512Mi'} · {c.resources?.cpus || 1} CPU
        </Item>
        {cmd.length > 0 && (
          <Item label="Command">
            <span className="mono">{cmd.join(' ')}</span>
          </Item>
        )}
        <Item label="Environment">
          <Pairs entries={Object.entries(c.env ?? {})} sep="=" />
        </Item>
        <Item label="Secrets">
          <Pairs entries={Object.entries(c.secrets ?? {})} sep=" ← " />
        </Item>
        <Item label="Volumes">
          <Pairs entries={Object.entries(c.volumes ?? {})} sep=" → " />
        </Item>
      </div>
    </div>
  )
}

function Item({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div className="label">{label}</div>
      <div className="value">{children}</div>
    </div>
  )
}
