package deploy

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// TestDeployGitAsyncRecordsGitSHAAndSource proves DeployGitAsync creates a
// "git"-sourced deployment row with the caller-supplied commit SHA
// recorded immediately (deployments.git_sha — migration
// 00007_git_sources.sql), and that a background build+rollout still
// completes it to "healthy" exactly like DeployBuildAsync's tarball path.
func TestDeployGitAsyncRecordsGitSHAAndSource(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
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

	dep, err := eng.DeployGitAsync(ctx, app, prepared, builder, "deadbeef01234567", "api")
	if err != nil {
		t.Fatalf("DeployGitAsync: %v", err)
	}
	if dep.Source != "git" {
		t.Fatalf("Source = %q, want git", dep.Source)
	}
	if dep.GitSha != "deadbeef01234567" {
		t.Fatalf("GitSha = %q, want deadbeef01234567", dep.GitSha)
	}
	if dep.TriggerKind != "api" {
		t.Fatalf("TriggerKind = %q, want api", dep.TriggerKind)
	}

	// Recorded on the row synchronously, before the build even runs.
	stored, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GitSha != "deadbeef01234567" || stored.Source != "git" {
		t.Fatalf("stored deployment = %+v, want git_sha/source recorded immediately", stored)
	}

	waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "healthy" {
		t.Fatalf("final.Status = %q, want healthy", final.Status)
	}
	if final.GitSha != "deadbeef01234567" {
		t.Fatalf("final.GitSha = %q, want it to survive the background build", final.GitSha)
	}
}

// fetchResult scripts a fake clone for TestDeployGitCloneAsync*: either a
// gzipped tar (success) or an error (clone failure).
type fetchResult struct {
	tar     io.Reader
	headSHA string
	err     error
}

func fakeFetch(entered chan<- struct{}, release <-chan struct{}, result func() fetchResult) func(ctx context.Context) (io.ReadCloser, string, error) {
	return func(ctx context.Context) (io.ReadCloser, string, error) {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if release != nil {
			<-release
		}
		r := result()
		if r.err != nil {
			return nil, "", r.err
		}
		return io.NopCloser(r.tar), r.headSHA, nil
	}
}

// TestDeployGitCloneAsyncReturnsBeforeCloneFinishes proves
// DeployGitCloneAsync's core contract — the whole point of its existence,
// per its doc comment: a webhook HTTP response must not block on a clone
// that can take minutes. The deployment row is created and returned
// immediately (still "deploying"), well before the fake, deliberately
// blocked, fetch call is ever released.
func TestDeployGitCloneAsyncReturnsBeforeCloneFinishes(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	builder := build.New(&fakeBuildRuntime{logLine: "Successfully built abc\n"}, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fetch := fakeFetch(entered, release, func() fetchResult {
		return fetchResult{tar: gzipTarWithContainerfile(t), headSHA: "resolvedsha123"}
	})

	done := make(chan struct{})
	dep, err := eng.DeployGitCloneAsync(ctx, app, fetch, builder, "webhook", "payloadsha", func(*store.Deployment, error) {
		close(done)
	})
	if err != nil {
		t.Fatalf("DeployGitCloneAsync: %v", err)
	}
	if dep.Status != "deploying" {
		t.Fatalf("dep.Status = %q, want deploying (the clone has not run yet)", dep.Status)
	}
	if dep.GitSha != "payloadsha" {
		t.Fatalf("dep.GitSha = %q, want the caller's initialSHA before the clone resolves its own", dep.GitSha)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background clone never started")
	}

	// Still "deploying" in the store too — the clone is genuinely blocked,
	// not just racily unobserved.
	stored, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "deploying" {
		t.Fatalf("stored status = %q, want deploying while the clone is blocked", stored.Status)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never called")
	}
	waitForBackgroundDeploysOrFail(t, eng, 5*time.Second)

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "healthy" {
		t.Fatalf("final.Status = %q, want healthy", final.Status)
	}
	// The clone's own resolved HEAD supersedes the caller's initial guess —
	// the payload never gets to pick what's actually recorded as built.
	if final.GitSha != "resolvedsha123" {
		t.Fatalf("final.GitSha = %q, want the clone's own resolved HEAD (resolvedsha123)", final.GitSha)
	}
}

// TestDeployGitCloneAsyncCloneFailureFailsDeploymentAndCallsOnDone proves a
// clone failure (a bad branch, an unreachable remote, ...) lands on the
// ALREADY-CREATED deployment row as "failed" — not silently dropped — and
// still invokes onDone with a non-nil error, so a caller (the webhook
// receiver's coalescer) always learns the app's "running" slot is free
// again even when the clone itself never got as far as a build.
func TestDeployGitCloneAsyncCloneFailureFailsDeploymentAndCallsOnDone(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	builder := build.New(&fakeBuildRuntime{}, t.TempDir(), 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	cloneErr := errors.New("gitsource: git failed: unknown branch")
	fetch := fakeFetch(nil, nil, func() fetchResult { return fetchResult{err: cloneErr} })

	var gotErr error
	var gotDep *store.Deployment
	done := make(chan struct{})
	dep, err := eng.DeployGitCloneAsync(ctx, app, fetch, builder, "webhook", "", func(d *store.Deployment, err error) {
		gotDep, gotErr = d, err
		close(done)
	})
	if err != nil {
		t.Fatalf("DeployGitCloneAsync: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never called")
	}

	if gotErr == nil {
		t.Fatal("expected onDone to receive the clone error")
	}
	if gotDep == nil || gotDep.ID != dep.ID {
		t.Fatalf("expected onDone's deployment to be the one DeployGitCloneAsync created, got %+v", gotDep)
	}

	final, err := st.DeploymentByNumber(app.ID, dep.Number)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != "failed" {
		t.Fatalf("final.Status = %q, want failed", final.Status)
	}

	gotApp, err := st.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if gotApp.Status != "error" {
		t.Fatalf("app.Status = %q, want error (no prior healthy deployment)", gotApp.Status)
	}
}
