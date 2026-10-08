// Logs for one service: any recent revision and container, newest 200
// lines, with older pages and an optional follow mode.

import { useQuery } from '@tanstack/react-query'
import { useLayoutEffect, useRef, useState } from 'react'
import { ApiError, api } from '~/lib/api'
import { order } from '~/lib/spec'
import { toast } from '~/lib/stores'
import type { ServiceDetail } from '~/lib/types'

const TAIL = 200

export function LogView({ name, detail }: { name: string; detail: ServiceDetail }) {
  const [rev, setRev] = useState(0) // 0: the live revision, else the newest
  const [picked, setPicked] = useState('') // '': the ingress
  const [follow, setFollow] = useState(false)
  const [older, setOlder] = useState<{ lines: string[]; cursor?: string }>()
  const out = useRef<HTMLDivElement>(null)
  const stick = useRef(true)

  const { service: svc, revisions } = detail
  const r = rev ? revisions.find((x) => x.rev === rev) : (revisions.find((x) => x.rev === svc.current_rev) ?? revisions[0])
  const names = r ? order(r.spec) : []
  const ingress = names.find((n) => (r!.spec.containers[n]!.port ?? 0) > 0) ?? names[0] ?? ''
  const container = picked || ingress

  const page = useQuery({
    queryKey: ['logs', name, rev, container],
    queryFn: ({ signal }) => api.logs(name, { rev, container, tail: TAIL }, signal),
    enabled: !!container,
    refetchInterval: follow ? 3000 : false,
    retry: false,
  })

  // Keep the view pinned to the newest line unless the reader scrolled up.
  useLayoutEffect(() => {
    const el = out.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [page.data])

  function choose(nextRev: number, nextContainer: string) {
    setRev(nextRev)
    setPicked(nextContainer)
    setOlder(undefined)
    stick.current = true
  }

  async function loadOlder() {
    const cursor = older ? older.cursor : page.data?.next_cursor
    if (!cursor) return
    const el = out.current!
    const height = el.scrollHeight
    try {
      const p = await api.logs(name, { rev, container, tail: TAIL, cursor })
      setOlder({ lines: [...p.lines, ...(older?.lines ?? [])], cursor: p.next_cursor })
      requestAnimationFrame(() => {
        el.scrollTop = el.scrollHeight - height
      })
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'bad')
      if (err instanceof ApiError && err.code === 'invalid_request') {
        setOlder(undefined)
        void page.refetch()
      }
    }
  }

  const more = older ? older.cursor : page.data?.next_cursor
  const lines = [...(older?.lines ?? []), ...(page.data?.lines ?? [])]

  return (
    <section className="terminal" aria-labelledby="logs-h">
      <div className="terminal-head">
        <h2 id="logs-h">Logs</h2>
        <select aria-label="Revision" value={rev} onChange={(e) => choose(Number(e.target.value), '')}>
          <option value={0}>{svc.current_rev ? `Live (r${svc.current_rev})` : 'Newest revision'}</option>
          {revisions.map((x) => (
            <option key={x.rev} value={x.rev}>
              r{x.rev} · {x.state}
            </option>
          ))}
        </select>
        {names.length > 1 && (
          <span className="tseg" role="group" aria-label="Container">
            {names.map((n) => (
              <button key={n} type="button" aria-pressed={n === container} onClick={() => choose(rev, n)}>
                {n}
              </button>
            ))}
          </span>
        )}
        <button
          className="tbtn"
          type="button"
          onClick={() => {
            setOlder(undefined)
            stick.current = true
            void page.refetch()
          }}
        >
          Refresh
        </button>
        <button
          className="tbtn"
          type="button"
          aria-pressed={follow}
          onClick={() => {
            setFollow(!follow)
            setOlder(undefined)
          }}
        >
          Follow
        </button>
        <span className="note">Secret values are redacted on the host</span>
      </div>
      <div
        className="log"
        ref={out}
        tabIndex={0}
        role="log"
        aria-label="Log output"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
      >
        {page.error ? (
          <div className="log-status">Logs are unavailable: {page.error.message}</div>
        ) : !page.data ? (
          <div className="log-status">Loading…</div>
        ) : (
          <>
            {more && !follow && (
              <div className="older">
                <button className="tbtn" type="button" onClick={loadOlder}>
                  Load older lines
                </button>
              </div>
            )}
            {!lines.length && <div className="log-status">No output yet.</div>}
            {lines.map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </>
        )}
      </div>
    </section>
  )
}
