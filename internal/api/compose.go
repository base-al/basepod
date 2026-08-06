// v0.5 Task 8: compose apply — the HTTP endpoint and orchestration that
// turns an uploaded compose.yaml (plus any per-service build contexts) into
// one BasePod app per service, deployed in dependency order.
//
// Request shape (decided here, since the milestone plan left it open):
// POST /api/v1/compose/up?project=<name>&dry_run=1|0, body a gzipped tar
// (Content-Type application/gzip or application/x-gzip — the exact same
// convention handleDeployTarball already uses) carrying compose.yaml (or
// compose.yml, or docker-compose.yml) at its root, plus zero or more
// services' own build contexts as subdirectories. A JSON envelope was
// rejected: it would force embedding either a whole multi-file build
// context or a base64 blob inside JSON, mangling exactly the bytes a
// `build:` service's context needs byte-for-byte; a bare multipart form
// was rejected too, since a tar is already the format `basepod compose
// up`/`basepod deploy` pack build contexts into, and gzip-tar-as-body is
// the format the existing tarball-deploy route already established as
// BasePod's convention for "upload a build context, get a deploy" — this
// keeps one convention instead of two. `project` is a query parameter
// (not a form field) so a client can construct the request without
// touching the tar at all; it falls back to the compose file's own
// top-level `name:` when omitted, and 422s only if neither is present.
//
// Idempotence and orphans: re-applying the same project updates the
// existing apps for its (project, service) pairs (see
// compose.BuildPlan's Action field) rather than erroring or duplicating.
// A service that existed on a prior apply but is absent from the file
// being applied now is never deleted automatically — deleting a user's
// running workload as a side effect of an unrelated apply is not
// acceptable (the same stance store.DeleteApp's volumes and
// handleDeleteApp's data-dir cleanup already take toward user data) — it
// is reported back as an "orphan" (see composePlanResponse.Orphans) and
// left running exactly as it was. An operator who actually wants it gone
// deletes that one app explicitly (DELETE /api/v1/apps/{slug}); a
// project-wide "delete everything no longer in the file" convenience is
// left to the dashboard's compose UI (Task 10), issuing the individual
// deletes itself.
//
// Partial failure: every service's deployment row is created
// synchronously, in dependency order, before the 202 response is ever
// written (see applyComposePlan) — so the response's per-service
// deployment_number is real and immediately pollable
// (GET /apps/{slug}/deployments/{n}), not a prediction. The actual
// build/deploy work then runs on one background goroutine
// (runComposeOrchestration) that walks the services in that same order and
// stops attempting new work on the first failure: every earlier service
// that already went healthy is left running untouched (no automatic
// rollback of healthy services — the same trade-off DeployStrategyReplace
// documents elsewhere: BasePod never guesses at a "safe" corrective action
// on a partial failure), and every later, not-yet-attempted service's
// already-created deployment row is finished "failed" with
// "aborted: <service> failed", so GET .../deployments/{n} tells the whole
// story for every service without the caller having to infer anything
// from silence.
package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/base-al/basepod/internal/compose"
	"github.com/base-al/basepod/internal/store"
	"github.com/base-al/basepod/internal/tarpack"
)

// maxComposeBody reuses the tarball-deploy upload cap: a compose upload is
// a gzipped tar too, just carrying compose.yaml plus per-service build
// contexts instead of one app's.
const maxComposeBody = maxTarballBody

// maxComposeDecompressed bounds the total decompressed size of a compose
// upload's extracted files — mirrors internal/build.Builder's own
// ErrContextTooLarge guard against a small compressed upload expanding
// into an enormous one (a "gzip bomb"), reimplemented locally since that
// guard is scoped to a single build context inside internal/build, not a
// whole multi-service compose upload here.
const maxComposeDecompressed = 1 << 30 // 1 GiB

// composeOrchestrationTimeout bounds a background compose orchestration
// run (runComposeOrchestration): generous enough to cover several
// services' builds run sequentially, on a context detached from the
// triggering HTTP request the same way DeployBuildAsync's own background
// goroutine is (see deploy.Engine.DeployBuildAsync's doc comment) — that
// request's context dies the moment the 202 response is written, long
// before a multi-service apply can finish.
const composeOrchestrationTimeout = 2 * time.Hour

// composeServiceResponse is one service's entry in composePlanResponse —
// shared by the dry-run preview and the real apply response so a client
// (the CLI, the dashboard's compose preview — Task 10) doesn't need two
// separate shapes to render. DeploymentNumber is 0 (omitted) for a
// dry-run entry, since nothing was created.
//
// Image, Build, Volumes, and EnvKeys exist so a reviewer looking at a
// dry-run preview can tell what a service will actually run — the image
// ref, its build context (when it's a `build:` service rather than an
// `image:` one), and its named volumes — without those fields, the
// preview couldn't answer "is this pulling the image I expect, is my
// database volume attached?" at all (issue #12). EnvKeys deliberately
// carries only the compose file's env var *keys*, never their values:
// env values may be secrets, and every other surface in this product
// (env vars themselves, the git deploy token, GET .../apps/{slug}/env)
// treats a value as write-only once it could be sensitive — a compose
// preview leaking a value straight from the uploaded file would be the
// one place that promise didn't hold.
type composeServiceResponse struct {
	Name             string                         `json:"name"`
	Slug             string                         `json:"slug"`
	Action           string                         `json:"action"`
	Internal         bool                           `json:"internal"`
	Port             int                            `json:"port"`
	Alias            string                         `json:"alias"`
	Image            string                         `json:"image,omitempty"`
	Build            *composeServiceBuildResponse   `json:"build,omitempty"`
	Volumes          []composeServiceVolumeResponse `json:"volumes,omitempty"`
	EnvKeys          []string                       `json:"env_keys,omitempty"`
	DeployStrategy   string                         `json:"deploy_strategy,omitempty"`
	DeploymentNumber int                            `json:"deployment_number,omitempty"`
	Warnings         []string                       `json:"warnings,omitempty"`
}

// composeServiceBuildResponse is a service's `build:` block, present in
// composeServiceResponse only for a build service (Build == nil for a
// plain `image:` service) — Context is never empty when this is
// non-nil (see compose.BuildRef's doc comment); Dockerfile is "" unless
// the compose file named a custom one.
type composeServiceBuildResponse struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// composeServiceVolumeResponse is one named-volume mount: the volume's
// name and the container path it's mounted at — enough for a reviewer to
// check "is my database volume attached?" without exposing anything
// sensitive (a volume name and mount path are never secret).
type composeServiceVolumeResponse struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// composeServiceBuild converts a compose.BuildRef into the wire shape,
// nil in ⇄ nil out.
func composeServiceBuild(b *compose.BuildRef) *composeServiceBuildResponse {
	if b == nil {
		return nil
	}
	return &composeServiceBuildResponse{Context: b.Context, Dockerfile: b.Dockerfile}
}

// composeServiceVolumesResponse converts a service's volumes into the
// wire shape, nil for a volume-less service (so the JSON field is
// omitted rather than an empty array).
func composeServiceVolumesResponse(volumes []compose.VolumeRef) []composeServiceVolumeResponse {
	if len(volumes) == 0 {
		return nil
	}
	out := make([]composeServiceVolumeResponse, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, composeServiceVolumeResponse{Name: v.Name, Path: v.Target})
	}
	return out
}

// composeServiceEnvKeys returns env's keys, sorted, and NEVER its
// values — see composeServiceResponse's doc comment on why a value must
// never reach this response. nil for an env-less service (omitted JSON
// field rather than an empty array).
func composeServiceEnvKeys(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// composePlanResponse is handleComposeUp's response body for both a
// dry run (200, nothing changed) and a real apply (202, per-service
// deployments queued in dependency order).
type composePlanResponse struct {
	Project  string                   `json:"project"`
	DryRun   bool                     `json:"dry_run"`
	Services []composeServiceResponse `json:"services"`
	// Orphans lists the slugs of apps that belong to this compose
	// project (from a prior apply) but have no corresponding service in
	// the file being applied now — see this file's package doc comment
	// on why they are reported, not deleted.
	Orphans  []string `json:"orphans,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// isTruthy reports whether a query-parameter value should be treated as
// boolean true — used only for ?dry_run=.
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// handleComposeUp implements POST /api/v1/compose/up — see this file's
// package doc comment for the full request/response contract.
func (a *api) handleComposeUp(w http.ResponseWriter, r *http.Request) {
	if !gzipContentTypes[r.Header.Get("Content-Type")] {
		writeError(w, http.StatusBadRequest, "invalid_content_type", "Content-Type must be application/gzip")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxComposeBody)

	dir, err := extractComposeUpload(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "invalid_upload", err.Error())
		return
	}
	// Ownership of dir past this point: every early-return below removes
	// it itself; the last one (a successful apply) hands it to the
	// background goroutine, which removes it when done.

	composePath, err := findComposeFile(dir)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusUnprocessableEntity, "no_compose_file", err.Error())
		return
	}

	f, err := os.Open(composePath)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "internal", "failed to read compose file")
		return
	}
	file, _, parseErr := compose.Parse(f)
	f.Close()
	if parseErr != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusUnprocessableEntity, "compose_parse_error", parseErr.Error())
		return
	}

	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		project = file.Project
	}
	if project == "" {
		os.RemoveAll(dir)
		writeError(w, http.StatusUnprocessableEntity, "validation",
			"project name is required: pass ?project=<name> or set a top-level `name:` in the compose file")
		return
	}

	planCtx, err := a.buildComposePlanContext(project)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "internal", "failed to load existing compose state")
		return
	}

	plan, planErr := compose.BuildPlan(file, project, planCtx)
	if planErr != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusUnprocessableEntity, "compose_plan_error", planErr.Error())
		return
	}

	orphans, err := a.composeOrphans(project, plan)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "internal", "failed to compute orphaned services")
		return
	}

	if isTruthy(r.URL.Query().Get("dry_run")) {
		os.RemoveAll(dir)
		writeJSON(w, http.StatusOK, composePlanResponse{
			Project:  plan.Project,
			DryRun:   true,
			Services: dryRunServiceResponses(plan),
			Orphans:  orphans,
			Warnings: plan.Warnings,
		})
		return
	}

	items, applyErr := a.applyComposePlan(project, plan)
	if applyErr != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "internal", applyErr.Error())
		return
	}

	resp := composePlanResponse{Project: plan.Project, Orphans: orphans, Warnings: plan.Warnings}
	resp.Services = make([]composeServiceResponse, 0, len(items))
	for _, it := range items {
		resp.Services = append(resp.Services, it.response)
	}

	a.composeWG.Add(1)
	go func() {
		defer a.composeWG.Done()
		defer os.RemoveAll(dir)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), composeOrchestrationTimeout)
		defer cancel()
		a.runComposeOrchestration(ctx, dir, items)
	}()

	writeJSON(w, http.StatusAccepted, resp)
}

// dryRunServiceResponses builds a preview response from plan without
// touching the store — every field a dry run can know without creating
// anything: the recommended strategy and its reason are surfaced as an
// extra warning, same as a real apply does, so the preview and the apply
// response read the same way.
func dryRunServiceResponses(plan *compose.Plan) []composeServiceResponse {
	out := make([]composeServiceResponse, 0, len(plan.Services))
	for _, sp := range plan.Services {
		warnings := append([]string(nil), sp.Warnings...)
		if sp.RecommendedStrategy != "" {
			warnings = append(warnings, sp.StrategyReason)
		}
		out = append(out, composeServiceResponse{
			Name: sp.Name, Slug: sp.Slug, Action: sp.Action, Internal: sp.Internal,
			Port: sp.Port, Alias: sp.Alias, DeployStrategy: sp.RecommendedStrategy,
			Image: sp.Image, Build: composeServiceBuild(sp.Build),
			Volumes: composeServiceVolumesResponse(sp.Volumes), EnvKeys: composeServiceEnvKeys(sp.Env),
			Warnings: warnings,
		})
	}
	return out
}

// buildComposePlanContext assembles compose.PlanContext from the store —
// see internal/compose's own doc comment on PlanContext for what each
// field means to BuildPlan.
func (a *api) buildComposePlanContext(project string) (compose.PlanContext, error) {
	existingApps, err := a.st.ListAppsByComposeProject(project)
	if err != nil {
		return compose.PlanContext{}, err
	}
	allComposeApps, err := a.st.ListComposeApps()
	if err != nil {
		return compose.PlanContext{}, err
	}

	ctx := compose.PlanContext{
		ExistingApps: make(map[compose.ServiceKey]compose.ExistingApp, len(existingApps)),
		TakenAliases: make(map[string]string, len(allComposeApps)),
	}
	for _, app := range existingApps {
		ctx.ExistingApps[compose.ServiceKey{Project: project, Service: app.ComposeService}] = compose.ExistingApp{
			Slug: app.Slug, Alias: app.ComposeService,
		}
	}
	// Every compose-managed app across every project claims its bare
	// service name as a network alias (see internal/deploy's runRollout);
	// this is the full set BuildPlan's alias-collision check runs
	// against — including this project's own prior aliases, which
	// BuildPlan recognizes as self-owned via ExistingApps rather than by
	// inspecting this map's description text (see compose.PlanContext's
	// doc comment).
	for _, app := range allComposeApps {
		ctx.TakenAliases[app.ComposeService] = fmt.Sprintf("compose project %q, service %q", app.ComposeProject, app.ComposeService)
	}
	return ctx, nil
}

// composeOrphans returns the slugs (sorted) of apps that belong to
// project (from a prior compose apply) but have no corresponding service
// in plan — see this file's package doc comment on why they're reported,
// not deleted.
func (a *api) composeOrphans(project string, plan *compose.Plan) ([]string, error) {
	existingApps, err := a.st.ListAppsByComposeProject(project)
	if err != nil {
		return nil, err
	}
	inPlan := make(map[string]bool, len(plan.Services))
	for _, sp := range plan.Services {
		inPlan[sp.Name] = true
	}
	var orphans []string
	for _, app := range existingApps {
		if !inPlan[app.ComposeService] {
			orphans = append(orphans, app.Slug)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// composeOrchestrationItem carries one service's already-created app row
// and deployment row (see applyComposePlan) from the synchronous apply
// step into the background orchestrator (runComposeOrchestration).
type composeOrchestrationItem struct {
	service  compose.ServicePlan
	app      *store.App
	dep      *store.Deployment
	response composeServiceResponse
}

// applyComposePlan is the synchronous half of a real (non-dry-run) apply:
// for every service, in plan's dependency order, create or update its app
// row, apply its volumes/deploy-strategy/env, and create its deployment
// row (via the store directly, not through Deployer — see this file's
// package doc comment on why the row must exist before the HTTP response
// is written). It does no build/deploy I/O itself — that's
// runComposeOrchestration's job, run later on a background goroutine.
//
// An error here aborts the WHOLE apply (returns before any deployment row
// exists for services after the failing one) — unlike a build/deploy
// failure later in the orchestrator, a failure at this stage (a store
// write erroring) means BasePod itself is unhealthy, not that one
// service's container failed, so partial bookkeeping isn't something a
// caller can usefully act on service-by-service.
func (a *api) applyComposePlan(project string, plan *compose.Plan) ([]composeOrchestrationItem, error) {
	items := make([]composeOrchestrationItem, 0, len(plan.Services))
	for _, sp := range plan.Services {
		app, err := a.upsertComposeApp(project, sp)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", sp.Name, err)
		}

		warnings := append([]string(nil), sp.Warnings...)

		for _, v := range sp.Volumes {
			if _, err := a.st.UpsertVolume(app.ID, v.Name, v.Target); err != nil {
				return nil, fmt.Errorf("service %q: declare volume %q: %w", sp.Name, v.Name, err)
			}
		}

		// Volume-bearing services get deploy_strategy=replace applied
		// automatically (Task 6's rationale: a zero-downtime overlap
		// would give two containers write access to the same named
		// volume) — surfaced back to the caller as a warning naming why,
		// not silently applied.
		if sp.RecommendedStrategy != "" && app.DeployStrategy != sp.RecommendedStrategy {
			if err := a.st.UpdateAppDeployStrategy(app.ID, sp.RecommendedStrategy); err != nil {
				return nil, fmt.Errorf("service %q: set deploy strategy: %w", sp.Name, err)
			}
			app.DeployStrategy = sp.RecommendedStrategy
			warnings = append(warnings, sp.StrategyReason)
		}

		envWarnings, err := a.applyComposeEnv(app.ID, sp.Env)
		if err != nil {
			return nil, fmt.Errorf("service %q: apply env: %w", sp.Name, err)
		}
		warnings = append(warnings, envWarnings...)

		source := "image"
		initialImage := sp.Image
		if sp.Build != nil {
			// Mirrors DeployBuild's own contract: the image tag isn't
			// known until the build actually runs.
			source = "tarball"
			initialImage = ""
		}
		dep, err := a.st.CreateDeploymentFull(app.ID, initialImage, source, "compose")
		if err != nil {
			return nil, fmt.Errorf("service %q: create deployment: %w", sp.Name, err)
		}

		items = append(items, composeOrchestrationItem{
			service: sp, app: app, dep: dep,
			response: composeServiceResponse{
				Name: sp.Name, Slug: sp.Slug, Action: sp.Action, Internal: sp.Internal,
				Port: sp.Port, Alias: sp.Alias, DeployStrategy: app.DeployStrategy,
				Image: sp.Image, Build: composeServiceBuild(sp.Build),
				Volumes: composeServiceVolumesResponse(sp.Volumes), EnvKeys: composeServiceEnvKeys(sp.Env),
				DeploymentNumber: dep.Number, Warnings: warnings,
			},
		})
	}
	return items, nil
}

// upsertComposeApp creates a fresh app for sp (Action == "create") or
// updates the existing one's port/internal flag in place (Action ==
// "update" — its slug, image, and compose_project/compose_service never
// change across a re-apply; only its container-affecting config can).
func (a *api) upsertComposeApp(project string, sp compose.ServicePlan) (*store.App, error) {
	if sp.Action == "update" {
		app, err := a.st.AppBySlug(sp.Slug)
		if err != nil {
			return nil, err
		}
		if err := a.st.UpdateAppPort(app.ID, sp.Port); err != nil {
			return nil, err
		}
		if err := a.st.UpdateAppInternal(app.ID, sp.Internal); err != nil {
			return nil, err
		}
		app.Port = sp.Port
		app.Internal = sp.Internal
		return app, nil
	}
	return a.st.CreateComposeApp(sp.Slug, sp.Image, sp.Port, sp.Internal, project, sp.Name)
}

// applyComposeEnv upserts env's non-secret values onto appID, skipping
// (with a warning naming the key) any key that's already stored as a
// secret — a compose env value must never silently overwrite a secret an
// operator configured through the dashboard/API (doc 06's ruling,
// mirrored from the milestone brief).
func (a *api) applyComposeEnv(appID int64, env map[string]string) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	existing, err := a.st.ListEnvVars(appID)
	if err != nil {
		return nil, err
	}
	secretKeys := make(map[string]bool, len(existing))
	for _, ev := range existing {
		if ev.IsSecret {
			secretKeys[ev.Key] = true
		}
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var warnings []string
	for _, k := range keys {
		if secretKeys[k] {
			warnings = append(warnings, fmt.Sprintf("env var %q is set as a secret outside compose; not overwritten", k))
			continue
		}
		sealed, err := a.seal(appID, k, env[k])
		if err != nil {
			return nil, fmt.Errorf("seal env var %s: %w", k, err)
		}
		if err := a.st.UpsertEnvVar(appID, k, sealed, false); err != nil {
			return nil, fmt.Errorf("store env var %s: %w", k, err)
		}
	}
	return warnings, nil
}

// runComposeOrchestration is the background half of a real apply: it
// walks items in dependency order and, for each, either drives an actual
// deploy (image pull or build+rollout) or — once an earlier service has
// failed — finishes that service's already-created deployment row
// "failed" without attempting anything, naming which earlier service
// caused the abort. See this file's package doc comment for the full
// partial-failure contract: earlier successes are never rolled back,
// later services are never started.
func (a *api) runComposeOrchestration(ctx context.Context, uploadDir string, items []composeOrchestrationItem) {
	abortedBy := ""
	for _, it := range items {
		if abortedBy != "" {
			_ = a.st.FinishDeployment(it.dep.ID, "failed", fmt.Sprintf("aborted: %s failed", abortedBy))
			continue
		}

		var err error
		if it.service.Build != nil {
			err = a.deployComposeBuildService(ctx, uploadDir, it)
		} else {
			_, err = a.dep.DeployExisting(ctx, it.app, it.dep, it.service.Image)
		}
		if err != nil {
			abortedBy = it.service.Name
		}
	}
}

// deployComposeBuildService repacks the service's build context — a
// subtree of uploadDir named by it.service.Build.Context — via
// tarpack.PackDir (the same packer git deploys and `basepod deploy` use)
// into a fresh gzipped tar, then runs it through
// Deployer.DeployBuildExisting.
//
// compose's build.dockerfile (a custom Dockerfile name/path) is
// accommodated by copying that file to "Containerfile" at the context
// root before packing: the shared build pipeline (internal/build) only
// ever looks for "Containerfile" or "Dockerfile" verbatim at a build
// context's root, and this package must not modify internal/build to
// teach it a third name.
func (a *api) deployComposeBuildService(ctx context.Context, uploadDir string, it composeOrchestrationItem) error {
	contextDir := filepath.Join(uploadDir, filepath.FromSlash(it.service.Build.Context))

	if dockerfile := it.service.Build.Dockerfile; dockerfile != "" && dockerfile != "Containerfile" && dockerfile != "Dockerfile" {
		src := filepath.Join(contextDir, filepath.FromSlash(dockerfile))
		if err := copyFile(src, filepath.Join(contextDir, "Containerfile")); err != nil {
			return a.markComposeServiceFailed(it.app, it.dep,
				fmt.Errorf("service %q: custom dockerfile %q: %w", it.service.Name, dockerfile, err))
		}
	}

	f, _, err := tarpack.PackToTempFile(contextDir)
	if err != nil {
		return a.markComposeServiceFailed(it.app, it.dep,
			fmt.Errorf("service %q: pack build context %q: %w", it.service.Name, it.service.Build.Context, err))
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	_, err = a.dep.DeployBuildExisting(ctx, it.app, it.dep, f, a.builder)
	return err
}

// markComposeServiceFailed finishes dep "failed" and, if app never had a
// running container to begin with (a brand-new compose service whose
// very first attempt failed before ever reaching the Deployer — e.g. a
// missing build context in the upload), marks it "error" — mirroring
// deploy.Engine's own fail() for the cases that DO reach the Deployer. An
// UPDATE to a previously-running app is left alone: its old container,
// if any, is presumably still up and serving traffic.
func (a *api) markComposeServiceFailed(app *store.App, dep *store.Deployment, cause error) error {
	_ = a.st.FinishDeployment(dep.ID, "failed", cause.Error())
	if app.Status == "created" || app.Status == "" {
		_ = a.st.UpdateAppStatus(app.ID, "error")
	}
	return cause
}

// copyFile copies src to dst, overwriting dst if it exists.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// findComposeFile looks for compose.yaml, then compose.yml, then
// docker-compose.yml (compose's own documented precedence) as a regular
// file at dir's root.
func findComposeFile(dir string) (string, error) {
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yml"} {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose.yaml, compose.yml, or docker-compose.yml found at the root of the upload")
}

// extractComposeUpload decompresses and extracts gzTar into a fresh
// temporary directory, returning its path. The caller owns removing it
// (os.RemoveAll) once done.
//
// Entries are validated the same way internal/build's (unexported)
// safeTarPath guards a single build context — no absolute path, no ".."
// traversal segment — reimplemented locally (composeSafeTarPath) rather
// than reaching into that package's unexported helper for one shared
// rule (matching internal/compose's own stated policy of not reaching
// into sibling packages for small duplicated rules). Total decompressed
// size is bounded by maxComposeDecompressed, mirroring
// internal/build.Builder's own gzip-bomb guard.
func extractComposeUpload(gzTar io.Reader) (dir string, err error) {
	dir, err = os.MkdirTemp("", "basepod-compose-*")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()

	gz, err := gzip.NewReader(gzTar)
	if err != nil {
		return "", fmt.Errorf("upload is not valid gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(terr, &maxErr) {
				return "", terr
			}
			return "", fmt.Errorf("reading tar: %w", terr)
		}
		if !composeSafeTarPath(hdr.Name) {
			return "", fmt.Errorf("tar contains an entry with an unsafe path: %q", hdr.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			wf, ferr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if ferr != nil {
				return "", ferr
			}
			remaining := maxComposeDecompressed - total + 1
			n, cerr := io.Copy(wf, io.LimitReader(tr, remaining))
			wf.Close()
			if cerr != nil {
				return "", cerr
			}
			total += n
			if total > maxComposeDecompressed {
				return "", fmt.Errorf("decompressed upload exceeds the %d byte size limit", maxComposeDecompressed)
			}
		default:
			// Symlinks and other special entry types are silently
			// skipped — matching tarpack.PackDir's own "not portable
			// into a container build context" stance; nothing in a
			// compose upload has a legitimate reason to carry one.
		}
	}
	return dir, nil
}

// composeSafeTarPath reports whether a tar entry's name is safe to
// extract under a fresh temp directory: not absolute, and containing no
// ".." path-traversal segment once cleaned.
func composeSafeTarPath(name string) bool {
	if name == "" {
		return false
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	return true
}
