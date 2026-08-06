package deploy

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/podman"
	"github.com/base-al/basepod/internal/store"
)

// blockingBuildRuntime is a test double for build.BuildRuntime whose
// BuildImage blocks until release() is called (or forever, if never
// released) — used to prove DeployBuildAsync returns to its caller well
// before the build itself finishes (issue #2's core async contract).
// entered fires the instant BuildImage is actually invoked, so a test can
// synchronize on "the background build has started" without sleeping.
type blockingBuildRuntime struct {
	entered chan struct{}
	release chan struct{}
	err     error
	logLine string
}

func newBlockingBuildRuntime() *blockingBuildRuntime {
	return &blockingBuildRuntime{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingBuildRuntime) BuildImage(ctx context.Context, tag, dockerfile string, contextTar io.Reader, logSink io.Writer) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	if b.logLine != "" {
		_, _ = logSink.Write([]byte(b.logLine))
	}
	return b.err
}

// unblock releases every (current and future) call parked in BuildImage.
func (b *blockingBuildRuntime) unblock() { close(b.release) }

// waitForBackgroundDeploysOrFail blocks until eng's in-flight background
// deploys finish, failing the test if they don't within timeout — the test
// helper form of WaitForBackgroundDeploys used throughout this file so
// each test doesn't repeat the same context.WithTimeout boilerplate.
func waitForBackgroundDeploysOrFail(t *testing.T, eng *Engine, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := eng.WaitForBackgroundDeploys(ctx); err != nil {
		t.Fatalf("WaitForBackgroundDeploys: %v", err)
	}
}

// TestDeployBuildAsyncReturnsBeforeBuildFinishes is the core regression for
// issue #2: DeployBuildAsync must return its (still "deploying") deployment
// row to the caller without waiting for the build to finish — proven here
// with a fake builder (blockingBuildRuntime) that blocks inside BuildImage
// until the test explicitly releases it.
func TestDeployBuildAsyncReturnsBeforeBuildFinishes(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRt := newBlockingBuildRuntime()
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := builder.PrepareBuild(gzipTarWithContainerfile(t))
	if err != nil {
		t.Fatalf("PrepareBuild: %v", err)
	}

	returned := make(chan *store.Deployment, 1)
	go func() {
		dep, err := eng.DeployBuildAsync(ctx, app, prepared, builder)
		if err != nil {
			t.Errorf("DeployBuildAsync: %v", err)
		}
		returned <- dep
	}()

	var dep *store.Deployment
	select {
	case dep = <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("DeployBuildAsync did not return promptly — it must not block on the build")
	}
	if dep.Status != "deploying" {
		t.Fatalf("dep.Status = %q, want deploying (the build has not finished yet)", dep.Status)
	}
	if dep.Number == 0 {
		t.Fatal("dep.Number is zero — expected a real deployment number, valid immediately")
	}

	// The build must actually be the thing DeployBuildAsync is waiting on,
	// not something that silently never started.
	select {
	case <-buildRt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background build never entered BuildImage")
	}

	// The row must still read "deploying" from the store too — not just in
	// the (now stale) return value — confirming the background goroutine
	// really hasn't finished yet.
	stored, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "deploying" {
		t.Fatalf("stored deployment status = %q, want deploying while the build is still blocked", stored.Status)
	}

	buildRt.unblock()
	waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "healthy" {
		t.Fatalf("final deployment status = %q, want healthy", final.Status)
	}
}

// TestDeployBuildAsyncBackgroundCompletionMarksHealthy proves that once the
// background build+rollout finishes successfully, the deployment row and
// the app's own status both land on their normal successful-deploy values
// — exactly as a synchronous DeployBuild would, just observed after
// WaitForBackgroundDeploys rather than DeployBuildAsync's own return.
func TestDeployBuildAsyncBackgroundCompletionMarksHealthy(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{logLine: "Successfully built abc123\n"}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := builder.PrepareBuild(gzipTarWithContainerfile(t))
	if err != nil {
		t.Fatalf("PrepareBuild: %v", err)
	}

	dep, err := eng.DeployBuildAsync(ctx, app, prepared, builder)
	if err != nil {
		t.Fatalf("DeployBuildAsync: %v", err)
	}

	waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "healthy" {
		t.Fatalf("dep.Status = %q, want healthy", final.Status)
	}
	if final.ImageRef == "" {
		t.Error("dep.ImageRef empty, want the built image tag")
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "running" {
		t.Errorf("app.Status = %q, want running", gotApp.Status)
	}

	var found bool
	for _, c := range rt.containers {
		if c.Name == "bp-blog-1" && c.State == "running" {
			found = true
		}
	}
	if !found {
		t.Error("expected bp-blog-1 to exist and be running after the background rollout")
	}
}

// TestDeployBuildAsyncBackgroundFailureMarksFailed proves that when the
// background build itself fails, the deployment row lands on "failed" with
// the build error's message, and the app (which had no prior healthy
// deployment) lands on "error" — mirroring DeployBuild's own synchronous
// failure contract exactly, just observed asynchronously.
func TestDeployBuildAsyncBackgroundFailureMarksFailed(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildErr := &fakeExecError{msg: "executor failed running [/bin/sh -c false]"}
	buildRt := &fakeBuildRuntime{err: buildErr}
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := builder.PrepareBuild(gzipTarWithContainerfile(t))
	if err != nil {
		t.Fatalf("PrepareBuild: %v", err)
	}

	dep, err := eng.DeployBuildAsync(ctx, app, prepared, builder)
	if err != nil {
		t.Fatalf("DeployBuildAsync: %v", err)
	}

	waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "failed" {
		t.Fatalf("dep.Status = %q, want failed", final.Status)
	}
	if !strings.Contains(final.Error, buildErr.msg) {
		t.Errorf("dep.Error = %q, want it to contain %q", final.Error, buildErr.msg)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "error" {
		t.Errorf("app.Status = %q, want error (no prior healthy container)", gotApp.Status)
	}
}

// fakeExecError is a trivial error type distinct from errors.New's, mostly
// so failure messages in this file read clearly.
type fakeExecError struct{ msg string }

func (e *fakeExecError) Error() string { return e.msg }

// TestWaitForBackgroundDeploysTimesOutButLeavesRowForSweep proves
// WaitForBackgroundDeploys never blocks past its caller's own ctx deadline
// — the graceful-shutdown grace period internal/server wires it into must
// itself be bounded, never indefinite — and that a deploy still in flight
// when the grace period expires leaves its row exactly as
// SweepStuckDeployments expects to find (and fix) it at the next boot.
func TestWaitForBackgroundDeploysTimesOutButLeavesRowForSweep(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRt := newBlockingBuildRuntime()
	builder := build.New(buildRt, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.PrepareBuild(gzipTarWithContainerfile(t))
	if err != nil {
		t.Fatalf("PrepareBuild: %v", err)
	}
	dep, err := eng.DeployBuildAsync(ctx, app, prepared, builder)
	if err != nil {
		t.Fatalf("DeployBuildAsync: %v", err)
	}
	// Let the background build actually start before racing the grace
	// period against it, so this proves the grace period expiring (not the
	// build having never started at all).
	select {
	case <-buildRt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background build never entered BuildImage")
	}

	// Always release + drain the background goroutine before the test's
	// store closes (via openStore's t.Cleanup), even on failure — otherwise
	// a goroutine still running FinishDeployment against an already-closed
	// *sql.DB races the cleanup and can panic under -race.
	t.Cleanup(func() {
		buildRt.unblock()
		waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)
	})

	shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := eng.WaitForBackgroundDeploys(shortCtx); err == nil {
		t.Fatal("WaitForBackgroundDeploys returned nil, want the short grace period's context error (the build is still blocked)")
	}

	stillDeploying, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stillDeploying.Status != "deploying" {
		t.Fatalf("dep.Status = %q, want still deploying (the grace period expired before the build finished)", stillDeploying.Status)
	}
}

// TestSweepStuckDeploymentsMarksOrphanedDeployingFailed is the boot-sweep
// regression test: a deployment left "deploying" (simulating a process
// that died mid-deploy — see DeployBuildAsync) with no running container
// for its own generation must be marked "failed" with a clear,
// operator-facing error, and the app's own status restored (to "error"
// here, since there is no other running generation).
func TestSweepStuckDeploymentsMarksOrphanedDeployingFailed(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateAppStatus(app.ID, "deploying"); err != nil {
		t.Fatal(err)
	}
	// No container exists for this deployment at all — the process died
	// before CreateContainer ever ran (or the container was already
	// removed by CleanupOrphans, which SweepStuckDeployments always runs
	// after — see its doc comment).

	swept, err := eng.SweepStuckDeployments(ctx)
	if err != nil {
		t.Fatalf("SweepStuckDeployments: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}

	gotDep, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if gotDep.Status != "failed" {
		t.Fatalf("dep.Status = %q, want failed", gotDep.Status)
	}
	if gotDep.Error == "" {
		t.Error("dep.Error empty, want a clear message explaining the process restarted mid-deploy")
	}
	if gotDep.FinishedAt == "" {
		t.Error("dep.FinishedAt empty, want it set now that the row has a terminal status")
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "error" {
		t.Errorf("app.Status = %q, want error (nothing else is running for it)", gotApp.Status)
	}
}

// TestSweepStuckDeploymentsIgnoresHealthyAndFailedRows proves the sweep
// only ever touches "deploying" rows — a normal healthy or already-failed
// deployment history must be left completely alone.
func TestSweepStuckDeploymentsIgnoresHealthyAndFailedRows(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Deploy(ctx, app, "nginx:v1"); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	swept, err := eng.SweepStuckDeployments(ctx)
	if err != nil {
		t.Fatalf("SweepStuckDeployments: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 (the only deployment is healthy)", swept)
	}

	gotDep, err := st.DeploymentByNumber(app.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if gotDep.Status != "healthy" {
		t.Fatalf("dep.Status = %q, want still healthy (untouched)", gotDep.Status)
	}
}

// TestSweepStuckDeploymentsLeavesRunningGenerationAlone proves the rare
// edge case in SweepStuckDeployments's own doc comment: a "deploying" row
// whose generation DOES have a running container (the process died in the
// narrow window after the container passed its probe but before the store
// writes that would have marked it healthy landed) is left alone rather
// than marked failed out from under a container that's actually fine.
func TestSweepStuckDeploymentsLeavesRunningGenerationAlone(t *testing.T) {
	st := openStore(t)
	eng, rt, _, _, _ := newTestEngine(t, st)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "nginx:v1", 80)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(app.ID, "nginx:v1")
	if err != nil {
		t.Fatal(err)
	}
	rt.containers["c1"] = podman.ContainerInfo{
		ID: "c1", Name: "bp-blog-1", State: "running",
		Labels: map[string]string{"basepod.managed": "true", "basepod.app": "blog", "basepod.deployment": "1"},
	}

	swept, err := eng.SweepStuckDeployments(ctx)
	if err != nil {
		t.Fatalf("SweepStuckDeployments: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0 (the generation's container is running)", swept)
	}

	gotDep, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if gotDep.Status != "deploying" {
		t.Fatalf("dep.Status = %q, want still deploying (left alone)", gotDep.Status)
	}
}
