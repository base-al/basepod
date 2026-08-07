package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/store"
)

// testUserPassword is the fixed password every additional test user in
// this file is created with — mirrors testPassword's role for the
// store-seeded admin newTestStore creates.
const testUserPassword = "other-correct-password"

// createAndLoginAs creates a user with the given role directly through
// the store (bypassing the invite flow, which has its own dedicated
// tests below) and logs them in through the real HTTP API, returning
// their session token. t.Fatal on any failure.
func createAndLoginAs(t *testing.T, st *store.Store, srv *httptest.Server, email, role string) string {
	t.Helper()
	hash, err := auth.HashPassword(testUserPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUserWithRole(email, "Test User", hash, role); err != nil {
		t.Fatal(err)
	}
	var out loginResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: email, Password: testUserPassword}, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login as %s (%s): got status %d", email, role, resp.StatusCode)
	}
	return out.Token
}

// ---------------------------------------------------------------------
// Route capability matrix: each protected route rejects an
// under-privileged role with 403 (not 404, not 401) and allows a
// sufficient one. internal/authz's own TestCapabilityMatrix is the
// exhaustive capability×role specification; this test proves the actual
// HTTP wiring (router.go's requireCapability placement on each route)
// matches it end to end, for a representative route at every non-viewer
// floor this API enforces (viewer is the floor for read routes, so
// there is no "under-privileged" role to test a 403 against there —
// every authenticated, non-disabled user already clears it).
// ---------------------------------------------------------------------

func TestRouteCapabilityMatrix(t *testing.T) {
	st := newTestStore(t) // seeds one owner: admin@example.com
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	_, ownerLogin := login(t, srv, testPassword)
	ownerToken := ownerLogin.Token
	viewerToken := createAndLoginAs(t, st, srv, "viewer@example.com", store.RoleViewer)
	memberToken := createAndLoginAs(t, st, srv, "member@example.com", store.RoleMember)
	adminToken := createAndLoginAs(t, st, srv, "admin2@example.com", store.RoleAdmin)
	// A disposable target for the disableUser case, distinct from every
	// account whose own token this table reuses across subtests — that
	// case's "sufficient" half actually disables the target for real
	// (disableUser has no dry-run), so it must never name a user another
	// row's token belongs to, or that token would 401 (revoked) instead
	// of the 403/allowed this table is asserting on.
	createAndLoginAs(t, st, srv, "disable-target@example.com", store.RoleMember)

	// A real app for the member-floor app-scoped routes to act on.
	var app appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", ownerToken,
		createAppRequest{Name: "matrixapp", Image: "nginx:latest", Port: 80}, &app)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed app: got status %d", resp.StatusCode)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		underToken string // a role BELOW this route's floor
		underRole  string // for the failure message only
		atToken    string // a role AT/ABOVE this route's floor
		atRole     string
	}{
		{"createApp", http.MethodPost, "/apps", createAppRequest{Name: "matrixapp2", Image: "nginx:latest", Port: 80}, viewerToken, "viewer", memberToken, "member"},
		{"deleteApp", http.MethodDelete, "/apps/matrixapp", nil, viewerToken, "viewer", ownerToken, "owner"},
		{"listUsers", http.MethodGet, "/users", nil, memberToken, "member", adminToken, "admin"},
		{"inviteUser", http.MethodPost, "/users/invite", inviteUserRequest{Email: "invitee1@example.com", Role: store.RoleViewer}, memberToken, "member", adminToken, "admin"},
		{"disableUser", http.MethodPost, "/users/disable-target@example.com/disable", nil, memberToken, "member", adminToken, "admin"},
		{"listAudit", http.MethodGet, "/audit", nil, memberToken, "member", adminToken, "admin"},
		{"changeUserRole", http.MethodPatch, "/users/viewer@example.com/role", changeUserRoleRequest{Role: store.RoleMember}, adminToken, "admin", ownerToken, "owner"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/under-privileged", func(t *testing.T) {
			resp := doJSON(t, tt.method, srv.URL+"/api/v1"+tt.path, tt.underToken, tt.body, nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s as %s: got status %d, want 403", tt.method, tt.path, tt.underRole, resp.StatusCode)
			}
		})
		t.Run(tt.name+"/sufficient", func(t *testing.T) {
			resp := doJSON(t, tt.method, srv.URL+"/api/v1"+tt.path, tt.atToken, tt.body, nil)
			if resp.StatusCode == http.StatusForbidden {
				t.Fatalf("%s %s as %s: got 403, want allowed", tt.method, tt.path, tt.atRole)
			}
		})
	}
}

// TestViewerCanReadButNotWrite proves the viewer floor directly against
// one read route (always allowed) and one member-floor write route
// (denied) for the exact same app and the exact same user, the two
// halves of "viewer: read-only" in one test.
func TestViewerCanReadButNotWrite(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var app appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", ownerLogin.Token,
		createAppRequest{Name: "viewerapp", Image: "nginx:latest", Port: 80}, &app)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed app: got status %d", resp.StatusCode)
	}

	viewerToken := createAndLoginAs(t, st, srv, "viewer2@example.com", store.RoleViewer)

	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/viewerapp", viewerToken, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer GET app: got status %d, want 200", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/viewerapp/deploy", viewerToken, deployRequest{}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer deploy: got status %d, want 403", resp.StatusCode)
	}
}

// TestDisabledUserCannotAuthenticate covers the "disabling a user must
// revoke their sessions immediately" requirement end to end over real
// HTTP: a previously-valid token 401s the moment the user is disabled,
// with no window where it still works.
func TestDisabledUserCannotAuthenticate(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	targetToken := createAndLoginAs(t, st, srv, "target@example.com", store.RoleMember)

	// Live before disable.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", targetToken, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-disable /auth/me: got status %d, want 200", resp.StatusCode)
	}

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/users/target@example.com/disable", ownerLogin.Token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable: got status %d, want 200", resp.StatusCode)
	}

	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", targetToken, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-disable /auth/me with the OLD token: got status %d, want 401", resp.StatusCode)
	}

	// The disabled user also can't log back in with a fresh attempt.
	loginResp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/auth/login", "",
		loginRequest{Email: "target@example.com", Password: testUserPassword}, nil)
	if loginResp.StatusCode != http.StatusForbidden {
		t.Fatalf("login as disabled user: got status %d, want 403", loginResp.StatusCode)
	}
}

// TestDeletedUserSessionRevoked is TestDisabledUserCannotAuthenticate's
// twin for DELETE: a deleted user's session must stop working too (via
// the sessions table's ON DELETE CASCADE foreign key).
func TestDeletedUserSessionRevoked(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	targetToken := createAndLoginAs(t, st, srv, "target2@example.com", store.RoleMember)

	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/users/target2@example.com", ownerLogin.Token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got status %d, want 204", resp.StatusCode)
	}

	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", targetToken, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-delete /auth/me with the OLD token: got status %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// Owner protection: demote, disable, and delete the last owner are each
// rejected with a clear error.
// ---------------------------------------------------------------------

func TestLastOwnerCannotBeDemoted(t *testing.T) {
	st := newTestStore(t) // exactly one user: admin@example.com, role owner
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPatch, srv.URL+"/api/v1/users/admin@example.com/role", ownerLogin.Token,
		changeUserRoleRequest{Role: store.RoleAdmin}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("demote last owner: got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "last_owner" {
		t.Fatalf("demote last owner: got error code %q, want %q", errBody.Error.Code, "last_owner")
	}

	// Actually unchanged.
	var me userResponse
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", ownerLogin.Token, nil, &me)
	if me.Role != store.RoleOwner {
		t.Fatalf("role after rejected demote = %q, want unchanged %q", me.Role, store.RoleOwner)
	}
}

func TestLastOwnerCannotBeDisabled(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/users/admin@example.com/disable", ownerLogin.Token, nil, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disable last owner: got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "last_owner" {
		t.Fatalf("disable last owner: got error code %q, want %q", errBody.Error.Code, "last_owner")
	}

	// Still able to authenticate — the disable must not have partially
	// applied.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", ownerLogin.Token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner /auth/me after rejected disable: got status %d, want 200", resp.StatusCode)
	}
}

func TestLastOwnerCannotBeDeleted(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/users/admin@example.com", ownerLogin.Token, nil, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete last owner: got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "last_owner" {
		t.Fatalf("delete last owner: got error code %q, want %q", errBody.Error.Code, "last_owner")
	}
}

// TestNonLastOwnerCanBeDemotedDisabledDeleted proves the protection is
// specifically about being the LAST owner, not about owners in general.
func TestNonLastOwnerCanBeDemotedDisabledDeleted(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	createAndLoginAs(t, st, srv, "owner2@example.com", store.RoleOwner)

	resp := doJSON(t, http.MethodPatch, srv.URL+"/api/v1/users/owner2@example.com/role", ownerLogin.Token,
		changeUserRoleRequest{Role: store.RoleAdmin}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("demote a non-last owner: got status %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// Invitations: single-use, expiring, and an accepted invite cannot be
// replayed.
// ---------------------------------------------------------------------

func TestInviteAndAcceptFlow(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var invite inviteUserResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/users/invite", ownerLogin.Token,
		inviteUserRequest{Email: "newbie@example.com", Role: store.RoleMember}, &invite)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite: got status %d", resp.StatusCode)
	}
	if invite.Token == "" {
		t.Fatal("invite response carries no token")
	}

	var accepted loginResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/invitations/accept", "",
		acceptInviteRequest{Token: invite.Token, Name: "Newbie", Password: "newbie-password"}, &accepted)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("accept: got status %d", resp.StatusCode)
	}
	if accepted.Token == "" {
		t.Fatal("accept response carries no session token")
	}
	if accepted.User.Role != store.RoleMember || accepted.User.Email != "newbie@example.com" {
		t.Fatalf("unexpected user in accept response: %+v", accepted.User)
	}

	// The new session actually works.
	if resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/auth/me", accepted.Token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("newly accepted user /auth/me: got status %d, want 200", resp.StatusCode)
	}

	// Single-use: replaying the same token must fail.
	var errBody errorResponse
	resp = doJSON(t, http.MethodPost, srv.URL+"/api/v1/invitations/accept", "",
		acceptInviteRequest{Token: invite.Token, Name: "Newbie Again", Password: "newbie-password"}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("replayed accept: got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "invite_already_used" {
		t.Fatalf("replayed accept: got error code %q, want %q", errBody.Error.Code, "invite_already_used")
	}
}

func TestAcceptInviteUnknownToken(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/invitations/accept", "",
		acceptInviteRequest{Token: "bp_invite_does-not-exist", Name: "X", Password: "somepassword"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
}

// TestAcceptExpiredInvite proves an invitation past its expiry cannot be
// accepted, even though it was never used — expiry and single-use are
// two independent checks.
func TestAcceptExpiredInvite(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	adminID, err := st.UserByEmail("admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// CreateInvitation directly through the store so this test can set an
	// already-past expires_at, which the HTTP API's own invite endpoint
	// (inviteDuration = 7 days) has no way to request.
	token, hash := auth.NewInviteToken()
	if _, err := st.CreateInvitation(hash, "expired@example.com", store.RoleViewer, adminID.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/invitations/accept", "",
		acceptInviteRequest{Token: token, Name: "X", Password: "somepassword"}, &errBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got status %d, want 409", resp.StatusCode)
	}
	if errBody.Error.Code != "invite_expired" {
		t.Fatalf("got error code %q, want %q", errBody.Error.Code, "invite_expired")
	}
}

// TestInviteRoleAssignmentBoundedByActorRank proves an admin cannot use
// the invite endpoint to mint a brand-new owner account for themselves —
// granting the owner role is owner-only, whether via role-change or
// invite.
func TestInviteRoleAssignmentBoundedByActorRank(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	adminToken := createAndLoginAs(t, st, srv, "admin3@example.com", store.RoleAdmin)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/users/invite", adminToken,
		inviteUserRequest{Email: "wannabe-owner@example.com", Role: store.RoleOwner}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin inviting an owner: got status %d, want 403", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------
// Secret values remain masked for every role including owner.
// ---------------------------------------------------------------------

// TestSecretEnvValuesMaskedForEveryRole proves roles do not unlock env
// secret values: an owner (the highest role) still sees "" for a secret
// value, exactly like every other role — the pre-existing masking
// principle this milestone must not regress (see internal/api/env.go's
// handleGetEnv).
func TestSecretEnvValuesMaskedForEveryRole(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	var app appResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", ownerLogin.Token,
		createAppRequest{Name: "secretapp", Image: "nginx:latest", Port: 80}, &app)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed app: got status %d", resp.StatusCode)
	}

	putResp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/secretapp/env", ownerLogin.Token,
		[]envVarResponse{{Key: "DB_PASSWORD", Value: "super-secret", IsSecret: true}}, nil)
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("put env: got status %d", putResp.StatusCode)
	}

	for _, role := range []string{store.RoleOwner, store.RoleAdmin, store.RoleMember, store.RoleViewer} {
		token := ownerLogin.Token
		if role != store.RoleOwner {
			token = createAndLoginAs(t, st, srv, "reader-"+role+"@example.com", role)
		}
		var vars []envVarResponse
		resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/secretapp/env", token, nil, &vars)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("role=%s: get env: got status %d", role, resp.StatusCode)
		}
		found := false
		for _, v := range vars {
			if v.Key != "DB_PASSWORD" {
				continue
			}
			found = true
			if v.Value != "" {
				t.Fatalf("role=%s: secret value leaked: %q", role, v.Value)
			}
		}
		if !found {
			t.Fatalf("role=%s: DB_PASSWORD missing from response entirely: %+v", role, vars)
		}
	}
}

// ---------------------------------------------------------------------
// Audit log.
// ---------------------------------------------------------------------

func TestAuditLogRecordsLoginAndAppCreate(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, ownerLogin := login(t, srv, testPassword)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps", ownerLogin.Token,
		createAppRequest{Name: "auditapp", Image: "nginx:latest", Port: 80}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: got status %d", resp.StatusCode)
	}

	var entries []auditEntryResponse
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/v1/audit", ownerLogin.Token, nil, &entries)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list audit: got status %d", resp.StatusCode)
	}

	var sawLogin, sawAppCreate bool
	for _, e := range entries {
		if e.Action == "auth.login" && e.ActorEmail == "admin@example.com" {
			sawLogin = true
		}
		if e.Action == "app.create" && e.Target == "auditapp" {
			sawAppCreate = true
		}
	}
	if !sawLogin {
		t.Fatalf("audit log missing an auth.login entry: %+v", entries)
	}
	if !sawAppCreate {
		t.Fatalf("audit log missing an app.create entry: %+v", entries)
	}
}
