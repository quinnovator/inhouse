// The console's frame: identity, navigation and toasts. Nothing renders
// until whoami answers, so every page can assume a known caller.

import { Link, useRouterState } from '@tanstack/react-router'
import { createContext, type ReactNode, useContext, useEffect, useRef } from 'react'
import { type Access, access } from '~/lib/access'
import { ApiError } from '~/lib/api'
import { useWhoami } from '~/lib/queries'
import { useConnected, useToasts } from '~/lib/stores'

const AccessContext = createContext<Access | null>(null)

export function useAccess() {
  const can = useContext(AccessContext)
  if (!can) throw new Error('useAccess outside the console shell')
  return can
}

export function Shell({ children }: { children: ReactNode }) {
  const me = useWhoami()
  if (me.error) return <NoAccess error={me.error} />
  if (!me.data) return <p className="boot">Loading…</p>
  const can = access(me.data)
  return (
    <AccessContext.Provider value={can}>
      <Header />
      <main id="main" className="page">
        {children}
      </main>
      <Toasts />
    </AccessContext.Provider>
  )
}

export function Logo() {
  return (
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true">
      <path d="M3 10.5 12 4l9 6.5V20H3z" />
      <circle cx="12" cy="14" r="2.2" />
    </svg>
  )
}

function Header() {
  const can = useAccess()
  const connected = useConnected()
  const path = useRouterState({ select: (s) => s.location.pathname })
  const section = path === '/' || path.startsWith('/services/') ? 'services' : path.slice(1)
  const current = (s: string) => (section === s ? 'page' : undefined)
  return (
    <header className="top">
      <div className="top-inner">
        <Link className="brand" to="/">
          <Logo />
          inhouse
        </Link>
        <span className="host-chip" title="Control node">
          {typeof location === 'undefined' ? '' : location.host}
        </span>
        <nav className="nav" aria-label="Primary">
          <Link to="/" aria-current={current('services')}>
            Services
          </Link>
          <Link to="/events" aria-current={current('events')}>
            Events
          </Link>
          {can.canDeployAny && (
            <Link to="/deploy" aria-current={current('deploy')}>
              Deploy
            </Link>
          )}
          {can.admin && (
            <Link to="/secrets" aria-current={current('secrets')}>
              Secrets
            </Link>
          )}
        </nav>
        <div className="top-right">
          <span className={connected ? 'conn' : 'conn on'} role="status">
            {connected ? '' : 'Reconnecting…'}
          </span>
          <WhoMenu />
        </div>
      </div>
    </header>
  )
}

function WhoMenu() {
  const can = useAccess()
  const ref = useRef<HTMLDetailsElement>(null)
  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) ref.current.open = false
    }
    document.addEventListener('click', close)
    return () => document.removeEventListener('click', close)
  }, [])
  return (
    <details className="who" ref={ref}>
      <summary>
        <span className="wrap-anywhere">{can.principal}</span>
        <span className="role">{can.role}</span>
      </summary>
      <div className="who-panel">
        <h3>Your grants</h3>
        <p className="sub">From the tailnet policy file. The console, the CLI and MCP all use them.</p>
        {can.grants.map((g, i) => (
          <dl className="grant" key={i}>
            <dt>Role</dt>
            <dd>{g.role}</dd>
            {g.services && (
              <>
                <dt>Services</dt>
                <dd className="mono">{g.services.join(', ')}</dd>
              </>
            )}
            {g.expose && (
              <>
                <dt>Exposure</dt>
                <dd>{g.expose.join(', ')}</dd>
              </>
            )}
            {g.max_ttl && (
              <>
                <dt>Max TTL</dt>
                <dd>{g.max_ttl}</dd>
              </>
            )}
          </dl>
        ))}
      </div>
    </details>
  )
}

function NoAccess({ error }: { error: unknown }) {
  const denied = error instanceof ApiError && error.status === 403
  return (
    <main className="page narrow">
      <title>No access · inhouse</title>
      <div className="card">
        <div className="card-body stack">
          <div className="row">
            <span className="brand">
              <Logo />
              inhouse
            </span>
          </div>
          <h1>{denied ? 'You have no access to this inhouse' : 'Cannot identify you'}</h1>
          <p>
            {denied
              ? 'Your tailnet identity has no grant for this instance. An admin can add one to the tailnet policy file.'
              : error instanceof Error
                ? error.message
                : String(error)}
          </p>
          {!denied && error instanceof ApiError && error.hint && <p className="sub">{error.hint}</p>}
        </div>
      </div>
    </main>
  )
}

function Toasts() {
  const toasts = useToasts()
  return (
    <div className="toasts" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={t.tone ? `toast ${t.tone}` : 'toast'}>
          {t.message}
        </div>
      ))}
    </div>
  )
}
