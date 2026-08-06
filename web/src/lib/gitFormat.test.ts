import { describe, expect, it } from 'vitest'

import { formatGitRef, shortSha } from './gitFormat'

describe('shortSha', () => {
  it('returns "" for an empty sha', () => {
    expect(shortSha('')).toBe('')
  })

  it('shortens a full sha to 7 characters', () => {
    expect(shortSha('a1b2c3d4e5f60718293a4b5c6d7e8f9012345678')).toBe('a1b2c3d')
  })

  it('leaves a sha already 7 characters or shorter unchanged', () => {
    expect(shortSha('a1b2c3d')).toBe('a1b2c3d')
    expect(shortSha('a1b')).toBe('a1b')
  })
})

describe('formatGitRef', () => {
  it('returns "" for an empty ref', () => {
    expect(formatGitRef('')).toBe('')
  })

  it('strips refs/heads/ down to the branch name', () => {
    expect(formatGitRef('refs/heads/main')).toBe('main')
    expect(formatGitRef('refs/heads/feature/x')).toBe('feature/x')
  })

  it('labels a tag ref distinctly', () => {
    expect(formatGitRef('refs/tags/v1.2.3')).toBe('tag: v1.2.3')
  })

  it('passes through an unrecognized ref shape unchanged', () => {
    expect(formatGitRef('refs/pull/12/merge')).toBe('refs/pull/12/merge')
  })
})
