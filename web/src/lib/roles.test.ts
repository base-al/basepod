import { describe, expect, it } from 'vitest'

import { assignableRoles, isAdminOrAbove, isLastActiveOwner, isOwner, roleAtLeast, roleRank } from './roles'

describe('roleRank', () => {
  it('ranks viewer < member < admin < owner', () => {
    expect(roleRank('viewer')).toBeLessThan(roleRank('member'))
    expect(roleRank('member')).toBeLessThan(roleRank('admin'))
    expect(roleRank('admin')).toBeLessThan(roleRank('owner'))
  })
})

describe('roleAtLeast', () => {
  it('is true when role outranks or matches floor', () => {
    expect(roleAtLeast('owner', 'admin')).toBe(true)
    expect(roleAtLeast('admin', 'admin')).toBe(true)
  })

  it('is false when role is below floor', () => {
    expect(roleAtLeast('member', 'admin')).toBe(false)
    expect(roleAtLeast('viewer', 'owner')).toBe(false)
  })
})

describe('isAdminOrAbove', () => {
  it('allows admin and owner', () => {
    expect(isAdminOrAbove('admin')).toBe(true)
    expect(isAdminOrAbove('owner')).toBe(true)
  })

  it('denies member and viewer', () => {
    expect(isAdminOrAbove('member')).toBe(false)
    expect(isAdminOrAbove('viewer')).toBe(false)
  })
})

describe('isOwner', () => {
  it('is true for owner only', () => {
    expect(isOwner('owner')).toBe(true)
    expect(isOwner('admin')).toBe(false)
    expect(isOwner('member')).toBe(false)
    expect(isOwner('viewer')).toBe(false)
  })
})

describe('assignableRoles', () => {
  it('an owner may assign any role', () => {
    expect(assignableRoles('owner')).toEqual(['viewer', 'member', 'admin', 'owner'])
  })

  it('an admin may assign up to admin, never owner', () => {
    expect(assignableRoles('admin')).toEqual(['viewer', 'member', 'admin'])
  })

  it('a member may only assign viewer/member', () => {
    expect(assignableRoles('member')).toEqual(['viewer', 'member'])
  })

  it('a viewer may only assign viewer', () => {
    expect(assignableRoles('viewer')).toEqual(['viewer'])
  })
})

describe('isLastActiveOwner', () => {
  it('is true for the sole active owner', () => {
    const target = { role: 'owner' as const, disabled: false }
    expect(isLastActiveOwner(target, [target])).toBe(true)
  })

  it('is false when another active owner exists', () => {
    const target = { role: 'owner' as const, disabled: false }
    const other = { role: 'owner' as const, disabled: false }
    expect(isLastActiveOwner(target, [target, other])).toBe(false)
  })

  it('does not count a disabled owner toward the "another owner exists" check', () => {
    const target = { role: 'owner' as const, disabled: false }
    const disabledOwner = { role: 'owner' as const, disabled: true }
    expect(isLastActiveOwner(target, [target, disabledOwner])).toBe(true)
  })

  it('is false for a non-owner', () => {
    const target = { role: 'admin' as const, disabled: false }
    expect(isLastActiveOwner(target, [target])).toBe(false)
  })

  it('is false for an already-disabled owner (they cannot log in either way)', () => {
    const target = { role: 'owner' as const, disabled: true }
    expect(isLastActiveOwner(target, [target])).toBe(false)
  })
})
