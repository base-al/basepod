package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleSystemRedactsPodmanError proves a podman ping failure never
// relays the underlying error text (which can carry infrastructure
// detail — e.g. a unix socket path) to the API client (audit finding
// L5): the "podman" field reports the fixed string "error", not the raw
// error message.
func TestHandleSystemRedactsPodmanError(t *testing.T) {
	st := newTestStore(t)

	// newTestServer wires a.ping via fakePinger(nil), which never fails —
	// build a server directly (bypassing that helper) with a failing
	// pinger to exercise the error path.
	leaky := errors.New("dial unix /run/podman/podman.sock: connect: no such file or directory")
	srv := httptest.NewServer(New(st, &fakeDeployer{st: st}, fakePinger(leaky), "test-version", testSeal, testOpen, &fakeRoutesApplier{}, unusedLogSource, nil))
	t.Cleanup(srv.Close)

	_, session := login(t, srv, testPassword)
	token := session.Token

	var out systemResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/system", token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.Podman != "error" {
		t.Fatalf("podman status = %q, want the fixed string %q", out.Podman, "error")
	}
	if strings.Contains(out.Podman, "podman.sock") || strings.Contains(out.Podman, "no such file") {
		t.Fatalf("podman status leaked infrastructure detail: %q", out.Podman)
	}
}

// TestHandleSystemOKWhenPingSucceeds proves the happy path still reports
// "ok".
func TestHandleSystemOKWhenPingSucceeds(t *testing.T) {
	st := newTestStore(t)
	srv := newTestServer(t, st, &fakeDeployer{st: st}, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)
	token := session.Token

	var out systemResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/system", token, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.Podman != "ok" {
		t.Fatalf("podman status = %q, want ok", out.Podman)
	}
}
