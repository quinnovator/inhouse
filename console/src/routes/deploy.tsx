import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, createFileRoute } from '@tanstack/react-router'
import { useEffect, useRef, useState } from 'react'
import { useAccess } from '~/components/shell'
import { OperationStatus, Pill, ProblemAlert } from '~/components/ui'
import { api, newKey } from '~/lib/api'
import { image, type Tone } from '~/lib/format'
import { useOperation } from '~/lib/queries'
import { deployable, shown } from '~/lib/spec'
import { toast } from '~/lib/stores'
import type { ContainerPlan, Operation, Plan } from '~/lib/types'

export const Route = createFileRoute('/deploy')({
  validateSearch: (search: Record<string, unknown>): { from?: string } =>
    typeof search.from === 'string' && search.from ? { from: search.from } : {},
  head: () => ({ meta: [{ title: 'Deploy · inhouse' }] }),
  component: Deploy,
})

const example = `name: hello
containers:
  web:
    image: docker.io/traefik/whoami:v1.10
    port: 80
    health:
      path: /health`

function Deploy() {
  const can = useAccess()
  const client = useQueryClient()
  const { from } = Route.useSearch()
  const [text, setText] = useState('')
  const [planned, setPlanned] = useState<{ text: string; plan: Plan }>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [started, setStarted] = useState<Operation>()
  const keys = useRef(new Map<string, string>())
  const op = useOperation(started).data

  // Edit and deploy: start from the service's live spec.
  const source = useQuery({
    queryKey: ['service', from],
    queryFn: ({ signal }) => api.service(from!, signal),
    enabled: !!from,
    staleTime: Infinity,
  })
  const seeded = useRef(false)
  useEffect(() => {
    if (seeded.current || !source.data) return
    seeded.current = true
    const live = shown(source.data.service, source.data.revisions)
    if (live) setText(deployable(live.spec))
  }, [source.data])

  useEffect(() => {
    if (op?.state === 'succeeded') toast(`Deployed ${op.service}`)
    if (op && op.state !== 'running') void client.invalidateQueries({ queryKey: ['services'] })
  }, [op?.state, op, client])

  const current = planned && planned.text === text ? planned.plan : undefined
  const mayDeploy = !!current && current.may_apply && can.canDeploy(current.service)

  async function plan() {
    if (!text.trim()) {
      setError(new Error('Paste a stack spec first.'))
      return
    }
    setBusy(true)
    setError(undefined)
    setStarted(undefined)
    try {
      setPlanned({ text, plan: await api.plan(text) })
    } catch (err) {
      setPlanned(undefined)
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  async function deploy() {
    if (!current) return
    if (!keys.current.has(text)) keys.current.set(text, newKey())
    setBusy(true)
    setError(undefined)
    try {
      setStarted(await api.deploy(text, keys.current.get(text)!))
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  const running = op?.state === 'running'
  return (
    <>
      <section className="card" aria-labelledby="deploy-h">
        <div className="card-head">
          <div>
            <h1 id="deploy-h">Deploy</h1>
            <p className="sub">
              Paste a stack spec. Plan shows what would change without changing anything; Deploy records a new revision and the
              platform rolls it out.
            </p>
          </div>
          <a
            className="small"
            href="https://github.com/quinnovator/inhouse/blob/main/docs/stack-spec.md"
            target="_blank"
            rel="noopener noreferrer"
          >
            Stack spec reference
          </a>
        </div>
        <div className="card-body stack">
          {!can.canDeployAny && (
            <div className="alert">
              <strong>You can plan, but not deploy</strong>
              <span className="hint">Your grants allow reading services only.</span>
            </div>
          )}
          <label className="field">
            Stack spec
            <textarea
              className="code"
              rows={22}
              spellCheck={false}
              autoComplete="off"
              autoFocus
              value={from && source.isPending ? '# Loading…' : text}
              disabled={running || busy}
              placeholder={`# A stack spec in YAML or JSON, for example:\n${example}`}
              onChange={(e) => setText(e.target.value)}
            />
          </label>
          <div className="row">
            <button className={mayDeploy ? 'btn' : 'btn primary'} type="button" disabled={busy || running} onClick={plan}>
              Plan
            </button>
            <button className={mayDeploy ? 'btn primary' : 'btn'} type="button" disabled={!mayDeploy || busy || running} onClick={deploy}>
              Deploy
            </button>
            <span className="sub grow">Image tags are pinned to digests when the deploy runs.</span>
          </div>
          {error ? <ProblemAlert error={error} /> : null}
          {source.error ? <ProblemAlert error={source.error} /> : null}
          {op && (
            <>
              <OperationStatus op={op} />
              <p className="row">
                <Link className="btn" to="/services/$name" params={{ name: op.service }}>
                  Open {op.service}
                </Link>
              </p>
            </>
          )}
        </div>
      </section>
      {current && <PlanView plan={current} />}
    </>
  )
}

function PlanView({ plan }: { plan: Plan }) {
  const names = [...new Set([...Object.keys(plan.before ?? {}), ...Object.keys(plan.after ?? {})])]
  return (
    <section className="card" aria-labelledby="plan-h">
      <div className="card-head">
        <div>
          <h2 id="plan-h">Plan for {plan.service}</h2>
          <p className="sub">
            {plan.exists
              ? plan.live_rev
                ? `Replaces the live revision r${plan.live_rev}.`
                : 'The service exists but nothing is live.'
              : 'Creates a new service and its tailnet node.'}
          </p>
        </div>
        {plan.may_apply ? <Pill tone="ok">Allowed</Pill> : <Pill tone="bad">Would be refused</Pill>}
      </div>
      <div className="card-body stack">
        <dl className="facts">
          <dt>Exposure</dt>
          <dd>{plan.expose}</dd>
          <dt>Kind</dt>
          <dd>{plan.ttl ? `ephemeral, deleted ${plan.ttl} after its last deploy` : 'persistent'}</dd>
          <dt>Update strategy</dt>
          <dd>{plan.update_strategy}</dd>
        </dl>
        {plan.warnings.length > 0 && (
          <div className="alert warn">
            <strong>Warnings</strong>
            <ul className="warnings">
              {plan.warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </div>
        )}
        {!plan.may_apply && (
          <div className="alert bad">
            <strong>Deploying this spec would be refused.</strong>
            <span className="hint">No single grant of yours covers this name, exposure and TTL, or a warning above explains why.</span>
          </div>
        )}
      </div>
      <div className="card-body flush">
        <div className="table-wrap">
          <table className="grid plan">
            <thead>
              <tr>
                <th scope="col">Container</th>
                <th scope="col">Change</th>
                <th scope="col">Image</th>
                <th scope="col">Environment</th>
                <th scope="col">Secrets</th>
                <th scope="col">Volumes</th>
                <th scope="col">Resources</th>
              </tr>
            </thead>
            <tbody>
              {names.map((n) => (
                <PlanRow key={n} name={n} before={plan.before?.[n]} after={plan.after?.[n]} />
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </section>
  )
}

const pairs = (o: Record<string, string> | undefined, sep: string) =>
  Object.entries(o ?? {})
    .map(([k, v]) => `${k}${sep}${v}`)
    .join(', ') || '—'

function PlanRow({ name, before, after }: { name: string; before?: ContainerPlan; after?: ContainerPlan }) {
  const [tone, label]: [Tone, string] = !before
    ? ['ok', 'Added']
    : !after
      ? ['bad', 'Removed']
      : JSON.stringify(before) === JSON.stringify(after)
        ? ['none', 'Unchanged']
        : ['busy', 'Changed']
  const c = (after ?? before)!
  // diff shows one value, or the old one struck through over the new.
  const diff = (pick: (x: ContainerPlan) => string) => {
    const a = before && pick(before)
    const b = after && pick(after)
    if (!before || !after || a === b) return b ?? a
    return (
      <>
        <div className="diff-old">{a}</div>
        <div className="diff-new">{b}</div>
      </>
    )
  }
  return (
    <tr>
      <td>
        <strong>{name}</strong>
        <div className="sub">{c.port ? `ingress :${c.port}` : 'sidecar'}</div>
      </td>
      <td>
        <Pill tone={tone}>{label}</Pill>
      </td>
      <td className="image">
        {diff((x) => {
          const img = image(x.image)
          return img.digest ? `${img.name} @${img.digest}` : img.name
        })}
      </td>
      <td className="mono small">{diff((x) => x.env_keys.join(', ') || '—')}</td>
      <td className="mono small">{diff((x) => pairs(x.secrets, ' ← '))}</td>
      <td className="mono small">{diff((x) => pairs(x.volumes, ' → '))}</td>
      <td className="small nowrap">{diff((x) => `${x.resources.memory} · ${x.resources.cpus} CPU`)}</td>
    </tr>
  )
}
