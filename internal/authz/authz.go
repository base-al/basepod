// Package authz is BasePod's single point of role-based authorization
// decision-making. Every route in internal/api that isn't public or
// purely self-scoped (a caller managing their own session/password) is
// gated by exactly one Capability, checked here — never by a handler
// hand-rolling its own role comparison. The capability→minimum-role table
// (minRole, below) is the specification: authz_test.go's matrix test
// exhaustively checks every Capability against every store.Role and is
// the deliverable this package exists to make possible.
//
// Scope note: this milestone is users + roles, deliberately NOT teams —
// every app stays shared across the whole BasePod instance; a role
// governs *what a user may do*, not *which apps they can see*. See
// docs/plan/08.teams-and-rbac.md for the original team-scoped design this
// intentionally diverges from, and the users-roles report for why.
package authz

import (
	"errors"

	"github.com/base-al/basepod/internal/store"
)

// Capability identifies one authorizable operation. Every route this API
// registers (other than POST /auth/login, GET /openapi.yaml, the git
// webhook receiver, and POST /invitations/accept — none of which have an
// authenticated caller to authorize — and the small set of purely
// self-scoped auth routes: GET /auth/me, POST /auth/logout, GET/DELETE
// /auth/sessions[/{id}], POST /auth/password, POST /stream-token, which
// operate on the caller's own account/access with no role distinction
// between an instance's viewer and its owner) maps to exactly one of
// these via internal/api/router.go's route table.
type Capability string

const (
	// --- viewer floor: read-only visibility into apps and the instance ---

	CapAppsRead    Capability = "apps:read" // list/get apps, deployments, volumes
	CapEnvRead     Capability = "env:read"  // view an app's env (secrets stay masked for everyone)
	CapDomainsRead Capability = "domains:read"
	CapGitRead     Capability = "git:read" // an app's connected git source config + delivery history
	CapLogsRead    Capability = "logs:read"
	CapStatsRead   Capability = "stats:read"
	CapSystemRead  Capability = "system:read"

	// --- member: the above, plus mutating an app's own configuration and deploying it ---

	CapAppsWrite    Capability = "apps:write"  // create/delete apps, patch resource limits/deploy strategy
	CapDeploy       Capability = "apps:deploy" // deploy (image/tarball/git), rollback, restart (redeploy), compose apply
	CapEnvWrite     Capability = "env:write"
	CapDomainsWrite Capability = "domains:write"
	CapGitWrite     Capability = "git:write" // connect/disconnect git source, rotate its webhook secret

	// --- admin: the above, plus instance administration ---

	CapUsersRead        Capability = "users:read"        // list users
	CapUsersInvite      Capability = "users:invite"      // create an invitation
	CapUsersDisable     Capability = "users:disable"     // disable/enable a user (not role change or removal — see owner-only below)
	CapRegistriesManage Capability = "registries:manage" // no route yet — reserved for when a registries feature exists
	CapInstanceSettings Capability = "instance:settings" // no route yet — reserved for when instance-settings routes exist
	CapExec             Capability = "exec"              // no route yet — reserved for container exec/terminal
	CapAuditRead        Capability = "audit:read"

	// --- owner: the above, plus the operations that can make an instance unadministrable if misused ---

	CapUsersRoleChange Capability = "users:role_change" // change another user's role
	CapUsersRemove     Capability = "users:remove"      // delete a user
)

// minRole is the capability→minimum-role table — the single source of
// truth Authorize checks against. A capability with no entry here is
// unknown and always denied (fail closed) rather than silently allowed;
// see Authorize.
var minRole = map[Capability]string{
	CapAppsRead:    store.RoleViewer,
	CapEnvRead:     store.RoleViewer,
	CapDomainsRead: store.RoleViewer,
	CapGitRead:     store.RoleViewer,
	CapLogsRead:    store.RoleViewer,
	CapStatsRead:   store.RoleViewer,
	CapSystemRead:  store.RoleViewer,

	CapAppsWrite:    store.RoleMember,
	CapDeploy:       store.RoleMember,
	CapEnvWrite:     store.RoleMember,
	CapDomainsWrite: store.RoleMember,
	CapGitWrite:     store.RoleMember,

	CapUsersRead:        store.RoleAdmin,
	CapUsersInvite:      store.RoleAdmin,
	CapUsersDisable:     store.RoleAdmin,
	CapRegistriesManage: store.RoleAdmin,
	CapInstanceSettings: store.RoleAdmin,
	CapExec:             store.RoleAdmin,
	CapAuditRead:        store.RoleAdmin,

	CapUsersRoleChange: store.RoleOwner,
	CapUsersRemove:     store.RoleOwner,
}

// roleRank orders roles from least to most privileged, so Authorize can
// implement "at least this role" as a plain integer comparison rather
// than an enumerated allow-list per role.
var roleRank = map[string]int{
	store.RoleViewer: 0,
	store.RoleMember: 1,
	store.RoleAdmin:  2,
	store.RoleOwner:  3,
}

// Rank returns role's privilege rank (higher is more privileged), or -1
// for an unrecognized role. Exposed for CanAssignRole and for callers
// (e.g. the invite/role-change handlers) that need to compare two roles
// directly rather than go through Authorize's fixed capability table.
func Rank(role string) int {
	r, ok := roleRank[role]
	if !ok {
		return -1
	}
	return r
}

var (
	// ErrUnknownCapability is returned by Authorize for a Capability with
	// no entry in minRole — a bug (a route wired to a capability that was
	// never added to the table), not a legitimate denial, but still fails
	// closed rather than panicking or allowing.
	ErrUnknownCapability = errors.New("authz: unknown capability")
	// ErrForbidden is returned by Authorize when user's role is beneath
	// capability's minimum, or user is nil.
	ErrForbidden = errors.New("authz: role does not have this capability")
	// ErrDisabled is returned by Authorize for a disabled user, regardless
	// of role or capability. In practice this is defense in depth, not
	// the primary enforcement: a disabled user's sessions are revoked
	// immediately (store.DisableUser) and store.UserBySessionTokenHash
	// additionally excludes disabled users, so requireAuth already
	// rejects them with 401 before a request ever reaches a capability
	// check — this branch exists in case a future caller resolves a
	// *store.User some other way.
	ErrDisabled = errors.New("authz: user is disabled")
)

// Authorize reports whether user may exercise capability: nil (allowed)
// or one of ErrDisabled / ErrForbidden / ErrUnknownCapability. This is
// the ONE function every authorization decision in this codebase must
// route through — see the package doc comment.
func Authorize(user *store.User, capability Capability) error {
	if user == nil {
		return ErrForbidden
	}
	if user.DisabledAt != "" {
		return ErrDisabled
	}
	min, ok := minRole[capability]
	if !ok {
		return ErrUnknownCapability
	}
	userRank := Rank(user.Role)
	if userRank < 0 || userRank < Rank(min) {
		return ErrForbidden
	}
	return nil
}

// CanAssignRole reports whether an actor holding actorRole may assign
// targetRole to another user (via invite or a role change): actorRole
// must be at least as privileged as targetRole. This is what keeps
// granting the owner role itself an owner-only act in practice — an
// admin (CapUsersInvite's floor) can invite a viewer/member/admin but
// never an owner, since admin's rank is below owner's — without a
// separate hand-rolled comparison at each of the two call sites (invite,
// role-change) that need it.
func CanAssignRole(actorRole, targetRole string) bool {
	ar, tr := Rank(actorRole), Rank(targetRole)
	return ar >= 0 && tr >= 0 && ar >= tr
}
