import { describe, expect, it } from 'vitest'

import { slugify } from './slugify'

// Mirrors internal/api/apps.go's slugify() exactly: lowercase, spaces ->
// hyphens, nothing else stripped. These tests pin down that exact
// (deliberately narrow) behavior, extracted verbatim from NewApp.vue.
describe('slugify', () => {
  it('lowercases the input', () => {
    expect(slugify('MyApp')).toBe('myapp')
  })

  it('replaces spaces with hyphens', () => {
    expect(slugify('My Blog')).toBe('my-blog')
  })

  it('replaces every space, not just the first', () => {
    expect(slugify('a b c')).toBe('a-b-c')
  })

  it('collapses nothing — consecutive spaces become consecutive hyphens', () => {
    expect(slugify('a  b')).toBe('a--b')
  })

  it('does not strip punctuation other than spaces', () => {
    expect(slugify('My_App!')).toBe('my_app!')
  })

  it('leaves an already-slug-shaped string unchanged', () => {
    expect(slugify('my-app-2')).toBe('my-app-2')
  })

  it('returns an empty string for empty input', () => {
    expect(slugify('')).toBe('')
  })
})
