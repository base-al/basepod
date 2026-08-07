package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/authz"
	"github.com/base-al/basepod/internal/store"
)

// Stream-token scopes: the shapes an SSE route in this API authenticates.
// See store.StreamToken's doc comment for what each one carries, and
// requireAuthAppLogs/requireAuthBuildLog/requireAuthStats below for where
// they're checked against an incoming request.
const (
	streamScopeAppLogs  = "app_logs"
	streamScopeBuildLog = "build_log"
	// streamScopeStats is the stats SSE route's scope (v0.5 plan Task 11):
	// like streamScopeAppLogs, it names a slug and no deployment number.
	// The stream_tokens.scope column is free TEXT (see store.StreamToken),
	// so adding this scope needs no migration.
	streamScopeStats = "stats"
	// streamScopeAllStats is the batch-stats SSE route's scope
	// (GET /api/v1/stats — the sparklines-on-the-apps-list milestone): a
	// deliberately DIFFERENT scope from streamScopeStats even though both
	// ultimately stream podman stats, because they authenticate different
	// routes with different blast radii — a token minted for one app's
	// stats must never double as a token that can read every app's, and a
	// batch-stats token must never be mistaken for a single-app one. This
	// scope names neither a slug nor a deployment number (see
	// handleCreateStreamToken's early-return branch for it, and
	// requireAuthAllStats below); the stream_tokens row it mints still
	// stores slug "" (the column is NOT NULL, and "" satisfies that with
	// no schema change) purely so CreateStreamToken's signature doesn't
	// need a nilable slug just for this one scope.
	streamScopeAllStats = "all_stats"
)

// streamTokenDuration is how long a minted stream token remains valid.
// Short enough that a leaked/logged URL — the exact scenario this token
// type exists to defang (audit finding M4: the old ?access_token= carried
// the full 30-day session token) — is only a live credential for a few
// minutes; long enough to comfortably cover one SSE connection attempt.
// web/src/lib/sse.ts does not try to make a single token outlive a
// long-lived log view — it mints a fresh one before every connection
// attempt, including reconnects, rather than racing this expiry.
const streamTokenDuration = 5 * time.Minute

// streamTokenRequest is the body of POST /api/v1/stream-token.
// DeploymentNumber is required for scope "build_log" and must be absent
// for "app_logs" — see handleCreateStreamToken's coherence check.
type streamTokenRequest struct {
	Scope            string `json:"scope"`
	Slug             string `json:"slug"`
	DeploymentNumber *int   `json:"deployment_number,omitempty"`
}

// streamTokenResponse is the wire shape of a successful
// POST /api/v1/stream-token response. ExpiresAt is RFC3339 UTC.
type streamTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// handleCreateStreamToken mints a short-lived (streamTokenDuration),
// single-purpose stream token for the caller's own SSE use: scoped to
// exactly one (scope, slug, deployment) triple, so it can safely travel
// in a URL query string (see requireAuthAppLogs/requireAuthBuildLog) the
// way the caller's own session token never should.
//
// Validates that the named app exists (404 "app_not_found" otherwise)
// and that scope/deployment_number form a coherent request: "app_logs"
// must not carry a deployment_number, "build_log" must carry one naming
// an actual deployment of that app — either failure is 422 "validation"
// (an unrecognized deployment number under "build_log" is 404
// "deployment_not_found" instead, matching handleDeploymentLog's own
// mapping for the same lookup).
func (a *api) handleCreateStreamToken(w http.ResponseWriter, r *http.Request) {
	var req streamTokenRequest
	if !readJSON(w, r, &req) {
		return
	}

	if req.Scope != streamScopeAppLogs && req.Scope != streamScopeBuildLog && req.Scope != streamScopeStats && req.Scope != streamScopeAllStats {
		writeError(w, http.StatusUnprocessableEntity, "validation", `scope must be "app_logs", "build_log", "stats", or "all_stats"`)
		return
	}

	// streamScopeAllStats names no single app — unlike every other scope,
	// it must NOT go through the AppBySlug lookup below (which would 404
	// for the empty slug this scope requires) or the deployment_number
	// switch (which only makes sense once an app is in hand). Handled as
	// its own early return rather than folded into the switch below so
	// that switch can stay "given a real app, validate scope coherence"
	// without an app-less case awkwardly threaded through it.
	if req.Scope == streamScopeAllStats {
		if req.Slug != "" {
			writeError(w, http.StatusUnprocessableEntity, "validation", `slug must not be set for scope "all_stats"`)
			return
		}
		if req.DeploymentNumber != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation", `deployment_number must not be set for scope "all_stats"`)
			return
		}

		user := userFromContext(r.Context())
		token, tokenHash := auth.NewStreamToken()
		expiresAt := time.Now().Add(streamTokenDuration)
		if err := a.st.CreateStreamToken(user.ID, tokenHash, req.Scope, "", nil, expiresAt); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to create stream token")
			return
		}
		writeJSON(w, http.StatusOK, streamTokenResponse{
			Token:     token,
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		})
		return
	}

	app, err := a.st.AppBySlug(req.Slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "app_not_found", "app not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up app")
		}
		return
	}

	var deploymentNumber *int64
	switch req.Scope {
	case streamScopeAppLogs, streamScopeStats:
		if req.DeploymentNumber != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation", fmt.Sprintf("deployment_number must not be set for scope %q", req.Scope))
			return
		}
	case streamScopeBuildLog:
		if req.DeploymentNumber == nil {
			writeError(w, http.StatusUnprocessableEntity, "validation", "deployment_number is required for scope \"build_log\"")
			return
		}
		if _, err := a.st.DeploymentByNumber(app.ID, *req.DeploymentNumber); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "deployment_not_found", "deployment not found")
			} else {
				writeError(w, http.StatusInternalServerError, "internal", "failed to look up deployment")
			}
			return
		}
		n := int64(*req.DeploymentNumber)
		deploymentNumber = &n
	}

	user := userFromContext(r.Context())
	token, tokenHash := auth.NewStreamToken()
	expiresAt := time.Now().Add(streamTokenDuration)
	// app.Slug (not req.Slug) is what gets bound into the token, so a
	// stray case/whitespace difference between the two — impossible
	// today since AppBySlug is an exact match, but not worth relying on —
	// can never let the minted token disagree with the app it was
	// actually validated against.
	if err := a.st.CreateStreamToken(user.ID, tokenHash, req.Scope, app.Slug, deploymentNumber, expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create stream token")
		return
	}

	writeJSON(w, http.StatusOK, streamTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

// validateStreamToken resolves a stream token (as minted by
// handleCreateStreamToken) into its owning *store.User, requiring it to
// name exactly the (scope, slug, deploymentNumber) triple of the SSE
// route actually being requested — a token minted for one app's
// container logs must not open a different app's, or a build log, and
// vice versa. deploymentNumber is nil for the app-logs route (whose
// tokens never carry one) and the deployment's number for the build-log
// route. Reports false for a missing/expired token or any mismatch in
// that triple.
func (a *api) validateStreamToken(token, scope, slug string, deploymentNumber *int64) (*store.User, bool) {
	if token == "" {
		return nil, false
	}
	st, err := a.st.StreamTokenByHash(auth.HashToken(token))
	if err != nil {
		return nil, false
	}
	if st.Scope != scope || st.Slug != slug || !sameDeploymentNumber(st.DeploymentNumber, deploymentNumber) {
		return nil, false
	}
	user, err := a.st.UserByID(st.UserID)
	if err != nil {
		return nil, false
	}
	return user, true
}

// sameDeploymentNumber compares two possibly-nil deployment-number
// pointers for equality: both nil, or both non-nil with equal values.
func sameDeploymentNumber(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// requireAuthSSE builds the shared shape behind requireAuthAppLogs and
// requireAuthBuildLog: an Authorization header, if present, is validated
// exactly like requireAuth's — a session token, full account authority,
// unscoped to any one app; this is the CLI's path (see
// internal/cli/client.go's LogsStream) and must keep working unchanged.
// Only once the header is absent does it fall back to ?access_token=,
// resolved by streamAuth into the route-specific stream-token check
// (scope + slug [+ deployment number]) the caller supplies. A session
// token placed in ?access_token= is never accepted here: streamAuth only
// ever looks a query token up in the stream_tokens table (via
// validateStreamToken), and a session token's hash will never appear
// there.
//
// Once a user is resolved (by either path), it is authorized against
// capability exactly like requireCapability does for every other route —
// an SSE route is no less capability-gated than a normal JSON one just
// because it also accepts a stream token; a viewer-floor capability here
// (every SSE route's is — see router.go) means this is in practice a
// no-op beyond "not disabled", but it keeps every capability check
// routing through the same authz.Authorize call rather than SSE routes
// being a scope-check-only, role-check-free exception.
func (a *api) requireAuthSSE(capability authz.Capability, streamAuth func(r *http.Request, token string) (*store.User, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var user *store.User
			if token, ok := bearerToken(r.Header.Get("Authorization")); ok && token != "" {
				u, ok := a.validateToken(token)
				if !ok {
					writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
					return
				}
				user = u
			} else {
				token := r.URL.Query().Get("access_token")
				if token == "" {
					writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization")
					return
				}
				u, ok := streamAuth(r, token)
				if !ok {
					writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
					return
				}
				user = u
			}

			if err := authz.Authorize(user, capability); err != nil {
				writeError(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
		})
	}
}

// requireAuthAppLogs is requireAuth's variant for
// GET .../apps/{slug}/logs (see router.go): a native browser EventSource
// cannot set request headers, so this route also accepts a stream token
// minted with scope "app_logs" for this exact slug via ?access_token=
// (see requireAuthSSE's doc comment for the header-vs-query split, and
// handleCreateStreamToken for how that token is minted).
func (a *api) requireAuthAppLogs(next http.Handler) http.Handler {
	return a.requireAuthSSE(authz.CapLogsRead, func(r *http.Request, token string) (*store.User, bool) {
		return a.validateStreamToken(token, streamScopeAppLogs, chi.URLParam(r, "slug"), nil)
	})(next)
}

// requireAuthBuildLog is requireAuthAppLogs's twin for
// GET .../apps/{slug}/deployments/{number}/log: a query-string stream
// token must have been minted with scope "build_log" for this exact slug
// AND deployment number. A {number} that fails to parse as an integer can
// never match any minted token's deployment number, so it's treated as a
// non-match (401) at this layer — handleDeploymentLog separately reports
// 400 "invalid_request" for a malformed number on the header-auth path,
// since that's a request-shape problem, not an auth one, once a session
// token is what authenticated the request.
func (a *api) requireAuthBuildLog(next http.Handler) http.Handler {
	return a.requireAuthSSE(authz.CapLogsRead, func(r *http.Request, token string) (*store.User, bool) {
		number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
		if err != nil {
			return nil, false
		}
		return a.validateStreamToken(token, streamScopeBuildLog, chi.URLParam(r, "slug"), &number)
	})(next)
}

// requireAuthStats is requireAuthAppLogs's twin for
// GET .../apps/{slug}/stats (v0.5 plan Task 11): a query-string stream
// token must have been minted with scope "stats" for this exact slug — a
// token minted for "app_logs" or "build_log" (even for the same app and
// user) does not authenticate this route, and vice versa, since each SSE
// route's stream token is its own separate credential (see
// handleCreateStreamToken and validateStreamToken).
func (a *api) requireAuthStats(next http.Handler) http.Handler {
	return a.requireAuthSSE(authz.CapStatsRead, func(r *http.Request, token string) (*store.User, bool) {
		return a.validateStreamToken(token, streamScopeStats, chi.URLParam(r, "slug"), nil)
	})(next)
}

// requireAuthAllStats is requireAuthStats' batch-route twin for
// GET /api/v1/stats: a query-string stream token must have been minted
// with scope "all_stats" (see streamScopeAllStats's doc comment for why
// that's a separate scope from "stats" rather than reusing it). There is
// no {slug} URL param on this route, so the token's own slug (always ""
// for this scope — see handleCreateStreamToken) is compared against a
// fixed "" rather than a route param, mirroring how validateStreamToken
// already compares scope/slug/deploymentNumber as a fixed triple for
// every other SSE route.
func (a *api) requireAuthAllStats(next http.Handler) http.Handler {
	return a.requireAuthSSE(authz.CapStatsRead, func(r *http.Request, token string) (*store.User, bool) {
		return a.validateStreamToken(token, streamScopeAllStats, "", nil)
	})(next)
}
