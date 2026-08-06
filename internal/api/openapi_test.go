package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// httpMethods is every HTTP method OpenAPI's Path Item Object recognizes
// as an operation key — used to distinguish an actual method entry (e.g.
// "get") from a path-item-level sibling key like "parameters" or
// "description" when walking a parsed spec document.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// specDoc is the sliver of an OpenAPI 3.1 document this conformance test
// needs: enough to enumerate every documented path+method pair. Path item
// values are decoded as plain maps (rather than a fully-typed Path Item
// Object) since the only thing extracted from them is which of their keys
// are HTTP methods.
type specDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

// loadSpec reads and parses api/openapi.yaml — found by walking up from
// the current package directory to the repo root, since Go test binaries
// run with the package directory as their working directory regardless of
// where `go test` was invoked from.
func loadSpec(t *testing.T) specDoc {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/api -> repo root is two levels up.
	root := filepath.Dir(filepath.Dir(dir))
	specPath := filepath.Join(root, "api", "openapi.yaml")

	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v (expected the repo root two levels above internal/api)", specPath, err)
	}

	var doc specDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s parsed with zero paths — parser or fixture problem", specPath)
	}
	return doc
}

// specRouteSet returns every "METHOD path" pair the spec documents, e.g.
// "GET /apps/{slug}/stats" — paths are relative to the spec's declared
// server (/api/v1), matching how routerRouteSet strips that same prefix
// from the live router's routes.
func specRouteSet(doc specDoc) map[string]bool {
	set := make(map[string]bool)
	for path, item := range doc.Paths {
		for key := range item {
			method := strings.ToLower(key)
			if !httpMethods[method] {
				continue
			}
			set[strings.ToUpper(method)+" "+path] = true
		}
	}
	return set
}

// apiV1Prefix is the mount point every route this package registers lives
// under (see router.go's `r.Route("/api/v1", ...)`) and the spec's
// `servers[0].url` — stripped from chi's routes so both sides of the
// conformance comparison use the same relative-path form the spec's
// `paths` keys already use.
const apiV1Prefix = "/api/v1"

// routerRouteSet returns every "METHOD path" pair the real chi router
// registers, walked via chi.Walk against the exact handler construction
// New uses (newRouter — see its doc comment: split out of New precisely
// so tests can mount the same route table without going through New's
// full dependency wiring). Routes are relative to apiV1Prefix, stripped
// off, to match specRouteSet's form.
func routerRouteSet(t *testing.T) map[string]bool {
	t.Helper()

	a := &api{} // no fields dereferenced by route registration itself — see newRouter
	handler := newRouter(a)
	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("newRouter's returned http.Handler does not implement chi.Routes (%T)", handler)
	}

	set := make(map[string]bool)
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		rel := strings.TrimPrefix(route, apiV1Prefix)
		if rel == "" {
			rel = "/"
		}
		set[method+" "+rel] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return set
}

// TestOpenAPIConformance is the drift check the v0.5 plan requires
// (doc 05's checklist item, Task 11): api/openapi.yaml and the real
// router's registered route table must describe exactly the same set of
// (method, path) pairs, in both directions. A handler added to router.go
// without a matching spec entry — or a spec entry with no backing
// route — fails this test, so the spec can't silently rot the way a
// hand-written-and-unenforced one would within a milestone.
//
// This test was verified to actually fail once during development, by
// temporarily commenting out the stats route's registration in router.go
// (see this test's PASS/FAIL note in the v0.5 Task 11 report) — proving
// it's a real drift detector, not a tautology that always passes.
func TestOpenAPIConformance(t *testing.T) {
	doc := loadSpec(t)
	specRoutes := specRouteSet(doc)
	routerRoutes := routerRouteSet(t)

	var onlyInSpec, onlyInRouter []string
	for r := range specRoutes {
		if !routerRoutes[r] {
			onlyInSpec = append(onlyInSpec, r)
		}
	}
	for r := range routerRoutes {
		if !specRoutes[r] {
			onlyInRouter = append(onlyInRouter, r)
		}
	}
	sort.Strings(onlyInSpec)
	sort.Strings(onlyInRouter)

	if len(onlyInSpec) > 0 {
		t.Errorf("api/openapi.yaml documents %d route(s) the router does not register (stale spec entries):\n%s",
			len(onlyInSpec), strings.Join(onlyInSpec, "\n"))
	}
	if len(onlyInRouter) > 0 {
		t.Errorf("the router registers %d route(s) api/openapi.yaml does not document (missing spec entries):\n%s",
			len(onlyInRouter), strings.Join(onlyInRouter, "\n"))
	}
}

// TestOpenAPISpecHasNoEmptyPathItems is a light sanity check on the parsed
// fixture itself — every path in the spec must document at least one
// method, since a path with zero methods would silently contribute
// nothing to specRouteSet and could mask a typo (e.g. an indentation
// mistake nesting a method under the wrong path) rather than failing
// loudly.
func TestOpenAPISpecHasNoEmptyPathItems(t *testing.T) {
	doc := loadSpec(t)
	for path, item := range doc.Paths {
		hasMethod := false
		for key := range item {
			if httpMethods[strings.ToLower(key)] {
				hasMethod = true
				break
			}
		}
		if !hasMethod {
			t.Errorf("path %q has no HTTP method operations", path)
		}
	}
}

// TestHandleOpenAPISpecServesEmbeddedYAML proves GET /openapi.yaml is
// mounted publicly (no auth) and serves the same bytes embedded from
// api/openapi.yaml, byte for byte.
func TestHandleOpenAPISpecServesEmbeddedYAML(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})

	resp, err := http.Get(srv.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth required)", resp.StatusCode)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(dir))
	want, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != string(want) {
		t.Fatalf("served spec (%d bytes) does not match api/openapi.yaml on disk (%d bytes)", len(got), len(want))
	}
}
