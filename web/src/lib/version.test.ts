import { describe, expect, it } from 'vitest'

import { formatVersion } from './version'

describe('formatVersion', () => {
  it('leaves an already-v-prefixed version alone', () => {
    expect(formatVersion('v0.4.1')).toBe('v0.4.1')
  })

  it('prepends v to a bare version', () => {
    expect(formatVersion('0.4.1')).toBe('v0.4.1')
  })

  it('never produces a double v regardless of input', () => {
    expect(formatVersion('v0.4.1')).not.toBe('vv0.4.1')
    expect(formatVersion('V0.4.1')).toBe('v0.4.1')
  })

  it('trims surrounding whitespace before normalizing', () => {
    expect(formatVersion('  v0.4.1  ')).toBe('v0.4.1')
  })

  it('handles an empty string without throwing', () => {
    expect(formatVersion('')).toBe('v')
  })
})
