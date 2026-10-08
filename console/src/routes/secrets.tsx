import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { type FormEvent, useRef, useState } from 'react'
import { useAccess } from '~/components/shell'
import { Modal, ProblemAlert, Time } from '~/components/ui'
import { api } from '~/lib/api'
import { useSecrets } from '~/lib/queries'
import { toast } from '~/lib/stores'

export const Route = createFileRoute('/secrets')({
  head: () => ({ meta: [{ title: 'Secrets · inhouse' }] }),
  component: Secrets,
})

// Secrets are write-only: this page lists names and who set them, and can
// replace or delete a value, but nothing here can read one back.
function Secrets() {
  const can = useAccess()
  if (!can.admin) {
    return (
      <div className="card">
        <div className="empty">
          <strong>Only admins can see or set secrets</strong>
          <span>Ask an admin to set the secrets your services need.</span>
        </div>
      </div>
    )
  }
  return <SecretsAdmin />
}

function SecretsAdmin() {
  const client = useQueryClient()
  const secrets = useSecrets()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<unknown>()
  const [deleting, setDeleting] = useState<string>()
  const valueRef = useRef<HTMLTextAreaElement>(null)

  async function save(e: FormEvent) {
    e.preventDefault()
    setSaving(true)
    setError(undefined)
    try {
      await api.setSecret(name.trim(), value)
      toast(`Secret ${name.trim()} saved`)
      setName('')
      setValue('')
      void client.invalidateQueries({ queryKey: ['secrets'] })
    } catch (err) {
      setError(err)
    } finally {
      setSaving(false)
    }
  }

  const list = secrets.data
  return (
    <div className="split">
      <section className="card main" aria-labelledby="secrets-h">
        <div className="card-head">
          <div>
            <h1 id="secrets-h">Secrets</h1>
            <p className="sub">Encrypted with the host’s age key. Values are never shown again and are redacted from logs.</p>
          </div>
        </div>
        <div className="card-body flush">
          {!list ? (
            secrets.error ? (
              <div className="card-body">
                <ProblemAlert error={secrets.error} />
              </div>
            ) : (
              <div className="empty">Loading…</div>
            )
          ) : !list.length ? (
            <div className="empty">
              <strong>No secrets yet</strong>
              <span>
                Set one here or with <code>inhouse secrets set NAME</code>.
              </span>
            </div>
          ) : (
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Last set</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((s) => (
                    <tr key={s.name}>
                      <td className="mono wrap-anywhere">{s.name}</td>
                      <td>
                        <Time ts={s.updated_at} as="ago" />
                        <div className="sub wrap-anywhere">{s.updated_by}</div>
                      </td>
                      <td className="num">
                        <button
                          className="btn small"
                          type="button"
                          onClick={() => {
                            setName(s.name)
                            valueRef.current?.focus()
                          }}
                        >
                          Replace
                        </button>{' '}
                        <button className="btn small danger" type="button" onClick={() => setDeleting(s.name)}>
                          Delete
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </section>
      <section className="card side" aria-labelledby="set-h">
        <div className="card-head">
          <h2 id="set-h">Set a secret</h2>
        </div>
        <form className="card-body stack" onSubmit={save}>
          <label className="field">
            Name
            <span className="hint">A DNS label, or registry-auth/&lt;host&gt; for image-pull credentials.</span>
            <input type="text" autoComplete="off" spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="field">
            Value
            <span className="hint">
              Replaces any current value. Running revisions keep the version they were deployed with; the next deploy picks this up.
            </span>
            <textarea
              ref={valueRef}
              className="code"
              rows={4}
              autoComplete="off"
              spellCheck={false}
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          </label>
          <div className="row">
            <button className="btn primary" type="submit" disabled={saving || !name.trim()}>
              Save secret
            </button>
          </div>
          {error ? <ProblemAlert error={error} /> : null}
        </form>
      </section>
      {deleting && <DeleteSecret name={deleting} onClose={() => setDeleting(undefined)} />}
    </div>
  )
}

function DeleteSecret({ name, onClose }: { name: string; onClose: () => void }) {
  const client = useQueryClient()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()

  async function remove() {
    setBusy(true)
    try {
      await api.deleteSecret(name)
      toast(`Secret ${name} deleted`)
      void client.invalidateQueries({ queryKey: ['secrets'] })
      onClose()
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }

  return (
    <Modal
      title={`Delete ${name}?`}
      onClose={onClose}
      actions={
        <>
          <button className="btn" type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="btn danger solid" type="button" disabled={busy} onClick={remove}>
            Delete secret
          </button>
        </>
      }
    >
      <p>
        Services that reference it can no longer be deployed until it is set again. A secret still used by a running revision
        cannot be deleted.
      </p>
      {error ? <ProblemAlert error={error} /> : null}
    </Modal>
  )
}
