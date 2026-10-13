import type { MetricStatus } from './types'

export function bytes(value: number | undefined) {
  if (value === undefined || !Number.isFinite(value) || value < 0) return 'Unavailable'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return `${value.toLocaleString(undefined, { maximumFractionDigits: i ? 1 : 0 })} ${units[i]}`
}

export function percent(value: number | undefined) {
  if (value === undefined || !Number.isFinite(value) || value < 0 || value > 100) return 'Unavailable'
  return `${value.toFixed(1)}%`
}

// A missing reading and a measured zero must stay distinct. Never show
// a number carried by a failed metric, even if an older server sends it.
export function metric(status: MetricStatus, value: number | undefined, format: (n: number | undefined) => string) {
  return status === 'warming_up' ? 'Collecting…' : status === 'ok' ? format(value) : 'Unavailable'
}

export function metricReason(status: MetricStatus, reason?: string) {
  if (status === 'warming_up') return 'CPU use needs two readings. The next refresh will measure the interval.'
  if (status === 'ok') return ''
  return reason === 'unsupported_platform' ? 'Host metrics are supported on Linux.' : 'The host reading could not be collected.'
}
