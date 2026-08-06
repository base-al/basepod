package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestDomainAddRejectsOverlongHostname proves a hostname whose overall
// length exceeds 253 characters is rejected with 422 even though every
// individual DNS label is within hostnamePattern's own 63-character
// per-label bound (audit finding L7): hostnamePattern alone says nothing
// about the total FQDN length.
func TestDomainAddRejectsOverlongHostname(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	// Build a hostname of valid 63-char labels (well-formed per label)
	// that together comfortably exceed 253 characters overall.
	label := strings.Repeat("a", 63)
	hostname := label + "." + label + "." + label + "." + label + ".example.com"
	if len(hostname) <= maxHostnameLength {
		t.Fatalf("test fixture hostname is only %d chars, want > %d", len(hostname), maxHostnameLength)
	}

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: hostname}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("error code = %q, want validation", errBody.Error.Code)
	}
}

// TestDomainAddRejectsAtMaxCustomDomains proves an app already at
// maxCustomDomainsPerApp custom domains gets 422 on the next add (audit
// finding L7) — capping how many Let's Encrypt certificates a single
// stolen session token could cause BasePod to attempt against the root
// domain's ACME quota.
func TestDomainAddRejectsAtMaxCustomDomains(t *testing.T) {
	srv, _, token, _, _ := setupEnvDomainsTest(t)

	for i := 0; i < maxCustomDomainsPerApp; i++ {
		hostname := fmt.Sprintf("domain%d.example.org", i)
		resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
			addDomainRequest{Hostname: hostname}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add #%d: status = %d, want 201", i, resp.StatusCode)
		}
	}

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "one-too-many.example.org"}, &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if errBody.Error.Code != "validation" {
		t.Fatalf("error code = %q, want validation", errBody.Error.Code)
	}
}

// TestDomainAddRoutesFailureDoesNotLeakDetail proves a routes-apply
// failure's error message is a generic, fixed string rather than the raw
// underlying error (audit finding L5) — the raw detail (which could name
// internal infrastructure, e.g. a Caddy admin-socket path) is logged
// server-side instead (not observable from this test, which only checks
// what the client sees).
func TestDomainAddRoutesFailureDoesNotLeakDetail(t *testing.T) {
	srv, _, token, _, routes := setupEnvDomainsTest(t)
	routes.err = fmt.Errorf("dial unix /run/basepod/caddy-admin.sock: connect: no such file or directory")

	var errBody errorResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "rollback.example.org"}, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(errBody.Error.Message, "caddy-admin.sock") || strings.Contains(errBody.Error.Message, "no such file") {
		t.Fatalf("error message leaked infrastructure detail: %q", errBody.Error.Message)
	}
}

// TestDomainDeleteRoutesFailureDoesNotLeakDetail is
// TestDomainAddRoutesFailureDoesNotLeakDetail's counterpart for
// handleDeleteDomain's own routes-apply rollback path.
func TestDomainDeleteRoutesFailureDoesNotLeakDetail(t *testing.T) {
	srv, _, token, _, routes := setupEnvDomainsTest(t)

	var added domainResponse
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/v1/apps/my-blog/domains", token,
		addDomainRequest{Hostname: "keep.example.org"}, &added)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add: status = %d, want 201", resp.StatusCode)
	}

	routes.err = fmt.Errorf("dial unix /run/basepod/caddy-admin.sock: connect: no such file or directory")

	var errBody errorResponse
	resp = doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/api/v1/apps/my-blog/domains/%d", srv.URL, added.ID), token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(errBody.Error.Message, "caddy-admin.sock") || strings.Contains(errBody.Error.Message, "no such file") {
		t.Fatalf("error message leaked infrastructure detail: %q", errBody.Error.Message)
	}
}

// TestHandleDeleteAppRemoveFailureDoesNotLeakDetail proves a RemoveApp
// (teardown) failure's error message is generic, not the raw underlying
// error (audit finding L5) — distinct from a deploy/build failure, which
// must stay verbatim since it's the user's own build output.
func TestHandleDeleteAppRemoveFailureDoesNotLeakDetail(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.CreateApp("blog", "nginx:latest", 80); err != nil {
		t.Fatal(err)
	}
	dep := &fakeDeployer{st: st, removeErr: fmt.Errorf("dial unix /run/podman/podman.sock: connect: no such file or directory")}
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := doJSON(t, http.MethodDelete, srv.URL+"/api/v1/apps/blog", session.Token, nil, &errBody)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if errBody.Error.Code != "remove_failed" {
		t.Fatalf("error code = %q, want remove_failed", errBody.Error.Code)
	}
	if strings.Contains(errBody.Error.Message, "podman.sock") || strings.Contains(errBody.Error.Message, "no such file") {
		t.Fatalf("error message leaked infrastructure detail: %q", errBody.Error.Message)
	}
}
