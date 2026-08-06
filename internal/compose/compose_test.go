package compose

import (
	"sort"
	"strings"
	"testing"
)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ---- Parse ----

func TestParseRealisticMultiService(t *testing.T) {
	doc := `
name: shop
services:
  web:
    build:
      context: ./web
      dockerfile: Containerfile.web
      args:
        NODE_ENV: production
    environment:
      - API_URL=http://api:3000
      - DEBUG=false
    expose:
      - "8080"
    depends_on:
      - api
  api:
    image: ghcr.io/example/api:latest
    environment:
      PORT: 3000
      LOG_LEVEL: info
    expose:
      - 3000
    depends_on:
      - postgres
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: secret
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Parse: unexpected warnings: %v", warnings)
	}
	if f.Project != "shop" {
		t.Errorf("Project = %q, want shop", f.Project)
	}
	if len(f.Services) != 3 {
		t.Fatalf("len(Services) = %d, want 3", len(f.Services))
	}

	web := f.Services["web"]
	if web.Build == nil || web.Build.Context != "./web" || web.Build.Dockerfile != "Containerfile.web" {
		t.Fatalf("web.Build = %+v, want context ./web dockerfile Containerfile.web", web.Build)
	}
	if web.Build.Args["NODE_ENV"] != "production" {
		t.Errorf("web.Build.Args[NODE_ENV] = %q, want production", web.Build.Args["NODE_ENV"])
	}
	if web.Environment["API_URL"] != "http://api:3000" || web.Environment["DEBUG"] != "false" {
		t.Errorf("web.Environment = %+v", web.Environment)
	}
	if len(web.Expose) != 1 || web.Expose[0] != 8080 {
		t.Errorf("web.Expose = %v, want [8080]", web.Expose)
	}
	if !equalStrings(web.DependsOn, []string{"api"}) {
		t.Errorf("web.DependsOn = %v, want [api]", web.DependsOn)
	}

	api := f.Services["api"]
	if api.Image != "ghcr.io/example/api:latest" {
		t.Errorf("api.Image = %q", api.Image)
	}
	if api.Environment["PORT"] != "3000" || api.Environment["LOG_LEVEL"] != "info" {
		t.Errorf("api.Environment = %+v", api.Environment)
	}
	if !equalStrings(api.DependsOn, []string{"postgres"}) {
		t.Errorf("api.DependsOn = %v, want [postgres]", api.DependsOn)
	}

	pg := f.Services["postgres"]
	if len(pg.Expose) != 0 {
		t.Errorf("postgres.Expose = %v, want empty (internal service)", pg.Expose)
	}
	if len(pg.Volumes) != 1 || pg.Volumes[0].Name != "pgdata" || pg.Volumes[0].Target != "/var/lib/postgresql/data" {
		t.Fatalf("postgres.Volumes = %+v", pg.Volumes)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	for _, doc := range []string{"", "\n", "null\n", "# just a comment\n"} {
		f, warnings, err := Parse(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error: %v", doc, err)
		}
		if len(warnings) != 0 {
			t.Fatalf("Parse(%q): unexpected warnings: %v", doc, warnings)
		}
		if f.Project != "" || len(f.Services) != 0 {
			t.Fatalf("Parse(%q): got non-empty file %+v", doc, f)
		}
	}
}

func TestParseBuildForms(t *testing.T) {
	doc := `
services:
  a:
    build: ./ctx-a
  b:
    build:
      context: ./ctx-b
      dockerfile: Containerfile
      args:
        FOO: bar
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if f.Services["a"].Build.Context != "./ctx-a" {
		t.Errorf("a.Build.Context = %q, want ./ctx-a", f.Services["a"].Build.Context)
	}
	if f.Services["a"].Build.Dockerfile != "" {
		t.Errorf("a.Build.Dockerfile = %q, want empty", f.Services["a"].Build.Dockerfile)
	}
	b := f.Services["b"].Build
	if b.Context != "./ctx-b" || b.Dockerfile != "Containerfile" || b.Args["FOO"] != "bar" {
		t.Errorf("b.Build = %+v", b)
	}
}

func TestParseBuildMappingDefaultsContext(t *testing.T) {
	doc := `
services:
  a:
    build:
      dockerfile: Containerfile
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if f.Services["a"].Build.Context != "." {
		t.Errorf("Build.Context = %q, want \".\" (default)", f.Services["a"].Build.Context)
	}
}

func TestParseEnvironmentForms(t *testing.T) {
	doc := `
services:
  a:
    image: x
    environment:
      FOO: bar
      COUNT: 5
  b:
    image: x
    environment:
      - FOO=bar
      - COUNT=5
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := map[string]string{"FOO": "bar", "COUNT": "5"}
	for _, name := range []string{"a", "b"} {
		got := f.Services[name].Environment
		if got["FOO"] != want["FOO"] || got["COUNT"] != want["COUNT"] {
			t.Errorf("%s.Environment = %+v, want %+v", name, got, want)
		}
	}
}

func TestParseUnsupportedKeysWarnByName(t *testing.T) {
	doc := `
services:
  web:
    image: nginx
    restart: always
    command: ["nginx", "-g", "daemon off;"]
    networks:
      - default
    deploy:
      replicas: 2
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost"]
    ports:
      - "8080:80"
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	wantSubstrings := []string{
		`unsupported key "restart"`,
		`unsupported key "command"`,
		`unsupported key "networks"`,
		`unsupported key "deploy"`,
		`unsupported key "healthcheck"`,
		`'ports' (host port publishing) is not supported`,
	}
	for _, want := range wantSubstrings {
		found := false
		for _, w := range warnings {
			if strings.Contains(w, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("warnings %v missing substring %q", warnings, want)
		}
	}
	// The rest of the service still translates despite the warnings.
	if f.Services["web"].Image != "nginx" {
		t.Errorf("web.Image = %q, want nginx (must still translate)", f.Services["web"].Image)
	}
}

func TestParseUnknownTopLevelFieldWarns(t *testing.T) {
	doc := `
name: shop
totally-unknown: yes
services:
  web:
    image: nginx
`
	_, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	want := []string{"unknown top-level field: totally-unknown"}
	if !equalStrings(warnings, want) {
		t.Fatalf("warnings = %v, want %v", warnings, want)
	}
}

func TestParseDependsOnLongFormWarns(t *testing.T) {
	doc := `
services:
  web:
    image: nginx
    depends_on:
      api:
        condition: service_healthy
  api:
    image: nginx
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "depends_on's long form (condition) is accepted") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v missing depends_on long-form warning", warnings)
	}
	if !equalStrings(f.Services["web"].DependsOn, []string{"api"}) {
		t.Errorf("web.DependsOn = %v, want [api]", f.Services["web"].DependsOn)
	}
}

func TestParseExposeExtraPortsWarns(t *testing.T) {
	doc := `
services:
  web:
    image: nginx
    expose:
      - "8080"
      - "9090"
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	gotExpose := f.Services["web"].Expose
	if len(gotExpose) != 2 || gotExpose[0] != 8080 || gotExpose[1] != 9090 {
		t.Errorf("web.Expose = %v, want [8080 9090] (order preserved)", gotExpose)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "expose lists multiple ports") && strings.Contains(w, "9090") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v missing extra-expose-ports warning", warnings)
	}
}

func TestParseBindMountRejected(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"relative dot", `
services:
  web:
    image: nginx
    volumes:
      - ./data:/data
`},
		{"absolute host path", `
services:
  web:
    image: nginx
    volumes:
      - /etc:/etc
`},
		{"long form bind", `
services:
  web:
    image: nginx
    volumes:
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Parse(strings.NewReader(tt.doc))
			if err == nil {
				t.Fatalf("Parse(%s): expected bind-mount error, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), "bind mount") || !strings.Contains(err.Error(), `service "web"`) {
				t.Fatalf("Parse(%s): error = %q, want it to name the service and say bind mount", tt.name, err.Error())
			}
		})
	}
}

func TestParseNamedVolumeAccepted(t *testing.T) {
	doc := `
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data:ro
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if len(f.Services["db"].Volumes) != 1 {
		t.Fatalf("db.Volumes = %+v", f.Services["db"].Volumes)
	}
	v := f.Services["db"].Volumes[0]
	if v.Name != "pgdata" || v.Target != "/var/lib/postgresql/data" {
		t.Fatalf("db.Volumes[0] = %+v", v)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "mount mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning about the unenforced 'ro' mode, got %v", warnings)
	}
}

func TestParseMalformedYAML(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"unterminated flow", "services: [unterminated\n"},
		{"tab indentation", "services:\n\tweb:\n\t\timage: nginx\n"},
		{"top-level scalar", "just a string\n"},
		{"top-level list", "- a\n- b\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Parse(%s) panicked: %v", tt.name, r)
				}
			}()
			_, _, err := Parse(strings.NewReader(tt.doc))
			if err == nil {
				t.Fatalf("Parse(%s): expected error, got nil", tt.name)
			}
		})
	}
}

func TestParseServiceWithoutImageOrBuildErrors(t *testing.T) {
	doc := `
services:
  web:
    environment:
      FOO: bar
`
	_, _, err := Parse(strings.NewReader(doc))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), "'image' or 'build'") {
		t.Fatalf("error = %q, want it to name the service and mention image/build", err.Error())
	}
}

func TestParseServicesNotAMappingErrors(t *testing.T) {
	_, _, err := Parse(strings.NewReader("services: [1, 2, 3]\n"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "field 'services' must be a mapping") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestParseImageWrongTypeErrors(t *testing.T) {
	_, _, err := Parse(strings.NewReader("services:\n  web:\n    image: 5\n"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `field "image" must be a string`) {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestParseTopLevelVolumesUsageCrossCheck(t *testing.T) {
	doc := `
services:
  db:
    image: postgres
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
  cache:
`
	f, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, `top-level volume "cache" is declared but not used`) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v missing unused-top-level-volume warning", warnings)
	}
	if !equalStrings(f.Volumes, []string{"cache", "pgdata"}) {
		t.Errorf("f.Volumes = %v", f.Volumes)
	}
}

func TestParseUndeclaredTopLevelVolumeWarns(t *testing.T) {
	doc := `
services:
  db:
    image: postgres
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  other:
`
	_, warnings, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, `volume "pgdata" is not declared in top-level`) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v missing undeclared-volume warning", warnings)
	}
}

// ---- Plan ----

func basePlanFile(t *testing.T) *File {
	t.Helper()
	doc := `
name: shop
services:
  web:
    image: nginx
    expose:
      - "8080"
    depends_on:
      - api
  api:
    image: ghcr.io/example/api
    expose:
      - 3000
    depends_on:
      - db
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	return f
}

func TestPlanRealisticMultiService(t *testing.T) {
	f := basePlanFile(t)
	plan, err := BuildPlan(f, "shop", PlanContext{})
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	if plan.Project != "shop" {
		t.Errorf("Project = %q", plan.Project)
	}
	if len(plan.Services) != 3 {
		t.Fatalf("len(Services) = %d, want 3", len(plan.Services))
	}

	// Order: db before api before web (dependency order).
	order := make([]string, len(plan.Services))
	for i, sp := range plan.Services {
		order[i] = sp.Name
	}
	wantOrder := []string{"db", "api", "web"}
	for i := range order {
		if order[i] != wantOrder[i] {
			t.Fatalf("order = %v, want %v", order, wantOrder)
		}
	}

	byName := map[string]ServicePlan{}
	for _, sp := range plan.Services {
		byName[sp.Name] = sp
	}

	web := byName["web"]
	if web.Slug != "shop-web" || web.Alias != "web" || web.Internal || web.Port != 8080 {
		t.Errorf("web = %+v", web)
	}
	if web.Action != "create" {
		t.Errorf("web.Action = %q, want create", web.Action)
	}

	db := byName["db"]
	if !db.Internal || db.Port != 0 {
		t.Errorf("db should be internal with Port 0, got %+v", db)
	}
	if db.RecommendedStrategy != "replace" || db.StrategyReason == "" {
		t.Errorf("db should recommend replace strategy with a reason, got %+v", db)
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Name != "pgdata" {
		t.Errorf("db.Volumes = %+v", db.Volumes)
	}

	api := byName["api"]
	if api.Internal || api.Port != 3000 {
		t.Errorf("api = %+v", api)
	}
	if api.RecommendedStrategy != "" {
		t.Errorf("api should have no strategy recommendation (no volumes), got %q", api.RecommendedStrategy)
	}
}

func TestPlanCreateVsUpdate(t *testing.T) {
	f := basePlanFile(t)
	existing := PlanContext{
		ExistingApps: map[ServiceKey]ExistingApp{
			{Project: "shop", Service: "web"}: {Slug: "shop-web", Alias: "web"},
		},
		TakenAliases: map[string]string{
			"web": "compose project shop, service web",
		},
	}
	plan, err := BuildPlan(f, "shop", existing)
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	byName := map[string]ServicePlan{}
	for _, sp := range plan.Services {
		byName[sp.Name] = sp
	}
	if byName["web"].Action != "update" {
		t.Errorf("web.Action = %q, want update", byName["web"].Action)
	}
	if byName["api"].Action != "create" {
		t.Errorf("api.Action = %q, want create", byName["api"].Action)
	}
	if byName["db"].Action != "create" {
		t.Errorf("db.Action = %q, want create", byName["db"].Action)
	}
}

func TestPlanAliasConflictAcrossProjects(t *testing.T) {
	doc := `
services:
  db:
    image: postgres
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	existing := PlanContext{
		TakenAliases: map[string]string{
			"db": "compose project other, service db",
		},
	}
	_, err = BuildPlan(f, "shop", existing)
	if err == nil {
		t.Fatalf("expected alias-collision error, got nil")
	}
	if !strings.Contains(err.Error(), `alias "db"`) || !strings.Contains(err.Error(), "already claimed by compose project other, service db") {
		t.Fatalf("error = %q, want it to name the alias and the owner", err.Error())
	}
}

func TestPlanAliasSelfOwnedNotAConflict(t *testing.T) {
	doc := `
services:
  db:
    image: postgres
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	existing := PlanContext{
		ExistingApps: map[ServiceKey]ExistingApp{
			{Project: "shop", Service: "db"}: {Slug: "shop-db", Alias: "db"},
		},
		TakenAliases: map[string]string{
			"db": "compose project shop, service db",
		},
	}
	plan, err := BuildPlan(f, "shop", existing)
	if err != nil {
		t.Fatalf("Plan: unexpected error for self-owned alias: %v", err)
	}
	if plan.Services[0].Action != "update" {
		t.Errorf("Action = %q, want update", plan.Services[0].Action)
	}
}

func TestPlanSlugTooLong(t *testing.T) {
	doc := `
services:
  a-very-long-service-name-indeed:
    image: nginx
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	_, err = BuildPlan(f, "project", PlanContext{})
	if err == nil {
		t.Fatalf("expected slug-too-long error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeding the 32-character limit") {
		t.Fatalf("error = %q, want it to mention the 32-character limit", err.Error())
	}
}

func TestPlanReservedServiceName(t *testing.T) {
	tests := []string{"caddy", "basepod", "bp-foo", "app-foo"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			doc := "services:\n  " + name + ":\n    image: nginx\n"
			f, _, err := Parse(strings.NewReader(doc))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			_, err = BuildPlan(f, "shop", PlanContext{})
			if err == nil {
				t.Fatalf("Plan(%s): expected reserved-name error, got nil", name)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("Plan(%s): error = %q, want it to mention 'reserved'", name, err.Error())
			}
		})
	}
}

func TestPlanDependsOnCycleDetected(t *testing.T) {
	doc := `
services:
  a:
    image: nginx
    depends_on:
      - b
  b:
    image: nginx
    depends_on:
      - c
  c:
    image: nginx
    depends_on:
      - a
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	_, err = BuildPlan(f, "shop", PlanContext{})
	if err == nil {
		t.Fatalf("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %q, want it to mention 'cycle'", err.Error())
	}
	// The cycle must name all three participants.
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("cycle error %q does not name service %q", err.Error(), name)
		}
	}
}

func TestPlanDependsOnUndefinedServiceErrors(t *testing.T) {
	doc := `
services:
  web:
    image: nginx
    depends_on:
      - ghost
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	_, err = BuildPlan(f, "shop", PlanContext{})
	if err == nil {
		t.Fatalf("expected undefined-dependency error, got nil")
	}
	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("error = %q, want it to name the undefined service", err.Error())
	}
}

func TestPlanExposeInternalSplit(t *testing.T) {
	doc := `
services:
  web:
    image: nginx
    expose:
      - "80"
  worker:
    image: nginx
`
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	plan, err := BuildPlan(f, "shop", PlanContext{})
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	byName := map[string]ServicePlan{}
	for _, sp := range plan.Services {
		byName[sp.Name] = sp
	}
	if byName["web"].Internal || byName["web"].Port != 80 {
		t.Errorf("web = %+v, want routed on port 80", byName["web"])
	}
	if !byName["worker"].Internal || byName["worker"].Port != 0 {
		t.Errorf("worker = %+v, want internal with Port 0", byName["worker"])
	}
}

func TestPlanProjectNameNormalized(t *testing.T) {
	doc := "services:\n  web:\n    image: nginx\n"
	f, _, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	plan, err := BuildPlan(f, "My Shop", PlanContext{})
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	if plan.Services[0].Slug != "my-shop-web" {
		t.Errorf("Slug = %q, want my-shop-web", plan.Services[0].Slug)
	}
}

func TestPlanNilFileErrors(t *testing.T) {
	_, err := BuildPlan(nil, "shop", PlanContext{})
	if err == nil {
		t.Fatalf("expected error for nil file, got nil")
	}
}
