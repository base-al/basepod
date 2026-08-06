package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
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
	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/config"
	"github.com/base-al/basepod/internal/crypto"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/gitsource"
	"github.com/base-al/basepod/internal/instanceid"
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

// backgroundDeployGrace bounds how long Run waits, after the HTTP servers
// themselves have stopped accepting new requests, for any background
// tarball deploy goroutines (see deploy.Engine.DeployBuildAsync) that were
// already in flight to finish on their own. It is deliberately much larger
// than shutdownTimeout — a build can legitimately take minutes, and an
// abrupt cutoff mid-build is exactly the "silently orphaned" state issue #2
// set out to avoid — but still bounded: shutdown must never hang
// indefinitely on a slow or stuck build. A deploy still running when this
// expires is simply abandoned; its deployment row is left "deploying" for
// SweepStuckDeployments to reconcile (mark failed) at the next boot.
const backgroundDeployGrace = 30 * time.Second

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

// maxConcurrentBuilds bounds how many tarball-upload builds (see
// internal/build.Builder) may run at once across every app — a container
// build is CPU/IO-heavy enough that letting an unbounded number run
// simultaneously on a single control-plane host would be a foot-gun.
const maxConcurrentBuilds = 2

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

	// Git availability is checked once here, warn-only (git deploys are a
	// v0.5 addition; every existing image/tarball deploy path is
	// unaffected either way) — see gitsource.BinPath's resolution order
	// (cfg.GitPath, then $BASEPOD_GIT_BIN, then $PATH) and
	// gitsource.New's doc comment on why an empty path is a valid,
	// non-fatal Cloner state. gitCloner is constructed either way — with
	// gitBin == "" when unavailable — and passed to api.New unconditionally
	// (Task 4/5): every git deploy/webhook handler then fails with a
	// uniform 503 "git_unavailable" via gitsource.ErrGitUnavailable rather
	// than the API layer needing its own "is git installed" branch.
	gitBin, gitErr := gitsource.BinPath(cfg.GitPath)
	if gitErr != nil {
		log.Printf("basepod: git: %s — git push-to-deploy disabled until git is installed", gitErr)
	} else {
		log.Printf("basepod: git: using %s for git push-to-deploy", gitBin)
	}
	gitCloner := gitsource.New(gitBin, gitsource.DefaultOptions())

	// This instance's stable identity (issue #10): a random id persisted
	// in the data dir, next to secret.key, on first boot and never
	// regenerated — see instanceid.LoadOrCreate's doc comment. Every
	// resource BasePod creates from here on (bp-caddy, the "basepod"
	// network, every app container) is stamped basepod.instance=<id>, and
	// every "clean up what's mine" path (orphan GC in
	// engine.CleanupOrphans below, Caddy's own drift/recreate in
	// mgr.Ensure) filters on it — so a second BasePod instance sharing
	// this same Podman socket can never again mistake this instance's live
	// containers for its own orphans (the exact incident this fix exists
	// for). Resolved before mgr is built, since mgr.Ensure itself needs it
	// to decide whether an existing bp-caddy is this instance's own,
	// adoptable (unlabeled, pre-upgrade), or a different instance's
	// (refuse — see caddy.Manager.Ensure's doc comment).
	instID, err := instanceid.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("server: load/create instance id: %w", err)
	}

	mgr := caddy.NewManager(pc, caddy.PodmanExec, filepath.Join(cfg.DataDir, "caddy"), cfg.HTTPPort, cfg.HTTPSPort, instID)
	if err := mgr.Ensure(ctx); err != nil {
		return fmt.Errorf("server: ensure caddy: %w", err)
	}

	dashboardSetting, err := st.Setting("dashboard_domain")
	if err != nil {
		return fmt.Errorf("server: read dashboard_domain setting: %w", err)
	}
	dashboardDomain, writeDefaultDashboardDomain := resolveDashboardDomain(dashboardSetting, rootDomain)
	if writeDefaultDashboardDomain {
		// First boot (or an operator cleared the setting): persist the
		// computed default so it shows up in the settings table for an
		// operator to later find, override, or set to "off" — see
		// resolveDashboardDomain's doc comment.
		if err := st.SetSetting("dashboard_domain", dashboardDomain); err != nil {
			return fmt.Errorf("server: write default dashboard_domain setting: %w", err)
		}
	}

	// dashboardRoute and dashboardListener are only set when the dashboard
	// route is actually usable: the setting isn't "off", and a unix
	// socket bound at mgr.SockDir()/DashboardSockName — the same host
	// directory Manager's create() bind-mounts into (only) the bp-caddy
	// container, at DashboardSockMountDest — actually opens (mgr.Ensure
	// above guarantees that directory already exists on disk). A unix
	// socket, rather than a TCP listener on the "basepod" network's
	// gateway address (this feature's first cut), is what actually
	// restricts the dashboard API to bp-caddy: no app container gets this
	// bind mount, so none can reach the socket file, whereas every
	// container on the network could reach a gateway-address TCP
	// listener. Any failure here disables the dashboard route (nil)
	// rather than failing boot — Caddy must never be pointed at a dead
	// upstream.
	var dashboardRoute *caddy.DashboardRoute
	var dashboardListener net.Listener
	if dashboardDomain != "" {
		sockPath := filepath.Join(mgr.SockDir(), caddy.DashboardSockName)
		l, failReason := prepareDashboardListener(sockPath)
		if failReason != "" {
			// Expected on macOS: podman machine shares host directories
			// into its VM over virtiofs, which doesn't carry unix sockets
			// across that boundary — the dashboard route is Linux-first
			// for that reason; macOS operators keep using
			// http://localhost:3080 (or an SSH tunnel to a remote box)
			// instead. Warn and continue rather than failing boot.
			log.Printf(
				"basepod: dashboard: %s, dashboard route disabled (expected on macOS — podman "+
					"machine's virtiofs bind mounts don't carry unix sockets across the VM boundary; "+
					"see README's Remote access section)",
				failReason,
			)
		} else {
			dashboardListener = l
			dashboardRoute = &caddy.DashboardRoute{Hostname: dashboardDomain, Upstream: caddy.DashboardSockDial()}
		}
	}
	// http.Server.Shutdown closes every listener passed to Serve, so this
	// is only a backstop for an error return before that point (e.g.
	// mgr.Apply below failing after the listener was already bound); a
	// second Close() past that point is a harmless no-op error, ignored.
	defer func() {
		if dashboardListener != nil {
			_ = dashboardListener.Close()
		}
	}()

	encKey, err := crypto.LoadOrCreateKey(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("server: load encryption key: %w", err)
	}
	// decrypt/encrypt bind every env value to its owning app's ID and its
	// own key as AEAD additional-authenticated-data (audit finding L9 —
	// see crypto.AAD/SealAAD/OpenAAD's doc comments): a ciphertext
	// relocated onto a different app's row, or a different key within the
	// same row, then fails to decrypt. decrypt's OpenAAD call falls back
	// to a legacy no-AAD open for rows sealed before this binding existed
	// (crypto.Seal, pre-dating this change); every encrypt call re-seals
	// with the binding, so a legacy row is upgraded the next time
	// something writes it (internal/api/env.go's PUT handler, or
	// internal/deploy's manifest env defaults) — a lazy upgrade, not a
	// migration.
	decrypt := func(appID int64, key, sealed string) (string, error) {
		return crypto.OpenAAD(encKey, sealed, crypto.AAD(appID, key))
	}
	encrypt := func(appID int64, key, plaintext string) (string, error) {
		return crypto.SealAAD(encKey, plaintext, crypto.AAD(appID, key))
	}

	engine := deploy.New(st, pc, mgr, deploy.CaddyProber(caddy.PodmanExec), rootDomain, decrypt, encrypt, dashboardRoute, instID)
	builder := build.New(pc, cfg.DataDir, maxConcurrentBuilds)

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

	// Stuck-deployment sweep: any deployment still "deploying" from a
	// previous run whose generation has no running container means the
	// process died mid-deploy (a crash, or a hard kill past
	// backgroundDeployGrace below) — mark it failed rather than leaving it
	// (and the app it belongs to) stuck forever. Runs right after orphan
	// GC, deliberately: CleanupOrphans may itself have just removed that
	// deployment's container as an orphan, so this sweep's own
	// no-running-container check reflects ground truth after GC, not a
	// possibly-stale view from before it (see SweepStuckDeployments's doc
	// comment). Best-effort, like orphan GC above: a failure here is
	// logged and boot continues.
	if swept, err := engine.SweepStuckDeployments(ctx); err != nil {
		log.Printf("basepod: stuck-deployment sweep: %v", err)
	} else if swept > 0 {
		log.Printf("basepod: stuck-deployment sweep: marked %d deployment(s) failed", swept)
	}

	// Reconcile: the Caddy config file is rebuilt from DB truth on every
	// boot, rather than trusting whatever current.json happened to
	// contain from a previous run.
	routes, err := engine.Routes()
	if err != nil {
		return fmt.Errorf("server: compute routes: %w", err)
	}
	if err := mgr.Apply(ctx, routes, dashboardRoute); err != nil {
		return fmt.Errorf("server: apply routes: %w", err)
	}

	// handler is shared by both listeners. The public loopback listener
	// (cfg.Listen) serves it as-is — untrusted. The dashboard's
	// unix-socket listener, reachable only by bp-caddy's container (see
	// prepareDashboardListener's doc comment), serves it wrapped in
	// api.TrustedProxyMiddleware — marking every request that arrives
	// through it as having passed through Caddy, so clientIP (see
	// internal/api/clientip.go) may honor its X-Forwarded-For header.
	// This wrapping is the ONLY thing that ever grants trusted status:
	// there is no header or other client-controlled input that can spoof
	// it, since a request reaching the loopback listener never touches
	// TrustedProxyMiddleware at all.
	handler := rootHandler(api.New(st, engine, pc.Ping, Version, encrypt, decrypt, engine, engine.AppLogs, builder, gitCloner, engine.AppStats))
	srv := newHTTPServer(cfg.Listen, handler)

	// dashboardSrv is a second *http.Server sharing every setting
	// newHTTPServer applies, but wired to the trust-marking handler above
	// and served over dashboardListener directly (Serve, not
	// ListenAndServe) rather than its own Addr, which is therefore left
	// unset (unused; Serve ignores http.Server.Addr entirely).
	var dashboardSrv *http.Server
	if dashboardListener != nil {
		dashboardSrv = newHTTPServer("", api.TrustedProxyMiddleware(handler))
	}

	log.Printf("basepod: listening on %s", cfg.Listen)
	log.Printf("basepod: root domain %s", rootDomain)
	log.Printf("basepod: data dir %s", cfg.DataDir)
	if dashboardRoute != nil {
		log.Printf("basepod: dashboard: serving https://%s (proxied by Caddy to %s)", dashboardRoute.Hostname, dashboardRoute.Upstream)
	}

	go pruneSessions(ctx, st, pruneInitialDelay, pruneInterval)

	// serveErr is sized for both listeners (the main cfg.Listen one, always
	// started, plus the optional dashboard unix socket) — each of up to
	// two goroutines below sends exactly one value, and the buffer means
	// neither can block trying to send after the other has already caused
	// Run to return.
	serveErr := make(chan error, 2)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	if dashboardListener != nil {
		go func() {
			if err := dashboardSrv.Serve(dashboardListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
				return
			}
			serveErr <- nil
		}()
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var shutdownErrs []error
		if err := srv.Shutdown(shutdownCtx); err != nil {
			shutdownErrs = append(shutdownErrs, err)
		}
		if dashboardSrv != nil {
			if err := dashboardSrv.Shutdown(shutdownCtx); err != nil {
				shutdownErrs = append(shutdownErrs, err)
			}
		}

		// Both HTTP servers have now stopped accepting new requests and
		// finished in-flight ones (or been force-cut at shutdownTimeout) —
		// give any background tarball builds still running (started by a
		// request that already got its 202 response and returned) a
		// further, separate grace period to finish, rather than abandoning
		// them the instant the HTTP layer itself is done. Never blocks past
		// backgroundDeployGrace; see its doc comment.
		graceCtx, graceCancel := context.WithTimeout(context.Background(), backgroundDeployGrace)
		if err := engine.WaitForBackgroundDeploys(graceCtx); err != nil {
			log.Printf("basepod: shutdown: background deploy(s) still running after %s grace period — leaving their rows for the next boot's stuck-deployment sweep", backgroundDeployGrace)
		}
		graceCancel()

		if err := errors.Join(shutdownErrs...); err != nil {
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

// resolveDashboardDomain interprets the raw dashboard_domain setting value
// (current, exactly as read from the store — "" if never set) against
// rootDomain, returning the effective dashboard hostname to route through
// Caddy and whether the caller must persist a newly-computed default back
// to the store.
//
// "" (unset — the common case, before any operator has touched the
// setting) computes a default of "basepod.<rootDomain>" and reports
// writeDefault=true, so Run persists it: an operator can then find (and
// later override, or clear back to "" to pick up a rootDomain change, or
// set to "off") the value BasePod chose, in the settings table. The
// literal value "off" disables the dashboard route outright (domain ""
// with writeDefault=false — Run treats an empty returned domain as
// "don't route the dashboard at all", regardless of why). Any other value
// is an operator-chosen hostname, used as-is.
//
// This is its own function — called from Run right after mgr.Ensure, but
// not itself doing any I/O — so tests can exercise the resolution logic
// directly against literal setting values, without needing to run all of
// Run against a fake podman daemon (see TestResolveDashboardDomain).
func resolveDashboardDomain(current, rootDomain string) (domain string, writeDefault bool) {
	switch current {
	case "":
		return "basepod." + rootDomain, true
	case "off":
		return "", false
	default:
		return current, false
	}
}

// dashboardSockPerm is the dashboard's unix socket's file mode:
// owner+group read/write, no "other" access at all. This is defense in
// depth on top of the bind mount itself (see DashboardSockMountDest's doc
// comment) — only bp-caddy's container ever sees this file in the first
// place, since no app container gets that mount, but a restrictive mode
// means even a process inside bp-caddy's own container running as an
// unexpected UID/GID can't reach it either.
const dashboardSockPerm = 0o660

// prepareDashboardListener binds a unix socket at sockPath for the
// dashboard's second listener, applying dashboardSockPerm to it. Every
// failure mode — removing a stale socket file left behind by a previous
// run (net.Listen would otherwise fail with "address already in use"
// against it), the bind itself (e.g. macOS, where podman machine's
// virtiofs bind mounts don't carry unix sockets across the VM boundary),
// or the chmod — is reported through the string return rather than an
// error: every one of them means "disable the dashboard route and keep
// booting", never "fail Run", so there is nothing for a caller to
// meaningfully do with a typed error here.
//
// This is its own function — called from Run right after mgr.Ensure, but
// only touching the filesystem (no podman/store I/O) — so tests can
// exercise it directly against a temp directory, without needing to run
// all of Run against a fake podman daemon (see
// TestPrepareDashboardListener*).
func prepareDashboardListener(sockPath string) (l net.Listener, failReason string) {
	if err := os.RemoveAll(sockPath); err != nil {
		return nil, fmt.Sprintf("could not remove stale socket %s: %v", sockPath, err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Sprintf("could not bind unix socket %s: %v", sockPath, err)
	}
	if err := os.Chmod(sockPath, dashboardSockPerm); err != nil {
		_ = l.Close()
		return nil, fmt.Sprintf("could not chmod socket %s: %v", sockPath, err)
	}
	return l, ""
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
//
// Every response — dashboard pages and API responses alike — goes
// through securityHeaders first (audit finding M2): the dashboard's
// session token lives in localStorage, not an HttpOnly cookie, so the
// browser-security headers below matter for the HTML/JS that touches it,
// not just the API's JSON.
func rootHandler(apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", dashboardHandler())
	return securityHeaders(mux)
}

// cspScriptHash is the CSP script-src hash-source for the one inline
// <script> block web/index.html ships: the pre-paint dark/light class
// flip that reads localStorage before Vue mounts, so there's no flash of
// the wrong theme. It's a `sha256-<base64>` of the exact bytes between
// that block's <script> and </script> tags. Vite leaves this block
// untouched (byte-for-byte) when it builds web/dist, so the hash stays
// valid across rebuilds as long as the script itself doesn't change; if
// it does, TestSecurityHeadersCSPScriptHashMatchesDashboard in
// server_test.go fails and names the fix (recompute this constant, or —
// preferably, if the script grows — move it to an external same-origin
// file instead and drop the hash-source entirely). This keeps CSP's
// script-src at 'self' plus this one exact, named exception — never
// 'unsafe-inline'.
const cspScriptHash = "sha256-5guJyYK5v2PtGhrjDvi2ZURW/XRuzCbI4oTsaQIS49E="

// cspHeader is the dashboard/API's Content-Security-Policy value (audit
// finding M2). default-src 'self' plus the explicit denials
// (frame-ancestors/base-uri/object-src) block the SPA from ever loading
// or being loaded as anything but itself: no framing (clickjacking),
// no <base>-tag hijacking, no plugin/object embeds. script-src repeats
// 'self' and adds only cspScriptHash — never 'unsafe-inline' — so the
// one legitimate inline script (see cspScriptHash's doc comment) is
// allowed by exact content match and nothing else inline can run. No
// style-src override is needed: the built dashboard has no inline
// <style> tag or style="" attribute (verified against `npm run build`'s
// actual dist/ output while implementing this).
const cspHeader = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"script-src 'self' '" + cspScriptHash + "'"

// securityHeaders sets a fixed set of browser-security response headers
// on every response (audit finding M2): CSP (see cspHeader),
// X-Content-Type-Options to stop MIME-sniffing, a strict Referrer-Policy
// so URLs (which may include ?access_token= on the log-streaming routes)
// never leak to a Referer header, X-Frame-Options as a legacy backstop
// for browsers that don't honor CSP's frame-ancestors, and
// Strict-Transport-Security.
//
// HSTS is set unconditionally even though every listener this process
// itself binds — the loopback :3080 listener and the dashboard's unix
// socket — only ever speaks plain HTTP, never TLS: browsers only act on
// Strict-Transport-Security when the response that carried it already
// arrived over HTTPS in the first place (RFC 6797 §7.2), so a client
// hitting http://127.0.0.1:3080 directly just ignores the header — it's
// inert there, not a lie. The header earns its keep on the one path a
// browser actually sees this response over TLS: proxied through Caddy at
// the dashboard's public hostname, where it now tells the browser to
// never downgrade that hostname back to plain HTTP.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", cspHeader)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
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
