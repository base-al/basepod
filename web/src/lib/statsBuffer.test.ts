import { describe, expect, it } from 'vitest'

import type { AllStatsSample } from './api'
import {
  SPARKLINE_WINDOW,
  emptyStatsEntry,
  formatCpuPercent,
  formatMemUsedLive,
  pushStatsSample,
  type AppStatsBySlug,
} from './statsBuffer'

function sample(slug: string, cpuPercent: number): AllStatsSample {
  return {
    slug,
    cpu_percent: cpuPercent,
    mem_used_bytes: 1000,
    mem_limit_bytes: 2000,
    pids: 5,
    net_rx_bytes: 0,
    net_tx_bytes: 0,
    block_read_bytes: 0,
    block_write_bytes: 0,
  }
}

describe('emptyStatsEntry', () => {
  it('starts with no history and no latest sample', () => {
    expect(emptyStatsEntry()).toEqual({ cpuHistory: [], latest: null })
  })
})

describe('pushStatsSample', () => {
  it('creates a fresh entry for a slug not yet present', () => {
    const got = pushStatsSample({}, sample('blog', 12.5))
    expect(got.blog!.cpuHistory).toEqual([12.5])
    expect(got.blog!.latest).toEqual(sample('blog', 12.5))
  })

  it('appends to an existing history rather than replacing it', () => {
    let bySlug: AppStatsBySlug = {}
    bySlug = pushStatsSample(bySlug, sample('blog', 10))
    bySlug = pushStatsSample(bySlug, sample('blog', 20))
    expect(bySlug.blog!.cpuHistory).toEqual([10, 20])
    expect(bySlug.blog!.latest!.cpu_percent).toBe(20)
  })

  it('caps the history at SPARKLINE_WINDOW, dropping the oldest', () => {
    let bySlug: AppStatsBySlug = {}
    for (let i = 0; i < SPARKLINE_WINDOW + 5; i++) {
      bySlug = pushStatsSample(bySlug, sample('blog', i))
    }
    expect(bySlug.blog!.cpuHistory).toHaveLength(SPARKLINE_WINDOW)
    // The first 5 readings (0..4) should have been dropped; the window
    // should now hold 5..(SPARKLINE_WINDOW+4).
    expect(bySlug.blog!.cpuHistory[0]).toBe(5)
    expect(bySlug.blog!.cpuHistory[SPARKLINE_WINDOW - 1]).toBe(SPARKLINE_WINDOW + 4)
  })

  it('keeps separate histories per app slug', () => {
    let bySlug: AppStatsBySlug = {}
    bySlug = pushStatsSample(bySlug, sample('blog', 10))
    bySlug = pushStatsSample(bySlug, sample('shop', 90))
    expect(bySlug.blog!.cpuHistory).toEqual([10])
    expect(bySlug.shop!.cpuHistory).toEqual([90])
  })

  it('does not mutate the input object (immutable update)', () => {
    const before: AppStatsBySlug = { blog: { cpuHistory: [1], latest: sample('blog', 1) } }
    const after = pushStatsSample(before, sample('blog', 2))
    expect(before.blog!.cpuHistory).toEqual([1]) // unchanged
    expect(after.blog!.cpuHistory).toEqual([1, 2])
    expect(after).not.toBe(before)
  })
})

describe('formatCpuPercent', () => {
  it('rounds to the nearest whole percent', () => {
    expect(formatCpuPercent(12.4)).toBe('12%')
    expect(formatCpuPercent(12.5)).toBe('13%')
  })

  it('shows values above 100 for a multi-core-busy container', () => {
    expect(formatCpuPercent(184.2)).toBe('184%')
  })

  it('shows 0% for an idle container', () => {
    expect(formatCpuPercent(0)).toBe('0%')
  })
})

describe('formatMemUsedLive', () => {
  it('shows used / configured-limit in MB', () => {
    expect(formatMemUsedLive(41 * 1024 * 1024, 512)).toBe('41 / 512 MB')
  })

  it('shows just the used amount when the app has no configured limit', () => {
    expect(formatMemUsedLive(41 * 1024 * 1024, 0)).toBe('41 MB')
  })

  it('rounds bytes to the nearest MB', () => {
    expect(formatMemUsedLive(1.6 * 1024 * 1024, 0)).toBe('2 MB')
  })
})
