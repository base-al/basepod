package api

import "net/http"

type systemResponse struct {
	Version string `json:"version"`
	Podman  string `json:"podman"`
	Apps    int    `json:"apps"`
}

// handleSystem reports the running version, container runtime
// reachability, and total app count.
func (a *api) handleSystem(w http.ResponseWriter, r *http.Request) {
	podmanStatus := "ok"
	if err := a.ping(r.Context()); err != nil {
		podmanStatus = "error: " + err.Error()
	}

	apps, err := a.st.ListApps()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list apps")
		return
	}

	writeJSON(w, http.StatusOK, systemResponse{
		Version: a.version,
		Podman:  podmanStatus,
		Apps:    len(apps),
	})
}
