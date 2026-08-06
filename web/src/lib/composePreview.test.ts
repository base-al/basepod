import { describe, expect, it } from 'vitest'

import type { ComposePlan, ComposeService } from './api'
import { hasOrphans, planWarningCount, serviceActionLabel, servicePortLabel } from './composePreview'

function makeService(overrides: Partial<ComposeService> = {}): ComposeService {
  return {
    name: 'web',
    slug: 'myproj-web',
    action: 'create',
    internal: false,
    port: 8080,
    alias: 'web',
    ...overrides,
  }
}

describe('servicePortLabel', () => {
  it('labels an internal service', () => {
    expect(servicePortLabel(makeService({ internal: true, port: 0 }))).toBe('internal')
  })

  it('labels a routed service with its port', () => {
    expect(servicePortLabel(makeService({ internal: false, port: 5432 }))).toBe('port 5432')
  })
})

describe('serviceActionLabel', () => {
  it('labels create', () => {
    expect(serviceActionLabel('create')).toBe('Create')
  })

  it('labels update', () => {
    expect(serviceActionLabel('update')).toBe('Update')
  })

  it('falls back to the raw string for an unknown action', () => {
    expect(serviceActionLabel('mystery')).toBe('mystery')
  })
})

describe('planWarningCount', () => {
  it('is 0 for a clean plan', () => {
    const plan: ComposePlan = { project: 'p', dry_run: true, services: [makeService()] }
    expect(planWarningCount(plan)).toBe(0)
  })

  it('sums top-level and per-service warnings', () => {
    const plan: ComposePlan = {
      project: 'p',
      dry_run: true,
      warnings: ['top-level warning'],
      services: [
        makeService({ warnings: ['a', 'b'] }),
        makeService({ name: 'db', warnings: ['c'] }),
      ],
    }
    expect(planWarningCount(plan)).toBe(4)
  })
})

describe('hasOrphans', () => {
  it('is false with no orphans field', () => {
    const plan: ComposePlan = { project: 'p', dry_run: true, services: [] }
    expect(hasOrphans(plan)).toBe(false)
  })

  it('is false with an empty orphans array', () => {
    const plan: ComposePlan = { project: 'p', dry_run: true, services: [], orphans: [] }
    expect(hasOrphans(plan)).toBe(false)
  })

  it('is true when orphans are present', () => {
    const plan: ComposePlan = { project: 'p', dry_run: true, services: [], orphans: ['p-old'] }
    expect(hasOrphans(plan)).toBe(true)
  })
})
