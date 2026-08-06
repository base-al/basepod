import { describe, expect, it } from 'vitest'

import { isPendingPlaceholder } from './pendingImage'

describe('isPendingPlaceholder', () => {
  it('matches the exact placeholder shape NewApp.vue creates', () => {
    expect(isPendingPlaceholder('localhost/basepod/my-app:pending')).toBe(true)
  })

  it('matches regardless of slug content, as long as one is present', () => {
    expect(isPendingPlaceholder('localhost/basepod/a:pending')).toBe(true)
    expect(isPendingPlaceholder('localhost/basepod/my-really-long-slug-name:pending')).toBe(true)
  })

  it('does not match a real numbered build tag under the same prefix', () => {
    // internal/build/build.go tags a completed build
    // "localhost/basepod/<slug>:<deploymentNumber>" — must not be
    // mistaken for the never-built placeholder.
    expect(isPendingPlaceholder('localhost/basepod/my-app:1')).toBe(false)
    expect(isPendingPlaceholder('localhost/basepod/my-app:42')).toBe(false)
  })

  it('does not match a user image merely tagged ":pending" on another registry', () => {
    expect(isPendingPlaceholder('docker.io/library/my-app:pending')).toBe(false)
    expect(isPendingPlaceholder('my-app:pending')).toBe(false)
    expect(isPendingPlaceholder('basepod/my-app:pending')).toBe(false)
  })

  it('does not match the prefix alone, without the ":pending" tag', () => {
    expect(isPendingPlaceholder('localhost/basepod/my-app')).toBe(false)
    expect(isPendingPlaceholder('localhost/basepod/my-app:latest')).toBe(false)
  })

  it('does not match a degenerate placeholder with no slug', () => {
    expect(isPendingPlaceholder('localhost/basepod/:pending')).toBe(false)
  })

  it('does not match a suffix that merely contains, but does not end with, ":pending"', () => {
    expect(isPendingPlaceholder('localhost/basepod/my-app:pending2')).toBe(false)
    expect(isPendingPlaceholder('localhost/basepod/my-app:pending-rc1')).toBe(false)
  })

  it('returns false for an empty string', () => {
    expect(isPendingPlaceholder('')).toBe(false)
  })
})
