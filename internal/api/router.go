// Package api implements BasePod's REST API v1: session auth, app
// lifecycle (create/list/get/delete), synchronous deploys, and a system
// status endpoint. It is transport plumbing only — all persistence goes
// through internal/store and all container orchestration goes through the
// Deployer interface below.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/auth"
	"github.com/base-al/basepod/internal/store"
)

// Deployer is the subset of *deploy.Engine the API needs: trigger a deploy
// and tear an app down. It is declared here (rather than the API depending
// on package deploy's concrete Engine type) so handlers can be tested
// against a fake instead of a real container runtime. *deploy.Engine
// satisfies this interface; see the compile-time assertion in api_test.go.
type Deployer interface {
	Deploy(ctx context.Context, app *store.App, imageRef string) (*store.Deployment, error)
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
const (
	loginRateLimit  = 10
	loginRateWindow = time.Minute
)

// api holds the dependencies every handler needs.
type api struct {
	st      *store.Store
	dep     Deployer
	ping    Pinger
	version string
	limiter *rateLimiter

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
}

// New builds the BasePod REST API v1 handler, mounted under /api/v1.
func New(st *store.Store, dep Deployer, ping Pinger, version string, seal func(string) (string, error), open func(string) (string, error), routes RoutesApplier, logs LogSource) http.Handler {
	a := &api{
		st:      st,
		dep:     dep,
		ping:    ping,
		version: version,
		limiter: newRateLimiter(loginRateLimit, loginRateWindow),
		seal:    seal,
		open:    open,
		routes:  routes,
		logs:    logs,
	}

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", a.handleLogin)

		r.Group(func(r chi.Router) {
			r.Use(a.requireAuth)
			r.Get("/auth/me", a.handleMe)
			r.Get("/system", a.handleSystem)
			r.Post("/apps", a.handleCreateApp)
			r.Get("/apps", a.handleListApps)
			r.Get("/apps/{slug}", a.handleGetApp)
			r.Post("/apps/{slug}/deploy", a.handleDeploy)
			r.Delete("/apps/{slug}", a.handleDeleteApp)
			r.Get("/apps/{slug}/logs", a.handleAppLogs)
			r.Get("/apps/{slug}/env", a.handleGetEnv)
			r.Put("/apps/{slug}/env", a.handlePutEnv)
			r.Get("/apps/{slug}/domains", a.handleListDomains)
			r.Post("/apps/{slug}/domains", a.handleAddDomain)
			r.Delete("/apps/{slug}/domains/{id}", a.handleDeleteDomain)
		})
	})
	return r
}

// userContextKey is the context key holding the authenticated *store.User,
// set by requireAuth.
type userContextKey struct{}

// requireAuth resolves "Authorization: Bearer <token>" into a *store.User
// and attaches it to the request context, or fails the request with 401
// "unauthorized" if the header is missing, malformed, or names a session
// that doesn't exist (or has expired).
func (a *api) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization header")
			return
		}

		user, err := a.st.UserBySessionTokenHash(auth.HashToken(token))
		if err != nil {
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

// rateLimiter is a naive fixed-window-per-key request limiter.
//
// v0.1-naive: a single map guarded by one mutex, with per-key history
// pruned lazily on access rather than via a background sweep. This is
// intentionally simple — fine for a single control-plane process gating
// login attempts from a handful of admins — and not meant to survive
// process restarts or scale past one instance.
type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	attempts map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, attempts: make(map[string][]time.Time)}
}

// Allow records an attempt for key and reports whether it is within the
// configured limit for the trailing window.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

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
