package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests run against the embedded dist/ as it exists at `go test`
// time. In a plain checkout that's just the committed placeholder
// index.html (see embed.go's doc comment) — every case here is written to
// hold with the placeholder alone, since CI and local dev may or may not
// have run `make ui` first.

func TestHandler_RootServesIndex(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want to contain text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET / Cache-Control = %q, want %q", cc, "no-cache")
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET / body does not look like HTML: %q", rec.Body.String())
	}
}

func TestHandler_MissingAsset404sCleanly(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/missing.js status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	// Must be a clean 404, not the SPA shell — a missing asset should
	// never silently succeed with index.html.
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET /assets/missing.js fell back to the SPA shell instead of 404ing: %q", rec.Body.String())
	}
}

func TestHandler_UnknownExtensionlessPathFallsBackToIndex(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps/some-slug", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps/some-slug status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET /apps/some-slug Cache-Control = %q, want %q", cc, "no-cache")
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET /apps/some-slug did not fall back to the SPA shell: %q", rec.Body.String())
	}
}

func TestHandler_HTMLAcceptingUnknownPathFallsBackToIndex(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/some/deep/route", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /some/deep/route status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET /some/deep/route did not fall back to the SPA shell: %q", rec.Body.String())
	}
}

func TestHandler_IndexHTMLDirectRequestNoCache(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /index.html status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("GET /index.html Cache-Control = %q, want %q", cc, "no-cache")
	}
}
