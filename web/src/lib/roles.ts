// Client-side mirror of internal/authz's role-rank rules (viewer < member
// < admin < owner — see internal/authz/authz.go's capability grants and
// authz_test.go's own doc comment for the grouping this mirrors). The
// server is the actual enforcement point for every one of these (a 403
// `forbidden` is always possible regardless of what this file says) —
// these predicates exist so Users.vue can render the right controls up
// front instead of showing something that's only going to fail, and so
// the "you don't have access" state doesn't require a doomed network
// round trip to discover.

export type Role = 'viewer' | 'member' | 'admin' | 'owner'

/** Every role, lowest to highest rank — the exact order
 * api/openapi.yaml's `role` enum lists them in, and the order role
 * selects (invite, change-role) present options in. */
export const ROLES: Role[] = ['viewer', 'member', 'admin', 'owner']

export const ROLE_LABELS: Record<Role, string> = {
  viewer: 'Viewer',
  member: 'Member',
  admin: 'Admin',
  owner: 'Owner',
}

const RANK: Record<Role, number> = { viewer: 0, member: 1, admin: 2, owner: 3 }

export function roleRank(role: Role): number {
  return RANK[role]
}

/** True when `role` is at or above `floor`'s rank — the general form
 * behind every one of this file's admin+/owner-only predicates below. */
export function roleAtLeast(role: Role, floor: Role): boolean {
  return roleRank(role) >= roleRank(floor)
}

/** Gates GET /users, POST /users/invite, disable, and enable — every
 * user-management capability except role change and remove (see
 * isOwner below). Also the floor for viewing the audit log
 * (`audit:read` — same admin floor per api/openapi.yaml). */
export function isAdminOrAbove(role: Role): boolean {
  return roleAtLeast(role, 'admin')
}

/** Gates PATCH .../role and DELETE .../{email} — the one pair of
 * operations admin does NOT get (api/openapi.yaml's `users:role_change`
 * and `users:remove` capabilities are both owner-floor). */
export function isOwner(role: Role): boolean {
  return role === 'owner'
}

/** Roles `currentRole` may assign when inviting or changing another
 * user's role, per api/openapi.yaml's inviteUser description: "the
 * caller may only invite a role at or below their own rank — an admin
 * can invite a viewer/member/admin but never an owner." Filtering by
 * rank alone is sufficient to encode that exception: owner is the only
 * role ranked above admin, so it's naturally excluded for every
 * non-owner caller without a separate special case. */
export function assignableRoles(currentRole: Role): Role[] {
  return ROLES.filter((role) => roleRank(role) <= roleRank(currentRole))
}

/** A minimal shape covering just what isLastActiveOwner needs from a
 * UserSummary (see lib/api.ts) — kept structural rather than importing
 * the full type so this module (and its tests) stay free of any API
 * dependency. */
export interface RoleAndDisabled {
  role: Role
  disabled: boolean
}

/** True when `target` is the instance's last remaining active (non-
 * disabled) owner — the exact condition the server 409s `last_owner`
 * for on role-change, disable, and delete (api/openapi.yaml's three
 * `last_owner` responses; internal/store's SetUserRole doc comment).
 * Used to pre-emptively disable those three controls in the UI rather
 * than let the operator discover the rule by failing — but the server
 * call is still what actually enforces it; this is a UX nicety, not the
 * source of truth. A disabled owner doesn't count: they can't log in
 * either way, so they're not "the one owner keeping the instance
 * administrable" in the sense this rule protects. */
export function isLastActiveOwner(target: RoleAndDisabled, allUsers: RoleAndDisabled[]): boolean {
  if (target.role !== 'owner' || target.disabled) return false
  const activeOwners = allUsers.filter((u) => u.role === 'owner' && !u.disabled)
  return activeOwners.length <= 1
}
