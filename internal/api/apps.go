package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/deploy"
	"github.com/base-al/basepod/internal/store"
)

// slugPattern is the DNS-label-safe shape a slug must have after
// slugify: it becomes a hostname component (slug + "." + rootDomain), so
// it must start with a letter and contain only lowercase letters,
// digits, and hyphens, up to 32 characters.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// slugify derives a candidate slug from a user-supplied app name:
// lowercase, spaces become hyphens. The result still needs validating
// against slugPattern — this step does not strip other punctuation.
func slugify(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

type appResponse struct {
	Slug   string `json:"slug"`
	Image  string `json:"image"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

func toAppResponse(app *store.App) appResponse {
	return appResponse{Slug: app.Slug, Image: app.ImageRef, Port: app.Port, Status: app.Status}
}

type deploymentResponse struct {
	Number      int    `json:"number"`
	Image       string `json:"image"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	Source      string `json:"source"`
	Trigger     string `json:"trigger"`
	HasBuildLog bool   `json:"has_build_log"`
}

func toDeploymentResponse(d store.Deployment) deploymentResponse {
	return deploymentResponse{
		Number:      d.Number,
		Image:       d.ImageRef,
		Status:      d.Status,
		Error:       d.Error,
		StartedAt:   d.StartedAt,
		FinishedAt:  d.FinishedAt,
		Source:      d.Source,
		Trigger:     d.TriggerKind,
		HasBuildLog: d.BuildLogPath != "",
	}
}

type appDetailResponse struct {
	appResponse
	Deployments []deploymentResponse `json:"deployments"`
}

type createAppRequest struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Port  int    `json:"port"`
}

// handleCreateApp validates name/image/port, derives a slug from name,
// and creates the app. Duplicate slugs are rejected with 409 whether
// caught by the pre-check or (in a race) by the store's UNIQUE
// constraint.
func (a *api) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppRequest
	if !readJSON(w, r, &req) {
		return
	}

	slug := slugify(req.Name)
	if !slugPattern.MatchString(slug) {
		writeError(w, http.StatusUnprocessableEntity, "validation",
			"name must start with a letter and contain only letters, digits, spaces, and hyphens (max 32 characters)")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "image must not be empty")
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusUnprocessableEntity, "validation", "port must be between 1 and 65535")
		return
	}

	rootDomain, err := a.st.Setting("root_domain")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read root domain")
		return
	}
	dashboardDomain, err := a.st.Setting("dashboard_domain")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read dashboard domain")
		return
	}
	// See the matching check in domains.go's handleAddDomain: "" (unset)
	// and the literal "off" both mean the dashboard isn't actually routed
	// anywhere, so neither is a real collision.
	if dashboardDomain != "" && dashboardDomain != "off" && slug+"."+rootDomain == dashboardDomain {
		writeError(w, http.StatusUnprocessableEntity, "validation", "app slug's generated hostname collides with the dashboard domain")
		return
	}

	// Check for bidirectional hostname collision: reject if the generated
	// hostname (slug + "." + root_domain) matches an existing custom domain.
	generatedHostname := slug + "." + rootDomain
	if _, err := a.st.DomainByHostname(generatedHostname); err == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "app slug's generated hostname collides with an existing custom domain")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", "failed to check for hostname collision")
		return
	}

	if _, err := a.st.AppBySlug(slug); err == nil {
		writeError(w, http.StatusConflict, "app_exists", "an app with this name already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", "failed to check for existing app")
		return
	}

	app, err := a.st.CreateApp(slug, req.Image, req.Port)
	if err != nil {
		if isUniqueConstraintErr(err) {
			// Lost the race against the pre-check above: another request
			// created the same slug in between. Same outcome either way.
			writeError(w, http.StatusConflict, "app_exists", "an app with this name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to create app")
		return
	}

	writeJSON(w, http.StatusCreated, toAppResponse(app))
}

// isUniqueConstraintErr reports whether err looks like a SQLite UNIQUE
// constraint violation. modernc.org/sqlite surfaces SQLite's own error
// text ("UNIQUE constraint failed: apps.slug") rather than a typed
// sentinel, so this is a substring check rather than errors.Is/As.
func isUniqueConstraintErr(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint")
}

// handleListApps returns every app.
func (a *api) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.st.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list apps")
		return
	}
	out := make([]appResponse, 0, len(apps))
	for i := range apps {
		out = append(out, toAppResponse(&apps[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// appBySlugOrNotFound looks up the app named by the {slug} URL param,
// writing a 404 "app_not_found" (or 500 on an unexpected store error) and
// returning ok=false if it can't be found.
func (a *api) appBySlugOrNotFound(w http.ResponseWriter, r *http.Request) (app *store.App, ok bool) {
	slug := chi.URLParam(r, "slug")
	app, err := a.st.AppBySlug(slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "app_not_found", "app not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up app")
		}
		return nil, false
	}
	return app, true
}

// handleGetApp returns an app's details along with its deployment history.
func (a *api) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	deployments, err := a.st.ListDeployments(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list deployments")
		return
	}

	out := appDetailResponse{
		appResponse: toAppResponse(app),
		Deployments: make([]deploymentResponse, 0, len(deployments)),
	}
	for _, d := range deployments {
		out.Deployments = append(out.Deployments, toDeploymentResponse(d))
	}
	writeJSON(w, http.StatusOK, out)
}

type deployRequest struct {
	Image string `json:"image"`
}

// handleDeploy triggers a synchronous deploy of an app, optionally to a
// new image (an empty/omitted image reuses the app's current one). The
// request is bounded by deployTimeout regardless of the caller's own
// timeout, since a deploy pulls an image and polls health probes.
func (a *api) handleDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	var req deployRequest
	if err := decodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeDecodeError(w, err)
		return
	}

	image := req.Image
	if image == "" {
		image = app.ImageRef
	}

	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()

	dep, err := a.dep.Deploy(ctx, app, image)
	if err != nil {
		writeError(w, http.StatusBadGateway, "deploy_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toDeploymentResponse(*dep))
}

type rollbackRequest struct {
	Number int `json:"number"`
}

// handleRollback triggers a synchronous rollback of an app to an earlier
// deployment's exact image (body: {"number": N}). Bounded by the same
// deployTimeout as handleDeploy, since — like a normal deploy — it may
// pull an image and runs the same probe-gated rollout.
func (a *api) handleRollback(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	var req rollbackRequest
	if !readJSON(w, r, &req) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()

	dep, err := a.dep.Rollback(ctx, app, req.Number)
	if err != nil {
		writeRollbackError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, toDeploymentResponse(*dep))
}

// writeRollbackError maps a Rollback failure to the documented
// status/error code: deploy_engine's typed pre-flight errors (target
// missing/unhealthy, or a local build image no longer available) each get
// their own 404/409 code; anything else (a rollout failure past that
// point, e.g. a failed pull or probe) falls through to the same generic
// 502 "deploy_failed" handleDeploy's own catch-all uses.
func writeRollbackError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, deploy.ErrRollbackTargetNotFound):
		writeError(w, http.StatusNotFound, "rollback_target_not_found", err.Error())
	case errors.Is(err, deploy.ErrRollbackTargetUnhealthy):
		writeError(w, http.StatusConflict, "rollback_target_unhealthy", err.Error())
	case errors.Is(err, deploy.ErrRollbackImageMissing):
		writeError(w, http.StatusConflict, "rollback_image_missing", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "deploy_failed", err.Error())
	}
}

// maxTarballBody caps a tarball deploy upload at 256 MiB — generously
// larger than any legitimate build context BasePod expects, but small
// enough to bound worst-case memory/disk use against a malicious or
// buggy client. It is enforced by this route's own http.MaxBytesReader
// rather than bodyLimit's maxJSONBody: the route is deliberately
// registered outside that middleware's group (see router.go) so a build
// upload isn't bound by a cap sized for JSON payloads.
const maxTarballBody = 256 << 20 // 256 MiB

// buildTimeout bounds a single tarball deploy request — much longer than
// deployTimeout, since it also has to decompress the upload and run a
// full container build before the existing pull-less rollout even
// starts.
const buildTimeout = 60 * time.Minute

// gzipContentTypes are the Content-Type header values handleDeployTarball
// accepts for the raw gzipped-tar upload body.
var gzipContentTypes = map[string]bool{
	"application/gzip":   true,
	"application/x-gzip": true,
}

// handleDeployTarball triggers a build-from-upload deploy: the request
// body must be a raw gzipped tar (Content-Type application/gzip or
// application/x-gzip) containing a Containerfile or Dockerfile at its
// root. It streams the body straight into Deployer.DeployBuild (via
// a.builder) rather than buffering it itself, bounded by maxTarballBody
// and buildTimeout regardless of the caller's own timeout.
//
// Errors: 400 "invalid_content_type" for a non-gzip Content-Type, 413
// "request_too_large" if the body exceeds maxTarballBody, 422
// "no_containerfile"/"bad_path" for a build context that fails
// validation (see internal/build.ErrNoContainerfile / ErrBadPath), 502
// "deploy_failed" for any other build/rollout failure.
func (a *api) handleDeployTarball(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	if !gzipContentTypes[r.Header.Get("Content-Type")] {
		writeError(w, http.StatusBadRequest, "invalid_content_type", "Content-Type must be application/gzip")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTarballBody)

	ctx, cancel := context.WithTimeout(r.Context(), buildTimeout)
	defer cancel()

	dep, err := a.dep.DeployBuild(ctx, app, r.Body, a.builder)
	if err != nil {
		writeDeployBuildError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, toDeploymentResponse(*dep))
}

// writeDeployBuildError maps a DeployBuild failure to the documented
// status/error code: an oversize upload (bodyLimit's *http.MaxBytesError,
// surfacing here through the io.Copy chain inside the build pipeline
// rather than a JSON decode) is 413, as is a build context that decompresses
// past the pipeline's own size cap (build.ErrContextTooLarge — a defense
// against a small compressed upload expanding into a much larger one, a
// "gzip bomb"); a build-context validation failure is 422 with a code
// naming which check failed; anything else is a generic 502
// "deploy_failed", matching handleDeploy's own catch-all.
func writeDeployBuildError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
	case errors.Is(err, build.ErrContextTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "context_too_large", err.Error())
	case errors.Is(err, build.ErrNoContainerfile):
		writeError(w, http.StatusUnprocessableEntity, "no_containerfile", err.Error())
	case errors.Is(err, build.ErrBadPath):
		writeError(w, http.StatusUnprocessableEntity, "bad_path", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "deploy_failed", err.Error())
	}
}

// handleDeleteApp tears down an app's containers/routes via the Deployer
// and then removes its store row. If teardown fails, the app row is left
// in place (matching the store's foreign-key ownership of deployments)
// so a retry has something to act on.
//
// Once both the containers and the store row are gone, it also best-effort
// removes the app's on-disk data directory (build spool artifacts and
// build logs — see internal/build.Builder) via appDataDir + os.RemoveAll:
// nothing about the app's on-disk footprint should survive a delete
// forever (see H3 in the v0.3 security audit — an app that's built and
// redeployed many times before being deleted otherwise leaves its build
// logs on disk permanently). A failure here is logged, not surfaced as an
// error response: the app is already gone from the API's/store's
// perspective by this point, so there's nothing left for the caller to
// retry.
func (a *api) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	if err := a.dep.RemoveApp(r.Context(), app); err != nil {
		writeError(w, http.StatusBadGateway, "remove_failed", err.Error())
		return
	}

	if err := a.st.DeleteApp(app.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete app")
		return
	}

	if a.builder != nil {
		if dir, ok := appDataDir(a.builder.DataDir(), app.Slug); ok {
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("api: delete app %s: remove data dir %s: %v", app.Slug, dir, err)
			}
		} else {
			// Should be unreachable — slugPattern (see handleCreateApp)
			// already forbids anything but [a-z][a-z0-9-]{0,31}, which can
			// never produce a path-traversal segment. Logged rather than
			// silently skipped so a future change to slug validation that
			// broke this invariant would actually be noticed.
			log.Printf("api: delete app %s: slug does not resolve inside the data directory — refusing to remove anything", app.Slug)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// appDataDir returns the data-directory path for slug's app files
// (<dataDir>/apps/<slug> — see internal/build.Builder's LogPath doc
// comment for the layout underneath it), or ("", false) if slug would
// resolve outside <dataDir>/apps once cleaned. Defense in depth for
// handleDeleteApp's os.RemoveAll: slugPattern already forbids a slug
// containing "/" or ".." at creation time (see handleCreateApp), but this
// still verifies the join can never escape the intended directory before
// a delete request is allowed to recursively remove anything from disk.
func appDataDir(dataDir, slug string) (string, bool) {
	base := filepath.Clean(filepath.Join(dataDir, "apps"))
	dir := filepath.Clean(filepath.Join(base, slug))
	// dir must be a strict descendant of base: this also rejects an empty
	// (or "."/".." -only) slug, which would otherwise resolve to base
	// itself — never a valid single app's directory to recursively remove.
	if !strings.HasPrefix(dir, base+string(filepath.Separator)) {
		return "", false
	}
	return dir, true
}
