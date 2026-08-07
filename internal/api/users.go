// User management + invitations (users + roles milestone). Every handler
// in this file that requires an authenticated caller runs behind
// requireCapability in router.go's route table — see internal/authz's
// Capability doc comment for exactly which role floor each requires.
// handleAcceptInvite is the one exception: it is deliberately
// unauthenticated (see its own doc comment and router.go's mounting of
// POST /invitations/accept outside the requireAuth group).
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/authz"
	"github.com/base-al/basepod/internal/store"
)

// inviteDuration is how long an invitation link stays valid before it
// must be re-issued — mirrors docs/plan/08.teams-and-rbac.md's "7-day
// expiry, single use" v1 design.
const inviteDuration = 7 * 24 * time.Hour

// userSummary is the wire shape of one user, used by GET /users and every
// user-management response that returns the affected user.
type userSummary struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
}

func toUserSummary(u *store.User) userSummary {
	return userSummary{
		ID:        u.ID,
		Email:     u.Email,
		Name:      u.Name,
		Role:      u.Role,
		Disabled:  u.DisabledAt != "",
		CreatedAt: u.CreatedAt,
	}
}

// handleListUsers returns every user on the instance. Requires
// authz.CapUsersRead (admin floor).
func (a *api) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.st.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list users")
		return
	}
	out := make([]userSummary, 0, len(users))
	for i := range users {
		out = append(out, toUserSummary(&users[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// userByEmailOrNotFound looks up the user named by the {email} URL param,
// writing a 404 "user_not_found" (or 500 on an unexpected store error)
// and returning ok=false if it can't be found — the user-management
// counterpart to apps.go's appBySlugOrNotFound.
func (a *api) userByEmailOrNotFound(w http.ResponseWriter, r *http.Request) (user *store.User, ok bool) {
	email := chi.URLParam(r, "email")
	user, err := a.st.UserByEmail(email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user_not_found", "user not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up user")
		}
		return nil, false
	}
	return user, true
}

type inviteUserRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// inviteUserResponse carries the raw invite token in plaintext exactly
// once — see store.Invitation's doc comment: it is never persisted or
// retrievable again after this response. The caller (basepod users
// invite, or the dashboard's future Users screen) is responsible for
// delivering it to the invitee out of band; this API has no notion of
// the dashboard's own public URL to build a clickable link from, so it
// hands back the bare token for the caller to embed into whatever
// accept-invite URL it constructs.
type inviteUserResponse struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// handleInviteUser creates a single-use, expiring invitation for email at
// role. Requires authz.CapUsersInvite (admin floor); additionally, the
// caller may only invite a role at or below their own rank
// (authz.CanAssignRole) — otherwise an admin could mint a brand-new owner
// account for themselves, which would make "role changes are owner-only"
// meaningless. Rejects (409 "user_exists") an email that's already a
// registered user.
func (a *api) handleInviteUser(w http.ResponseWriter, r *http.Request) {
	var req inviteUserRequest
	if !readJSON(w, r, &req) {
		return
	}

	if req.Email == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "email must not be empty")
		return
	}
	if !store.ValidRole(req.Role) {
		writeError(w, http.StatusUnprocessableEntity, "validation", `role must be one of "viewer", "member", "admin", "owner"`)
		return
	}

	actor := userFromContext(r.Context())
	if !authz.CanAssignRole(actor.Role, req.Role) {
		writeError(w, http.StatusForbidden, "forbidden", "cannot invite a user with a role higher than your own")
		return
	}

	if _, err := a.st.UserByEmail(req.Email); err == nil {
		writeError(w, http.StatusConflict, "user_exists", "a user with this email already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", "failed to check for existing user")
		return
	}

	token, tokenHash := auth.NewInviteToken()
	expiresAt := time.Now().Add(inviteDuration)
	if _, err := a.st.CreateInvitation(tokenHash, req.Email, req.Role, actor.ID, expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create invitation")
		return
	}

	a.audit(r, "user.invite", req.Email, "role="+req.Role)

	writeJSON(w, http.StatusCreated, inviteUserResponse{
		Email:     req.Email,
		Role:      req.Role,
		Token:     token,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

type acceptInviteRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// handleAcceptInvite is the unauthenticated counterpart to
// handleInviteUser: it consumes a single-use invitation token, creates
// the invited user at the role the inviting admin chose, and immediately
// logs them in (same response shape as POST /auth/login) so accepting an
// invite and landing in the dashboard is one step, not two.
//
// Errors: 404 "invite_not_found" for an unknown token (deliberately not
// distinguished from a garbled one — no oracle for whether a token was
// ever valid), 409 "invite_already_used" for a previously accepted one,
// 409 "invite_expired" for one past its expiry, 422 "validation" for a
// missing name or a password under minPasswordLength, 409 "user_exists"
// in the (rare, race-only) case the invited email was registered by some
// other path between the invite being created and being accepted.
func (a *api) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req acceptInviteRequest
	if !readJSON(w, r, &req) {
		return
	}

	if req.Token == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "token must not be empty")
		return
	}

	inv, err := a.st.InvitationByTokenHash(auth.HashToken(req.Token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invite_not_found", "invitation not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up invitation")
		}
		return
	}
	if inv.AcceptedAt != "" {
		writeError(w, http.StatusConflict, "invite_already_used", "this invitation has already been accepted")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil || time.Now().After(expiresAt) {
		writeError(w, http.StatusConflict, "invite_expired", "this invitation has expired")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "name must not be empty")
		return
	}
	if len(req.Password) < minPasswordLength {
		writeError(w, http.StatusUnprocessableEntity, "validation", "password must be at least 8 characters")
		return
	}

	if _, err := a.st.UserByEmail(inv.Email); err == nil {
		writeError(w, http.StatusConflict, "user_exists", "a user with this email already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", "failed to check for existing user")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to hash password")
		return
	}

	userID, err := a.st.CreateUserWithRole(inv.Email, req.Name, hash, inv.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create user")
		return
	}

	// Single-use: mark the invitation accepted in the same request that
	// consumed it, so a replay of this exact token is rejected by the
	// AcceptedAt check above from here on — see AcceptInvitation's doc
	// comment for the (much narrower) crash-recovery window this still
	// leaves.
	if err := a.st.AcceptInvitation(inv.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "user created, but failed to mark invitation accepted")
		return
	}

	a.auditAs(userID, inv.Email, "user.invite_accepted", inv.Email, "role="+inv.Role)

	token, tokenHash := auth.NewSessionToken()
	if err := a.st.CreateSession(userID, tokenHash, time.Now().Add(sessionDuration)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "user created, but failed to create session")
		return
	}

	writeJSON(w, http.StatusCreated, loginResponse{
		Token: token,
		User:  userResponse{ID: userID, Email: inv.Email, Name: req.Name, Role: inv.Role},
	})
}

type changeUserRoleRequest struct {
	Role string `json:"role"`
}

// handleChangeUserRole changes a user's role. Requires
// authz.CapUsersRoleChange (owner floor); authz.CanAssignRole additionally
// bounds the target role by the caller's own rank (see handleInviteUser's
// doc comment for the same rule) — in practice always satisfied here
// since owner already outranks every role, but kept for symmetry and
// defense in depth against the capability floor ever being loosened.
// Returns 409 "last_owner" if this would leave the instance with no
// active owner (store.ErrLastOwner — see store.SetUserRole).
func (a *api) handleChangeUserRole(w http.ResponseWriter, r *http.Request) {
	target, ok := a.userByEmailOrNotFound(w, r)
	if !ok {
		return
	}

	var req changeUserRoleRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !store.ValidRole(req.Role) {
		writeError(w, http.StatusUnprocessableEntity, "validation", `role must be one of "viewer", "member", "admin", "owner"`)
		return
	}

	actor := userFromContext(r.Context())
	if !authz.CanAssignRole(actor.Role, req.Role) {
		writeError(w, http.StatusForbidden, "forbidden", "cannot assign a role higher than your own")
		return
	}

	if err := a.st.SetUserRole(target.ID, req.Role); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(w, http.StatusConflict, "last_owner", "cannot change the role of the last remaining owner")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to change role")
		return
	}

	a.audit(r, "user.role_change", target.Email, "role="+target.Role+"->"+req.Role)

	target.Role = req.Role
	writeJSON(w, http.StatusOK, toUserSummary(target))
}

// handleDisableUser disables a user, immediately revoking every one of
// their live sessions (see store.DisableUser). Requires
// authz.CapUsersDisable (admin floor). Returns 409 "last_owner" if the
// target is the last remaining active owner (store.ErrLastOwner).
func (a *api) handleDisableUser(w http.ResponseWriter, r *http.Request) {
	target, ok := a.userByEmailOrNotFound(w, r)
	if !ok {
		return
	}

	if err := a.st.DisableUser(target.ID); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(w, http.StatusConflict, "last_owner", "cannot disable the last remaining owner")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to disable user")
		return
	}

	a.audit(r, "user.disable", target.Email, "")

	target.DisabledAt = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, toUserSummary(target))
}

// handleEnableUser re-enables a previously disabled user. It does NOT
// restore any session that was live before the disable — DisableUser
// already revoked those — so the user must log in again. Requires
// authz.CapUsersDisable (admin floor); not an error if the target was
// already enabled.
func (a *api) handleEnableUser(w http.ResponseWriter, r *http.Request) {
	target, ok := a.userByEmailOrNotFound(w, r)
	if !ok {
		return
	}

	if err := a.st.EnableUser(target.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to enable user")
		return
	}

	a.audit(r, "user.enable", target.Email, "")

	target.DisabledAt = ""
	writeJSON(w, http.StatusOK, toUserSummary(target))
}

// handleDeleteUser permanently removes a user; its sessions cascade-
// delete with it (see store.DeleteUser). Requires authz.CapUsersRemove
// (owner floor). Returns 409 "last_owner" if the target is the last
// remaining active owner (store.ErrLastOwner).
func (a *api) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	target, ok := a.userByEmailOrNotFound(w, r)
	if !ok {
		return
	}

	if err := a.st.DeleteUser(target.ID); err != nil {
		if errors.Is(err, store.ErrLastOwner) {
			writeError(w, http.StatusConflict, "last_owner", "cannot delete the last remaining owner")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete user")
		return
	}

	a.audit(r, "user.delete", target.Email, "")

	w.WriteHeader(http.StatusNoContent)
}
