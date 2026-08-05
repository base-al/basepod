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
	"strconv"
	"strings"
	"time"

	"github.com/base-al/basepod/internal/caddy"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// Engine drives app deployments: pull, create, start, probe, cut traffic
// over, and tear down the previous container generation.
type Engine struct {
	st         *store.Store
	rt         Runtime
	router     Router
	probe      Prober
	rootDomain string

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
func New(st *store.Store, rt Runtime, router Router, probe Prober, rootDomain string) *Engine {
	return &Engine{
		st:            st,
		rt:            rt,
		router:        router,
		probe:         probe,
		rootDomain:    rootDomain,
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

	routes, err := e.Routes()
	if err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: compute routes: %w", err))
	}
	if err := e.router.Apply(ctx, routes); err != nil {
		return e.fail(ctx, app, dep, id, fmt.Errorf("deploy: apply routes: %w", err))
	}

	// Traffic is now on the new container's alias; only past this point
	// is it safe to tear down the previous generation.
	e.removeOldContainers(ctx, app.Slug, dep.Number)

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
	if err := e.router.Apply(ctx, filtered); err != nil {
		return fmt.Errorf("deploy: apply routes: %w", err)
	}
	return nil
}

// Routes builds the current route set: every app whose store status is
// "running", mapped to its stable alias upstream. The result is not
// sorted; caddy.Render sorts by slug itself.
func (e *Engine) Routes() ([]caddy.AppRoute, error) {
	apps, err := e.st.ListApps()
	if err != nil {
		return nil, fmt.Errorf("deploy: list apps: %w", err)
	}
	var routes []caddy.AppRoute
	for _, a := range apps {
		if a.Status != "running" {
			continue
		}
		routes = append(routes, caddy.AppRoute{
			Slug:     a.Slug,
			Hostname: a.Slug + "." + e.rootDomain,
			Upstream: fmt.Sprintf("bp-%s:%d", a.Slug, a.Port),
		})
	}
	return routes, nil
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
