package api

import (
	"log"
	"net/http"
	"strconv"

	"github.com/base-al/basepod/internal/store"
)

// defaultAuditLimit and maxAuditLimit bound GET /audit's page size —
// mirroring GetGitDeliveries' own limit handling (internal/api/git.go):
// a caller-supplied ?limit= is honored up to a cap, and an unset or
// non-positive one falls back to the default, rather than an unbounded
// query being able to pull the entire table in one response.
const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

// audit records one append-only audit_log row for a mutating operation,
// attributing it to the caller authenticated on r (via userFromContext —
// every call site is behind requireAuth, directly or through
// requireCapability). Never call this with a secret value in detail (env
// var values, passwords, tokens) — see store.AuditEntry's doc comment;
// every call site in this codebase logs only keys/counts/identifiers, by
// convention, since there is no enforcement here that could distinguish a
// secret string from any other.
//
// Failures are logged, not surfaced to the caller: the operation the
// audit entry describes has already succeeded (this is always called
// after the mutation it records, mirroring handleDeleteApp's
// log-not-relay treatment of its own best-effort cleanup steps) — a
// caller retrying a failed request because its audit trail write had a
// transient DB error would be actively wrong, since retrying would
// perform the mutation itself a second time.
func (a *api) audit(r *http.Request, action, target, detail string) {
	user := userFromContext(r.Context())
	var actorID int64
	var actorEmail string
	if user != nil {
		actorID = user.ID
		actorEmail = user.Email
	}
	a.auditAs(actorID, actorEmail, action, target, detail)
}

// auditAs is audit's lower-level twin for the two call sites with no
// request-context user to read: handleLogin (the actor's session doesn't
// exist yet at the point the login itself needs to be recorded) and
// handleAcceptInvite (unauthenticated by design — the newly created
// user, not a caller, is the natural actor for its own
// "invite accepted" entry). Every other mutating handler in this
// package should use audit, not this, so the actor is always read from
// the same place (the authenticated request context) rather than each
// handler threading it through by hand.
func (a *api) auditAs(actorID int64, actorEmail, action, target, detail string) {
	if err := a.st.InsertAuditLog(actorID, actorEmail, action, target, detail); err != nil {
		logAuditError(action, target, err)
	}
}

// logAuditError is a package var (not a bare log.Printf call) so
// audit_test.go can substitute a spy and assert audit's failure path is
// actually exercised, the same pattern verifyDummyLogin uses in auth.go.
var logAuditError = func(action, target string, err error) {
	log.Printf("api: audit log: action=%s target=%s: %v", action, target, err)
}

type auditEntryResponse struct {
	ID         int64  `json:"id"`
	ActorEmail string `json:"actor_email"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}

func toAuditEntryResponse(e store.AuditEntry) auditEntryResponse {
	return auditEntryResponse{
		ID:         e.ID,
		ActorEmail: e.ActorEmail,
		Action:     e.Action,
		Target:     e.Target,
		Detail:     e.Detail,
		CreatedAt:  e.CreatedAt,
	}
}

// handleListAudit returns the most recent audit log entries, newest
// first, capped at ?limit= (default defaultAuditLimit, hard-capped at
// maxAuditLimit). Requires authz.CapAuditRead (admin floor).
func (a *api) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit := defaultAuditLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "validation", "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}

	entries, err := a.st.ListAuditLog(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list audit log")
		return
	}
	out := make([]auditEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, toAuditEntryResponse(e))
	}
	writeJSON(w, http.StatusOK, out)
}
