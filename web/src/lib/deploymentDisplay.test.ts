import { describe, expect, it } from 'vitest'

import type { Deployment } from './api'
import { canRollBackTo, currentHealthyNumber, displayError, ERROR_TRUNCATE_AT, isErrorTruncated } from './deploymentDisplay'

function deployment(overrides: Partial<Deployment> = {}): Deployment {
  return {
    number: 1,
    image: 'ghcr.io/example/app:latest',
    status: 'healthy',
    error: '',
    started_at: '2026-01-01T00:00:00Z',
    finished_at: '2026-01-01T00:01:00Z',
    source: 'image',
    trigger: 'api',
    has_build_log: false,
    git_sha: '',
    ...overrides,
  }
}

describe('isErrorTruncated', () => {
  it('is false for an empty string', () => {
    expect(isErrorTruncated('')).toBe(false)
  })

  it('is false at exactly the truncation length', () => {
    expect(isErrorTruncated('x'.repeat(ERROR_TRUNCATE_AT))).toBe(false)
  })

  it('is true one character past the truncation length', () => {
    expect(isErrorTruncated('x'.repeat(ERROR_TRUNCATE_AT + 1))).toBe(true)
  })
})

describe('displayError', () => {
  it('returns a short error unchanged whether or not expanded', () => {
    expect(displayError('pull failed', false)).toBe('pull failed')
    expect(displayError('pull failed', true)).toBe('pull failed')
  })

  it('truncates a long error with an ellipsis when collapsed', () => {
    const long = 'x'.repeat(ERROR_TRUNCATE_AT + 20)
    const result = displayError(long, false)
    expect(result).toBe(`${'x'.repeat(ERROR_TRUNCATE_AT)}…`)
    expect(result.length).toBe(ERROR_TRUNCATE_AT + 1)
  })

  it('returns the full error when expanded', () => {
    const long = 'x'.repeat(ERROR_TRUNCATE_AT + 20)
    expect(displayError(long, true)).toBe(long)
  })
})

describe('currentHealthyNumber', () => {
  it('returns null when no deployment is healthy', () => {
    const rows = [deployment({ number: 2, status: 'failed' }), deployment({ number: 1, status: 'deploying' })]
    expect(currentHealthyNumber(rows)).toBeNull()
  })

  it('returns the first healthy deployment in a newest-first list', () => {
    const rows = [
      deployment({ number: 3, status: 'failed' }),
      deployment({ number: 2, status: 'healthy' }),
      deployment({ number: 1, status: 'healthy' }),
    ]
    expect(currentHealthyNumber(rows)).toBe(2)
  })

  it('returns null for an empty list', () => {
    expect(currentHealthyNumber([])).toBeNull()
  })
})

describe('canRollBackTo', () => {
  it('is false for the currently-live healthy deployment', () => {
    const d = deployment({ number: 5, status: 'healthy' })
    expect(canRollBackTo(d, 5)).toBe(false)
  })

  it('is true for a different healthy deployment', () => {
    const d = deployment({ number: 3, status: 'healthy' })
    expect(canRollBackTo(d, 5)).toBe(true)
  })

  it('is false for a non-healthy deployment regardless of current', () => {
    expect(canRollBackTo(deployment({ number: 3, status: 'failed' }), 5)).toBe(false)
    expect(canRollBackTo(deployment({ number: 3, status: 'deploying' }), null)).toBe(false)
  })

  it('is true when nothing is currently healthy', () => {
    const d = deployment({ number: 3, status: 'healthy' })
    expect(canRollBackTo(d, null)).toBe(true)
  })
})
