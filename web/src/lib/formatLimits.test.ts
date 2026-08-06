import { describe, expect, it } from 'vitest'

import { formatCpuLimit, formatLimitsSummary, formatMemoryLimit } from './formatLimits'

describe('formatMemoryLimit', () => {
  it('shows Unlimited for 0', () => {
    expect(formatMemoryLimit(0)).toBe('Unlimited')
  })

  it('shows MiB below 1024', () => {
    expect(formatMemoryLimit(512)).toBe('512 MiB')
  })

  it('shows GiB for exact multiples of 1024', () => {
    expect(formatMemoryLimit(2048)).toBe('2 GiB')
  })

  it('falls back to MiB when not an exact GiB multiple', () => {
    expect(formatMemoryLimit(1536)).toBe('1536 MiB')
  })
})

describe('formatCpuLimit', () => {
  it('shows Unlimited for 0', () => {
    expect(formatCpuLimit(0)).toBe('Unlimited')
  })

  it('shows the core count otherwise', () => {
    expect(formatCpuLimit(1.5)).toBe('1.5 vCPU')
  })
})

describe('formatLimitsSummary', () => {
  it('collapses to a single Unlimited when both are unset', () => {
    expect(formatLimitsSummary(0, 0)).toBe('Unlimited')
  })

  it('combines both limits when either is set', () => {
    expect(formatLimitsSummary(512, 1)).toBe('512 MiB · 1 vCPU')
  })

  it('combines a set memory limit with an unlimited CPU', () => {
    expect(formatLimitsSummary(512, 0)).toBe('512 MiB · Unlimited')
  })
})
