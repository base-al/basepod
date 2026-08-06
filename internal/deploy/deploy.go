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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/caddy"
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

	// decrypt turns an EnvVar.ValueEncrypted into its plaintext value.
	// nil disables env injection entirely: Deploy fails rather than ship
	// a container silently missing configured env vars (see Deploy).
	decrypt func(string) (string, error)

	// probeInterval and probeAttempts control the health-probe retry
	// loop; they default to 1s/30 (New) and are unexported so tests can
	// shrink them to keep the suite fast.
	probeInterval time.Duration
	probeAttempts int

	// log receives best-effort teardown/bookkeeping errors that must not
	// fail an otherwise-successful (or already-failed) deploy.
	log io.Writer
}

// New builds an Engine. rootDomain is appended to an app's slug to form
// its hostname (e.g. rootDomain "apps.example.com" -> "blog.apps.example.com").
// decrypt turns a stored EnvVar's encrypted value into plaintext; pass nil
// to disable env injection (Deploy then fails rather than silently
// omitting configured env vars). dashboard is the dashboard route (or nil
// to disable it) every router.Apply call made through this Engine carries
// alongside the app route set — see the dashboard field's doc comment.
func New(st *store.Store, rt Runtime, router Router, probe Prober, rootDomain string, decrypt func(string) (string, error), dashboard *caddy.DashboardRoute) *Engine {
	return &Engine{
		st:            st,
		rt:            rt,
		router:        router,
		probe:         probe,
		rootDomain:    rootDomain,
		decrypt:       decrypt,
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
// builder.Build to decompress, validate, and build it into a local image
// tag.
//
// On a build failure, this goes through the same fail() path Deploy uses
// for every other failure: the deployment is marked failed and the app's
// status is restored (to "running" if other containers are still up for
// it, "error" otherwise) — no container was ever created for a build
// failure, so fail() is called with an empty newContainerID.
//
// On a successful build, the image tag is recorded on the deployment and
// rollout proceeds via the same runRollout Deploy uses — but, critically,
// skipping Deploy's PullImage step: builder.Build produces a local
// "localhost/basepod/..." tag, and pulling a localhost/ tag from a
// registry would either fail outright or (worse) silently pull the wrong
// thing.
//
// If the build succeeds but the rollout that follows it fails (a failed
// probe, a container that won't start, ...), the freshly-built tag never
// becomes a running deployment's image and is removed as an orphan (see
// removeOrphanBuildImage) — runRollout's own retention (pruneBuiltImages)
// only prunes on a *successful* rollout, so without this a string of
// repeated failed builds would leave one dead tag behind per attempt,
// forever.
func (e *Engine) DeployBuild(ctx context.Context, app *store.App, gzTar io.Reader, builder *build.Builder) (*store.Deployment, error) {
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

	tag, _, buildErr := builder.Build(ctx, app.Slug, dep.Number, gzTar)
	if buildErr != nil {
		return e.fail(ctx, app, dep, "", fmt.Errorf("deploy: build: %w", buildErr))
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
		NetworkName:    caddy.NetworkName,
		NetworkAliases: []string{"bp-" + app.Slug},
		RestartPolicy:  "always",
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
			plain, err := e.decrypt(ev.ValueEncrypted)
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
		routes = append(routes, caddy.AppRoute{
			Slug:      a.Slug,
			Hostnames: hostnames,
			Upstream:  fmt.Sprintf("bp-%s:%d", a.Slug, a.Port),
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
