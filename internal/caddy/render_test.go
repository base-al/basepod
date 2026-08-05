package caddy

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRenderTwoAppsGolden(t *testing.T) {
	got, err := Render([]AppRoute{
		{Slug: "wiki", Hostname: "wiki.apps.example.com", Upstream: "bp-wiki:3000"},
		{Slug: "blog", Hostname: "blog.apps.example.com", Upstream: "bp-blog:8080"},
	})
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

func TestRenderEmptyIsValidJSON(t *testing.T) {
	got, _ := Render(nil)
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
}
