import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { relativeTime } from './relativeTime'

const NOW = new Date('2026-01-01T12:00:00.000Z')

function isoOffset(seconds: number): string {
  return new Date(NOW.getTime() + seconds * 1000).toISOString()
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})

afterEach(() => {
  vi.useRealTimers()
})

describe('relativeTime', () => {
  it('returns "" for an empty input', () => {
    expect(relativeTime('')).toBe('')
  })

  it('returns "" for an unparseable input', () => {
    expect(relativeTime('not-a-date')).toBe('')
  })

  it('treats anything within 5s — past, future, or exactly now — as "just now"', () => {
    expect(relativeTime(isoOffset(0))).toBe('just now')
    expect(relativeTime(isoOffset(-4))).toBe('just now')
    expect(relativeTime(isoOffset(4))).toBe('just now')
  })

  it('crosses the 5s threshold into a real unit', () => {
    expect(relativeTime(isoOffset(-5))).toBe('5 seconds ago')
    expect(relativeTime(isoOffset(5))).toBe('in 5 seconds')
  })

  it('formats past deltas across every unit', () => {
    expect(relativeTime(isoOffset(-30))).toBe('30 seconds ago')
    expect(relativeTime(isoOffset(-60))).toBe('1 minute ago')
    expect(relativeTime(isoOffset(-120))).toBe('2 minutes ago')
    expect(relativeTime(isoOffset(-3600))).toBe('1 hour ago')
    expect(relativeTime(isoOffset(-86400))).toBe('1 day ago')
    expect(relativeTime(isoOffset(-604800))).toBe('1 week ago')
    expect(relativeTime(isoOffset(-2592000))).toBe('1 month ago')
    expect(relativeTime(isoOffset(-31536000))).toBe('1 year ago')
  })

  // Regression: relativeTime's "just now" check must use Math.abs(deltaSeconds),
  // not deltaSeconds directly — a future timestamp makes deltaSeconds negative,
  // and an unguarded `< 5` would swallow every future delta (any negative
  // number is < 5), never reaching the "in N units" branch below.
  it('formats future deltas as "in N units" (future-timestamp regression)', () => {
    expect(relativeTime(isoOffset(120))).toBe('in 2 minutes')
    expect(relativeTime(isoOffset(3600))).toBe('in 1 hour')
    expect(relativeTime(isoOffset(86400))).toBe('in 1 day')
  })
})
