package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/manifest"
	"github.com/base-al/basepod/internal/store"
)

// TestApplyManifestDefaultsFillsPortOnFreshApp proves applyManifestDefaults
// fills in an app's Port from the manifest when the app doesn't already
// have one set (Port == 0) — resolving issue #1.
func TestApplyManifestDefaultsFillsPortOnFreshApp(t *testing.T) {
	app := &store.App{Port: 0, MemoryLimitMB: store.DefaultMemoryLimitMB, CPULimit: store.DefaultCPULimit}
	mf := &manifest.Manifest{Port: 9090}

	changed := applyManifestDefaults(app, mf)

	if !changed {
		t.Fatal("expected applyManifestDefaults to report a change")
	}
	if app.Port != 9090 {
		t.Fatalf("app.Port = %d, want 9090", app.Port)
	}
}

// TestApplyManifestDefaultsIgnoresPortWhenAppAlreadyHasOne proves the
// precedence rule: an app's own stored port always wins over the
// manifest's, so a manifest port never silently overrides an operator's
// dashboard-configured value.
func TestApplyManifestDefaultsIgnoresPortWhenAppAlreadyHasOne(t *testing.T) {
	app := &store.App{Port: 3000, MemoryLimitMB: store.DefaultMemoryLimitMB, CPULimit: store.DefaultCPULimit}
	mf := &manifest.Manifest{Port: 9090}

	changed := applyManifestDefaults(app, mf)

	if changed {
		t.Fatal("expected applyManifestDefaults to report no change (app.Port already set)")
	}
	if app.Port != 3000 {
		t.Fatalf("app.Port = %d, want unchanged 3000", app.Port)
	}
}

// TestApplyManifestDefaultsFillsResourcesAtDefaults proves manifest
// resources (memory/cpus) are applied when the app is still at the
// schema's default limits (i.e. never explicitly customized).
func TestApplyManifestDefaultsFillsResourcesAtDefaults(t *testing.T) {
	app := &store.App{
		Port:          8080,
		MemoryLimitMB: store.DefaultMemoryLimitMB,
		CPULimit:      store.DefaultCPULimit,
		PidsLimit:     store.DefaultPidsLimit,
	}
	mf := &manifest.Manifest{Resources: manifest.Resources{Memory: "1g", CPUs: 2.0}}

	changed := applyManifestDefaults(app, mf)

	if !changed {
		t.Fatal("expected applyManifestDefaults to report a change")
	}
	if app.MemoryLimitMB != 1024 {
		t.Errorf("app.MemoryLimitMB = %d, want 1024", app.MemoryLimitMB)
	}
	if app.CPULimit != 2.0 {
		t.Errorf("app.CPULimit = %g, want 2.0", app.CPULimit)
	}
	// The manifest has no pids field — must stay untouched.
	if app.PidsLimit != store.DefaultPidsLimit {
		t.Errorf("app.PidsLimit = %d, want unchanged %d", app.PidsLimit, store.DefaultPidsLimit)
	}
}

// TestApplyManifestDefaultsIgnoresCustomizedResources proves manifest
// resources are never applied once an app's limits have been explicitly
// set away from the schema defaults (e.g. via PATCH /api/v1/apps/{slug}).
func TestApplyManifestDefaultsIgnoresCustomizedResources(t *testing.T) {
	app := &store.App{
		Port:          8080,
		MemoryLimitMB: 2048,
		CPULimit:      3.0,
		PidsLimit:     100,
	}
	mf := &manifest.Manifest{Resources: manifest.Resources{Memory: "1g", CPUs: 2.0}}

	changed := applyManifestDefaults(app, mf)

	if changed {
		t.Fatal("expected applyManifestDefaults to report no change (resources already customized)")
	}
	if app.MemoryLimitMB != 2048 || app.CPULimit != 3.0 || app.PidsLimit != 100 {
		t.Fatalf("app resources mutated: %+v, want unchanged 2048/3.0/100", app)
	}
}

// TestApplyManifestDefaultsNoManifestFieldsNoChange proves an empty
// manifest (e.g. one with only a `name:` field) never reports a change,
// even against a fresh app.
func TestApplyManifestDefaultsNoManifestFieldsNoChange(t *testing.T) {
	app := &store.App{Port: 0, MemoryLimitMB: store.DefaultMemoryLimitMB, CPULimit: store.DefaultCPULimit}
	mf := &manifest.Manifest{Name: "blog"}

	if applyManifestDefaults(app, mf) {
		t.Fatal("expected no change from a manifest with no port/resources fields")
	}
	if app.Port != 0 {
		t.Errorf("app.Port = %d, want unchanged 0", app.Port)
	}
}

// gzipTarWithManifest builds a minimal valid gzipped-tar upload body
// containing a root Containerfile and a root basepod.yaml with the given
// contents, for feeding into DeployBuild.
func gzipTarWithManifest(t *testing.T, manifestYAML string) *bytes.Buffer {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	writeEntry := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("Containerfile", []byte("FROM alpine\n"))
	writeEntry("basepod.yaml", []byte(manifestYAML))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return &gzBuf
}

// TestDeployBuildAppliesManifestPortToFreshApp is the end-to-end
// (DeployBuild) counterpart to TestApplyManifestDefaultsFillsPortOnFreshApp:
// an app created with no port set (Port == 0, bypassing the API's own
// 1-65535 validation — a store-level "fresh" app) gets the manifest's port
// applied and persisted, and the very first rollout's health probe uses
// it.
func TestDeployBuildAppliesManifestPortToFreshApp(t *testing.T) {
	st := openStore(t)
	eng, _, _, prober, _ := newTestEngine(t, st)
	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.DeployBuild(ctx, app, gzipTarWithManifest(t, "port: 9090\n"), builder)
	if err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}
	if dep.Status != "healthy" {
		t.Fatalf("dep.Status = %q, want healthy", dep.Status)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Port != 9090 {
		t.Fatalf("app.Port = %d, want 9090 (from manifest)", gotApp.Port)
	}

	found := false
	for _, call := range prober.calls {
		if call == "bp-blog-1:9090" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prober.calls = %v, want a probe against bp-blog-1:9090", prober.calls)
	}
}

// TestDeployBuildIgnoresManifestPortWhenAppAlreadyConfigured proves the
// same precedence rule end-to-end: an app created with an explicit port
// (as every app created through the real API is) keeps it regardless of
// what basepod.yaml says.
func TestDeployBuildIgnoresManifestPortWhenAppAlreadyConfigured(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 3000)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.DeployBuild(ctx, app, gzipTarWithManifest(t, "port: 9090\n"), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Port != 3000 {
		t.Fatalf("app.Port = %d, want unchanged 3000 (dashboard-configured port wins)", gotApp.Port)
	}
}

// TestDeployBuildAppliesManifestResourcesAtDefaults proves resources
// (memory/cpus) from basepod.yaml are applied end-to-end and carried into
// the very first rollout's CreateSpec.
func TestDeployBuildAppliesManifestResourcesAtDefaults(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.DeployBuild(ctx, app, gzipTarWithManifest(t, "resources:\n  memory: 1g\n  cpus: 2.0\n"), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.MemoryLimitMB != 1024 || gotApp.CPULimit != 2.0 {
		t.Fatalf("app resources = mem=%d cpu=%g, want mem=1024 cpu=2.0", gotApp.MemoryLimitMB, gotApp.CPULimit)
	}

	spec, ok := rt.createdSpecs["bp-blog-1"]
	if !ok {
		t.Fatal("CreateContainer spec not recorded for bp-blog-1")
	}
	wantMemBytes := int64(1024) * 1024 * 1024
	if spec.MemoryLimitBytes != wantMemBytes || spec.CPUQuota != 2.0 {
		t.Fatalf("spec limits = mem=%d cpu=%g, want mem=%d cpu=2.0", spec.MemoryLimitBytes, spec.CPUQuota, wantMemBytes)
	}
}

// TestDeployBuildIgnoresManifestResourcesWhenCustomized proves manifest
// resources never override limits an operator already set explicitly.
func TestDeployBuildIgnoresManifestResourcesWhenCustomized(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppLimits(app.ID, 4096, 4.0, 200); err != nil {
		t.Fatal(err)
	}
	app, err = st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.DeployBuild(ctx, app, gzipTarWithManifest(t, "resources:\n  memory: 1g\n  cpus: 2.0\n"), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.MemoryLimitMB != 4096 || gotApp.CPULimit != 4.0 || gotApp.PidsLimit != 200 {
		t.Fatalf("app resources = %+v, want unchanged 4096/4.0/200", gotApp)
	}
}

// TestDeployBuildAppliesManifestEnvDefaultsOnlyForMissingKeys proves
// basepod.yaml's `env:` defaults are applied only for keys the app
// doesn't already have — an existing plain value and an existing secret
// must both survive untouched, while a genuinely-missing key gets filled
// in from the manifest.
func TestDeployBuildAppliesManifestEnvDefaultsOnlyForMissingKeys(t *testing.T) {
	st := openStore(t)
	ops := &[]string{}
	rt := newFakeRuntime(ops)
	router := &fakeRouter{ops: ops}
	prober := &fakeProber{ops: ops}
	var sealed []string
	encrypt := func(appID int64, key, plaintext string) (string, error) {
		v := "sealed:" + plaintext
		sealed = append(sealed, key)
		return v, nil
	}
	// runRollout injects the app's (now three) env vars into the
	// container spec and refuses to deploy with env vars present but no
	// decrypt func (see Deploy's doc comment) — a no-op identity decrypt
	// is enough here since this test only asserts on what's stored, not
	// on the rolled-out container's plaintext env.
	decrypt := func(appID int64, key, sealedVal string) (string, error) { return sealedVal, nil }
	eng := New(st, rt, router, prober.probe, "apps.localhost", decrypt, encrypt, nil, testInstanceID)
	eng.probeInterval = 0
	eng.probeAttempts = 1

	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	// KEY1 already exists as a plain var with a real (non-manifest) value.
	if err := st.UpsertEnvVar(app.ID, "KEY1", "existing-sealed-value", false); err != nil {
		t.Fatal(err)
	}
	// SECRET1 already exists as a secret — the manifest must never be able
	// to clobber it, even if it names the same key.
	if err := st.UpsertEnvVar(app.ID, "SECRET1", "existing-secret-sealed", true); err != nil {
		t.Fatal(err)
	}

	manifestYAML := "env:\n  KEY1: from-manifest\n  KEY2: new-from-manifest\n  SECRET1: attempted-overwrite\n"
	if _, err := eng.DeployBuild(ctx, app, gzipTarWithManifest(t, manifestYAML), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	vars, err := st.ListEnvVars(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]store.EnvVar, len(vars))
	for _, v := range vars {
		byKey[v.Key] = v
	}

	if got := byKey["KEY1"]; got.ValueEncrypted != "existing-sealed-value" || got.IsSecret {
		t.Errorf("KEY1 = %+v, want untouched existing-sealed-value/non-secret", got)
	}
	if got := byKey["SECRET1"]; got.ValueEncrypted != "existing-secret-sealed" || !got.IsSecret {
		t.Errorf("SECRET1 = %+v, want untouched existing-secret-sealed/secret", got)
	}
	if got := byKey["KEY2"]; got.ValueEncrypted != "sealed:new-from-manifest" || got.IsSecret {
		t.Errorf("KEY2 = %+v, want sealed:new-from-manifest/non-secret (filled in from the manifest)", got)
	}
	for _, k := range sealed {
		if k == "KEY1" || k == "SECRET1" {
			t.Errorf("encrypt was called for existing key %q — a manifest default must never re-seal a key the app already has", k)
		}
	}
}

// TestDeployBuildNoManifestBehavesExactlyAsBefore proves a build context
// with no basepod.yaml at all leaves the app's port, resources, and env
// completely untouched — no accidental behavior change for the (still
// overwhelmingly common) zero-manifest deploy.
func TestDeployBuildNoManifestBehavesExactlyAsBefore(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRuntime := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRuntime, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	dep, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder)
	if err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}
	if dep.Status != "healthy" {
		t.Fatalf("dep.Status = %q, want healthy", dep.Status)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Port != 80 {
		t.Errorf("app.Port = %d, want unchanged 80", gotApp.Port)
	}
	if gotApp.MemoryLimitMB != store.DefaultMemoryLimitMB || gotApp.CPULimit != store.DefaultCPULimit || gotApp.PidsLimit != store.DefaultPidsLimit {
		t.Errorf("app resources = %+v, want unchanged schema defaults", gotApp)
	}
	vars, err := st.ListEnvVars(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 0 {
		t.Errorf("env vars = %+v, want none", vars)
	}
}
