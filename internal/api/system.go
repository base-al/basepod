package api

import (
	"log"
	"net/http"
)

type systemResponse struct {
	Version string `json:"version"`
	Podman  string `json:"podman"`
	Apps    int    `json:"apps"`
}

// handleSystem reports the running version, container runtime
// reachability, and total app count.
//
// A podman ping failure is reported to the caller as the fixed string
// "error" (audit finding L5): the underlying error can carry internal
// infrastructure detail — e.g. the unix socket path podman.Client dials
// (see internal/podman.socketPath) — that has no business reaching an
// API client. The actual detail is logged server-side instead, where an
// operator diagnosing "podman: error" in the dashboard can find it.
func (a *api) handleSystem(w http.ResponseWriter, r *http.Request) {
	podmanStatus := "ok"
	if err := a.ping(r.Context()); err != nil {
		log.Printf("api: podman ping failed: %v", err)
		podmanStatus = "error"
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
