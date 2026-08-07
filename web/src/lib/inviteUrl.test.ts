import { describe, expect, it } from 'vitest'

import { buildInviteAcceptUrl } from './inviteUrl'

describe('buildInviteAcceptUrl', () => {
  it('builds an /accept-invite URL carrying the token as a query param', () => {
    expect(buildInviteAcceptUrl('https://basepod.example.com', 'tok_abc123')).toBe(
      'https://basepod.example.com/accept-invite?token=tok_abc123',
    )
  })

  it('percent-encodes a token containing URL-unsafe characters', () => {
    expect(buildInviteAcceptUrl('https://basepod.example.com', 'a b+c/d')).toBe(
      'https://basepod.example.com/accept-invite?token=a+b%2Bc%2Fd',
    )
  })

  it('respects a non-default port on the origin', () => {
    expect(buildInviteAcceptUrl('http://localhost:5173', 'tok_xyz')).toBe(
      'http://localhost:5173/accept-invite?token=tok_xyz',
    )
  })
})
