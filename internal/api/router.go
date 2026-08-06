// Package api implements BasePod's REST API v1: session auth, app
// lifecycle (create/list/get/delete), synchronous deploys, and a system
// status endpoint. It is transport plumbing only — all persistence goes
// through internal/store and all container orchestration goes through the
// Deployer interface below.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// Deployer is the subset of *deploy.Engine the API needs: trigger a deploy
// and tear an app down. It is declared here (rather than the API depending
// on package deploy's concrete Engine type) so handlers can be tested
// against a fake instead of a real container runtime. *deploy.Engine
// satisfies this interface; see the compile-time assertion in api_test.go.
type Deployer interface {
	Deploy(ctx context.Context, app *store.App, imageRef string) (*store.Deployment, error)
	// DeployBuild runs a tarball-sourced deploy: gzTar is the raw gzipped
	// tar upload body, and builder is the shared build pipeline
	// (internal/build.Builder) handleDeployTarball hands every call —
	// passed per-call rather than baked into the Deployer at construction
	// so the API layer owns configuring build concurrency without
	// changing deploy.New's signature.
	DeployBuild(ctx context.Context, app *store.App, gzTar io.Reader, builder *build.Builder) (*store.Deployment, error)
	// Rollback redeploys app to an earlier deployment's exact image. See
	// deploy.Engine.Rollback's doc comment for its typed failure modes
	// (deploy.ErrRollbackTargetNotFound / ErrRollbackTargetUnhealthy /
	// ErrRollbackImageMissing), which handleRollback maps to specific HTTP
	// status/error codes — see writeRollbackError in apps.go.
	Rollback(ctx context.Context, app *store.App, targetNumber int) (*store.Deployment, error)
	RemoveApp(ctx context.Context, app *store.App) error
}

// Pinger checks that the container runtime is reachable. It is satisfied
// by (*podman.Client).Ping.
type Pinger func(ctx context.Context) error

// RoutesApplier recomputes and pushes the current route set to the router
// (Caddy). It is declared here (rather than the API depending on package
// deploy's concrete Engine type) so handlers can be tested against a fake
// instead of a real container runtime. *deploy.Engine satisfies this
// interface; see the compile-time assertion in api_test.go.
type RoutesApplier interface {
	ApplyRoutes(ctx context.Context) error
}

// LogSource streams a running app's raw (still-multiplexed — see
// podman.DemuxLogs) container log data by slug. It is declared here
// (rather than the API depending on package deploy's concrete Engine
// type) so handlers can be tested against a fake instead of a real
// container runtime; *deploy.Engine's AppLogs method satisfies it. It
// must return store.ErrNotFound for an unknown slug and deploy.ErrNotRunning
// when the app has no running container — handleAppLogs maps both to the
// documented HTTP status/error codes.
type LogSource func(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error)

// deployTimeout bounds a single deploy request. v0.1 deploys run
// synchronously inside the HTTP handler (no SSE build-log streaming until
// v0.2's real builds), so a stuck pull or health-probe loop must not hang
// the request forever.
const deployTimeout = 5 * time.Minute

// sessionDuration is how long a login session stays valid before it must
// be re-established.
const sessionDuration = 30 * 24 * time.Hour

// loginRateLimit and loginRateWindow bound login attempts per client IP.
// globalLoginRateLimit bounds FAILED login attempts across every client
// IP combined, over the same window — a backstop against a distributed
// flood (many source IPs, each well under loginRateLimit individually)
// that the per-IP limiter alone can't see, without letting a single IP
// exhaust it and lock everyone else out the way audit finding H1's
// degenerate-key bug did. globalLimiterKey is the single fixed key that
// backstop is tracked under, reusing rateLimiter's per-key map machinery
// as a plain counter.
const (
	loginRateLimit  = 10
	loginRateWindow = time.Minute

	globalLoginRateLimit = 100
	globalLimiterKey     = "global"
)

// maxJSONBody caps the size of a JSON request body handlers behind
// bodyLimit will decode. It exists to bound memory use against a
// malicious or buggy client streaming an enormous body at a JSON
// endpoint — 1 MiB is generously larger than any legitimate BasePod
// request payload (env var sets, domain names, app metadata) will ever
// be. The future tarball upload route (Task 6) needs a much larger cap of
// its own and is deliberately registered outside the group bodyLimit is
// applied to, rather than this constant being raised to accommodate it.
const maxJSONBody = 1 << 20 // 1 MiB

// bodyLimit wraps r.Body in an http.MaxBytesReader capped at
// maxJSONBody, so a request whose body exceeds it fails fast — as a
// *http.MaxBytesError surfacing through the next JSON decode (see
// readJSON) — rather than being read unbounded into memory first.
func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		next.ServeHTTP(w, r)
	})
}

// api holds the dependencies every handler needs.
type api struct {
	st      *store.Store
	dep     Deployer
	ping    Pinger
	version string

	// limiter bounds login attempts per client IP (see clientIP);
	// globalLimiter additionally bounds failed login attempts summed
	// across every IP, under the single fixed key globalLimiterKey — see
	// the loginRateLimit/globalLoginRateLimit doc comment.
	limiter       *rateLimiter
	globalLimiter *rateLimiter

	// seal/open encrypt and decrypt EnvVar.ValueEncrypted values. They
	// close over the process's encryption key (see internal/crypto) so
	// this package never handles the raw key itself.
	seal func(string) (string, error)
	open func(string) (string, error)

	// routes recomputes and pushes the current route set to the router
	// (Caddy) after a domain is added or removed.
	routes RoutesApplier

	// logs streams an app's raw container log data for handleAppLogs to
	// demux into SSE events.
	logs LogSource

	// builder is the shared tarball-build pipeline, passed straight
	// through to every Deployer.DeployBuild call by handleDeployTarball —
	// see the Deployer.DeployBuild doc comment for why it's threaded
	// through per-call rather than held only by dep.
	builder *build.Builder
}

// New builds the BasePod REST API v1 handler, mounted under /api/v1.
func New(st *store.Store, dep Deployer, ping Pinger, version string, seal func(string) (string, error), open func(string) (string, error), routes RoutesApplier, logs LogSource, builder *build.Builder) http.Handler {
	a := &api{
		st:            st,
		dep:           dep,
		ping:          ping,
		version:       version,
		limiter:       newRateLimiter(loginRateLimit, loginRateWindow),
		globalLimiter: newRateLimiter(globalLoginRateLimit, loginRateWindow),
		seal:          seal,
		open:          open,
		routes:        routes,
		logs:          logs,
		builder:       builder,
	}

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		// bodyLimit is applied per-group (here, and again below) rather
		// than once on the whole /api/v1 router, so a route that needs a
		// different cap — the future tarball upload route (Task 6) is the
		// known case — can be registered in its own group without it,
		// instead of every route having to opt out of a blanket wrapper.
		r.With(bodyLimit).Post("/auth/login", a.handleLogin)

		r.Group(func(r chi.Router) {
			r.Use(bodyLimit)
			r.Use(a.requireAuth)
			r.Get("/auth/me", a.handleMe)
			r.Post("/auth/logout", a.handleLogout)
			r.Get("/system", a.handleSystem)
			r.Post("/apps", a.handleCreateApp)
			r.Get("/apps", a.handleListApps)
			r.Get("/apps/{slug}", a.handleGetApp)
			r.Post("/apps/{slug}/deploy", a.handleDeploy)
			r.Post("/apps/{slug}/rollback", a.handleRollback)
			r.Delete("/apps/{slug}", a.handleDeleteApp)
			r.Get("/apps/{slug}/env", a.handleGetEnv)
			r.Put("/apps/{slug}/env", a.handlePutEnv)
			r.Get("/apps/{slug}/domains", a.handleListDomains)
			r.Post("/apps/{slug}/domains", a.handleAddDomain)
			r.Delete("/apps/{slug}/domains/{id}", a.handleDeleteDomain)
		})

		// The tarball upload route needs its own, much larger body cap
		// (see maxTarballBody in apps.go) — a bare http.MaxBytesReader in
		// the handler itself, not bodyLimit's 1 MiB — so it's registered
		// in its own group with only requireAuth, not bodyLimit.
		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.Post("/apps/{slug}/deploy/tarball", a.handleDeployTarball)
		})

		// The logs route gets its own auth middleware rather than
		// requireAuth: native EventSource cannot set an Authorization
		// header, so this route (and only this route, plus the build-log
		// route below — same reasoning) also accepts the session token via
		// ?access_token=. See requireAuthLogs.
		r.Group(func(r chi.Router) {
			r.Use(a.requireAuthLogs)
			r.Get("/apps/{slug}/logs", a.handleAppLogs)
			r.Get("/apps/{slug}/deployments/{number}/log", a.handleDeploymentLog)
		})
	})
	return r
}

// userContextKey is the context key holding the authenticated *store.User,
// set by requireAuth.
type userContextKey struct{}

// validateToken resolves a raw session token (however it was transported)
// into its *store.User via the session-hash lookup, or reports false if
// the token is missing/unknown/expired. Shared by requireAuth and
// requireAuthLogs so their validation logic — which must stay identical,
// a query token being an alternate transport for the same credential, not
// a weaker one — can't silently drift apart.
func (a *api) validateToken(token string) (*store.User, bool) {
	if token == "" {
		return nil, false
	}
	user, err := a.st.UserBySessionTokenHash(auth.HashToken(token))
	if err != nil {
		return nil, false
	}
	return user, true
}

// bearerPrefix is the "Authorization" header scheme this API accepts,
// compared case-insensitively by bearerToken — RFC 7235 defines
// auth-scheme as case-insensitive, and some HTTP clients/libraries send
// "bearer" or "BEARER" rather than the canonical "Bearer".
const bearerPrefix = "bearer "

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, matching the "Bearer" scheme case-insensitively (see
// bearerPrefix) while leaving the token itself — everything after the
// scheme and the single space — untouched, byte for byte. Shared by
// requireAuth and requireAuthLogs so their token-extraction logic can't
// silently drift apart.
func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return header[len(bearerPrefix):], true
}

// requireAuth resolves "Authorization: Bearer <token>" into a *store.User
// and attaches it to the request context, or fails the request with 401
// "unauthorized" if the header is missing, malformed, or names a session
// that doesn't exist (or has expired).
func (a *api) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization header")
			return
		}

		user, ok := a.validateToken(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	})
}

// requireAuthLogs is requireAuth's variant for the log-streaming route
// only: native EventSource cannot set request headers, so a browser
// client has no way to send "Authorization: Bearer <token>" when opening
// the stream. This middleware therefore also accepts the session token as
// a ?access_token= query parameter, tried only after the Authorization
// header comes up empty (so a valid header always wins over a query
// token, bogus or not — see TestHandleAppLogsBearerPrecedenceOverQueryToken
// in logs_test.go), and validated through the exact same validateToken
// helper requireAuth uses.
//
// This fallback is deliberately wired to this one route (see the router
// group above) rather than folded into requireAuth: query strings end up
// in access logs, browser history, and Referer headers far more readily
// than headers do, so every other endpoint should keep requiring the
// header. The token itself is never logged or echoed back by this
// middleware or handleAppLogs's error paths.
func (a *api) requireAuthLogs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || token == "" {
			token = r.URL.Query().Get("access_token")
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization")
			return
		}

		user, ok := a.validateToken(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired session")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	})
}

// userFromContext returns the authenticated user attached by requireAuth.
// Only called from handlers mounted behind that middleware, so it is
// always present there.
func userFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey{}).(*store.User)
	return u
}

// errorResponse is the wire shape of every error BasePod's API returns:
// {"error":{"code":"...","message":"..."}}.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError writes a JSON error response with the given status, code, and
// human-readable message.
func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

// writeJSON writes v as a JSON response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON decodes r's JSON body into v, with no error-response writing
// of its own — used directly by handlers (handleDeploy is the one case
// today) that need to inspect or tolerate a specific decode error (an
// empty body) themselves rather than going through readJSON's blanket
// handling.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// writeDecodeError translates a decodeJSON failure into the appropriate
// error envelope: 413 "request_too_large" if it's a body-size violation
// from bodyLimit's http.MaxBytesReader (detected via errors.As against
// *http.MaxBytesError), otherwise the generic 400 "invalid_request" every
// other malformed body gets.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
}

// readJSON decodes r's JSON body into v and reports whether it succeeded,
// writing the appropriate error response (via writeDecodeError) and
// returning false on failure. Handlers that don't need decodeJSON's
// finer-grained error handling should use this instead.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(r, v); err != nil {
		writeDecodeError(w, err)
		return false
	}
	return true
}

// sweepInterval and sweepThreshold gate rateLimiter's lazy eviction sweep
// (see maybeSweep): a sweep runs at most once per sweepInterval, unless
// the map has grown past sweepThreshold distinct keys, in which case it
// runs regardless of how recently the last one happened — bounding
// worst-case memory use even under a fast-moving flood of distinct keys
// within a single window.
const (
	sweepInterval  = 5 * time.Minute
	sweepThreshold = 1000
)

// rateLimiter is a naive fixed-window-per-key request limiter.
//
// v0.1-naive: a single map guarded by one mutex, with per-key history
// pruned lazily on access rather than via a background sweep. This is
// intentionally simple — fine for a single control-plane process gating
// login attempts from a handful of admins — and not meant to survive
// process restarts or scale past one instance.
//
// v0.3: the accessed key's own history was always pruned on access, but
// keys that are never accessed again (e.g. a flood of one-shot attempts
// from many distinct source IPs) used to live in the map forever.
// maybeSweep now also periodically drops any key whose newest attempt has
// aged out of the window, bounding the map's overall size.
type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	attempts map[string][]time.Time

	// nowFunc stands in for time.Now, injectable so tests can drive the
	// window and sweep logic without sleeping in real time.
	nowFunc func() time.Time

	lastSweep time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:     limit,
		window:    window,
		attempts:  make(map[string][]time.Time),
		nowFunc:   time.Now,
		lastSweep: time.Now(),
	}
}

// Allow records an attempt for key and reports whether it is within the
// configured limit for the trailing window.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.nowFunc()
	cutoff := now.Add(-rl.window)

	rl.maybeSweep(now, cutoff)

	kept := rl.attempts[key][:0]
	for _, t := range rl.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.attempts[key] = kept
		return false
	}
	rl.attempts[key] = append(kept, now)
	return true
}

// Blocked reports whether key is already at its limit for the trailing
// window, WITHOUT recording a new attempt. Used to gate a request before
// deciding whether it should even count as an attempt (see
// api.handleLogin): checking status must never itself consume a slot, or
// a caller sitting exactly at the limit could never find out they're
// blocked without also being the one to (re-)trip it.
func (rl *rateLimiter) Blocked(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.nowFunc()
	cutoff := now.Add(-rl.window)

	rl.maybeSweep(now, cutoff)

	count := 0
	for _, t := range rl.attempts[key] {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= rl.limit
}

// maybeSweep drops every key in the map whose newest recorded attempt is
// older than cutoff (i.e. has aged out of the window entirely), bounding
// the map to roughly the set of keys with recent activity. It only scans
// the whole map — an O(n) cost otherwise avoided by Allow's normal
// per-key pruning — when the map has grown past sweepThreshold or more
// than sweepInterval has passed since the last sweep, so a busy limiter
// isn't paying that cost on every call.
func (rl *rateLimiter) maybeSweep(now, cutoff time.Time) {
	if len(rl.attempts) <= sweepThreshold && now.Sub(rl.lastSweep) < sweepInterval {
		return
	}
	rl.lastSweep = now
	for key, times := range rl.attempts {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(rl.attempts, key)
		}
	}
}
