import { createFileRoute } from '@tanstack/react-router'
import { RunningOperations } from '~/components/RunningOperations'
import { useAccess } from '~/components/shell'
import { ProblemAlert, Time } from '~/components/ui'
import { duration } from '~/lib/format'
import { bytes, metric, metricReason, percent } from '~/lib/platform'
import { usePlatform } from '~/lib/queries'
import type { HostSnapshot } from '~/lib/types'

export const Route = createFileRoute('/platform')({
  head: () => ({ meta: [{ title: 'Platform · inhouse' }] }),
  component: PlatformPage,
})

function PlatformPage() {
  const can = useAccess()
  const platform = usePlatform(can.admin)
  if (!can.admin) return <div className="alert" role="status">The platform is visible to admins. Your service pages show operations on services you can read.</div>
  const data = platform.data
  return (
    <div className="stack">
      <div className="row between">
        <div><h1>Platform</h1><p className="sub">The daemon and the single host running your services. Refreshes every 5 seconds.</p></div>
        <button type="button" className="btn" disabled={platform.isFetching} onClick={() => void platform.refetch()}>Refresh</button>
      </div>
      {platform.error && <ProblemAlert error={platform.error} />}
      {!data ? <p className="sub" role="status">{platform.error ? 'Platform data is unavailable.' : 'Loading platform…'}</p> : (
        <>
          <div className="split">
            <section className="card side" aria-labelledby="daemon-h">
              <div className="card-head"><h2 id="daemon-h">Daemon</h2></div>
              <div className="card-body">
                <dl className="facts">
                  <dt>Version</dt><dd className="mono">{data.daemon.version}</dd>
                  <dt>Started</dt><dd><Time ts={data.daemon.started_at} /></dd>
                  <dt>Uptime</dt><dd>{duration(data.daemon.uptime_seconds)}</dd>
                </dl>
              </div>
            </section>
            <section className="card main" aria-labelledby="host-h">
              <div className="card-head">
                <div><h2 id="host-h">Host: {data.host.hostname || 'name unavailable'}</h2><p className="sub">{data.host.os} · {data.host.arch} · {data.host.logical_cpus} logical CPUs</p></div>
                <span className="sub">Last sampled <Time ts={data.host.sampled_at} as="ago" /></span>
              </div>
              <div className="card-body"><HostMetrics host={data.host} /></div>
            </section>
          </div>
          <section className="card" aria-labelledby="operations-h">
            <div className="card-head"><h2 id="operations-h">Running operations</h2><span className="sub">Across all services on this host</span></div>
            <div className="card-body"><RunningOperations operations={data.running_operations} /></div>
          </section>
        </>
      )}
    </div>
  )
}

function HostMetrics({ host: { storage, cpu, memory } }: { host: HostSnapshot }) {
  return (
    <div className="host-metrics">
      <section aria-labelledby="storage-h">
        <h3 id="storage-h">Storage available</h3>
        <p className="metric-value">{metric(storage.status, storage.available_bytes, bytes)}</p>
        <p className="sub">{storage.status === 'ok' ? `of ${bytes(storage.total_bytes)} on the data filesystem, writable by inhouse` : metricReason(storage.status, storage.reason)}</p>
      </section>
      <section aria-labelledby="cpu-h">
        <h3 id="cpu-h">CPU use</h3>
        <p className="metric-value">{metric(cpu.status, cpu.utilization_percent, percent)}</p>
        <p className="sub">{cpu.status === 'ok' ? 'Whole-host busy time since the previous reading' : metricReason(cpu.status, cpu.reason)}</p>
      </section>
      <section aria-labelledby="memory-h">
        <h3 id="memory-h">Memory</h3>
        <p className="metric-value">{metric(memory.status, memory.available_bytes, bytes)}{memory.status === 'ok' && ' available'}</p>
        <p className="sub">{memory.status === 'ok' ? `${bytes(memory.used_bytes)} used of ${bytes(memory.total_bytes)}` : metricReason(memory.status, memory.reason)}</p>
        {memory.status === 'ok' && <p className="sub">Swap free: {bytes(memory.swap_free_bytes)} of {bytes(memory.swap_total_bytes)}</p>}
      </section>
      <section aria-labelledby="pressure-h">
        <h3 id="pressure-h">Memory pressure</h3>
        <p className="metric-value">{metric(memory.status, memory.pressure_some_avg10_percent, percent)}</p>
        <p className="sub">{memory.status === 'ok' ? (memory.pressure_some_avg10_percent === undefined ? 'The kernel does not report memory pressure.' : 'Time some task stalled on memory, averaged over 10 seconds (PSI)') : metricReason(memory.status, memory.reason)}</p>
      </section>
    </div>
  )
}
