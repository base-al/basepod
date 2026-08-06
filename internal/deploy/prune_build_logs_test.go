package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/base-al/basepod/internal/build"
	"github.com/base-al/basepod/internal/store"
)

// seedBuildLogs creates n.log files (empty contents — pruneBuildLogs never
// reads them) under dir for every number in numbers, and returns dir.
func seedBuildLogs(t *testing.T, dir string, numbers []int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range numbers {
		path := filepath.Join(dir, fmt.Sprintf("%d.log", n))
		if err := os.WriteFile(path, []byte("build output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// remainingLogNumbers returns the sorted set of "<n>.log" numbers still
// present in dir.
func remainingLogNumbers(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d.log", &n); err == nil {
			got = append(got, n)
		}
	}
	sort.Ints(got)
	return got
}

// TestPruneBuildLogsNoopWithoutBuildLogPath proves a deployment with no
// recorded build log (a registry-image deploy, or a rollback that reused
// an existing image without building) is a complete no-op: nothing on
// disk is touched, and no directory needs to exist at all.
func TestPruneBuildLogsNoopWithoutBuildLogPath(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	// No BuildLogPath at all — must not panic or try to list "" as a
	// directory.
	eng.pruneBuildLogs(&store.Deployment{Number: 1})
}

// TestPruneBuildLogsNoopUnderRetentionLimit proves pruneBuildLogs removes
// nothing when there are retainBuildLogs or fewer numbered log files.
func TestPruneBuildLogsNoopUnderRetentionLimit(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	dir := seedBuildLogs(t, filepath.Join(t.TempDir(), "apps", "blog", "builds"), []int{1, 2, 3, 4, 5})
	dep := &store.Deployment{Number: 5, BuildLogPath: filepath.Join(dir, "5.log")}

	eng.pruneBuildLogs(dep)

	got := remainingLogNumbers(t, dir)
	want := []int{1, 2, 3, 4, 5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remaining logs = %v, want %v (no pruning under the retention limit)", got, want)
	}
}

// TestPruneBuildLogsKeepsTopNRemovesRest proves pruneBuildLogs keeps the
// retainBuildLogs highest-numbered logs and removes the rest.
func TestPruneBuildLogsKeepsTopNRemovesRest(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	dir := seedBuildLogs(t, filepath.Join(t.TempDir(), "apps", "blog", "builds"), []int{1, 2, 3, 4, 5, 6, 7})
	dep := &store.Deployment{Number: 7, BuildLogPath: filepath.Join(dir, "7.log")}

	eng.pruneBuildLogs(dep)

	got := remainingLogNumbers(t, dir)
	want := []int{3, 4, 5, 6, 7} // top 5 (retainBuildLogs == retainBuiltImages == 5)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remaining logs = %v, want %v", got, want)
	}
}

// TestPruneBuildLogsNeverRemovesCurrentDeployment proves the log for
// dep.Number is protected from removal even when it falls outside the
// numeric top N — mirroring pruneBuiltImages's currentImageRef
// protection, and directly covering the "never delete the log of the
// currently-running or in-flight deployment" requirement.
func TestPruneBuildLogsNeverRemovesCurrentDeployment(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	// Seven newer logs (10-16) plus the "current" one, numbered 1 — well
	// outside the numeric top 5 on its own.
	dir := seedBuildLogs(t, filepath.Join(t.TempDir(), "apps", "blog", "builds"),
		[]int{1, 10, 11, 12, 13, 14, 15, 16})
	dep := &store.Deployment{Number: 1, BuildLogPath: filepath.Join(dir, "1.log")}

	eng.pruneBuildLogs(dep)

	got := remainingLogNumbers(t, dir)
	want := []int{1, 12, 13, 14, 15, 16} // protected {1} ∪ top-5 {12..16}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remaining logs = %v, want %v", got, want)
	}
}

// TestPruneBuildLogsIgnoresNonNumericFiles proves a stray non-"<n>.log"
// file in the builds directory is left alone entirely, exactly like
// pruneBuiltImages leaves a non-numeric image tag (e.g. "latest") alone.
func TestPruneBuildLogsIgnoresNonNumericFiles(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	dir := seedBuildLogs(t, filepath.Join(t.TempDir(), "apps", "blog", "builds"), []int{1, 2, 3, 4, 5, 6, 7})
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{Number: 7, BuildLogPath: filepath.Join(dir, "7.log")}

	eng.pruneBuildLogs(dep)

	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("notes.txt should never be touched (not a numbered log): %v", err)
	}
}

// TestPruneBuildLogsMissingDirectoryIsNotAnError proves a BuildLogPath
// whose directory doesn't exist on disk at all (e.g. it was already
// cleaned up by an app deletion racing this call) is treated as "nothing
// to prune", not a failure — pruneBuildLogs must never panic or be
// mistaken for a real I/O error in that case.
func TestPruneBuildLogsMissingDirectoryIsNotAnError(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)

	dep := &store.Deployment{
		Number:       1,
		BuildLogPath: filepath.Join(t.TempDir(), "apps", "gone", "builds", "1.log"),
	}
	eng.pruneBuildLogs(dep) // must not panic
}

// TestDeployBuildTriggersLogRetentionAfterSuccess is an integration-level
// regression test proving pruneBuildLogs is actually wired into
// runRollout's success path (alongside pruneBuiltImages): with more than
// retainBuildLogs pre-existing build logs on disk for an app, a single
// successful DeployBuild prunes the oldest down to retainBuildLogs
// automatically, keeping the just-created deployment's own log.
func TestDeployBuildTriggersLogRetentionAfterSuccess(t *testing.T) {
	st := openStore(t)
	eng, _, _, _, _ := newTestEngine(t, st)
	buildRt := &fakeBuildRuntime{}
	dataDir := t.TempDir()
	builder := build.New(buildRt, dataDir, 2)
	ctx := context.Background()

	app, err := st.CreateApp("blog", "", 80)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-seed 6 older build logs (well above the deployment number this
	// DeployBuild call will actually produce, which is 1 for a fresh
	// app's first deployment), mirroring
	// TestDeployBuildTriggersRetentionAfterSuccess's own image-tag setup.
	logsDir := filepath.Join(dataDir, "apps", "blog", "builds")
	seedBuildLogs(t, logsDir, []int{10, 11, 12, 13, 14, 15})

	if _, err := eng.DeployBuild(ctx, app, gzipTarWithContainerfile(t), builder); err != nil {
		t.Fatalf("DeployBuild: %v", err)
	}

	got := remainingLogNumbers(t, logsDir)
	want := []int{1, 11, 12, 13, 14, 15} // this deploy's own log (1) + top 5 of the seeded ones
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("remaining logs = %v, want %v", got, want)
	}
}
