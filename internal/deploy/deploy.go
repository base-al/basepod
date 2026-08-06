// Package deploy implements BasePod's deploy engine: a probe-gated
// container cutover built on top of the store, podman, and caddy
// packages. Deploy pulls an image, starts a new container generation
// alongside the old one, waits for it to answer health probes, cuts
// traffic over via the Caddy router, and only then tears down the
// previous generation — so a bad deploy never takes down a working app.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/manifest"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// ErrNotRunning is returned by AppLogs when an app has no running
// container to stream logs from (e.g. it was created but never deployed,
// or its last deploy failed before any container reached "running").
var ErrNotRunning = errors.New("deploy: app has no running container")

// Rollback's typed failure modes — the API layer (see
// internal/api/apps.go's writeRollbackError) maps each to its own HTTP
// status/error code, so these must stay distinguishable via errors.Is
// rather than collapsed into a single generic error.
var (
	// ErrRollbackTargetNotFound is returned when the requested deployment
	// number doesn't exist for the app.
	ErrRollbackTargetNotFound = errors.New("deploy: rollback target not found")
	// ErrRollbackTargetUnhealthy is returned when the requested deployment
	// exists but its Status isn't "healthy" (e.g. it failed, or never
	// finished) — there is nothing known-good to roll back to.
	ErrRollbackTargetUnhealthy = errors.New("deploy: rollback target is not healthy")
	// ErrRollbackImageMissing is returned when the target deployment's
	// image is a local build tag ("localhost/basepod/<slug>:<n>") that is
	// no longer present in the local image store (e.g. pruned by
	// retention, or removed out-of-band) — unlike a registry ref, it can
	// never be re-fetched by a pull.
	ErrRollbackImageMissing = errors.New("deploy: rollback image is no longer available locally")
)

// localImagePrefix marks an image ref as a BasePod-built local tag (see
// internal/build.Builder) rather than a registry reference: it can never
// be pulled, so both Rollback and pruneBuiltImages treat it specially.
const localImagePrefix = "localhost/"

// retainBuiltImages is how many of an app's built image tags
// (localhost/basepod/<slug>:<n>) pruneBuiltImages keeps, newest-numbered
// first, after every successful rollout.
const retainBuiltImages = 5

// aliasV2Prefix and legacyAliasPrefix are the two network-alias prefixes
// an app container can carry (audit finding M1). Every rollout from this
// release on creates its container with aliasV2Prefix+slug (see
// runRollout) — disjoint from the "bp-<slug>-<n>" container-name
// namespace, so the two can never collide, unlike the legacy scheme's
// "bp-<slug>" alias which shared that namespace. Containers created before
// this fix still carry only legacyAliasPrefix+slug and are never
// recreated just to relabel them (see migration 00005_alias_scheme.sql and
// store.App.AliasScheme's doc comment) — Routes() picks the right prefix
// per app from its stored alias_scheme.
const (
	aliasV2Prefix     = "app-"
	legacyAliasPrefix = "bp-"
)

// Engine drives app deployments: pull, create, start, probe, cut traffic
// over, and tear down the previous container generation.
type Engine struct {
	st         *store.Store
	rt         Runtime
	router     Router
	probe      Prober
	rootDomain string

	// dashboard is the route (if any) BasePod's own web dashboard is
	// reachable through Caddy on — resolved once at boot (see
	// internal/server.Run) from the dashboard_domain setting and the
	// "basepod" network's gateway address, and passed straight through
	// into every router.Apply call alongside the app route set. It is nil
	// when the dashboard route is disabled (setting "off", or the gateway
	// listener failed to bind — see server.Run).
	dashboard *caddy.DashboardRoute

	// routesMu serializes every route render+apply critical section: the
	// Routes() DB read paired with the router.Apply call that renders it,
	// across ApplyRoutes (called directly by the API layer on domain
	// add/delete, and internally by Deploy's cutover) and RemoveApp's
	// filtered apply. These can run concurrently — e.g. a domain-add HTTP
	// handler racing an in-flight deploy's cutover — and both write the
	// same current.json.tmp (internal/caddy/manager.go writeFileAtomic),
	// so an unserialized interleaving can splice two writers' output into
	// a corrupt config, hit an ENOENT rename race that 502s a live
	// request, or apply a stale route set that silently drops a
	// just-added hostname. The lock must be held across the DB read too,
	// not just the Apply call: "last write wins" here has to mean
	// "rendered from the latest DB state at the time it ran", not just
	// "the last Apply call physically completed last" — a Routes() read
	// taken outside the lock could be stale by the time its Apply runs.
	// It must NOT wrap all of Deploy: pulls and probes are slow and must
	// not serialize with unrelated domain adds, so only the route-apply
	// step is under this lock.
	routesMu sync.Mutex

	// decrypt turns an EnvVar.ValueEncrypted into its plaintext value,
	// given the owning app's ID and the var's key (bound in as AEAD
	// additional-authenticated-data — audit finding L9 — so a ciphertext
	// relocated onto a different app's row, or a different key within the
	// same row, fails to decrypt; see internal/crypto's AAD/OpenAAD and
	// the closures internal/server.Run builds). nil disables env
	// injection entirely: Deploy fails rather than ship a container
	// silently missing configured env vars (see Deploy).
	decrypt func(appID int64, key, sealed string) (string, error)

	// encrypt is decrypt's inverse (crypto.SealAAD under the hood),
	// given the owning app's ID and the var's key. It is used only to
	// persist a basepod.yaml manifest's `env` defaults for keys an app
	// doesn't already have (see applyManifestEnvDefaults, resolving
	// issue #1) — every other env write goes through the API layer's own
	// seal closure (internal/api/env.go), not this one. nil skips
	// applying manifest env defaults entirely rather than failing the
	// deploy over a missing nice-to-have.
	encrypt func(appID int64, key, plaintext string) (string, error)

	// probeInterval and probeAttempts control the health-probe retry
	// loop; they default to 1s/30 (New) and are unexported so tests can
	// shrink them to keep the suite fast.
	probeInterval time.Duration
	probeAttempts int

	// log receives best-effort teardown/bookkeeping errors that must not
	// fail an otherwise-successful (or already-failed) deploy.
	log io.Writer

	// wg tracks every background deploy goroutine started by
	// DeployBuildAsync, so a graceful shutdown (see WaitForBackgroundDeploys)
	// can give in-flight builds a grace period to finish rather than
	// abandoning them the instant the process is asked to stop.
	wg sync.WaitGroup
}

// New builds an Engine. rootDomain is appended to an app's slug to form
// its hostname (e.g. rootDomain "apps.example.com" -> "blog.apps.example.com").
// decrypt turns a stored EnvVar's encrypted value into plaintext, given
// the owning app's ID and the var's key; pass nil to disable env
// injection (Deploy then fails rather than silently omitting configured
// env vars). encrypt is decrypt's inverse, used only to persist manifest
// `env` defaults (see the encrypt field's doc comment); pass nil to skip
// applying them. dashboard is the dashboard route (or nil to disable it)
// every router.Apply call made through this Engine carries alongside the
// app route set — see the dashboard field's doc comment.
func New(st *store.Store, rt Runtime, router Router, probe Prober, rootDomain string, decrypt func(appID int64, key, sealed string) (string, error), encrypt func(appID int64, key, plaintext string) (string, error), dashboard *caddy.DashboardRoute) *Engine {
	return &Engine{
		st:            st,
		rt:            rt,
		router:        router,
		probe:         probe,
		rootDomain:    rootDomain,
		decrypt:       decrypt,
		encrypt:       encrypt,
		dashboard:     dashboard,
		probeInterval: time.Second,
		probeAttempts: 30,
		log:           os.Stderr,
	}
}

// Deploy runs one deploy of app to imageRef (which may differ from
// app.ImageRef when redeploying with a new image): pull, create+start a
// new container, probe it, cut traffic over, and remove the previous
// container generation. On any failure the new container is cleaned up
// and the old (previous-generation) containers are left untouched.
func (e *Engine) Deploy(ctx context.Context, app *store.App, imageRef string) (*store.Deployment, error) {
	dep, err := e.st.CreateDeployment(app.ID, imageRef)
	if err != nil {
		return nil, fmt.Errorf("deploy: create deployment: %w", err)
	}
	if err := e.st.UpdateAppStatus(app.ID, "deploying"); err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: mark deploying: %w", err))
	}

	if err := e.rt.PullImage(ctx, imageRef); err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: pull %s: %w", imageRef, err))
	}

	return e.runRollout(ctx, app, dep, imageRef)
}

// DeployBuild runs one tarball-sourced deploy of app: it creates the
// deployment row up front (source "tarball", with no image_ref yet — see
// store.CreateDeploymentFull) so the deployment number exists for the
// build log's path, records that path on the deployment immediately
// (builder.LogPath is a pure computation from slug+number, so this needs
// no I/O and can happen before the build even starts — see its doc
// comment for why: it's what makes GET .../deployments/{n}/log able to
// live-tail the log for the build's entire duration, not just after it
// finishes), marks the app "deploying", then hands gzTar off to
// builder.BuildManifest to decompress, validate, and build it into a
// local image tag — additionally parsing a root-level basepod.yaml, if
// present, into a *manifest.Manifest (see build.Result).
//
// On a build failure, this goes through the same fail() path Deploy uses
// for every other failure: the deployment is marked failed and the app's
// status is restored (to "running" if other containers are still up for
// it, "error" otherwise) — no container was ever created for a build
// failure, so fail() is called with an empty newContainerID.
//
// On a successful build, any parsed manifest is applied onto app via
// applyManifestDefaults (port/resources — see its doc comment for the
// precedence rule) and applyManifestEnvDefaults (env), both persisted to
// the store before rollout proceeds — so the very first rollout of a
// zero-config deploy already honors basepod.yaml, not just the next one.
// The image tag is recorded on the deployment and rollout proceeds via
// the same runRollout Deploy uses — but, critically, skipping Deploy's
// PullImage step: the build produces a local "localhost/basepod/..."
// tag, and pulling a localhost/ tag from a registry would either fail
// outright or (worse) silently pull the wrong thing.
//
// If the build succeeds but the rollout that follows it fails (a failed
// probe, a container that won't start, ...), the freshly-built tag never
// becomes a running deployment's image and is removed as an orphan (see
// removeOrphanBuildImage) — runRollout's own retention (pruneBuiltImages)
// only prunes on a *successful* rollout, so without this a string of
// repeated failed builds would leave one dead tag behind per attempt,
// forever.
func (e *Engine) DeployBuild(ctx context.Context, app *store.App, gzTar io.Reader, builder *build.Builder) (*store.Deployment, error) {
	dep, err := e.createBuildDeployment(ctx, app, builder)
	if err != nil {
		return dep, err
	}

	prepared, err := builder.PrepareBuild(gzTar)
	if err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: build: %w", err))
	}

	return e.buildAndRollout(ctx, app, dep, prepared, builder)
}

// asyncBuildTimeout bounds a background tarball build+rollout started by
// DeployBuildAsync — generous enough to cover the slowest legitimate build
// plus a full rollout, on a context detached from the triggering HTTP
// request (which ends the moment its 202 response is written — see
// DeployBuildAsync's own doc comment). Deliberately the same bound
// buildTimeout enforces synchronously on the (unused, for this path) tarball
// request context in internal/api, so a background build gets no more grace
// than the old synchronous request-bound deploy ever did.
const asyncBuildTimeout = 60 * time.Minute

// DeployBuildAsync is DeployBuild's asynchronous twin: prepared is a build
// context builder.PrepareBuild has already spooled and validated
// SYNCHRONOUSLY by the caller (the API's handleDeployTarball, before ever
// responding — so a malformed upload still fails fast with the caller's
// own 413/422, and no deployment row exists at all for a request that
// never gets past validation).
//
// This method itself only does fast, synchronous bookkeeping — create the
// deployment row, record its build-log path, and mark the app "deploying"
// (see createBuildDeployment) — before the caller ever sees a response, so
// the deployment number in its return value is always immediately valid
// for a caller to act on (e.g. minting a build-log stream token for it).
// The actual build+rollout is handed off to a background goroutine running
// on ctx detached from the caller's own cancellation
// (context.WithoutCancel) with a fresh, generous timeout of its own
// (asyncBuildTimeout): an HTTP request's context dies the moment its
// response is written, well before a build can realistically finish.
//
// The returned deployment is the freshly created row, always still
// "deploying" — its terminal status only ever becomes visible later, via
// GET .../deployments/{n} or the build-log SSE stream (see
// internal/api.handleGetDeployment / handleDeploymentLog), never as this
// call's own return value. If bookkeeping itself fails before the
// goroutine ever starts (e.g. a store write error), the returned error is
// non-nil and no goroutine is started — prepared is closed either way,
// never leaked.
//
// The background goroutine is tracked in e.wg so a graceful shutdown (see
// WaitForBackgroundDeploys) can give it a grace period to finish rather
// than abandoning it mid-build the instant the process is asked to stop.
func (e *Engine) DeployBuildAsync(ctx context.Context, app *store.App, prepared *build.PreparedBuild, builder *build.Builder) (*store.Deployment, error) {
	dep, err := e.createBuildDeployment(ctx, app, builder)
	if err != nil {
		prepared.Close()
		return dep, err
	}

	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), asyncBuildTimeout)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		e.buildAndRollout(bgCtx, app, dep, prepared, builder)
	}()

	return dep, nil
}

// WaitForBackgroundDeploys blocks until every background deploy goroutine
// started by DeployBuildAsync has finished, or ctx is done — whichever
// comes first. Called from internal/server's graceful-shutdown path with a
// bounded grace period: if that grace period expires with deploys still
// running, this returns ctx's error and the caller proceeds with shutdown
// anyway (never blocking it indefinitely) — any deployment row still
// "deploying" at that point is left for SweepStuckDeployments to reconcile
// at the next boot.
func (e *Engine) WaitForBackgroundDeploys(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// createBuildDeployment is the fast, synchronous bookkeeping shared by
// DeployBuild and DeployBuildAsync: create the deployment row (source
// "tarball", no image_ref yet), record its build-log path (a pure path
// computation — see builder.LogPath's doc comment — so it's addressable
// for live tailing from before the build even starts), and mark the app
// "deploying". A failure marking the app "deploying" goes through the
// normal fail() path (the deployment row already exists by that point) and
// is returned as an error; every other error here (creating the row in the
// first place) has no deployment row to attach a failure to and is
// returned as a plain error instead, matching DeployBuild's pre-existing
// contract.
func (e *Engine) createBuildDeployment(ctx context.Context, app *store.App, builder *build.Builder) (*store.Deployment, error) {
	dep, err := e.st.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		return nil, fmt.Errorf("deploy: create deployment: %w", err)
	}

	logPath := builder.LogPath(app.Slug, dep.Number)
	if err := e.st.SetDeploymentBuildLog(dep.ID, logPath); err != nil {
		fmt.Fprintf(e.log, "deploy: set build log path for deployment %d: %v\n", dep.ID, err)
	}
	dep.BuildLogPath = logPath

	if err := e.st.UpdateAppStatus(app.ID, "deploying"); err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: mark deploying: %w", err))
	}

	return dep, nil
}

// buildAndRollout runs prepared (an already spooled+validated build
// context — see build.Builder.PrepareBuild) through the per-app-locked,
// semaphore-bounded build (builder.BuildPrepared) and the shared rollout
// pipeline, for a deployment row (dep) that createBuildDeployment has
// already created. Shared by DeployBuild (called synchronously, ctx is the
// caller's own) and DeployBuildAsync's background goroutine (ctx is a
// detached context with its own timeout — see that method's doc comment).
//
// See the former DeployBuild's doc comment (now split across this method
// and its callers) for the full behavior: applying a basepod.yaml
// manifest's defaults, recording the built image tag, running the shared
// rollout, and cleaning up an orphaned build image if the rollout fails.
func (e *Engine) buildAndRollout(ctx context.Context, app *store.App, dep *store.Deployment, prepared *build.PreparedBuild, builder *build.Builder) (*store.Deployment, error) {
	res, buildErr := builder.BuildPrepared(ctx, app.Slug, dep.Number, prepared)
	if buildErr != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: build: %w", buildErr))
	}
	tag := res.ImageTag

	// Apply the build context's basepod.yaml (if any) onto app BEFORE
	// runRollout builds the container spec below, so the very first
	// rollout of a zero-config deploy already reflects it (issue #1) —
	// see applyManifestDefaults/applyManifestEnvDefaults for the
	// precedence rules.
	if res.Manifest != nil {
		if applyManifestDefaults(app, res.Manifest) {
			if err := e.st.UpdateAppPort(app.ID, app.Port); err != nil {
				return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: apply manifest port: %w", err))
			}
			if err := e.st.UpdateAppLimits(app.ID, app.MemoryLimitMB, app.CPULimit, app.PidsLimit); err != nil {
				return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: apply manifest resources: %w", err))
			}
		}
		if err := e.applyManifestEnvDefaults(app, res.Manifest.Env); err != nil {
			return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: apply manifest env defaults: %w", err))
		}
	}

	if err := e.st.SetDeploymentImage(dep.ID, tag); err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: set deployment image: %w", err))
	}
	dep.ImageRef = tag

	result, rollErr := e.runRollout(ctx, app, dep, tag)
	if rollErr != nil {
		e.removeOrphanBuildImage(ctx, tag)
	}
	return result, rollErr
}

// applyManifestDefaults fills in app's Port, MemoryLimitMB, and CPULimit
// from mf (a basepod.yaml parsed by internal/build.Builder.BuildManifest)
// — resolving issue #1's TODO — and reports whether app was mutated (the
// caller, DeployBuild, owns persisting any change via store.UpdateAppPort
// / store.UpdateAppLimits before the container spec that uses these
// fields is built).
//
// Precedence rule: the app's own stored config always wins over the
// manifest. A field is only filled in when the app is still at its
// unset/default value:
//
//   - Port: only when app.Port == 0. In practice every app created
//     through the dashboard/API already has an explicit, validated
//     (1-65535) port (see handleCreateApp), so this fires only for an
//     app whose port was never set — it deliberately does NOT overwrite
//     an operator's dashboard-configured port just because a manifest
//     in a later upload names a different one.
//   - Resources (Memory/CPUs): only when the app's MemoryLimitMB/CPULimit
//     still equal store.DefaultMemoryLimitMB/store.DefaultCPULimit — the
//     values every app gets at creation (see store.CreateApp) — rather
//     than a value an operator (or an earlier manifest) already set
//     explicitly via PATCH /api/v1/apps/{slug}. There is no manifest
//     field for pids_limit, so it is never touched here.
//
// Healthcheck.Path is intentionally NOT applied: store.App has no column
// for it yet, and adding one is out of scope for this change (a schema
// migration is a separate, deliberate decision) — see the change's
// report for this call-out.
func applyManifestDefaults(app *store.App, mf *manifest.Manifest) bool {
	changed := false

	if mf.Port != 0 && app.Port == 0 {
		app.Port = mf.Port
		changed = true
	}

	if mf.Resources.Memory != "" && app.MemoryLimitMB == store.DefaultMemoryLimitMB {
		if bytesVal, err := manifest.ParseMemory(mf.Resources.Memory); err == nil {
			if mb := bytesVal / (1024 * 1024); mb > 0 {
				app.MemoryLimitMB = mb
				changed = true
			}
		}
	}

	if mf.Resources.CPUs != 0 && app.CPULimit == store.DefaultCPULimit {
		app.CPULimit = mf.Resources.CPUs
		changed = true
	}

	return changed
}

// applyManifestEnvDefaults persists defaults's non-secret env vars onto
// app for every key it doesn't already have — never overwriting an
// existing value (of either a plain var or a secret) and never touching
// anything already stored as a secret, matching basepod.yaml's own
// contract that `env:` is non-secret defaults only (see
// manifest.Manifest.Env's doc comment). It is a no-op when defaults is
// empty or e.encrypt is nil (env injection/sealing disabled — see the
// encrypt field's doc comment): a missing manifest env default is never
// worth failing an otherwise-successful build+deploy over.
func (e *Engine) applyManifestEnvDefaults(app *store.App, defaults map[string]string) error {
	if len(defaults) == 0 || e.encrypt == nil {
		return nil
	}

	existing, err := e.st.ListEnvVars(app.ID)
	if err != nil {
		return fmt.Errorf("deploy: list env vars: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, ev := range existing {
		have[ev.Key] = true
	}

	for key, value := range defaults {
		if have[key] {
			continue
		}
		sealed, err := e.encrypt(app.ID, key, value)
		if err != nil {
			return fmt.Errorf("deploy: seal manifest env default %s: %w", key, err)
		}
		if err := e.st.UpsertEnvVar(app.ID, key, sealed, false); err != nil {
			return fmt.Errorf("deploy: store manifest env default %s: %w", key, err)
		}
	}
	return nil
}

// removeOrphanBuildImage best-effort removes a freshly-built local image
// tag whose rollout failed before it ever became a running deployment's
// image — called by DeployBuild only when runRollout returns an error
// (see its doc comment). Logged and skipped on failure, like every other
// post-deploy cleanup step in this file: a stray failed-build tag is a
// disk-space nuisance, not a correctness problem. Runs on a context
// detached from ctx's cancellation, matching fail's/pruneBuiltImages's own
// cleanupCtx pattern, since this cleanup must happen even if the caller's
// request context is about to expire.
func (e *Engine) removeOrphanBuildImage(ctx context.Context, tag string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := e.rt.RemoveImage(cleanupCtx, tag, false); err != nil {
		fmt.Fprintf(e.log, "deploy: cleanup orphaned build image %s: %v\n", tag, err)
	}
}

// Rollback redeploys app to an earlier deployment's exact image:
// targetNumber must name an existing deployment (ErrRollbackTargetNotFound
// otherwise) whose Status is "healthy" (ErrRollbackTargetUnhealthy
// otherwise) — there is nothing else safe to roll back to.
//
// The target's image is resolved by reference (target.ImageRef), which
// works uniformly for both a locally-built tag
// ("localhost/basepod/<slug>:<n>") and a registry ref: if it's no longer
// present in the local image store, a registry ref is re-pulled (mirroring
// Deploy's own pull-then-rollout shape) but a local build tag fails
// outright with ErrRollbackImageMissing, since a "localhost/..." tag can
// never be fetched from anywhere else.
//
// On success this creates a new deployment row (source: the target's own
// Source, trigger_kind "rollback") and runs the normal rollout pipeline
// against it — so a rollback shows up in history as its own numbered
// deployment, not a mutation of the one it targets.
func (e *Engine) Rollback(ctx context.Context, app *store.App, targetNumber int) (*store.Deployment, error) {
	target, err := e.st.DeploymentByNumber(app.ID, targetNumber)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrRollbackTargetNotFound
		}
		return nil, fmt.Errorf("deploy: rollback: look up deployment %d: %w", targetNumber, err)
	}
	if target.Status != "healthy" {
		return nil, ErrRollbackTargetUnhealthy
	}

	image := target.ImageRef
	isLocalBuildTag := strings.HasPrefix(image, localImagePrefix)

	exists, err := e.rt.ImageExists(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("deploy: rollback: checking image %s: %w", image, err)
	}
	if !exists {
		if isLocalBuildTag {
			return nil, ErrRollbackImageMissing
		}
		if err := e.rt.PullImage(ctx, image); err != nil {
			return nil, fmt.Errorf("deploy: rollback: pull %s: %w", image, err)
		}
	}

	dep, err := e.st.CreateDeploymentFull(app.ID, image, target.Source, "rollback")
	if err != nil {
		return nil, fmt.Errorf("deploy: rollback: create deployment: %w", err)
	}
	if err := e.st.UpdateAppStatus(app.ID, "deploying"); err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: rollback: mark deploying: %w", err))
	}

	return e.runRollout(ctx, app, dep, image)
}

// runRollout is the shared back half of a deploy — create+start a new
// container, probe it, cut traffic over, and remove the previous
// container generation — used by both Deploy (after a successful pull)
// and DeployBuild (after a successful local build). It deliberately never
// pulls imageRef itself: Deploy pulls before calling this, and
// DeployBuild must not (see its doc comment) — a built image is already
// local.
func (e *Engine) runRollout(ctx context.Context, app *store.App, dep *store.Deployment, imageRef string) (*store.Deployment, error) {
	name := fmt.Sprintf("bp-%s-%d", app.Slug, dep.Number)
	spec := podman.CreateSpec{
		Name:  name,
		Image: imageRef,
		Labels: map[string]string{
			"basepod.managed":    "true",
			"basepod.app":        app.Slug,
			"basepod.deployment": strconv.Itoa(dep.Number),
		},
		NetworkName: caddy.NetworkName,
		// audit finding M1: the alias namespace ("app-<slug>") is
		// deliberately disjoint from the container-name namespace
		// ("bp-<slug>-<n>", see name above) — a slug containing digits
		// and hyphens (e.g. "foo-2") could otherwise make one app's
		// alias identical to a different app's container name, letting
		// Caddy deliver traffic into the wrong container. See
		// aliasScheme's own doc comment for how this interacts with
		// apps deployed before this fix existed.
		NetworkAliases: []string{aliasV2Prefix + app.Slug},
		RestartPolicy:  "always",
		// Resource limits (audit H2): every app container is created with
		// the app's stored limits, which default to a sane bounded value
		// for every new app (see store.CreateApp) and are retroactively
		// backfilled for pre-existing apps by migration
		// 00004_resource_limits.sql — so this is never accidentally
		// "unlimited" by omission. An admin can still explicitly set any
		// of the three to 0 via PATCH /api/v1/apps/{slug} to mean
		// unlimited (see podman.CreateSpec's doc comment), which
		// CreateContainer honors by omitting that field entirely.
		MemoryLimitBytes: app.MemoryLimitMB * 1024 * 1024,
		CPUQuota:         app.CPULimit,
		PidsLimit:        app.PidsLimit,
	}

	envVars, err := e.st.ListEnvVars(app.ID)
	if err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: list env vars: %w", err))
	}
	if len(envVars) > 0 {
		// A half-injected env must never ship silently: with no decrypt
		// func the caller has env injection disabled entirely, so refuse
		// to deploy an app that has configured env vars rather than
		// starting it with them silently missing.
		if e.decrypt == nil {
			return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: app has env vars but env injection is disabled"))
		}
		env := make(map[string]string, len(envVars))
		for _, ev := range envVars {
			plain, err := e.decrypt(app.ID, ev.Key, ev.ValueEncrypted)
			if err != nil {
				return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: decrypt env var %s: %w", ev.Key, err))
			}
			env[ev.Key] = plain
		}
		spec.Env = env
	}

	id, err := e.rt.CreateContainer(ctx, spec)
	if err != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: create container: %w", err))
	}

	if err := e.rt.StartContainer(ctx, id); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: start container: %w", err))
	}

	upstream := fmt.Sprintf("%s:%d", name, app.Port)
	if err := e.probeUntilUp(ctx, upstream); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: probe %s: %w", upstream, err))
	}

	if err := e.st.UpdateAppImage(app.ID, imageRef); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: update app image: %w", err))
	}
	// The container just created above carries the new "app-<slug>" alias
	// (see spec.NetworkAliases), so this app's alias_scheme must record
	// "v2" from here on — before Routes() is computed below, since
	// Routes() reads alias_scheme to decide which upstream to render (see
	// its doc comment) and must never render a stale "bp-<slug>" upstream
	// for a container that no longer carries that alias. A no-op (but
	// still executed) UPDATE for an app that was already "v2".
	if err := e.st.UpdateAppAliasScheme(app.ID, store.AliasSchemeV2); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: update alias scheme: %w", err))
	}
	// The new container just passed its health probe, so the app is
	// running from here on. Flip the store status to "running" before
	// computing Routes(): Routes() only includes apps whose store status
	// is "running", so this must happen before we call it or this
	// deploy's own route would be dropped from the set we apply below.
	if err := e.st.UpdateAppStatus(app.ID, "running"); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: mark running: %w", err))
	}

	if err := e.ApplyRoutes(ctx); err != nil {
		return e.fail(ctx, app, dep, id, err)
	}

	// Traffic is now on the new container's alias; only past this point
	// is it safe to tear down the previous generation.
	e.removeOldContainers(ctx, app.Slug, dep.Number)

	// Every successful rollout is a good point to prune old built images —
	// except when this deployment is registry-sourced ("image"): it can
	// never have produced a "localhost/basepod/<slug>:<n>" tag, so there's
	// nothing to prune and it's not worth the ListImageTags round trip.
	// imageRef (the image this rollout just cut traffic over to) is
	// threaded through as the protected "currently running" image — see
	// pruneBuiltImages's doc comment for why that matters.
	if dep.Source != "image" {
		e.pruneBuiltImages(ctx, app.Slug, imageRef)
		e.pruneBuildLogs(dep)
	}

	if err := e.st.FinishDeployment(dep.ID, "healthy", ""); err != nil {
		fmt.Fprintf(e.log, "deploy: finish deployment %d: %v\n", dep.ID, err)
	}
	dep.Status = "healthy"
	return dep, nil
}

// fail is the shared failure path for every step of Deploy after the
// deployment row exists: it best-effort removes the new (not-yet-live)
// container, marks the deployment failed, and restores the app's status
// to "running" if other (old) containers are still up for it, or "error"
// if this was the only container. It never touches old containers.
func (e *Engine) fail(ctx context.Context, app *store.App, dep *store.Deployment, newContainerID string, cause error) (*store.Deployment, error) {
	// Cleanup below must survive a cancelled/expired ctx: fail is reached
	// from mid-pull Ctrl-C or the deploy's own timeout expiring, and if we
	// used ctx directly every podman call here would fail instantly,
	// orphaning the just-created container still holding the live
	// bp-<slug> DNS alias. context.WithoutCancel (Go 1.21+) keeps ctx's
	// values but drops its cancellation/deadline; we still bound it with
	// our own timeout so a stuck podman call can't hang forever.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	if newContainerID != "" {
		if err := e.rt.RemoveContainer(cleanupCtx, newContainerID, true); err != nil {
			fmt.Fprintf(e.log, "deploy: cleanup failed container %s: %v\n", newContainerID, err)
		}
	}

	if err := e.st.FinishDeployment(dep.ID, "failed", cause.Error()); err != nil {
		fmt.Fprintf(e.log, "deploy: finish deployment %d: %v\n", dep.ID, err)
	}
	dep.Status = "failed"
	dep.Error = cause.Error()

	status := "error"
	containers, err := e.rt.ListContainers(cleanupCtx, map[string]string{"basepod.managed": "true", "basepod.app": app.Slug})
	if err != nil {
		fmt.Fprintf(e.log, "deploy: list containers for %s: %v\n", app.Slug, err)
	} else {
		keep := strconv.Itoa(dep.Number)
		for _, c := range containers {
			if c.Labels["basepod.deployment"] != keep {
				status = "running"
				break
			}
		}
	}
	if err := e.st.UpdateAppStatus(app.ID, status); err != nil {
		fmt.Fprintf(e.log, "deploy: update app status: %v\n", err)
	}

	return dep, cause
}

// removeOldContainers stops and removes every container labeled for app
// slug except the one from deployment keepNumber (the just-cut-over
// generation). Errors are logged and skipped rather than failing the
// deploy: by the time this runs, traffic has already moved to the new
// container, so a stray old container is a cleanup nuisance, not a
// correctness problem.
func (e *Engine) removeOldContainers(ctx context.Context, slug string, keepNumber int) {
	// Same reasoning as fail's cleanupCtx: this runs after traffic has
	// already cut over, so a cancelled/expired ctx must not stop us from
	// stopping/removing the previous generation — otherwise it's left
	// running as an orphan.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	containers, err := e.rt.ListContainers(cleanupCtx, map[string]string{"basepod.managed": "true", "basepod.app": slug})
	if err != nil {
		fmt.Fprintf(e.log, "deploy: list containers for %s: %v\n", slug, err)
		return
	}
	keep := strconv.Itoa(keepNumber)
	for _, c := range containers {
		if c.Labels["basepod.deployment"] == keep {
			continue
		}
		if err := e.rt.StopContainer(cleanupCtx, c.ID, 10); err != nil {
			fmt.Fprintf(e.log, "deploy: stop old container %s: %v\n", c.Name, err)
		}
		if err := e.rt.RemoveContainer(cleanupCtx, c.ID, false); err != nil {
			fmt.Fprintf(e.log, "deploy: remove old container %s: %v\n", c.Name, err)
		}
	}
}

// builtImageRepo returns the local image repository BasePod's build
// pipeline tags slug's built images under (see internal/build.Builder),
// without the ":<n>" tag suffix — the prefix pruneBuiltImages and
// Rollback's local-tag check both key off.
func builtImageRepo(slug string) string {
	return "localhost/basepod/" + slug
}

// pruneBuiltImages keeps the retainBuiltImages highest-numbered
// "localhost/basepod/<slug>:<n>" image tags and removes the rest, via
// e.rt.ListImageTags + RemoveImage. It's the back half of built-image
// retention, called after every successful rollout (see runRollout) so a
// long-lived app that redeploys frequently from tarball uploads doesn't
// accumulate an unbounded number of local image layers.
//
// currentImageRef — the image the app is actually running right now (the
// imageRef runRollout just cut traffic over to) — is always protected from
// removal, regardless of where it ranks numerically. This matters because
// a rollback can make an OLD, low-numbered tag the currently-running
// image: ranking purely by tag number would then treat that live image as
// one of the oldest and hand it straight to RemoveImage, deleting the
// image a running container still references. The protected set is
// therefore {currentImageRef} ∪ {the retainBuiltImages highest-numbered
// tags}, which can hold up to retainBuiltImages+1 entries when
// currentImageRef itself falls outside the numeric top N.
//
// Only tags whose suffix parses as a plain non-negative integer are
// considered "numbered" and eligible for this ordering; anything else
// (e.g. a stray "latest" tag) is left alone entirely — pruneBuiltImages
// has no way to know if it's still wanted, so removing it isn't safe to do
// automatically. A RemoveImage failure for one tag is logged and skipped
// rather than aborting the rest, mirroring removeOldContainers's
// best-effort teardown: by the time this runs, the deploy has already
// succeeded, so a stray old image is a disk-space nuisance, not a
// correctness problem.
//
// Runs on a context detached from ctx's cancellation (like
// removeOldContainers's cleanupCtx), since it happens after the deploy has
// already succeeded and must not be skipped just because the caller's
// request context is about to expire.
func (e *Engine) pruneBuiltImages(ctx context.Context, slug, currentImageRef string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	repo := builtImageRepo(slug)
	tags, err := e.rt.ListImageTags(cleanupCtx, repo)
	if err != nil {
		fmt.Fprintf(e.log, "deploy: retention: list image tags for %s: %v\n", repo, err)
		return
	}

	type numberedTag struct {
		number int
		tag    string
	}
	prefix := repo + ":"
	var numbered []numberedTag
	for _, tag := range tags {
		suffix, ok := strings.CutPrefix(tag, prefix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue // non-numeric tag (e.g. "latest") — never touched
		}
		numbered = append(numbered, numberedTag{number: n, tag: tag})
	}
	if len(numbered) <= retainBuiltImages {
		return
	}

	sort.Slice(numbered, func(i, j int) bool { return numbered[i].number > numbered[j].number })

	protected := map[string]bool{currentImageRef: true}
	for _, nt := range numbered[:retainBuiltImages] {
		protected[nt.tag] = true
	}

	for _, nt := range numbered[retainBuiltImages:] {
		if protected[nt.tag] {
			continue
		}
		if err := e.rt.RemoveImage(cleanupCtx, nt.tag, false); err != nil {
			fmt.Fprintf(e.log, "deploy: retention: remove image %s: %v\n", nt.tag, err)
		}
	}
}

// retainBuildLogs mirrors retainBuiltImages: how many of an app's build
// logs (<dataDir>/apps/<slug>/builds/<n>.log — see
// internal/build.Builder.LogPath) pruneBuildLogs keeps, newest-numbered
// first, after a successful rollout that ran a build. Kept equal to
// retainBuiltImages deliberately — a separate constant would just invite
// the two retention windows to drift apart for no real benefit, since
// every retained image tag has a corresponding log.
const retainBuildLogs = retainBuiltImages

// pruneBuildLogs removes build logs from dep's app's log directory whose
// deployment number falls outside the retainBuildLogs highest-numbered
// ones, mirroring pruneBuiltImages's own retention shape but for
// on-disk log files rather than local image tags.
//
// It derives the log directory from dep.BuildLogPath (set by DeployBuild
// via internal/build.Builder.LogPath — see its doc comment) rather than
// needing its own copy of the data directory or a *build.Builder
// reference: the directory containing any one deployment's log path is
// exactly the app's shared builds/ directory. A dep with no BuildLogPath
// (a registry-image deploy, or a rollback that reused an existing image
// without running a new build) never wrote a log file, so this is a
// no-op for it.
//
// dep.Number — the deployment that just finished, or is still in flight
// while its own log file is open for writing — is always protected from
// removal, regardless of where it ranks numerically, exactly like
// pruneBuiltImages's currentImageRef protection: this runs from
// runRollout only after dep's own build already completed, so "in
// flight" only matters in the sense that this must never be the call
// that deletes the log of the deployment it was invoked for.
//
// Every failure (listing the directory, removing one file) is logged and
// skipped rather than aborting, matching every other post-deploy cleanup
// step in this file: by the time this runs, the deploy has already
// succeeded, so a stray old log is a disk-space nuisance, not a
// correctness problem. A missing builds/ directory (e.g. a test fixture,
// or a log that was already cleaned up by an app deletion racing this
// call) is treated as "nothing to prune", not an error.
func (e *Engine) pruneBuildLogs(dep *store.Deployment) {
	if dep.BuildLogPath == "" {
		return
	}
	dir := filepath.Dir(dep.BuildLogPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(e.log, "deploy: retention: list build logs in %s: %v\n", dir, err)
		}
		return
	}

	type numberedLog struct {
		number int
		path   string
	}
	var numbered []numberedLog
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		suffix, ok := strings.CutSuffix(entry.Name(), ".log")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue // non-numeric log file name — never touched
		}
		numbered = append(numbered, numberedLog{number: n, path: filepath.Join(dir, entry.Name())})
	}
	if len(numbered) <= retainBuildLogs {
		return
	}

	sort.Slice(numbered, func(i, j int) bool { return numbered[i].number > numbered[j].number })

	protected := map[int]bool{dep.Number: true}
	for _, nl := range numbered[:retainBuildLogs] {
		protected[nl.number] = true
	}

	for _, nl := range numbered[retainBuildLogs:] {
		if protected[nl.number] {
			continue
		}
		if err := os.Remove(nl.path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(e.log, "deploy: retention: remove build log %s: %v\n", nl.path, err)
		}
	}
}

// probeUntilUp calls e.probe against upstream, retrying every
// probeInterval until it succeeds or probeAttempts total attempts have
// been made. It returns the last error on exhaustion, or the context's
// error if ctx is cancelled while waiting between attempts.
func (e *Engine) probeUntilUp(ctx context.Context, upstream string) error {
	attempts := e.probeAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		lastErr = e.probe(ctx, upstream)
		if lastErr == nil {
			return nil
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(e.probeInterval):
			}
		}
	}
	return lastErr
}

// RemoveApp stops and removes every container belonging to app, then
// re-applies the route set so its hostname is dropped from Caddy. It
// does not touch the store; the caller (API layer) owns deleting the
// app's row — which means RemoveApp typically runs while the app's
// store status is still "running" (handleDeleteApp calls RemoveApp
// before deleting the store row), so Routes() would otherwise still
// include this app. We explicitly drop app.Slug's route below rather
// than relying on caller ordering.
//
// Stop/remove is best-effort per container, mirroring
// removeOldContainers: a container that fails to stop or remove is
// logged and skipped rather than aborting the whole call, so one
// misbehaving container can never strand the rest, and — critically —
// can never skip the Routes()+router.Apply step below, which is what
// actually drops the app's stale route from Caddy. Only a failure to
// list containers in the first place (a precondition we can't work
// around) or a failure in Routes()/router.Apply itself is returned as
// an error.
func (e *Engine) RemoveApp(ctx context.Context, app *store.App) error {
	containers, err := e.rt.ListContainers(ctx, map[string]string{"basepod.managed": "true", "basepod.app": app.Slug})
	if err != nil {
		return fmt.Errorf("deploy: list containers for %s: %w", app.Slug, err)
	}

	// Teardown must survive a cancelled/expired ctx (see fail's
	// cleanupCtx comment) so a container isn't left running — and
	// holding the live bp-<slug> DNS alias — just because the caller's
	// request context ended first.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	for _, c := range containers {
		if err := e.rt.StopContainer(cleanupCtx, c.ID, 10); err != nil {
			fmt.Fprintf(e.log, "deploy: stop container %s: %v\n", c.Name, err)
		}
		if err := e.rt.RemoveContainer(cleanupCtx, c.ID, true); err != nil {
			fmt.Fprintf(e.log, "deploy: remove container %s: %v\n", c.Name, err)
		}
	}

	// See the routesMu field comment: this Routes()+filter+Apply sequence
	// must run as one atomic critical section relative to ApplyRoutes
	// (including Deploy's cutover, which calls it), so a concurrent
	// deploy's cutover can never read a Routes() snapshot from before
	// this removal, or write over the route set this call is about to
	// apply.
	e.routesMu.Lock()
	defer e.routesMu.Unlock()

	routes, err := e.Routes()
	if err != nil {
		return fmt.Errorf("deploy: compute routes: %w", err)
	}
	// Routes() includes every app whose store status is "running",
	// which this app still is at this point (the caller deletes the
	// store row only after RemoveApp returns). Drop its own route
	// explicitly so we never re-apply a route for the app we're in the
	// middle of removing, regardless of caller ordering.
	filtered := make([]caddy.AppRoute, 0, len(routes))
	for _, r := range routes {
		if r.Slug == app.Slug {
			continue
		}
		filtered = append(filtered, r)
	}
	if err := e.router.Apply(ctx, filtered, e.dashboard); err != nil {
		return fmt.Errorf("deploy: apply routes: %w", err)
	}
	return nil
}

// Routes builds the current route set: every app whose store status is
// "running", mapped to its stable alias upstream and a Hostnames list —
// the app's generated slug.rootDomain hostname first, followed by its
// custom domains (from ListAllDomains) sorted lexically. The result is
// not sorted by slug; caddy.Render sorts by slug itself.
//
// Each app's upstream is built from its own stored AliasScheme (audit
// finding M1) rather than unconditionally using the new "app-<slug>"
// alias: an app whose currently-running container was created before
// this fix (AliasScheme == store.AliasSchemeLegacy) still only answers on
// "bp-<slug>" — rendering "app-<slug>" for it would 502 every request
// until that app's next redeploy flips its AliasScheme to "v2" (see
// runRollout). This is what makes the migration safe for apps already
// running when this fix is deployed.
func (e *Engine) Routes() ([]caddy.AppRoute, error) {
	apps, err := e.st.ListApps()
	if err != nil {
		return nil, fmt.Errorf("deploy: list apps: %w", err)
	}
	domains, err := e.st.ListAllDomains()
	if err != nil {
		return nil, fmt.Errorf("deploy: list domains: %w", err)
	}
	// Group the single ListAllDomains query by app rather than issuing a
	// ListDomains query per app.
	customByApp := make(map[int64][]string, len(domains))
	for _, d := range domains {
		customByApp[d.AppID] = append(customByApp[d.AppID], d.Hostname)
	}

	var routes []caddy.AppRoute
	for _, a := range apps {
		if a.Status != "running" {
			continue
		}
		custom := customByApp[a.ID]
		sort.Strings(custom)
		hostnames := append([]string{a.Slug + "." + e.rootDomain}, custom...)
		aliasPrefix := aliasV2Prefix
		if a.AliasScheme == store.AliasSchemeLegacy {
			aliasPrefix = legacyAliasPrefix
		}
		routes = append(routes, caddy.AppRoute{
			Slug:      a.Slug,
			Hostnames: hostnames,
			Upstream:  fmt.Sprintf("%s%s:%d", aliasPrefix, a.Slug, a.Port),
		})
	}
	return routes, nil
}

// ApplyRoutes recomputes the current route set via Routes and pushes it
// to the router (Caddy). It's the shared Routes()+router.Apply sequence
// used by Deploy on every successful cutover, and is exported so the API
// layer can trigger a route refresh directly (e.g. after a domain is
// added or removed) without going through a deploy.
func (e *Engine) ApplyRoutes(ctx context.Context) error {
	// See the routesMu field comment: the DB read (Routes) and the apply
	// (router.Apply) must run as one atomic critical section relative to
	// RemoveApp's and any other ApplyRoutes call's own Routes()+Apply
	// pair.
	e.routesMu.Lock()
	defer e.routesMu.Unlock()

	routes, err := e.Routes()
	if err != nil {
		return fmt.Errorf("deploy: compute routes: %w", err)
	}
	if err := e.router.Apply(ctx, routes, e.dashboard); err != nil {
		return fmt.Errorf("deploy: apply routes: %w", err)
	}
	return nil
}

// AppLogs streams a running app's container logs. The returned
// ReadCloser is the raw, still-multiplexed stream straight from the
// runtime (see podman.DemuxLogs) — the caller owns closing it, which
// matters in particular for follow=true, whose stream never ends on its
// own.
//
// It looks the app up by slug — ErrNotFound passes through unchanged
// from the store lookup — then finds its running container via
// ListContainers filtered by the basepod.managed+basepod.app labels,
// picking the one with State=="running" (Deploy always tears down prior
// generations before returning, so at most one should ever be running).
// If none is running, it returns ErrNotRunning.
func (e *Engine) AppLogs(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
	app, err := e.st.AppBySlug(slug)
	if err != nil {
		return nil, err
	}

	containers, err := e.rt.ListContainers(ctx, map[string]string{"basepod.managed": "true", "basepod.app": app.Slug})
	if err != nil {
		return nil, fmt.Errorf("deploy: list containers for %s: %w", app.Slug, err)
	}

	var id string
	for _, c := range containers {
		if c.State == "running" {
			id = c.ID
			break
		}
	}
	if id == "" {
		return nil, ErrNotRunning
	}

	rc, err := e.rt.ContainerLogs(ctx, id, follow, tail)
	if err != nil {
		return nil, fmt.Errorf("deploy: logs for %s: %w", app.Slug, err)
	}
	return rc, nil
}

// orphanCleanupTimeout bounds CleanupOrphans's stop/remove work — it runs
// once at boot (see server.Run), detached from the caller's ctx like
// fail's/removeOldContainers's cleanupCtx (a slow boot racing a shutdown
// signal must not abandon a container mid-removal).
const orphanCleanupTimeout = 60 * time.Second

// CleanupOrphans removes containers left behind by a previous crash or
// interrupted deploy: a basepod.managed=true container whose
// basepod.app slug no longer has a matching app in the store (the app
// was deleted while its container survived), or whose
// basepod.deployment generation isn't the app's latest *healthy*
// deployment (a stale or never-finished generation from a cutover that
// never completed). bp-caddy is always skipped by name — it's labeled
// basepod.managed=true but has no basepod.app label and is a distinct
// lifecycle from app containers. Containers with basepod.managed=true
// but no basepod.app label are left alone with a warning: GC has no
// record of what they belong to, so removing them isn't safe to do
// automatically.
//
// Called once at boot (see server.Run), after the engine is constructed
// and before Caddy's route config is reconciled — an orphan surviving
// from a previous crash must never keep answering on a hostname (or
// port, via the container's DNS alias) a fresh deploy is about to reuse.
// Every failure along the way (listing containers, looking up an app,
// listing its deployments, stopping/removing a container) is logged via
// e.log and the loop continues rather than aborting: a partially-failing
// GC pass must never stop basepod from finishing boot. Only a failure to
// list containers in the first place (a precondition nothing else here
// can work around) is returned as an error.
func (e *Engine) CleanupOrphans(ctx context.Context) (int, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanCleanupTimeout)
	defer cancel()

	containers, err := e.rt.ListContainers(cleanupCtx, map[string]string{"basepod.managed": "true"})
	if err != nil {
		return 0, fmt.Errorf("deploy: orphan gc: list containers: %w", err)
	}

	removed := 0
	for _, c := range containers {
		if c.Name == caddy.ContainerName {
			continue
		}
		slug, ok := c.Labels["basepod.app"]
		if !ok {
			fmt.Fprintf(e.log, "deploy: orphan gc: %s is basepod.managed but has no basepod.app label — leaving it alone\n", c.Name)
			continue
		}

		app, err := e.st.AppBySlug(slug)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(e.log, "deploy: orphan gc: look up app %q: %v\n", slug, err)
				continue
			}
			fmt.Fprintf(e.log, "deploy: orphan gc: removing %s (app %q no longer exists)\n", c.Name, slug)
			e.removeOrphanContainer(cleanupCtx, c)
			removed++
			continue
		}

		latest, err := e.latestHealthyDeploymentNumber(app.ID)
		if err != nil {
			fmt.Fprintf(e.log, "deploy: orphan gc: list deployments for %q: %v\n", slug, err)
			continue
		}
		if latest == 0 {
			// The app exists but has never had a healthy deployment
			// (e.g. its only deploy is still in flight, or every past
			// attempt failed before going healthy) — nothing to compare
			// this container's generation against, so leave it alone.
			continue
		}
		if c.Labels["basepod.deployment"] != strconv.Itoa(latest) {
			fmt.Fprintf(e.log, "deploy: orphan gc: removing %s (deployment %s, latest healthy for %q is %d)\n",
				c.Name, c.Labels["basepod.deployment"], slug, latest)
			e.removeOrphanContainer(cleanupCtx, c)
			removed++
		}
	}
	return removed, nil
}

// removeOrphanContainer stops and force-removes a single orphaned
// container, logging (not failing on) either step — mirrors
// removeOldContainers's best-effort teardown, since by the time
// CleanupOrphans runs at boot there's no live traffic depending on the
// container staying reachable a moment longer.
func (e *Engine) removeOrphanContainer(ctx context.Context, c podman.ContainerInfo) {
	if err := e.rt.StopContainer(ctx, c.ID, 10); err != nil {
		fmt.Fprintf(e.log, "deploy: orphan gc: stop %s: %v\n", c.Name, err)
	}
	if err := e.rt.RemoveContainer(ctx, c.ID, true); err != nil {
		fmt.Fprintf(e.log, "deploy: orphan gc: remove %s: %v\n", c.Name, err)
	}
}

// stuckDeploymentError is the message FinishDeployment records for a
// deployment SweepStuckDeployments marks failed — see that function's doc
// comment.
const stuckDeploymentError = "basepod restarted while this deployment was still in progress"

// SweepStuckDeployments marks failed every deployment still "deploying" at
// boot whose (app, deployment-number) generation has no running container —
// i.e. a deploy whose background goroutine (DeployBuildAsync, or a
// synchronous Deploy/DeployBuild call) never reached a terminal status
// because the process died mid-deploy (a crash, or a hard kill past
// WaitForBackgroundDeploys's graceful-shutdown grace period) rather than
// finishing or being interrupted cleanly. Without this, such a row — and
// the app it belongs to, left "deploying" too — would stay stuck forever:
// nothing else in BasePod ever revisits a "deploying" row once the
// goroutine that owned it is gone.
//
// Called once at boot (see server.Run), right after CleanupOrphans: that
// ordering matters, since CleanupOrphans may itself remove a stuck
// deployment's container as an orphan (a container whose generation isn't
// the app's latest *healthy* deployment — which a still-"deploying" row
// never is) before this method ever checks whether one is running, so the
// container-existence check below reflects ground truth *after* orphan GC,
// not a possibly-stale view from before it.
//
// A deployment whose generation DOES have a running container (the rare
// case: the process died in the narrow window after the container passed
// its health probe but before FinishDeployment/UpdateAppStatus persisted,
// and CleanupOrphans didn't remove it) is left alone — the container is
// fine, and there is nothing to reconcile.
func (e *Engine) SweepStuckDeployments(ctx context.Context) (int, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanCleanupTimeout)
	defer cancel()

	deps, err := e.st.ListDeployingDeployments()
	if err != nil {
		return 0, fmt.Errorf("deploy: sweep stuck deployments: list: %w", err)
	}

	swept := 0
	for _, dep := range deps {
		app, err := e.st.AppByID(dep.AppID)
		if err != nil {
			fmt.Fprintf(e.log, "deploy: sweep stuck deployments: look up app %d for deployment %d: %v\n", dep.AppID, dep.ID, err)
			continue
		}

		containers, err := e.rt.ListContainers(cleanupCtx, map[string]string{
			"basepod.managed": "true", "basepod.app": app.Slug, "basepod.deployment": strconv.Itoa(dep.Number),
		})
		if err != nil {
			fmt.Fprintf(e.log, "deploy: sweep stuck deployments: list containers for %s: %v\n", app.Slug, err)
			continue
		}
		running := false
		for _, c := range containers {
			if c.State == "running" {
				running = true
				break
			}
		}
		if running {
			continue
		}

		if err := e.st.FinishDeployment(dep.ID, "failed", stuckDeploymentError); err != nil {
			fmt.Fprintf(e.log, "deploy: sweep stuck deployments: finish deployment %d: %v\n", dep.ID, err)
			continue
		}
		swept++

		// The app's own status must not be left "deploying" forever either
		// — restore it the same way fail() does for any other mid-deploy
		// failure: "running" if some other (older, still-healthy)
		// generation is up, "error" if nothing is.
		status := "error"
		appContainers, err := e.rt.ListContainers(cleanupCtx, map[string]string{"basepod.managed": "true", "basepod.app": app.Slug})
		if err != nil {
			fmt.Fprintf(e.log, "deploy: sweep stuck deployments: list containers for %s: %v\n", app.Slug, err)
		} else {
			for _, c := range appContainers {
				if c.State == "running" {
					status = "running"
					break
				}
			}
		}
		if err := e.st.UpdateAppStatus(app.ID, status); err != nil {
			fmt.Fprintf(e.log, "deploy: sweep stuck deployments: update app status for %s: %v\n", app.Slug, err)
		}
	}
	return swept, nil
}

// latestHealthyDeploymentNumber returns the Number of appID's most
// recent deployment with Status=="healthy". ListDeployments returns
// deployments newest-first, so this is just the first match; it returns
// 0 if appID has never had a healthy deployment.
func (e *Engine) latestHealthyDeploymentNumber(appID int64) (int, error) {
	deps, err := e.st.ListDeployments(appID)
	if err != nil {
		return 0, err
	}
	for _, d := range deps {
		if d.Status == "healthy" {
			return d.Number, nil
		}
	}
	return 0, nil
}

// CaddyProber returns a Prober that checks an upstream by running a
// wget --spider request inside the bp-caddy container (which shares the
// basepod network with every app container, and ships busybox wget in
// its alpine base image). Only HTTP(S) apps can be probed this way;
// non-HTTP TCP apps are out of scope for v0.1.
//
// wget exit status 8 means "the server returned an HTTP error response"
// (e.g. 404, 500) — the upstream answered, so the app is up even though
// the probed path isn't a success page — so it is treated as success
// alongside exit status 0. caddy.Execer returns an error for any
// nonzero exit; the production Execer (caddy.PodmanExec) wraps the
// underlying *exec.ExitError with %w (preserving the unwrap chain), so
// errors.As recovers the exit code even through that wrapping. As a
// belt-and-suspenders fallback for any other Execer implementation that
// returns a plain, unwrapped error, a string match for "exit status 8"
// is also accepted.
func CaddyProber(exec caddy.Execer) Prober {
	return func(ctx context.Context, upstream string) error {
		err := exec(ctx, caddy.ContainerName, "wget", "-q", "-T", "2", "--spider", "http://"+upstream+"/")
		if err == nil {
			return nil
		}
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 8 {
			return nil
		}
		if strings.Contains(err.Error(), "exit status 8") {
			return nil
		}
		return err
	}
}
