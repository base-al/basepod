package api

import (
	"net/http"

	openapispec "github.com/base-al/basepod/api"
)

// handleOpenAPISpec serves BasePod's hand-written OpenAPI 3.1 spec
// (api/openapi.yaml, embedded into the binary at build time — see
// api/embed.go) verbatim and read-only. Deliberately public (no auth,
// mounted outside every requireAuth* group — see router.go): the spec's
// source file is public in the repo regardless, and gating the route
// behind a login would just be friction for anyone trying to look up the
// API before they have credentials for it.
//
// See internal/api/openapi_test.go for the conformance check that keeps
// this file honest: it walks the real router with chi.Walk and asserts a
// bidirectional match against every path+method this spec documents.
func (a *api) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(openapispec.YAML)
}
