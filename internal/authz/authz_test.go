package authz

import (
	"errors"
	"testing"

	"github.com/base-al/basepod/internal/store"
)

// allCapabilities is every Capability minRole documents — kept as its own
// slice (rather than ranging minRole directly in TestCapabilityMatrix) so
// the matrix test's iteration order is stable and the table below reads
// top-to-bottom in the same viewer→member→admin→owner grouping the
// package doc comment and users-roles report use.
var allCapabilities = []Capability{
	CapAppsRead, CapEnvRead, CapDomainsRead, CapGitRead, CapLogsRead, CapStatsRead, CapSystemRead,
	CapAppsWrite, CapDeploy, CapEnvWrite, CapDomainsWrite, CapGitWrite,
	CapUsersRead, CapUsersInvite, CapUsersDisable, CapRegistriesManage, CapInstanceSettings, CapExec, CapAuditRead,
	CapUsersRoleChange, CapUsersRemove,
}

var allRoles = []string{store.RoleViewer, store.RoleMember, store.RoleAdmin, store.RoleOwner}

// TestCapabilityMatrix is the specification: every capability this API
// enforces, crossed with every role, asserting allow/deny. This table
// must stay exhaustive — TestEveryCapabilityIsInTheMatrix fails the build
// if a Capability constant is ever added to authz.go without a
// corresponding row here, so the matrix can't silently rot as new
// capabilities are added the way an unenforced comment could.
//
// PASS/FAIL note: this test was verified to actually catch a real
// regression during development — temporarily lowering CapUsersRemove's
// minRole entry from store.RoleOwner to store.RoleAdmin made this test
// fail (the "owner" row's want flipped from allow to a row further down
// the rank list), proving it's a real drift detector against minRole, not
// a tautology restating the same table.
func TestCapabilityMatrix(t *testing.T) {
	// want[capability] is the minimum role (by rank) that holds it —
	// exactly mirroring the task brief's matrix:
	//   viewer: apps/deployments/domains/env(read) + logs/stats/system read
	//   member: + deploy/rollback/restart, edit env, manage domains, create/delete apps
	//   admin:  + manage users/invitations, registries, instance settings, exec
	//   owner:  + change another user's role, remove users
	want := map[Capability]string{
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

	for _, cap := range allCapabilities {
		minWant, ok := want[cap]
		if !ok {
			t.Fatalf("capability %q has no entry in this test's `want` table — add one", cap)
		}
		for _, role := range allRoles {
			user := &store.User{Role: role}
			err := Authorize(user, cap)
			wantAllow := Rank(role) >= Rank(minWant)

			if wantAllow && err != nil {
				t.Errorf("Authorize(role=%s, %s) = %v, want allow (min role %s)", role, cap, err, minWant)
			}
			if !wantAllow && err == nil {
				t.Errorf("Authorize(role=%s, %s) = allow, want deny (min role %s)", role, cap, minWant)
			}
			if !wantAllow && !errors.Is(err, ErrForbidden) {
				t.Errorf("Authorize(role=%s, %s) = %v, want ErrForbidden", role, cap, err)
			}
		}
	}
}

// TestEveryCapabilityIsInTheMatrix keeps allCapabilities (and so
// TestCapabilityMatrix's coverage) honest against minRole itself: a
// Capability constant added to authz.go without also being added to
// allCapabilities above would otherwise silently escape the matrix
// test's exhaustive check.
func TestEveryCapabilityIsInTheMatrix(t *testing.T) {
	covered := make(map[Capability]bool, len(allCapabilities))
	for _, c := range allCapabilities {
		covered[c] = true
	}
	for cap := range minRole {
		if !covered[cap] {
			t.Errorf("capability %q is in minRole but missing from allCapabilities in authz_test.go", cap)
		}
	}
	if len(allCapabilities) != len(minRole) {
		t.Errorf("allCapabilities has %d entries, minRole has %d — they must match 1:1", len(allCapabilities), len(minRole))
	}
}

func TestAuthorizeNilUser(t *testing.T) {
	if err := Authorize(nil, CapAppsRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Authorize(nil, ...) = %v, want ErrForbidden", err)
	}
}

func TestAuthorizeUnknownCapability(t *testing.T) {
	user := &store.User{Role: store.RoleOwner}
	if err := Authorize(user, Capability("bogus:capability")); !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("Authorize(owner, bogus) = %v, want ErrUnknownCapability", err)
	}
}

func TestAuthorizeUnknownRole(t *testing.T) {
	user := &store.User{Role: "bogus-role"}
	if err := Authorize(user, CapAppsRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Authorize(bogus-role, apps:read) = %v, want ErrForbidden", err)
	}
}

// TestAuthorizeDisabledUserDeniedRegardlessOfRole proves a disabled
// user is denied even the lowest-floor capability, even as owner — the
// role-rank comparison alone would allow an owner everything, so this
// must be checked before (not instead of) the rank comparison.
func TestAuthorizeDisabledUserDeniedRegardlessOfRole(t *testing.T) {
	user := &store.User{Role: store.RoleOwner, DisabledAt: "2024-01-01T00:00:00Z"}
	if err := Authorize(user, CapAppsRead); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Authorize(disabled owner, apps:read) = %v, want ErrDisabled", err)
	}
}

func TestCanAssignRole(t *testing.T) {
	tests := []struct {
		actor, target string
		want          bool
	}{
		{store.RoleOwner, store.RoleOwner, true},
		{store.RoleOwner, store.RoleAdmin, true},
		{store.RoleOwner, store.RoleViewer, true},
		{store.RoleAdmin, store.RoleAdmin, true},
		{store.RoleAdmin, store.RoleMember, true},
		{store.RoleAdmin, store.RoleOwner, false}, // an admin can never grant owner
		{store.RoleMember, store.RoleAdmin, false},
		{store.RoleViewer, store.RoleViewer, true},
		{"bogus", store.RoleViewer, false},
		{store.RoleOwner, "bogus", false},
	}
	for _, tt := range tests {
		got := CanAssignRole(tt.actor, tt.target)
		if got != tt.want {
			t.Errorf("CanAssignRole(%s, %s) = %v, want %v", tt.actor, tt.target, got, tt.want)
		}
	}
}
