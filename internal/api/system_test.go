package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/base-al/basepod/internal/caddy"
)

// fakeCaddyHealthChecker is a test double for CaddyHealthChecker: it
// returns whatever err is scripted, letting a test drive handleSystem's
// "caddy" field through every state without a real bp-caddy container —
// see TestHandleSystemCaddyHealth.
type fakeCaddyHealthChecker struct {
	err error
}

func (f *fakeCaddyHealthChecker) Health(ctx context.Context) error {
	return f.err
}

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
	srv := httptest.NewServer(New(st, &fakeDeployer{st: st}, fakePinger(leaky), "test-version", testSeal, testOpen, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, unusedAllStatsProvider, SystemInfo{}))
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

// TestHandleSystemReportsRootDomainAndDashboardDomain proves the new
// root_domain and dashboard_domain fields (issue #16) reach the client
// exactly as internal/server.Run computed them — including the case that
// motivated the issue: a disabled dashboard route (dashboard_domain
// setting == "off") must report the "off" sentinel itself, never a
// hostname that isn't actually live. handleSystem has no logic of its
// own here beyond relaying a.sysInfo.DashboardDomain verbatim (the
// honesty work — never echoing a configured-but-inactive stored value —
// belongs to internal/server's construction of that string; see
// TestSystemDashboardDomain in internal/server/server_test.go for that
// half), so this proves the wiring end to end at the HTTP layer for all
// three shapes DashboardDomain can take.
func TestHandleSystemReportsRootDomainAndDashboardDomain(t *testing.T) {
	cases := []struct {
		name            string
		dashboardDomain string
	}{
		{"active: a live hostname", "basepod.apps.example.com"},
		{"disabled: the off sentinel, not a stored hostname", "off"},
		{"unbound: the unbound sentinel, not a stored hostname", "unbound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			sysInfo := SystemInfo{RootDomain: "apps.example.com", DashboardDomain: tc.dashboardDomain}
			srv := httptest.NewServer(New(st, &fakeDeployer{st: st}, fakePinger(nil), "test-version", testSeal, testOpen, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, unusedAllStatsProvider, sysInfo))
			t.Cleanup(srv.Close)

			_, session := login(t, srv, testPassword)
			token := session.Token

			var out systemResponse
			resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/system", token, nil, &out)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if out.RootDomain != "apps.example.com" {
				t.Errorf("root_domain = %q, want %q", out.RootDomain, "apps.example.com")
			}
			if out.DashboardDomain != tc.dashboardDomain {
				t.Errorf("dashboard_domain = %q, want %q", out.DashboardDomain, tc.dashboardDomain)
			}
		})
	}
}

// TestHandleSystemCaddyHealth is table-driven over every shape the
// "caddy" field can take: unwired (nil CaddyHealthChecker, e.g. a test
// that doesn't care), healthy, the two distinguished failure modes issue
// #16 asks for (container not running vs. admin unreachable), and a
// generic error as a fallback-shape safety net. Follows the "podman"
// field's own "ok"/fixed-string-on-error redaction convention (see
// handleSystem's doc comment) — an error's full detail is asserted to
// reach neither response body nor to be silently dropped (it's logged
// server-side; not independently asserted here, same as the existing
// podman redaction test doesn't capture log output either).
func TestHandleSystemCaddyHealth(t *testing.T) {
	cases := []struct {
		name      string
		checker   CaddyHealthChecker
		wantCaddy string
	}{
		{"unwired: no checker configured", nil, "unknown"},
		{"healthy", &fakeCaddyHealthChecker{err: nil}, "ok"},
		{
			"container not running",
			&fakeCaddyHealthChecker{err: fmt.Errorf("%w: no such container", caddy.ErrCaddyNotRunning)},
			"error: container not running",
		},
		{
			"admin unreachable",
			&fakeCaddyHealthChecker{err: fmt.Errorf("%w: connection refused", caddy.ErrCaddyAdminUnreachable)},
			"error: admin unreachable",
		},
		{
			"unrecognized error shape falls back to the fixed string",
			&fakeCaddyHealthChecker{err: errors.New("something else entirely")},
			"error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			sysInfo := SystemInfo{CaddyHealth: tc.checker}
			srv := httptest.NewServer(New(st, &fakeDeployer{st: st}, fakePinger(nil), "test-version", testSeal, testOpen, &fakeRoutesApplier{}, unusedLogSource, nil, nil, unusedStatsSource, unusedAllStatsProvider, sysInfo))
			t.Cleanup(srv.Close)

			_, session := login(t, srv, testPassword)
			token := session.Token

			var out systemResponse
			resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/system", token, nil, &out)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if out.Caddy != tc.wantCaddy {
				t.Errorf("caddy = %q, want %q", out.Caddy, tc.wantCaddy)
			}
			if strings.Contains(out.Caddy, "connection refused") || strings.Contains(out.Caddy, "something else entirely") {
				t.Errorf("caddy status leaked underlying error detail: %q", out.Caddy)
			}
		})
	}
}
