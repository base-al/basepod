package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

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
	Number int    `json:"number"`
	Image  string `json:"image"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func toDeploymentResponse(d store.Deployment) deploymentResponse {
	return deploymentResponse{Number: d.Number, Image: d.ImageRef, Status: d.Status, Error: d.Error}
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed request body")
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

// handleDeleteApp tears down an app's containers/routes via the Deployer
// and then removes its store row. If teardown fails, the app row is left
// in place (matching the store's foreign-key ownership of deployments)
// so a retry has something to act on.
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

	w.WriteHeader(http.StatusNoContent)
}
