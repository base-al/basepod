package caddy

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRenderTwoAppsGolden(t *testing.T) {
	got, err := Render([]AppRoute{
		{Slug: "wiki", Hostnames: []string{"wiki.apps.example.com", "wiki.custom.com"}, Upstream: "app-wiki:3000"},
		{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "app-blog:8080"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/two-apps.golden.json")
	if err != nil || string(got) != string(want) {
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			os.WriteFile("testdata/two-apps.golden.json", got, 0o644)
			return
		}
		t.Fatalf("golden mismatch (regen with UPDATE_GOLDEN=1):\n%s", got)
	}
}

// TestRenderTwoAppsWithDashboardGolden proves the dashboard route is
// prepended (before every app route) as a terminal host-match reverse
// proxy to its upstream — the dashboard API's unix socket dial string
// (see DashboardSockDial), not a network address, so only the bp-caddy
// container (which alone gets the socket's bind mount — see
// DashboardSockMountDest) can ever reach it.
func TestRenderTwoAppsWithDashboardGolden(t *testing.T) {
	got, err := Render([]AppRoute{
		{Slug: "wiki", Hostnames: []string{"wiki.apps.example.com", "wiki.custom.com"}, Upstream: "app-wiki:3000"},
		{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "app-blog:8080"},
	}, &DashboardRoute{Hostname: "basepod.apps.example.com", Upstream: DashboardSockDial()})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/two-apps-with-dashboard.golden.json")
	if err != nil || string(got) != string(want) {
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			os.WriteFile("testdata/two-apps-with-dashboard.golden.json", got, 0o644)
			return
		}
		t.Fatalf("golden mismatch (regen with UPDATE_GOLDEN=1):\n%s", got)
	}
}

func TestRenderEmptyIsValidJSON(t *testing.T) {
	got, _ := Render(nil, nil)
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
}

// TestRenderNilDashboardMatchesNoDashboardGolden proves a nil dashboard
// renders byte-identical output to the no-dashboard-arg call, i.e.
// omitting the dashboard route entirely rather than emitting an empty one.
func TestRenderNilDashboardMatchesNoDashboardGolden(t *testing.T) {
	routes := []AppRoute{{Slug: "blog", Hostnames: []string{"blog.apps.example.com"}, Upstream: "app-blog:8080"}}
	withoutArg, err := Render(routes, nil)
	if err != nil {
		t.Fatal(err)
	}
	explicitNil, err := Render(routes, (*DashboardRoute)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(withoutArg) != string(explicitNil) {
		t.Fatalf("Render output differs based on nil dashboard spelling:\n%s\nvs\n%s", withoutArg, explicitNil)
	}
}
