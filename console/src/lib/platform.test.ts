import { describe, expect, it } from 'vitest'
import { bytes, metric, percent } from './platform'

describe('host metric display', () => {
  it('preserves measured zero and distinguishes missing readings', () => {
    expect(metric('ok', 0, bytes)).toBe('0 B')
    expect(metric('ok', 0, percent)).toBe('0.0%')
    expect(metric('ok', undefined, percent)).toBe('Unavailable')
    expect(metric('warming_up', 0, percent)).toBe('Collecting…')
    expect(metric('unavailable', 35, percent)).toBe('Unavailable')
  })
  it('rejects invalid measurements rather than presenting numbers', () => {
    for (const value of [NaN, Infinity, -1]) {
      expect(bytes(value)).toBe('Unavailable')
      expect(percent(value)).toBe('Unavailable')
    }
    expect(percent(100.1)).toBe('Unavailable')
    expect(bytes(1024 ** 3)).toBe('1 GiB')
    expect(percent(1.25)).toBe('1.3%')
  })
})
