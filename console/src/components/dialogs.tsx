// Rollback, delete, restart, redeploy, stop and start: each records an
// operation, then follows it here.

import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useEffect, useRef, useState } from 'react'
import { useAccess } from '~/components/shell'
import { Modal, OperationStatus, ProblemAlert } from '~/components/ui'
import { api, newKey, type ServiceAction } from '~/lib/api'
import { actor, date, until } from '~/lib/format'
import { useOperation } from '~/lib/queries'
import { eligible, hasVolumes } from '~/lib/spec'
import { toast } from '~/lib/stores'
import type { Operation, ServiceDetail } from '~/lib/types'

// useAction runs one operation-starting call at a time and follows the
// operation it returns. Keys are per intent, so retrying after a network
// error can't start a second operation.
function useAction(service: string) {
  const client = useQueryClient()
  const keys = useRef(new Map<string, string>())
  const [started, setStarted] = useState<Operation>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const op = useOperation(started).data

  useEffect(() => {
    if (op && op.state !== 'running') {
      void client.invalidateQueries({ queryKey: ['service', service] })
      void client.invalidateQueries({ queryKey: ['services'] })
    }
  }, [op, client, service])

  async function run(intent: string, call: (key: string) => Promise<Operation>) {
    if (!keys.current.has(intent)) keys.current.set(intent, newKey())
    setBusy(true)
    setError(undefined)
    try {
      setStarted(await call(keys.current.get(intent)!))
      void client.invalidateQueries({ queryKey: ['service', service] })
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return { op, error, busy, run }
}

export function RollbackDialog({ detail, preselect, onClose }: { detail: ServiceDetail; preselect?: number; onClose: () => void }) {
  const can = useAccess()
  const svc = detail.service
  const targets = detail.revisions.filter((r) => eligible(svc, r))
  const [to, setTo] = useState(preselect ?? targets[0]?.rev ?? 0)
  const [restore, setRestore] = useState(false)
  const action = useAction(svc.name)
  const target = targets.find((r) => r.rev === to)
  const canRestore = can.admin && svc.kind === 'persistent' && hasVolumes(target?.spec)
  const withVolumes = canRestore && restore
  const locked = action.busy || !!action.op

  const finished = action.op && action.op.state !== 'running' ? action.op.state : undefined
  useEffect(() => {
    if (finished) toast(`Rollback of ${svc.name} ${finished}`, finished === 'failed' ? 'bad' : undefined)
  }, [finished, svc.name])

  return (
    <Modal
      title={`Roll back ${svc.name}`}
      onClose={onClose}
      actions={
        <>
          <button className="btn" type="button" onClick={onClose}>
            {action.op ? 'Close' : 'Cancel'}
          </button>
          {!action.op && (
            <button
              className="btn primary"
              type="button"
              disabled={locked || !target}
              onClick={() => action.run(`${to}/${withVolumes}`, (key) => api.rollback(svc.name, to, withVolumes, key))}
            >
              Roll back
            </button>
          )}
        </>
      }
    >
      <p>
        This deploys a new revision that runs exactly the images and settings of the one you pick. It goes through the same
        health checks as a deploy.
      </p>
      <label className="field">
        Revision
        <select value={to} disabled={locked} onChange={(e) => setTo(Number(e.target.value))}>
          {targets.map((r) => (
            <option key={r.rev} value={r.rev}>
              r{r.rev} · {r.state} · {date(r.created_at)} by {actor(r.created_by)}
            </option>
          ))}
        </select>
      </label>
      {canRestore && (
        <label className="check">
          <input type="checkbox" checked={restore} disabled={locked} onChange={(e) => setRestore(e.target.checked)} />
          <span>
            <strong>Also restore volume data</strong>
            <br />
            <span className="sub">
              Replaces current data with the snapshots taken when that revision was deployed. The live revision stops first,
              and current data moves to trash.
            </span>
          </span>
        </label>
      )}
      {action.error ? <ProblemAlert error={action.error} /> : null}
      {action.op && <OperationStatus op={action.op} />}
    </Modal>
  )
}

export function DeleteDialog({ detail, onClose }: { detail: ServiceDetail; onClose: () => void }) {
  const svc = detail.service
  const navigate = useNavigate()
  const [typed, setTyped] = useState('')
  const action = useAction(svc.name)
  const locked = action.busy || !!action.op

  useEffect(() => {
    if (action.op?.state === 'succeeded') {
      toast(`${svc.name} deleted`)
      void navigate({ to: '/' })
    }
  }, [action.op?.state, navigate, svc.name])

  return (
    <Modal
      title={`Delete ${svc.name}?`}
      onClose={onClose}
      actions={
        <>
          <button className="btn" type="button" onClick={onClose}>
            {action.op ? 'Close' : 'Cancel'}
          </button>
          {!action.op && (
            <button
              className="btn danger solid"
              type="button"
              disabled={locked || typed !== svc.name}
              onClick={() => action.run('delete', (key) => api.remove(svc.name, key))}
            >
              Delete {svc.name}
            </button>
          )}
        </>
      }
    >
      <p>
        {svc.kind === 'persistent'
          ? 'This stops every pod, removes the tailnet node and moves the volumes to trash for seven days. The name and URL stop working.'
          : 'This stops every pod and removes the tailnet node and the volumes. The name and URL stop working.'}
      </p>
      <label className="field">
        <span>
          Type <code>{svc.name}</code> to confirm
        </span>
        <input type="text" autoComplete="off" spellCheck={false} value={typed} disabled={locked} onChange={(e) => setTyped(e.target.value)} />
      </label>
      {action.error ? <ProblemAlert error={action.error} /> : null}
      {action.op && <OperationStatus op={action.op} />}
    </Modal>
  )
}

const actions: Record<ServiceAction, { title: string; button: string; done: string; danger?: boolean }> = {
  restart: { title: 'Restart', button: 'Restart', done: 'restarted' },
  redeploy: { title: 'Redeploy', button: 'Redeploy', done: 'redeployed' },
  stop: { title: 'Stop', button: 'Stop', done: 'stopped', danger: true },
  start: { title: 'Start', button: 'Start', done: 'started' },
}

// ActionDialog confirms an operation on the live revision and follows it.
export function ActionDialog({ detail, action, onClose }: { detail: ServiceDetail; action: ServiceAction; onClose: () => void }) {
  const svc = detail.service
  // The service refreshes while the operation runs; describe what was live
  // when the dialog opened.
  const [opened] = useState(svc.current_rev)
  const live = detail.revisions.find((r) => r.rev === opened)
  const rev = `r${opened}`
  const meta = actions[action]
  const run = useAction(svc.name)
  const locked = run.busy || !!run.op

  const op = run.op
  const finished = op && op.state !== 'running' ? op.state : undefined
  useEffect(() => {
    if (!finished || !op) return
    if (finished === 'failed') toast(`${meta.title} of ${svc.name} failed`, 'bad')
    else if (action === 'redeploy' && op.rev === opened) toast(`Nothing changed: no secret ${svc.name} uses has a new value`)
    else toast(`${svc.name} ${meta.done}`)
  }, [finished, op?.operation_id])

  let body: string
  switch (action) {
    case 'restart':
      body =
        live?.spec.update_strategy === 'recreate'
          ? `Starts a copy of ${rev} as a new revision, with the same images and secret values. The service updates by recreating, so ${rev} stops first and the service is unavailable until the copy passes its health checks.`
          : `Starts a copy of ${rev} as a new revision, with the same images and secret values, and moves traffic to it once it passes its health checks. ${rev} keeps serving until then.`
      break
    case 'redeploy':
      body = `Deploys ${rev} again as a new revision, with each secret at its current value. Images stay at the same digests; to pull a newer image, edit and deploy the spec with its tag. If no secret has changed since ${rev} was deployed, nothing happens.`
      break
    case 'stop':
      body =
        `Stops ${rev}. Its address answers 503 until you start it again, and deploys are refused meanwhile. Revisions, volumes and the tailnet node are kept.` +
        (svc.kind === 'ephemeral' && svc.expires_at ? ` It still expires ${until(svc.expires_at)}.` : '')
      break
    case 'start':
      body = `Starts ${rev} again from its pinned images. It gets traffic once it passes its health checks.`
      break
  }

  return (
    <Modal
      title={`${meta.title} ${svc.name}`}
      onClose={onClose}
      actions={
        <>
          <button className="btn" type="button" onClick={onClose}>
            {run.op ? 'Close' : 'Cancel'}
          </button>
          {!run.op && (
            <button
              className={meta.danger ? 'btn danger solid' : 'btn primary'}
              type="button"
              disabled={locked}
              onClick={() => run.run(action, (key) => api.act(svc.name, action, key))}
            >
              {meta.button}
            </button>
          )}
        </>
      }
    >
      <p>{body}</p>
      {run.error ? <ProblemAlert error={run.error} /> : null}
      {run.op && <OperationStatus op={run.op} />}
    </Modal>
  )
}
