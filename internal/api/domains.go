package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/store"
)

// hostnamePattern is the lowercase-FQDN shape a custom domain hostname
// must have.
var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

type domainResponse struct {
	ID       int64  `json:"id"`
	Hostname string `json:"hostname"`
}

func toDomainResponse(d store.Domain) domainResponse {
	return domainResponse{ID: d.ID, Hostname: d.Hostname}
}

type domainsListResponse struct {
	Generated string           `json:"generated"`
	Custom    []domainResponse `json:"custom"`
}

// handleListDomains returns an app's generated hostname (slug.root_domain)
// alongside its custom domains.
func (a *api) handleListDomains(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	rootDomain, err := a.st.Setting("root_domain")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read root domain")
		return
	}

	domains, err := a.st.ListDomains(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list domains")
		return
	}

	out := domainsListResponse{
		Generated: app.Slug + "." + rootDomain,
		Custom:    make([]domainResponse, 0, len(domains)),
	}
	for _, d := range domains {
		out.Custom = append(out.Custom, toDomainResponse(d))
	}
	writeJSON(w, http.StatusOK, out)
}

type addDomainRequest struct {
	Hostname string `json:"hostname"`
}

// handleAddDomain adds a custom domain to an app: validates its shape,
// lowercases it, rejects it if it collides with any app's generated
// hostname, and rejects duplicates. On success it triggers an immediate
// route refresh so Caddy picks the new hostname up without a redeploy.
func (a *api) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	var req addDomainRequest
	if !readJSON(w, r, &req) {
		return
	}

	hostname := strings.ToLower(req.Hostname)
	if !hostnamePattern.MatchString(hostname) {
		writeError(w, http.StatusUnprocessableEntity, "validation", "hostname must be a valid lowercase FQDN")
		return
	}

	rootDomain, err := a.st.Setting("root_domain")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read root domain")
		return
	}

	apps, err := a.st.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list apps")
		return
	}
	for _, other := range apps {
		if hostname == other.Slug+"."+rootDomain {
			writeError(w, http.StatusUnprocessableEntity, "validation", "hostname collides with a generated app hostname")
			return
		}
	}

	domain, err := a.st.AddDomain(app.ID, hostname)
	if err != nil {
		if isUniqueConstraintErr(err) {
			writeError(w, http.StatusConflict, "domain_exists", "this hostname is already in use")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			// Only reachable via a concurrent delete of this app between
			// appBySlugOrNotFound above and this insert (TOCTOU) — the
			// foreign key check fails because app.ID no longer exists.
			writeError(w, http.StatusNotFound, "app_not_found", "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "failed to add domain")
		return
	}

	if err := a.routes.ApplyRoutes(r.Context()); err != nil {
		// Best-effort rollback: the domain row was committed but Caddy
		// never actually picked it up, so don't leave the DB claiming a
		// domain that isn't routed. If this delete itself fails there's
		// nothing more to do here; the stale row surfaces on the next
		// list/reconcile rather than being silently invisible.
		_ = a.st.DeleteDomain(domain.ID)
		writeError(w, http.StatusBadGateway, "routes_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, toDomainResponse(*domain))
}

// handleDeleteDomain removes a custom domain from an app. The domain id
// must both exist and belong to this app — attempting to delete another
// app's domain (or a nonexistent one) returns 404 either way, so callers
// can't distinguish "wrong app" from "no such domain".
func (a *api) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "domain_not_found", "domain not found")
		return
	}

	domains, err := a.st.ListDomains(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list domains")
		return
	}
	var hostname string
	found := false
	for _, d := range domains {
		if d.ID == id {
			found = true
			hostname = d.Hostname
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "domain_not_found", "domain not found")
		return
	}

	if err := a.st.DeleteDomain(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete domain")
		return
	}

	if err := a.routes.ApplyRoutes(r.Context()); err != nil {
		// Best-effort rollback: the domain row is already gone but Caddy
		// never applied the change, so re-add it rather than silently
		// dropping a domain the operator still expects to be routed. The
		// restored row gets a new ID — callers must re-fetch via GET
		// rather than assuming the deleted ID still applies.
		_, _ = a.st.AddDomain(app.ID, hostname)
		writeError(w, http.StatusBadGateway, "routes_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
