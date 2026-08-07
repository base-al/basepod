package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsersListRendersTable(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-users")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/users" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSONResponse(w, http.StatusOK, []UserSummary{
			{ID: 1, Email: "admin@example.com", Name: "Admin", Role: "owner", Disabled: false, CreatedAt: "2024-01-01T00:00:00Z"},
			{ID: 2, Email: "viewer@example.com", Name: "Viewer", Role: "viewer", Disabled: true, CreatedAt: "2024-01-02T00:00:00Z"},
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-users")

	out, _, err := runCLI(t, "", "users", "list")
	if err != nil {
		t.Fatalf("users list: %v", err)
	}
	for _, want := range []string{"EMAIL", "admin@example.com", "owner", "viewer@example.com", "viewer", "true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestUsersInvitePrintsTokenOnce(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-invite")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users/invite" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSONResponse(w, http.StatusCreated, InviteResult{
			Email: "newbie@example.com", Role: "member", Token: "bp_invite_abc123", ExpiresAt: "2024-01-08T00:00:00Z",
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-invite")

	out, _, err := runCLI(t, "", "users", "invite", "newbie@example.com", "--role", "member")
	if err != nil {
		t.Fatalf("users invite: %v", err)
	}
	if !strings.Contains(out, "bp_invite_abc123") {
		t.Fatalf("output missing the invite token: %q", out)
	}
	if !strings.Contains(out, "newbie@example.com") {
		t.Fatalf("output missing the invited email: %q", out)
	}
}

func TestUsersInviteRequiresRoleFlag(t *testing.T) {
	setTestConfigPath(t)
	_, _, err := runCLI(t, "", "users", "invite", "newbie@example.com")
	if err == nil {
		t.Fatal("expected an error when --role is omitted")
	}
}

func TestUsersRolePatchesCorrectPath(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-role")
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/users/target@example.com/role" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSONResponse(w, http.StatusOK, UserSummary{Email: "target@example.com", Role: "admin"})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-role")

	out, _, err := runCLI(t, "", "users", "role", "target@example.com", "admin")
	if err != nil {
		t.Fatalf("users role: %v", err)
	}
	if !strings.Contains(out, "target@example.com") || !strings.Contains(out, "admin") {
		t.Fatalf("output missing expected fields: %q", out)
	}
}

func TestUsersDisableAndEnable(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-disable")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/users/target@example.com/disable":
			writeJSONResponse(w, http.StatusOK, UserSummary{Email: "target@example.com", Disabled: true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/users/target@example.com/enable":
			writeJSONResponse(w, http.StatusOK, UserSummary{Email: "target@example.com", Disabled: false})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-disable")

	out, _, err := runCLI(t, "", "users", "disable", "target@example.com")
	if err != nil {
		t.Fatalf("users disable: %v", err)
	}
	if !strings.Contains(out, "disabled target@example.com") {
		t.Fatalf("unexpected disable output: %q", out)
	}

	out, _, err = runCLI(t, "", "users", "enable", "target@example.com")
	if err != nil {
		t.Fatalf("users enable: %v", err)
	}
	if !strings.Contains(out, "enabled target@example.com") {
		t.Fatalf("unexpected enable output: %q", out)
	}
}

func TestUsersRemoveDeletesCorrectPath(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "tok-remove")
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/users/target@example.com" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-remove")

	out, _, err := runCLI(t, "", "users", "remove", "target@example.com")
	if err != nil {
		t.Fatalf("users remove: %v", err)
	}
	if !strings.Contains(out, "removed target@example.com") {
		t.Fatalf("unexpected remove output: %q", out)
	}
}

// TestUsersRoleSurfacesLastOwnerError proves the CLI relays the server's
// 409 last_owner error verbatim rather than swallowing or generic-izing
// it — an operator hitting this needs to know exactly why.
func TestUsersRoleSurfacesLastOwnerError(t *testing.T) {
	path := setTestConfigPath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusConflict, map[string]any{
			"error": map[string]string{"code": "last_owner", "message": "cannot change the role of the last remaining owner"},
		})
	}))
	defer srv.Close()
	saveTestContext(t, path, srv.URL, "tok-lastowner")

	_, _, err := runCLI(t, "", "users", "role", "admin@example.com", "viewer")
	if err == nil {
		t.Fatal("expected an error")
	}
	apiErr, ok := err.(*ApiError)
	if !ok {
		t.Fatalf("expected *ApiError, got %T: %v", err, err)
	}
	if apiErr.Code != "last_owner" {
		t.Fatalf("Code = %q, want %q", apiErr.Code, "last_owner")
	}
}
