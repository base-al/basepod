package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/base-al/basepod/internal/api"
	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/config"
	"github.com/base-al/basepod/internal/crypto"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
	"github.com/base-al/basepod/web"
)

// devUIEnv, when set, points at a local Vite dev server (e.g.
// http://localhost:5173) that the root handler reverse-proxies to instead
// of serving the embedded dashboard build — used for dashboard hot-reload
// during development.
const devUIEnv = "BASEPOD_DEV_UI"

// Version is the running BasePod version, threaded into the REST API's
// /system endpoint. cmd/basepod sets this from its own build-time Version
// var before calling Run.
var Version = "dev"

// shutdownTimeout bounds how long Run waits for in-flight requests to
// finish once a shutdown signal arrives.
const shutdownTimeout = 10 * time.Second

// pruneInterval is how often expired sessions are swept from the store.
// pruneInitialDelay defers the first sweep past boot, so it doesn't
// compete with the rest of Run's startup work.
const (
	pruneInterval     = time.Hour
	pruneInitialDelay = time.Minute
)

// readHeaderTimeout and idleTimeout bound the http.Server built by
// newHTTPServer. There is deliberately no ReadTimeout or WriteTimeout:
// see newHTTPServer's doc comment for why.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// minPodmanMajor and minPodmanMinor are the oldest libpod API version
// BasePod supports — see checkPodmanVersion.
const (
	minPodmanMajor = 4
	minPodmanMinor = 9
)

// Run loads configuration from cfgPath and serves the BasePod control
// plane until ctx is canceled or a SIGINT/SIGTERM is received.
//
// Boot order: load config, open the store, refuse to start if no admin
// user exists yet (run `basepod setup` first), resolve the root domain,
// connect to Podman, ensure the Caddy container is up (self-healing any
// drifted image/ports or a disabled-DNS network along the way — see
// caddy.Manager.Ensure and podman.Client.EnsureNetwork), build the deploy
// engine, garbage-collect orphaned containers left behind by a previous
// crash (see Engine.CleanupOrphans), reconcile Caddy's route config
// against the store's current app state (the config file is rebuilt from
// DB truth on every boot), then serve HTTP with a graceful shutdown.
func Run(ctx context.Context, cfgPath string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("server: load config %s: %w", cfgPath, err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "basepod.db"))
	if err != nil {
		return fmt.Errorf("server: open store: %w", err)
	}
	defer st.Close()

	count, err := st.CountUsers()
	if err != nil {
		return fmt.Errorf("server: count users: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("server: no admin user found — run `basepod setup` first")
	}

	rootDomain, err := st.Setting("root_domain")
	if err != nil {
		return fmt.Errorf("server: read root_domain setting: %w", err)
	}
	if rootDomain == "" {
		rootDomain = cfg.RootDomain
	}
	if rootDomain == "" {
		return fmt.Errorf("server: no root domain configured — run `basepod setup` or set root_domain in the config file")
	}

	pc, err := podman.New(cfg.PodmanSocket)
	if err != nil {
		return fmt.Errorf("server: connect to podman — is Podman running? try `podman machine start`: %w", err)
	}
	if err := pc.Ping(ctx); err != nil {
		return fmt.Errorf("server: podman not reachable — is Podman running? try `podman machine start`: %w", err)
	}

	podmanVersion, err := pc.Version(ctx)
	if err != nil {
		return fmt.Errorf("server: checking podman version: %w", err)
	}
	if err := checkPodmanVersion(podmanVersion); err != nil {
		return err
	}

	mgr := caddy.NewManager(pc, caddy.PodmanExec, filepath.Join(cfg.DataDir, "caddy"), cfg.HTTPPort, cfg.HTTPSPort)
	if err := mgr.Ensure(ctx); err != nil {
		return fmt.Errorf("server: ensure caddy: %w", err)
	}

	encKey, err := crypto.LoadOrCreateKey(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("server: load encryption key: %w", err)
	}
	decrypt := func(sealed string) (string, error) {
		return crypto.Open(encKey, sealed)
	}
	encrypt := func(plaintext string) (string, error) {
		return crypto.Seal(encKey, plaintext)
	}

	engine := deploy.New(st, pc, mgr, deploy.CaddyProber(caddy.PodmanExec), rootDomain, decrypt)

	// Orphan GC: remove containers left behind by a previous crash or
	// interrupted deploy (unknown app slug, or a stale non-current
	// generation) before routes are reconciled below, so a leftover
	// container can never keep answering on a hostname/alias a fresh
	// deploy is about to reuse. Best-effort: a failure here is logged and
	// boot continues rather than dying on it.
	if removed, err := engine.CleanupOrphans(ctx); err != nil {
		log.Printf("basepod: orphan container cleanup: %v", err)
	} else if removed > 0 {
		log.Printf("basepod: orphan container cleanup: removed %d orphaned container(s)", removed)
	}

	// Reconcile: the Caddy config file is rebuilt from DB truth on every
	// boot, rather than trusting whatever current.json happened to
	// contain from a previous run.
	routes, err := engine.Routes()
	if err != nil {
		return fmt.Errorf("server: compute routes: %w", err)
	}
	if err := mgr.Apply(ctx, routes); err != nil {
		return fmt.Errorf("server: apply routes: %w", err)
	}

	srv := newHTTPServer(cfg.Listen, rootHandler(api.New(st, engine, pc.Ping, Version, encrypt, decrypt, engine, engine.AppLogs)))

	log.Printf("basepod: listening on %s", cfg.Listen)
	log.Printf("basepod: root domain %s", rootDomain)
	log.Printf("basepod: data dir %s", cfg.DataDir)

	go pruneSessions(ctx, st, pruneInitialDelay, pruneInterval)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutdown: %w", err)
		}
		return nil
	}
}

// newHTTPServer builds the control plane's http.Server.
//
// ReadHeaderTimeout bounds how long a client may take sending request
// headers (a slowloris-style defense) and IdleTimeout bounds how long a
// keep-alive connection may sit idle between requests — both are safe
// fixed bounds regardless of what a request's handler does.
//
// There is deliberately no ReadTimeout or WriteTimeout, unlike most
// hardened http.Server setups:
//   - ReadTimeout would bound the entire request, including the body —
//     but a deploy request's body is read up front and then the handler
//     itself runs synchronously for up to deployTimeout (5 minutes, see
//     internal/api) pulling images and polling health probes; a
//     ReadTimeout tuned for normal requests would have nothing to do
//     with that and one tuned to accommodate it would defeat the point.
//   - WriteTimeout would cut off in-progress writes on a fixed
//     wall-clock deadline from when the connection was accepted,
//     including GET .../logs's Server-Sent-Events stream (see
//     handleAppLogs), which is designed to stay open indefinitely for
//     follow=1 — it has to be bounded some other way, not by an
//     http.Server-wide timer.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// pruneSessions runs a ticking loop that sweeps expired sessions from st
// via store.PruneExpiredSessions, stopping cleanly when ctx is done. The
// first sweep fires after initialDelay (pruneInitialDelay in Run); every
// sweep after that is spaced interval (pruneInterval in Run) apart. A
// sweep's row count is only logged when it removed something, so a quiet,
// healthy system doesn't get an hourly "pruned 0 sessions" log line
// forever.
//
// This is its own function — called from Run as a goroutine, but not
// itself doing any other Run setup — so tests can drive the loop directly
// with a real (temp-file) store and short durations, without needing to
// run all of Run against a fake podman daemon.
func pruneSessions(ctx context.Context, st *store.Store, initialDelay, interval time.Duration) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if n, err := st.PruneExpiredSessions(); err != nil {
				log.Printf("basepod: prune expired sessions: %v", err)
			} else if n > 0 {
				log.Printf("basepod: pruned %d expired session(s)", n)
			}
			timer.Reset(interval)
		}
	}
}

// checkPodmanVersion parses a libpod version string (e.g. "4.9.2" or
// "5.0.0-dev") and reports an actionable error naming the found version
// if it is older than minPodmanMajor.minPodmanMinor. Only major.minor are
// compared — the patch segment (and any "-dev"-style suffix fused onto
// it) is ignored entirely, so "4.9.0", "4.9.2", and "4.9.0-dev" are all
// treated alike.
//
// This is its own function — called from Run right after pc.Version, but
// not itself doing any I/O — so tests can exercise the version gate
// directly against a literal version string, without needing to run all
// of Run against a fake podman daemon (see TestCheckPodmanVersion).
func checkPodmanVersion(version string) error {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("server: could not parse podman version %q — expected a dotted major.minor(.patch) version", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("server: could not parse podman version %q: %w", version, err)
	}
	// The minor segment may itself carry a "-dev"-style suffix (e.g. a
	// two-segment version like "4.9-dev"); a three-segment version's
	// suffix instead lands on parts[2] (the patch segment), which is
	// ignored outright, so it never reaches here.
	minorStr, _, _ := strings.Cut(parts[1], "-")
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return fmt.Errorf("server: could not parse podman version %q: %w", version, err)
	}

	if major > minPodmanMajor || (major == minPodmanMajor && minor >= minPodmanMinor) {
		return nil
	}
	return fmt.Errorf(
		"server: found podman version %s, but basepod requires podman >= %d.%d — please upgrade podman",
		version, minPodmanMajor, minPodmanMinor,
	)
}

// rootHandler composes the process's single HTTP server: apiHandler (as
// returned by api.New, which routes everything under /api/v1 itself)
// handles /api/*, and the dashboard handles everything else. The
// dashboard is normally the embedded, built-in-the-binary web.Handler();
// if BASEPOD_DEV_UI is set to a Vite dev server URL (e.g.
// http://localhost:5173), requests to / are reverse-proxied there instead
// so the dashboard can be edited with hot-reload against a real running
// control plane.
func rootHandler(apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", dashboardHandler())
	return mux
}

// dashboardHandler returns the embedded dashboard, or a reverse proxy to
// a local dev server when BASEPOD_DEV_UI is set.
func dashboardHandler() http.Handler {
	devUI := os.Getenv(devUIEnv)
	if devUI == "" {
		return web.Handler()
	}

	target, err := url.Parse(devUI)
	if err != nil {
		log.Printf("basepod: invalid %s=%q, serving the embedded dashboard instead: %v", devUIEnv, devUI, err)
		return web.Handler()
	}

	log.Printf("basepod: %s set — proxying / to dev server %s", devUIEnv, target)
	return httputil.NewSingleHostReverseProxy(target)
}
