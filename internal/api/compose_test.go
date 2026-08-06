package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// composeFakeDeployer wraps fakeDeployer, additionally recording the
// order (by app slug) DeployExisting/DeployBuildExisting were called in
// and letting a test script one named slug to fail — the two building
// blocks internal/api/compose.go's orchestrator actually drives, so this
// is the fake that exercises dependency ordering and partial-failure
// behavior without a real container runtime.
type composeFakeDeployer struct {
	*fakeDeployer

	mu       sync.Mutex
	order    []string
	failSlug string
}

func newComposeFakeDeployer(st *store.Store) *composeFakeDeployer {
	return &composeFakeDeployer{fakeDeployer: &fakeDeployer{st: st}}
}

func (f *composeFakeDeployer) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *composeFakeDeployer) DeployExisting(ctx context.Context, app *store.App, dep *store.Deployment, imageRef string) (*store.Deployment, error) {
	f.mu.Lock()
	f.order = append(f.order, app.Slug)
	fail := f.failSlug != "" && f.failSlug == app.Slug
	f.mu.Unlock()

	if fail {
		_ = f.st.FinishDeployment(dep.ID, "failed", "scripted failure for "+app.Slug)
		_ = f.st.UpdateAppStatus(app.ID, "error")
		return nil, fmt.Errorf("scripted failure for %s", app.Slug)
	}
	_ = f.st.UpdateAppStatus(app.ID, "running")
	_ = f.st.FinishDeployment(dep.ID, "healthy", "")
	dep.Status = "healthy"
	return dep, nil
}

func (f *composeFakeDeployer) DeployBuildExisting(ctx context.Context, app *store.App, dep *store.Deployment, gzTar io.Reader, builder *build.Builder) (*store.Deployment, error) {
	_, _ = io.Copy(io.Discard, gzTar)
	return f.DeployExisting(ctx, app, dep, "")
}

// waitForCondition polls cond every 5ms until it reports true or timeout
// elapses, failing the test in the latter case. Used to deterministically
// wait for handleComposeUp's background orchestration goroutine (started
// after the 202 response is already written, so there's no synchronous
// hook to block on) to finish, without a fixed sleep.
func waitForCondition(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// postComposeUpload posts a gzipped tar (built from entries, always
// including one named "compose.yaml") to /api/v1/compose/up with the
// given query string ("" for none) and decodes the JSON response into
// out (skipped if out is nil).
func postComposeUpload(t *testing.T, srv string, token, query string, composeYAML string, extra ...tarEntry) *http.Response {
	t.Helper()
	entries := append([]tarEntry{{name: "compose.yaml", body: composeYAML}}, extra...)
	body := gzipTarBody(t, entries)
	url := srv + "/api/v1/compose/up"
	if query != "" {
		url += "?" + query
	}
	return postTarball(t, url, token, "application/gzip", body)
}

// TestComposeApplyCreatesAppsInDependencyOrder proves a real (non-dry-run)
// apply creates one app per service, in the plan's topological
// (depends_on) order — a service with no `expose:` (db) is created
// internal (no route, no probe target — see App.Internal's doc comment),
// one with `expose:` (web) is routed — and that the background
// orchestrator actually deploys them in that same order.
func TestComposeApplyCreatesAppsInDependencyOrder(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: shop\n" +
		"services:\n" +
		"  db:\n" +
		"    image: postgres:16\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n" +
		"    depends_on: [db]\n"

	var resp composePlanResponse
	httpResp := postComposeUpload(t, srv.URL, session.Token, "", yaml)
	if httpResp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &resp)

	if resp.Project != "shop" {
		t.Fatalf("project = %q, want shop", resp.Project)
	}
	if len(resp.Services) != 2 {
		t.Fatalf("expected 2 services, got %+v", resp.Services)
	}
	if resp.Services[0].Name != "db" || resp.Services[1].Name != "web" {
		t.Fatalf("expected dependency order [db, web], got [%s, %s]", resp.Services[0].Name, resp.Services[1].Name)
	}
	if !resp.Services[0].Internal {
		t.Fatalf("expected db to be internal, got %+v", resp.Services[0])
	}
	if resp.Services[1].Internal || resp.Services[1].Port != 80 {
		t.Fatalf("expected web to be routed on port 80, got %+v", resp.Services[1])
	}
	if resp.Services[0].DeploymentNumber == 0 || resp.Services[1].DeploymentNumber == 0 {
		t.Fatalf("expected real deployment numbers in the 202 response, got %+v", resp.Services)
	}

	dbApp, err := st.AppBySlug("shop-db")
	if err != nil {
		t.Fatalf("shop-db: %v", err)
	}
	if !dbApp.Internal || dbApp.ComposeProject != "shop" || dbApp.ComposeService != "db" {
		t.Fatalf("unexpected shop-db app: %+v", dbApp)
	}
	webApp, err := st.AppBySlug("shop-web")
	if err != nil {
		t.Fatalf("shop-web: %v", err)
	}
	if webApp.Internal || webApp.Port != 80 {
		t.Fatalf("unexpected shop-web app: %+v", webApp)
	}

	waitForCondition(t, 2*time.Second, "orchestrator to deploy both services", func() bool {
		return len(dep.callOrder()) == 2
	})
	order := dep.callOrder()
	if order[0] != "shop-db" || order[1] != "shop-web" {
		t.Fatalf("deployer called in order %v, want [shop-db shop-web]", order)
	}
}

// TestComposeReapplyUpdatesNotDuplicates proves re-applying the same
// compose project updates the existing apps (Action "update", same app
// row/ID) instead of erroring or creating duplicates.
func TestComposeReapplyUpdatesNotDuplicates(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: blog\n" +
		"services:\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n"

	var first composePlanResponse
	resp := postComposeUpload(t, srv.URL, session.Token, "", yaml)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first apply: status = %d, want 202", resp.StatusCode)
	}
	decodeInto(t, resp, &first)
	if first.Services[0].Action != "create" {
		t.Fatalf("first apply: action = %q, want create", first.Services[0].Action)
	}

	before, err := st.AppBySlug("blog-web")
	if err != nil {
		t.Fatal(err)
	}

	appsBefore, err := st.ListApps()
	if err != nil {
		t.Fatal(err)
	}

	var second composePlanResponse
	resp = postComposeUpload(t, srv.URL, session.Token, "", yaml)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second apply: status = %d, want 202", resp.StatusCode)
	}
	decodeInto(t, resp, &second)
	if second.Services[0].Action != "update" {
		t.Fatalf("second apply: action = %q, want update", second.Services[0].Action)
	}

	after, err := st.AppBySlug("blog-web")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatalf("re-apply created a different app row: before ID=%d after ID=%d", before.ID, after.ID)
	}

	appsAfter, err := st.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(appsAfter) != len(appsBefore) {
		t.Fatalf("expected no new app rows on re-apply, had %d now have %d", len(appsBefore), len(appsAfter))
	}
}

// TestComposeRemovedServiceReportedAsOrphanAndLeftRunning proves a
// service present on a prior apply but absent from the file being
// applied now is reported as an orphan and NOT deleted (still resolvable
// by slug afterward) — deleting a user's workload as a side effect of an
// unrelated apply is not acceptable.
func TestComposeRemovedServiceReportedAsOrphanAndLeftRunning(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	full := "name: shop\n" +
		"services:\n" +
		"  db:\n" +
		"    image: postgres:16\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n"

	resp := postComposeUpload(t, srv.URL, session.Token, "", full)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first apply: status = %d, want 202", resp.StatusCode)
	}

	waitForCondition(t, 2*time.Second, "first apply to finish deploying both services", func() bool {
		return len(dep.callOrder()) == 2
	})

	// Second apply: "db" removed from the file entirely.
	webOnly := "name: shop\n" +
		"services:\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n"

	var second composePlanResponse
	resp = postComposeUpload(t, srv.URL, session.Token, "", webOnly)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second apply: status = %d, want 202", resp.StatusCode)
	}
	decodeInto(t, resp, &second)

	if len(second.Orphans) != 1 || second.Orphans[0] != "shop-db" {
		t.Fatalf("expected orphans = [shop-db], got %v", second.Orphans)
	}

	// Still resolvable — never deleted.
	dbApp, err := st.AppBySlug("shop-db")
	if err != nil {
		t.Fatalf("shop-db should still exist: %v", err)
	}
	if dbApp.Status != "running" {
		t.Fatalf("shop-db status = %q, want running (left alone, not touched)", dbApp.Status)
	}
	if dep.removeCalled {
		t.Fatal("RemoveApp should never be called for an orphaned service")
	}
}

// TestComposeVolumeBearingServiceGetsReplaceStrategy proves a service
// declaring a named volume automatically gets deploy_strategy=replace
// (Task 6's rationale: a zero-downtime overlap would give two containers
// write access to the same volume), with the reason surfaced as a
// warning rather than applied silently.
func TestComposeVolumeBearingServiceGetsReplaceStrategy(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: dbproj\n" +
		"services:\n" +
		"  db:\n" +
		"    image: postgres:16\n" +
		"    volumes:\n" +
		"      - data:/var/lib/postgresql/data\n" +
		"volumes:\n" +
		"  data:\n"

	var resp composePlanResponse
	httpResp := postComposeUpload(t, srv.URL, session.Token, "", yaml)
	if httpResp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &resp)

	if len(resp.Services) != 1 {
		t.Fatalf("expected 1 service, got %+v", resp.Services)
	}
	svc := resp.Services[0]
	if svc.DeployStrategy != store.DeployStrategyReplace {
		t.Fatalf("deploy_strategy = %q, want replace", svc.DeployStrategy)
	}
	foundReason := false
	for _, w := range svc.Warnings {
		if strings.Contains(w, "replace") || strings.Contains(w, "volume") {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("expected a warning explaining the replace strategy, got %v", svc.Warnings)
	}

	app, err := st.AppBySlug("dbproj-db")
	if err != nil {
		t.Fatal(err)
	}
	if app.DeployStrategy != store.DeployStrategyReplace {
		t.Fatalf("stored deploy_strategy = %q, want replace", app.DeployStrategy)
	}

	volumes, err := st.ListVolumes(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || volumes[0].Name != "data" || volumes[0].ContainerPath != "/var/lib/postgresql/data" {
		t.Fatalf("unexpected volumes: %+v", volumes)
	}
}

// TestComposeCrossProjectAliasConflictRejected proves a second compose
// project trying to claim a bare service-name network alias already
// claimed by a different project's service is rejected with a clear
// message, rather than silently colliding (audit finding M1's
// regression risk).
func TestComposeCrossProjectAliasConflictRejected(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	first := "name: proj-a\n" +
		"services:\n" +
		"  cache:\n" +
		"    image: redis:7\n"
	resp := postComposeUpload(t, srv.URL, session.Token, "", first)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first apply: status = %d, want 202", resp.StatusCode)
	}

	second := "name: proj-b\n" +
		"services:\n" +
		"  cache:\n" +
		"    image: redis:7\n"
	var errBody errorResponse
	resp = postComposeUpload(t, srv.URL, session.Token, "", second)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("second apply: status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "compose_plan_error" {
		t.Fatalf("error code = %q, want compose_plan_error", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, "cache") || !strings.Contains(errBody.Error.Message, "already claimed") {
		t.Fatalf("expected message to explain the alias conflict, got %q", errBody.Error.Message)
	}

	// Never created — the whole apply must fail before touching the store.
	if _, err := st.AppBySlug("proj-b-cache"); err == nil {
		t.Fatal("expected proj-b-cache to not exist after a rejected apply")
	}
}

// TestComposePartialFailureLeavesEarlierServicesHealthy proves that when
// a middle service in a dependency chain fails, earlier services stay up
// (never rolled back) and later ones are never attempted — their
// already-created deployment rows are instead finished "failed" with an
// "aborted: <service> failed" message naming the cause, so polling any
// of the three deployments tells the whole story.
func TestComposePartialFailureLeavesEarlierServicesHealthy(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	dep.failSlug = "chain-b"
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: chain\n" +
		"services:\n" +
		"  a:\n" +
		"    image: alpine:3\n" +
		"  b:\n" +
		"    image: alpine:3\n" +
		"    depends_on: [a]\n" +
		"  c:\n" +
		"    image: alpine:3\n" +
		"    depends_on: [b]\n"

	var resp composePlanResponse
	httpResp := postComposeUpload(t, srv.URL, session.Token, "", yaml)
	if httpResp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &resp)
	if len(resp.Services) != 3 {
		t.Fatalf("expected 3 services, got %+v", resp.Services)
	}
	numbers := map[string]int{}
	for _, s := range resp.Services {
		numbers[s.Name] = s.DeploymentNumber
	}

	waitForCondition(t, 2*time.Second, "orchestrator to reach service b (and stop)", func() bool {
		order := dep.callOrder()
		return len(order) == 2
	})
	// Give the orchestrator a moment to finish c's bookkeeping (marking
	// it aborted) after b's failure — the call-order slice above only
	// proves the deployer stopped being invoked, not that the
	// aborted-remainder bookkeeping has landed yet.
	waitForCondition(t, 2*time.Second, "service c's deployment to be finished aborted", func() bool {
		appC, err := st.AppBySlug("chain-c")
		if err != nil {
			return false
		}
		d, err := st.DeploymentByNumber(appC.ID, numbers["c"])
		if err != nil {
			return false
		}
		return d.Status == "failed"
	})

	order := dep.callOrder()
	if len(order) != 2 || order[0] != "chain-a" || order[1] != "chain-b" {
		t.Fatalf("deployer called with %v, want exactly [chain-a chain-b] (c must never be attempted)", order)
	}

	appA, err := st.AppBySlug("chain-a")
	if err != nil {
		t.Fatal(err)
	}
	depA, err := st.DeploymentByNumber(appA.ID, numbers["a"])
	if err != nil {
		t.Fatal(err)
	}
	if depA.Status != "healthy" || appA.Status != "running" {
		t.Fatalf("service a: deployment status=%q app status=%q, want healthy/running (earlier success must survive)", depA.Status, appA.Status)
	}

	appB, err := st.AppBySlug("chain-b")
	if err != nil {
		t.Fatal(err)
	}
	depB, err := st.DeploymentByNumber(appB.ID, numbers["b"])
	if err != nil {
		t.Fatal(err)
	}
	if depB.Status != "failed" || !strings.Contains(depB.Error, "scripted failure") {
		t.Fatalf("service b: deployment = %+v, want failed with the scripted error", depB)
	}

	appC, err := st.AppBySlug("chain-c")
	if err != nil {
		t.Fatal(err)
	}
	depC, err := st.DeploymentByNumber(appC.ID, numbers["c"])
	if err != nil {
		t.Fatal(err)
	}
	if depC.Status != "failed" || !strings.Contains(depC.Error, "aborted") || !strings.Contains(depC.Error, "b") {
		t.Fatalf("service c: deployment = %+v, want failed naming b's abort, never attempted", depC)
	}
}

// TestComposeMalformedYAMLReturns422WithParsersMessage proves a compose
// file that fails to parse is rejected 422 "compose_parse_error" with the
// exact message internal/compose.Parse produced — not a generic wrapper.
func TestComposeMalformedYAMLReturns422WithParsersMessage(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	var errBody errorResponse
	resp := postComposeUpload(t, srv.URL, session.Token, "project=whatever", "not a mapping")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	decodeInto(t, resp, &errBody)
	if errBody.Error.Code != "compose_parse_error" {
		t.Fatalf("error code = %q, want compose_parse_error", errBody.Error.Code)
	}
	if !strings.Contains(errBody.Error.Message, "compose:") || !strings.Contains(errBody.Error.Message, "invalid yaml") {
		t.Fatalf("expected the parser's own error message, got %q", errBody.Error.Message)
	}
}

// TestComposeDryRunMutatesNothing proves a ?dry_run=1 apply returns the
// full plan (200, not 202) without creating any app, deployment, or
// volume row.
func TestComposeDryRunMutatesNothing(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: preview\n" +
		"services:\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n"

	appsBefore, err := st.ListApps()
	if err != nil {
		t.Fatal(err)
	}

	var resp composePlanResponse
	httpResp := postComposeUpload(t, srv.URL, session.Token, "dry_run=1", yaml)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &resp)
	if !resp.DryRun {
		t.Fatal("expected dry_run:true in the response")
	}
	if len(resp.Services) != 1 || resp.Services[0].Action != "create" {
		t.Fatalf("unexpected dry-run services: %+v", resp.Services)
	}
	if resp.Services[0].DeploymentNumber != 0 {
		t.Fatalf("dry run must not create a deployment, got number %d", resp.Services[0].DeploymentNumber)
	}

	appsAfter, err := st.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(appsAfter) != len(appsBefore) {
		t.Fatalf("dry run created app rows: before=%d after=%d", len(appsBefore), len(appsAfter))
	}
	if len(dep.callOrder()) != 0 {
		t.Fatalf("dry run must never call the deployer, got %v", dep.callOrder())
	}
}

// TestComposeDryRunPreviewIncludesImageBuildVolumesEnvKeysButNeverEnvValues
// proves issue #12's fix: a dry-run preview carries enough for an
// operator to review a plan before it mutates anything — each service's
// image ref, build context (for a `build:` service), named volumes, and
// env var keys — while NEVER leaking an env var's value anywhere in the
// response body, even though the uploaded compose file sets one to an
// unmistakably secret-shaped string. This is the response's one hard
// security property (env values are write-only everywhere else in the
// product; a preview must not be the exception), checked twice: once via
// the decoded struct (EnvKeys correct, values absent) and once as a raw
// substring search over the whole response body (defense in depth against
// a value leaking through some field this test didn't think to check).
func TestComposeDryRunPreviewIncludesImageBuildVolumesEnvKeysButNeverEnvValues(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	const secretValue = "sk_live_dry_run_should_never_leak_this_51fd2c"

	yaml := "name: preview-full\n" +
		"services:\n" +
		"  db:\n" +
		"    image: postgres:16\n" +
		"    environment:\n" +
		"      POSTGRES_PASSWORD: " + secretValue + "\n" +
		"      POSTGRES_USER: app\n" +
		"    volumes:\n" +
		"      - data:/var/lib/postgresql/data\n" +
		"  web:\n" +
		"    build:\n" +
		"      context: ./web\n" +
		"      dockerfile: Dockerfile.prod\n" +
		"    expose: [80]\n" +
		"    depends_on: [db]\n" +
		"volumes:\n" +
		"  data:\n"

	httpResp := postComposeUpload(t, srv.URL, session.Token, "dry_run=1", yaml)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", httpResp.StatusCode)
	}
	rawBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	// Defense in depth: the secret value must not appear ANYWHERE in the
	// raw response bytes, regardless of which field this test does or
	// doesn't otherwise assert on.
	if strings.Contains(string(rawBody), secretValue) {
		t.Fatalf("compose dry-run preview leaked an env var value into the response body: %s", rawBody)
	}

	var resp composePlanResponse
	if err := json.Unmarshal(rawBody, &resp); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if len(resp.Services) != 2 {
		t.Fatalf("expected 2 services, got %+v", resp.Services)
	}

	db := resp.Services[0]
	if db.Name != "db" {
		t.Fatalf("expected db first (dependency order), got %+v", resp.Services)
	}
	if db.Image != "postgres:16" {
		t.Fatalf("db.Image = %q, want postgres:16", db.Image)
	}
	if db.Build != nil {
		t.Fatalf("db.Build = %+v, want nil (db has no build: block)", db.Build)
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Name != "data" || db.Volumes[0].Path != "/var/lib/postgresql/data" {
		t.Fatalf("db.Volumes = %+v, want [{data /var/lib/postgresql/data}]", db.Volumes)
	}
	wantKeys := []string{"POSTGRES_PASSWORD", "POSTGRES_USER"}
	if len(db.EnvKeys) != len(wantKeys) || db.EnvKeys[0] != wantKeys[0] || db.EnvKeys[1] != wantKeys[1] {
		t.Fatalf("db.EnvKeys = %v, want %v (keys only, sorted)", db.EnvKeys, wantKeys)
	}

	web := resp.Services[1]
	if web.Name != "web" {
		t.Fatalf("expected web second, got %+v", resp.Services)
	}
	if web.Image != "" {
		t.Fatalf("web.Image = %q, want empty (web is a build service)", web.Image)
	}
	if web.Build == nil || web.Build.Context != "./web" || web.Build.Dockerfile != "Dockerfile.prod" {
		t.Fatalf("web.Build = %+v, want {./web Dockerfile.prod}", web.Build)
	}
	if len(web.Volumes) != 0 {
		t.Fatalf("web.Volumes = %+v, want none", web.Volumes)
	}
	if len(web.EnvKeys) != 0 {
		t.Fatalf("web.EnvKeys = %v, want none", web.EnvKeys)
	}
}

// TestComposeProjectQueryOverridesFileName proves ?project= takes
// precedence over the compose file's own top-level `name:` — and that
// omitting both is a 422.
func TestComposeProjectQueryOverridesFileName(t *testing.T) {
	st := newTestStore(t)
	dep := newComposeFakeDeployer(st)
	srv := newTestServer(t, st, dep, &fakeRoutesApplier{})
	_, session := login(t, srv, testPassword)

	yaml := "name: filename-project\n" +
		"services:\n" +
		"  web:\n" +
		"    image: nginx:alpine\n" +
		"    expose: [80]\n"

	var resp composePlanResponse
	httpResp := postComposeUpload(t, srv.URL, session.Token, "project=query-project&dry_run=1", yaml)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &resp)
	if resp.Project != "query-project" {
		t.Fatalf("project = %q, want query-project (the query param must win)", resp.Project)
	}

	noName := "services:\n  web:\n    image: nginx:alpine\n    expose: [80]\n"
	var errBody errorResponse
	httpResp = postComposeUpload(t, srv.URL, session.Token, "", noName)
	if httpResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no project: status = %d, want 422", httpResp.StatusCode)
	}
	decodeInto(t, httpResp, &errBody)
	if errBody.Error.Code != "validation" {
		t.Fatalf("no project: error code = %q, want validation", errBody.Error.Code)
	}
}
