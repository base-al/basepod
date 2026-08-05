import { describe, expect, it } from 'vitest'

import { parseDotEnv } from './envparse'

describe('parseDotEnv', () => {
  it('parses simple KEY=VALUE lines', () => {
    expect(parseDotEnv('FOO=bar\nBAZ=qux')).toEqual([
      { key: 'FOO', value: 'bar' },
      { key: 'BAZ', value: 'qux' },
    ])
  })

  it('ignores blank lines and # comment lines', () => {
    expect(parseDotEnv('\n# a comment\nFOO=bar\n\n  # indented comment\nBAZ=qux\n')).toEqual([
      { key: 'FOO', value: 'bar' },
      { key: 'BAZ', value: 'qux' },
    ])
  })

  it('splits on the first "=" only, so values may contain "="', () => {
    expect(parseDotEnv('DATABASE_URL=postgres://user:pass@host/db?sslmode=require')).toEqual([
      { key: 'DATABASE_URL', value: 'postgres://user:pass@host/db?sslmode=require' },
    ])
  })

  it('handles CRLF line endings', () => {
    expect(parseDotEnv('FOO=bar\r\nBAZ=qux\r\n# comment\r\n')).toEqual([
      { key: 'FOO', value: 'bar' },
      { key: 'BAZ', value: 'qux' },
    ])
  })

  it('trims whitespace around both key and value', () => {
    expect(parseDotEnv('  FOO  =  bar  ')).toEqual([{ key: 'FOO', value: 'bar' }])
  })

  it('skips lines with no "=" at all', () => {
    expect(parseDotEnv('not-a-var\nFOO=bar')).toEqual([{ key: 'FOO', value: 'bar' }])
  })

  it('skips a line whose key (before "=") is blank', () => {
    expect(parseDotEnv('=novalue\n   =also-blank\nFOO=bar')).toEqual([{ key: 'FOO', value: 'bar' }])
  })

  it('allows an empty value', () => {
    expect(parseDotEnv('FOO=')).toEqual([{ key: 'FOO', value: '' }])
  })

  it('keeps only the last value for a duplicate key, at its first-seen position', () => {
    expect(parseDotEnv('FOO=1\nBAR=2\nFOO=3')).toEqual([
      { key: 'FOO', value: '3' },
      { key: 'BAR', value: '2' },
    ])
  })

  it('returns an empty array for blank input', () => {
    expect(parseDotEnv('')).toEqual([])
    expect(parseDotEnv('\n\n  \n')).toEqual([])
  })
})
