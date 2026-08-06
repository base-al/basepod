package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSettings(t *testing.T) {
	s := open(t)
	if v, _ := s.Setting("root_domain"); v != "" {
		t.Fatal("unset setting must be empty")
	}
	s.SetSetting("root_domain", "apps.example.com")
	s.SetSetting("root_domain", "apps2.example.com") // upsert
	if v, _ := s.Setting("root_domain"); v != "apps2.example.com" {
		t.Fatalf("got %q", v)
	}
}

func TestSessionExpiry(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	s.CreateSession(uid, "th1", time.Now().Add(time.Hour))
	s.CreateSession(uid, "th2", time.Now().Add(-time.Hour))
	if u, err := s.UserBySessionTokenHash("th1"); err != nil || u.Email != "a@b.c" {
		t.Fatalf("valid session rejected: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("th2"); err != ErrNotFound {
		t.Fatalf("expired session accepted: %v", err)
	}
}

func TestDeleteSessionByTokenHash(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	s.CreateSession(uid, "th1", time.Now().Add(time.Hour))
	s.CreateSession(uid, "th2", time.Now().Add(time.Hour))

	if err := s.DeleteSessionByTokenHash("th1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := s.UserBySessionTokenHash("th1"); err != ErrNotFound {
		t.Fatalf("deleted session still valid: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("th2"); err != nil {
		t.Fatalf("unrelated session was also removed: %v", err)
	}

	// Deleting an absent hash is a no-op success, not an error.
	if err := s.DeleteSessionByTokenHash("does-not-exist"); err != nil {
		t.Fatalf("delete of absent hash: %v", err)
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	s.CreateSession(uid, "live1", time.Now().Add(time.Hour))
	s.CreateSession(uid, "live2", time.Now().Add(time.Hour))
	s.CreateSession(uid, "dead1", time.Now().Add(-time.Hour))
	s.CreateSession(uid, "dead2", time.Now().Add(-2*time.Hour))

	n, err := s.PruneExpiredSessions()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned %d rows, want 2", n)
	}

	if _, err := s.UserBySessionTokenHash("live1"); err != nil {
		t.Fatalf("live session was pruned: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("live2"); err != nil {
		t.Fatalf("live session was pruned: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("dead1"); err != ErrNotFound {
		t.Fatalf("expired session survived prune: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("dead2"); err != ErrNotFound {
		t.Fatalf("expired session survived prune: %v", err)
	}

	// Second run finds nothing left to prune.
	n, err = s.PruneExpiredSessions()
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if n != 0 {
		t.Fatalf("second prune removed %d rows, want 0", n)
	}
}

func TestListSessions(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	s.CreateSession(uid, "th1", time.Now().Add(time.Hour))
	s.CreateSession(uid, "th2", time.Now().Add(2*time.Hour))

	sessions, err := s.ListSessions(uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	hashes := map[string]bool{sessions[0].TokenHash: true, sessions[1].TokenHash: true}
	if !hashes["th1"] || !hashes["th2"] {
		t.Fatalf("unexpected session hashes: %+v", sessions)
	}
	for _, sess := range sessions {
		if sess.ID == 0 || sess.CreatedAt == "" || sess.ExpiresAt == "" {
			t.Fatalf("session missing expected fields: %+v", sess)
		}
	}
}

func TestListSessionsEmptyForUnknownUser(t *testing.T) {
	s := open(t)
	sessions, err := s.ListSessions(999)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("got %d sessions, want 0", len(sessions))
	}
}

// TestDeleteSessionOwnerScoping proves DeleteSession is scoped to the
// caller's own userID: user A cannot delete user B's session by id, even
// though the id alone would otherwise be enough to find it.
func TestDeleteSessionOwnerScoping(t *testing.T) {
	s := open(t)
	uidA, _ := s.CreateUser("a@example.com", "A", "hash", true)
	uidB, _ := s.CreateUser("b@example.com", "B", "hash", false)

	s.CreateSession(uidB, "b-token", time.Now().Add(time.Hour))
	bSessions, err := s.ListSessions(uidB)
	if err != nil {
		t.Fatal(err)
	}
	if len(bSessions) != 1 {
		t.Fatalf("got %d sessions for B, want 1", len(bSessions))
	}
	bSessionID := bSessions[0].ID

	// A cannot delete B's session.
	if err := s.DeleteSession(bSessionID, uidA); err != ErrNotFound {
		t.Fatalf("cross-owner delete: got %v, want ErrNotFound", err)
	}
	if _, err := s.UserBySessionTokenHash("b-token"); err != nil {
		t.Fatalf("B's session was deleted by A's request: %v", err)
	}

	// B can delete their own session.
	if err := s.DeleteSession(bSessionID, uidB); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("b-token"); err != ErrNotFound {
		t.Fatal("expected B's session to be gone after B deleted it")
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	if err := s.DeleteSession(999, uid); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteSessionsExcept(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)
	s.CreateSession(uid, "keep", time.Now().Add(time.Hour))
	s.CreateSession(uid, "other1", time.Now().Add(time.Hour))
	s.CreateSession(uid, "other2", time.Now().Add(time.Hour))

	// Another user's session must never be touched by this call.
	otherUID, _ := s.CreateUser("z@b.c", "Z", "hash", false)
	s.CreateSession(otherUID, "unrelated", time.Now().Add(time.Hour))

	n, err := s.DeleteSessionsExcept(uid, "keep")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d sessions, want 2", n)
	}

	if _, err := s.UserBySessionTokenHash("keep"); err != nil {
		t.Fatalf("kept session was deleted: %v", err)
	}
	if _, err := s.UserBySessionTokenHash("other1"); err != ErrNotFound {
		t.Fatal("expected other1 to be deleted")
	}
	if _, err := s.UserBySessionTokenHash("other2"); err != ErrNotFound {
		t.Fatal("expected other2 to be deleted")
	}
	if _, err := s.UserBySessionTokenHash("unrelated"); err != nil {
		t.Fatalf("unrelated user's session was deleted: %v", err)
	}
}

func TestUpdatePassword(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "old-hash", true)

	if err := s.UpdatePassword(uid, "new-hash"); err != nil {
		t.Fatal(err)
	}

	u, err := s.UserByEmail("a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	if u.PasswordHash != "new-hash" {
		t.Fatalf("PasswordHash = %q, want %q", u.PasswordHash, "new-hash")
	}
}

func TestDeploymentNumbering(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d1, _ := s.CreateDeployment(app.ID, "nginx:alpine")
	d2, _ := s.CreateDeployment(app.ID, "nginx:alpine")
	if d1.Number != 1 || d2.Number != 2 {
		t.Fatalf("numbers: %d, %d", d1.Number, d2.Number)
	}
	if d1.StartedAt == "" || d2.StartedAt == "" {
		t.Fatalf("expected CreateDeployment to set started_at: d1=%+v d2=%+v", d1, d2)
	}
	s.FinishDeployment(d2.ID, "healthy", "")
	ds, _ := s.ListDeployments(app.ID)
	if len(ds) != 2 || ds[0].Number != 2 || ds[0].Status != "healthy" {
		t.Fatalf("list wrong: %+v", ds)
	}
	if ds[0].StartedAt == "" {
		t.Fatalf("expected started_at on listed deployment, got %+v", ds[0])
	}
	if ds[0].FinishedAt == "" {
		t.Fatalf("expected finished_at on a finished deployment, got %+v", ds[0])
	}
	if ds[1].FinishedAt != "" {
		t.Fatalf("expected empty finished_at on a still-deploying deployment, got %+v", ds[1])
	}
}

// TestCreateDeploymentDefaultsToImageSource proves the original
// CreateDeployment (used by the registry-pull path) still defaults
// Source to "image" and TriggerKind to "api", now that it delegates to
// CreateDeploymentFull.
func TestCreateDeploymentDefaultsToImageSource(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d, err := s.CreateDeployment(app.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "image" || d.TriggerKind != "api" {
		t.Fatalf("d = %+v, want Source=image TriggerKind=api", d)
	}
	if d.BuildLogPath != "" {
		t.Fatalf("d.BuildLogPath = %q, want empty", d.BuildLogPath)
	}
}

// TestCreateDeploymentFullTarballSource proves CreateDeploymentFull
// persists an explicit source/triggerKind, and that a "" imageRef (the
// tarball path's shape before the build produces a tag) round-trips
// through ListDeployments untouched.
func TestCreateDeploymentFullTarballSource(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d, err := s.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != "tarball" || d.ImageRef != "" || d.TriggerKind != "api" {
		t.Fatalf("d = %+v, want Source=tarball ImageRef=\"\" TriggerKind=api", d)
	}

	ds, err := s.ListDeployments(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Source != "tarball" || ds[0].ImageRef != "" {
		t.Fatalf("ListDeployments = %+v, want one tarball deployment with empty image_ref", ds)
	}
}

// TestSetDeploymentImageAndBuildLog proves both setters persist and are
// visible through both ListDeployments and DeploymentByNumber.
func TestSetDeploymentImageAndBuildLog(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d, err := s.CreateDeploymentFull(app.ID, "", "tarball", "api")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetDeploymentImage(d.ID, "localhost/basepod/blog:1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentBuildLog(d.ID, "/data/apps/blog/builds/1.log"); err != nil {
		t.Fatal(err)
	}

	got, err := s.DeploymentByNumber(app.ID, d.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "localhost/basepod/blog:1" {
		t.Fatalf("ImageRef = %q, want localhost/basepod/blog:1", got.ImageRef)
	}
	if got.BuildLogPath != "/data/apps/blog/builds/1.log" {
		t.Fatalf("BuildLogPath = %q, want /data/apps/blog/builds/1.log", got.BuildLogPath)
	}
}

// TestDeploymentByNumberNotFound proves an unknown (appID, number) pair
// returns ErrNotFound.
func TestDeploymentByNumberNotFound(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	if _, err := s.DeploymentByNumber(app.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestAppByID proves AppByID resolves an app by its primary key (used by
// internal/deploy's SweepStuckDeployments, which only has a Deployment's
// AppID to work from) with the exact same fields AppBySlug returns, and
// ErrNotFound for an unknown id.
func TestAppByID(t *testing.T) {
	s := open(t)
	created, err := s.CreateApp("blog", "nginx:alpine", 80)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.AppByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "blog" || got.ImageRef != "nginx:alpine" || got.Port != 80 {
		t.Fatalf("AppByID = %+v, want the created app", got)
	}

	if _, err := s.AppByID(created.ID + 1000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestListDeployingDeployments proves it returns exactly the deployments
// (across every app) whose Status is "deploying", ordered by app then
// number — the boot-sweep query internal/deploy.Engine.SweepStuckDeployments
// drives.
func TestListDeployingDeployments(t *testing.T) {
	s := open(t)
	app1, err := s.CreateApp("blog", "nginx:alpine", 80)
	if err != nil {
		t.Fatal(err)
	}
	app2, err := s.CreateApp("shop", "nginx:alpine", 81)
	if err != nil {
		t.Fatal(err)
	}

	// app1: one healthy (finished), one still deploying.
	h, err := s.CreateDeployment(app1.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeployment(h.ID, "healthy", ""); err != nil {
		t.Fatal(err)
	}
	stuck1, err := s.CreateDeployment(app1.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}

	// app2: one still deploying.
	stuck2, err := s.CreateDeployment(app2.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}

	deps, err := s.ListDeployingDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 {
		t.Fatalf("ListDeployingDeployments = %+v, want exactly 2 (the finished one excluded)", deps)
	}
	if deps[0].AppID != app1.ID || deps[0].Number != stuck1.Number {
		t.Fatalf("deps[0] = %+v, want app1's stuck deployment", deps[0])
	}
	if deps[1].AppID != app2.ID || deps[1].Number != stuck2.Number {
		t.Fatalf("deps[1] = %+v, want app2's stuck deployment", deps[1])
	}
	for _, d := range deps {
		if d.Status != "deploying" {
			t.Fatalf("deps = %+v, every entry must have Status=deploying", deps)
		}
	}
}

func TestUserByEmailNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.UserByEmail("nobody@example.com"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUserByEmail(t *testing.T) {
	s := open(t)
	uid, err := s.CreateUser("a@b.c", "Alice", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByEmail("a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != uid || u.Email != "a@b.c" || u.Name != "Alice" || u.PasswordHash != "hash" || !u.IsSuperadmin {
		t.Fatalf("user mismatch: %+v", u)
	}
}

func TestAppCRUD(t *testing.T) {
	s := open(t)

	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if app.ID == 0 || app.Slug != "blog" || app.ImageRef != "nginx:alpine" || app.Port != 8080 || app.Status != "created" {
		t.Fatalf("unexpected app: %+v", app)
	}

	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != app.ID {
		t.Fatalf("AppBySlug mismatch: %+v", got)
	}

	// duplicate slug should error
	if _, err := s.CreateApp("blog", "nginx:alpine", 8081); err == nil {
		t.Fatal("expected error for duplicate slug")
	}

	// second app for list coverage
	app2, err := s.CreateApp("api", "myapi:latest", 9090)
	if err != nil {
		t.Fatal(err)
	}

	apps, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %d: %+v", len(apps), apps)
	}

	if err := s.UpdateAppStatus(app.ID, "running"); err != nil {
		t.Fatal(err)
	}
	got, err = s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("expected status running, got %q", got.Status)
	}

	if err := s.UpdateAppImage(app.ID, "nginx:1.27"); err != nil {
		t.Fatal(err)
	}
	got, err = s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageRef != "nginx:1.27" {
		t.Fatalf("expected image nginx:1.27, got %q", got.ImageRef)
	}

	if err := s.DeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppBySlug("blog"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	apps, err = s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].ID != app2.ID {
		t.Fatalf("expected only app2 remaining, got %+v", apps)
	}
}

func TestAppBySlugNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.AppBySlug("does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCreateAppDefaultsResourceLimits covers audit finding H2's default
// posture: a freshly created app is never accidentally unlimited — it gets
// the schema's bounded defaults (matching migration
// 00004_resource_limits.sql's column defaults exactly) on both the
// returned App and what AppBySlug/ListApps read back.
func TestCreateAppDefaultsResourceLimits(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if app.MemoryLimitMB != 512 || app.CPULimit != 1.0 || app.PidsLimit != 512 {
		t.Fatalf("unexpected default limits on created app: %+v", app)
	}

	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 512 || got.CPULimit != 1.0 || got.PidsLimit != 512 {
		t.Fatalf("unexpected default limits from AppBySlug: %+v", got)
	}

	apps, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].MemoryLimitMB != 512 || apps[0].CPULimit != 1.0 || apps[0].PidsLimit != 512 {
		t.Fatalf("unexpected default limits from ListApps: %+v", apps)
	}
}

// TestUpdateAppLimits covers the store-level half of PATCH
// /api/v1/apps/{slug} (internal/api/apps.go's handlePatchApp): the three
// limits update independently and persist, including setting one to 0
// (unlimited) — verifying UpdateAppLimits doesn't reject or coerce that
// the way a naive "0 means don't update" implementation might.
func TestUpdateAppLimits(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateAppLimits(app.ID, 1024, 2.5, 1024); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 1024 || got.CPULimit != 2.5 || got.PidsLimit != 1024 {
		t.Fatalf("unexpected limits after update: %+v", got)
	}

	// 0 means unlimited and must be settable, not silently ignored.
	if err := s.UpdateAppLimits(app.ID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err = s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 0 || got.CPULimit != 0 || got.PidsLimit != 0 {
		t.Fatalf("unexpected limits after setting unlimited: %+v", got)
	}
}

// TestCreateAppDefaultsAliasScheme proves a brand-new app starts with
// AliasScheme "legacy" (the schema default — see migration
// 00005_alias_scheme.sql and App.AliasScheme's doc comment) on the
// returned App and on what AppBySlug/ListApps read back. It only becomes
// "v2" once internal/deploy's runRollout actually creates a container
// with the new alias, which a freshly-created-but-never-deployed app
// hasn't had happen yet.
func TestCreateAppDefaultsAliasScheme(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if app.AliasScheme != AliasSchemeLegacy {
		t.Fatalf("app.AliasScheme = %q, want %q", app.AliasScheme, AliasSchemeLegacy)
	}

	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.AliasScheme != AliasSchemeLegacy {
		t.Fatalf("AppBySlug AliasScheme = %q, want %q", got.AliasScheme, AliasSchemeLegacy)
	}

	apps, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].AliasScheme != AliasSchemeLegacy {
		t.Fatalf("ListApps AliasScheme = %+v, want %q", apps, AliasSchemeLegacy)
	}
}

// TestUpdateAppAliasScheme proves UpdateAppAliasScheme persists a new
// alias_scheme value, and that it round-trips through both AliasScheme
// values (not just legacy->v2, in case a future rollback path needs the
// reverse — though internal/deploy's runRollout today only ever writes
// "v2").
func TestUpdateAppAliasScheme(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateAppAliasScheme(app.ID, AliasSchemeV2); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.AliasScheme != AliasSchemeV2 {
		t.Fatalf("AliasScheme after update = %q, want %q", got.AliasScheme, AliasSchemeV2)
	}

	if err := s.UpdateAppAliasScheme(app.ID, AliasSchemeLegacy); err != nil {
		t.Fatal(err)
	}
	got, err = s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.AliasScheme != AliasSchemeLegacy {
		t.Fatalf("AliasScheme after second update = %q, want %q", got.AliasScheme, AliasSchemeLegacy)
	}
}

// TestMigrationAliasSchemeDefaultsAppliesToExistingApp is migration
// 00005_alias_scheme.sql's regression, mirroring
// TestMigrationDefaultsApplyToExistingApp for migration 4: an app row
// inserted BEFORE migration 5 ever ran (a real pre-v0.4.x data dir
// upgrading in place, whose already-running container carries only the
// legacy "bp-<slug>" alias) must come out the other side as AliasScheme
// "legacy" — never NULL, never accidentally "v2" (which would make
// Routes() render an upstream ("app-<slug>") the app's actual,
// not-yet-redeployed container was never given, 502ing every request
// until its next redeploy).
func TestMigrationAliasSchemeDefaultsAppliesToExistingApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 4); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}

	// Insert exactly as version-4 schema's apps table would have accepted
	// — no alias_scheme column exists yet at this point.
	if _, err := db.Exec(`INSERT INTO apps(slug, image_ref, port) VALUES(?, ?, ?)`, "legacyapp", "nginx:alpine", 80); err != nil {
		t.Fatalf("insert pre-migration-5 row: %v", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.AppBySlug("legacyapp")
	if err != nil {
		t.Fatal(err)
	}
	if got.AliasScheme != AliasSchemeLegacy {
		t.Fatalf("pre-existing row's AliasScheme = %q, want %q", got.AliasScheme, AliasSchemeLegacy)
	}
}

// TestMigrationDefaultsApplyToExistingApp is the specific regression the
// v0.4 plan calls out for migration 00004_resource_limits.sql: an app row
// inserted BEFORE that migration ever ran (i.e. a real v0.3.0 data dir
// upgrading in place) must come out the other side with the same bounded
// defaults a brand-new app gets — not NULL, not 0 (accidentally
// unlimited), and not a migration failure. This bypasses Store.Open (which
// always migrates straight to the latest schema before returning) to
// reproduce that exact sequence: migrate up to version 3 only, insert a
// row the way version-3 schema would have, then migrate the rest of the
// way and read it back through the normal Store API.
func TestMigrationDefaultsApplyToExistingApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 3); err != nil {
		t.Fatalf("migrate to version 3: %v", err)
	}

	// Insert exactly as version-3 schema's apps table would have accepted
	// — no resource-limit columns exist yet at this point.
	if _, err := db.Exec(`INSERT INTO apps(slug, image_ref, port) VALUES(?, ?, ?)`, "legacy", "nginx:alpine", 80); err != nil {
		t.Fatalf("insert pre-migration-4 row: %v", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.AppBySlug("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 512 || got.CPULimit != 1.0 || got.PidsLimit != 512 {
		t.Fatalf("pre-existing row did not get migration defaults: %+v", got)
	}
}

// TestAllMigrationsApplyCleanly guards against the exact failure class this
// integration hit: two migrations racing for the same goose sequence number
// (both feat/stream-tokens and fix/alias-namespace originally shipped a
// 00005_*.sql). goose.Up hard-errors at boot on a duplicate version, so this
// opens a brand-new DB, drives every migration in migrations/*.sql in order,
// and asserts the full set landed — eight files, eight recorded versions —
// and that every branch's schema objects (stream_tokens table from
// migration 6, apps.alias_scheme column from migration 5, git_sources/
// git_deliveries tables + deployments.git_sha from migration 7, volumes
// table + apps.deploy_strategy from migration 8) exist side by side.
func TestAllMigrationsApplyCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}

	version, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 8 {
		t.Fatalf("db version after migrating = %d, want 8 (00001..00008)", version)
	}

	var appliedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE is_applied = 1`).Scan(&appliedCount); err != nil {
		t.Fatal(err)
	}
	// goose_db_version always carries a synthetic version-0 bootstrap row in
	// addition to one row per applied migration file.
	if appliedCount != 9 {
		t.Fatalf("applied migration rows = %d, want 9 (bootstrap + 8 migrations)", appliedCount)
	}

	// apps.alias_scheme (migration 00005_alias_scheme.sql, from
	// fix/alias-namespace) and apps.deploy_strategy (migration
	// 00008_volumes_strategy.sql) must exist.
	rows, err := db.Query(`PRAGMA table_info(apps)`)
	if err != nil {
		t.Fatal(err)
	}
	var sawAliasScheme, sawDeployStrategy bool
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == "alias_scheme" {
			sawAliasScheme = true
		}
		if name == "deploy_strategy" {
			sawDeployStrategy = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !sawAliasScheme {
		t.Fatal("apps.alias_scheme column missing after migrating to latest")
	}
	if !sawDeployStrategy {
		t.Fatal("apps.deploy_strategy column missing after migrating to latest")
	}

	// stream_tokens table (migration 00006_stream_tokens.sql, from
	// feat/stream-tokens) must exist.
	var tableName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'stream_tokens'`).Scan(&tableName); err != nil {
		t.Fatalf("stream_tokens table missing after migrating to latest: %v", err)
	}

	// git_sources and git_deliveries tables (migration
	// 00007_git_sources.sql, v0.5's Task 2) must exist.
	for _, want := range []string{"git_sources", "git_deliveries"} {
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, want).Scan(&tableName); err != nil {
			t.Fatalf("%s table missing after migrating to latest: %v", want, err)
		}
	}

	// volumes table (migration 00008_volumes_strategy.sql, v0.5's Task 6)
	// must exist.
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'volumes'`).Scan(&tableName); err != nil {
		t.Fatalf("volumes table missing after migrating to latest: %v", err)
	}

	// deployments.git_sha (migration 00007_git_sources.sql) must exist.
	rows, err = db.Query(`PRAGMA table_info(deployments)`)
	if err != nil {
		t.Fatal(err)
	}
	var sawGitSha bool
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == "git_sha" {
			sawGitSha = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !sawGitSha {
		t.Fatal("deployments.git_sha column missing after migrating to latest")
	}
}

func TestCountUsers(t *testing.T) {
	s := open(t)
	if count, err := s.CountUsers(); err != nil || count != 0 {
		t.Fatalf("expected 0 users, got %d (err: %v)", count, err)
	}
	s.CreateUser("a@b.c", "Alice", "hash", true)
	if count, err := s.CountUsers(); err != nil || count != 1 {
		t.Fatalf("expected 1 user, got %d (err: %v)", count, err)
	}
	s.CreateUser("c@d.e", "Charlie", "hash", false)
	if count, err := s.CountUsers(); err != nil || count != 2 {
		t.Fatalf("expected 2 users, got %d (err: %v)", count, err)
	}
}

func TestEnvVarUpsertOverwrites(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	// Initial insert
	err := s.UpsertEnvVar(app.ID, "DEBUG", "true", true)
	if err != nil {
		t.Fatalf("initial upsert failed: %v", err)
	}

	vars, _ := s.ListEnvVars(app.ID)
	if len(vars) != 1 || vars[0].Key != "DEBUG" || vars[0].ValueEncrypted != "true" || !vars[0].IsSecret {
		t.Fatalf("initial insert wrong: %+v", vars[0])
	}

	// Upsert (overwrite)
	err = s.UpsertEnvVar(app.ID, "DEBUG", "false", false)
	if err != nil {
		t.Fatalf("upsert overwrite failed: %v", err)
	}

	vars, _ = s.ListEnvVars(app.ID)
	if len(vars) != 1 || vars[0].ValueEncrypted != "false" || vars[0].IsSecret {
		t.Fatalf("overwrite failed: %+v", vars[0])
	}
}

func TestEnvVarPerAppIsolation(t *testing.T) {
	s := open(t)
	app1, _ := s.CreateApp("app1", "img:1.0", 8080)
	app2, _ := s.CreateApp("app2", "img:1.0", 8081)

	s.UpsertEnvVar(app1.ID, "SECRET", "s1", true)
	s.UpsertEnvVar(app2.ID, "SECRET", "s2", true)

	vars1, _ := s.ListEnvVars(app1.ID)
	vars2, _ := s.ListEnvVars(app2.ID)

	if len(vars1) != 1 || vars1[0].ValueEncrypted != "s1" {
		t.Fatalf("app1 vars wrong: %+v", vars1)
	}
	if len(vars2) != 1 || vars2[0].ValueEncrypted != "s2" {
		t.Fatalf("app2 vars wrong: %+v", vars2)
	}
}

func TestEnvVarCascadeDelete(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)
	s.UpsertEnvVar(app.ID, "VAR", "val", false)

	vars, _ := s.ListEnvVars(app.ID)
	if len(vars) != 1 {
		t.Fatalf("expected 1 var before delete, got %d", len(vars))
	}

	s.DeleteApp(app.ID)

	vars, _ = s.ListEnvVars(app.ID)
	if len(vars) != 0 {
		t.Fatalf("expected 0 vars after app delete, got %d: %+v", len(vars), vars)
	}
}

func TestEnvVarOrdering(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	s.UpsertEnvVar(app.ID, "ZEBRA", "z", false)
	s.UpsertEnvVar(app.ID, "APPLE", "a", false)
	s.UpsertEnvVar(app.ID, "MANGO", "m", false)

	vars, _ := s.ListEnvVars(app.ID)
	if len(vars) != 3 {
		t.Fatalf("expected 3 vars, got %d", len(vars))
	}
	if vars[0].Key != "APPLE" || vars[1].Key != "MANGO" || vars[2].Key != "ZEBRA" {
		t.Fatalf("ordering wrong: %+v", vars)
	}
}

func TestReplaceEnvVars(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	// seed an initial set
	if err := s.UpsertEnvVar(app.ID, "KEEP", "old-keep", false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEnvVar(app.ID, "DROP", "gone-soon", false); err != nil {
		t.Fatal(err)
	}

	// replace: KEEP gets a new value, DROP is omitted (pruned), ADD is new
	err := s.ReplaceEnvVars(app.ID, []EnvVar{
		{Key: "KEEP", ValueEncrypted: "new-keep", IsSecret: false},
		{Key: "ADD", ValueEncrypted: "added", IsSecret: true},
	})
	if err != nil {
		t.Fatalf("ReplaceEnvVars failed: %v", err)
	}

	vars, err := s.ListEnvVars(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars after replace, got %d: %+v", len(vars), vars)
	}
	byKey := make(map[string]EnvVar, len(vars))
	for _, v := range vars {
		byKey[v.Key] = v
	}
	if byKey["KEEP"].ValueEncrypted != "new-keep" || byKey["KEEP"].IsSecret {
		t.Fatalf("KEEP not updated correctly: %+v", byKey["KEEP"])
	}
	if byKey["ADD"].ValueEncrypted != "added" || !byKey["ADD"].IsSecret {
		t.Fatalf("ADD not inserted correctly: %+v", byKey["ADD"])
	}
	if _, ok := byKey["DROP"]; ok {
		t.Fatalf("expected DROP to be pruned, got %+v", vars)
	}

	// replace with an empty set prunes everything
	if err := s.ReplaceEnvVars(app.ID, nil); err != nil {
		t.Fatalf("ReplaceEnvVars(nil) failed: %v", err)
	}
	vars, err = s.ListEnvVars(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 0 {
		t.Fatalf("expected 0 vars after empty replace, got %d: %+v", len(vars), vars)
	}
}

func TestDeleteEnvVar(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	s.UpsertEnvVar(app.ID, "VAR1", "v1", false)
	s.UpsertEnvVar(app.ID, "VAR2", "v2", false)

	vars, _ := s.ListEnvVars(app.ID)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(vars))
	}

	err := s.DeleteEnvVar(app.ID, "VAR1")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	vars, _ = s.ListEnvVars(app.ID)
	if len(vars) != 1 || vars[0].Key != "VAR2" {
		t.Fatalf("after delete, expected VAR2, got: %+v", vars)
	}
}

func TestAddDomainInvalidAppID(t *testing.T) {
	s := open(t)
	// appID 99999 does not exist; should return ErrNotFound due to FK constraint
	_, err := s.AddDomain(99999, "nonexistent.com")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for invalid appID, got %v", err)
	}
}

func TestDomainUniquenessAcrossApps(t *testing.T) {
	s := open(t)
	app1, _ := s.CreateApp("app1", "img:1.0", 8080)
	app2, _ := s.CreateApp("app2", "img:1.0", 8081)

	domain1, err := s.AddDomain(app1.ID, "example.com")
	if err != nil {
		t.Fatalf("first domain failed: %v", err)
	}
	if domain1.Hostname != "example.com" {
		t.Fatalf("domain1 hostname wrong: %q", domain1.Hostname)
	}

	// Same hostname for another app should fail (UNIQUE constraint)
	if _, err := s.AddDomain(app2.ID, "example.com"); err == nil {
		t.Fatal("expected error for duplicate hostname")
	}
}

func TestDomainByHostnameNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.DomainByHostname("does-not-exist.com"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDomainByHostname(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	d, _ := s.AddDomain(app.ID, "myapp.example.com")
	found, err := s.DomainByHostname("myapp.example.com")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if found.ID != d.ID || found.Hostname != "myapp.example.com" {
		t.Fatalf("domain mismatch: %+v", found)
	}
}

func TestListDomains(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	s.AddDomain(app.ID, "d1.com")
	s.AddDomain(app.ID, "d2.com")

	domains, _ := s.ListDomains(app.ID)
	if len(domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(domains))
	}

	hostnames := map[string]bool{
		domains[0].Hostname: true,
		domains[1].Hostname: true,
	}
	if !hostnames["d1.com"] || !hostnames["d2.com"] {
		t.Fatalf("hostnames wrong: %+v", domains)
	}
}

func TestListAllDomains(t *testing.T) {
	s := open(t)
	app1, _ := s.CreateApp("app1", "img:1.0", 8080)
	app2, _ := s.CreateApp("app2", "img:1.0", 8081)

	d1, _ := s.AddDomain(app1.ID, "app1.example.com")
	d2, _ := s.AddDomain(app1.ID, "app1-alt.example.com")
	d3, _ := s.AddDomain(app2.ID, "app2.example.com")

	allDomains, err := s.ListAllDomains()
	if err != nil {
		t.Fatalf("list all domains failed: %v", err)
	}

	if len(allDomains) != 3 {
		t.Fatalf("expected 3 domains total, got %d: %+v", len(allDomains), allDomains)
	}

	// Verify all domains are present
	domainMap := make(map[int64]bool)
	for _, d := range allDomains {
		domainMap[d.ID] = true
	}
	if !domainMap[d1.ID] || !domainMap[d2.ID] || !domainMap[d3.ID] {
		t.Fatalf("not all domains found in list: %+v", allDomains)
	}
}

func TestDeleteDomain(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	d1, _ := s.AddDomain(app.ID, "d1.com")
	d2, _ := s.AddDomain(app.ID, "d2.com")

	domains, _ := s.ListDomains(app.ID)
	if len(domains) != 2 {
		t.Fatalf("expected 2 before delete, got %d", len(domains))
	}

	err := s.DeleteDomain(d1.ID)
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	domains, _ = s.ListDomains(app.ID)
	if len(domains) != 1 || domains[0].ID != d2.ID {
		t.Fatalf("after delete, expected only d2, got: %+v", domains)
	}
}

func TestDomainCascadeDelete(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("app", "img:1.0", 8080)

	s.AddDomain(app.ID, "app.example.com")

	domains, _ := s.ListDomains(app.ID)
	if len(domains) != 1 {
		t.Fatalf("expected 1 domain before app delete, got %d", len(domains))
	}

	s.DeleteApp(app.ID)

	domains, _ = s.ListDomains(app.ID)
	if len(domains) != 0 {
		t.Fatalf("expected 0 domains after app delete, got %d: %+v", len(domains), domains)
	}
}

func TestUserByID(t *testing.T) {
	s := open(t)
	uid, err := s.CreateUser("a@b.c", "A", "hash", true)
	if err != nil {
		t.Fatal(err)
	}

	u, err := s.UserByID(uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "a@b.c" || u.Name != "A" || !u.IsSuperadmin {
		t.Fatalf("unexpected user: %+v", u)
	}

	if _, err := s.UserByID(999); err != ErrNotFound {
		t.Fatalf("unknown id: got %v, want ErrNotFound", err)
	}
}

// TestStreamTokenRoundtrip proves a stream token minted for one (scope,
// slug, deploymentNumber) triple is found by hash with exactly that
// triple intact, for both the app_logs shape (no deployment number) and
// the build_log shape (a deployment number set).
func TestStreamTokenRoundtrip(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)

	if err := s.CreateStreamToken(uid, "th-logs", "app_logs", "blog", nil, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("create app_logs token: %v", err)
	}
	got, err := s.StreamTokenByHash("th-logs")
	if err != nil {
		t.Fatalf("lookup app_logs token: %v", err)
	}
	if got.UserID != uid || got.Scope != "app_logs" || got.Slug != "blog" || got.DeploymentNumber != nil {
		t.Fatalf("unexpected app_logs token: %+v", got)
	}

	n := int64(3)
	if err := s.CreateStreamToken(uid, "th-build", "build_log", "blog", &n, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("create build_log token: %v", err)
	}
	got, err = s.StreamTokenByHash("th-build")
	if err != nil {
		t.Fatalf("lookup build_log token: %v", err)
	}
	if got.Scope != "build_log" || got.DeploymentNumber == nil || *got.DeploymentNumber != 3 {
		t.Fatalf("unexpected build_log token: %+v (deployment_number=%v)", got, got.DeploymentNumber)
	}
}

// TestStreamTokenByHashUnknown proves an unrecognized hash reports
// ErrNotFound rather than a zero-value token.
func TestStreamTokenByHashUnknown(t *testing.T) {
	s := open(t)
	if _, err := s.StreamTokenByHash("no-such-hash"); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// TestStreamTokenExpiry proves an expired stream token is not returned by
// StreamTokenByHash, mirroring TestSessionExpiry.
func TestStreamTokenExpiry(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)

	if err := s.CreateStreamToken(uid, "live", "app_logs", "blog", nil, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateStreamToken(uid, "dead", "app_logs", "blog", nil, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.StreamTokenByHash("live"); err != nil {
		t.Fatalf("valid stream token rejected: %v", err)
	}
	if _, err := s.StreamTokenByHash("dead"); err != ErrNotFound {
		t.Fatalf("expired stream token accepted: %v", err)
	}
}

// TestStreamTokenByHashPrunesExpiredLazily proves StreamTokenByHash's
// side-effecting prune (see its doc comment) actually deletes expired
// rows — not just skips them in its own SELECT — by looking up an
// unrelated live token afterward and confirming
// PruneExpiredStreamTokens finds nothing left to do.
func TestStreamTokenByHashPrunesExpiredLazily(t *testing.T) {
	s := open(t)
	uid, _ := s.CreateUser("a@b.c", "A", "hash", true)

	if err := s.CreateStreamToken(uid, "dead", "app_logs", "blog", nil, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateStreamToken(uid, "live", "app_logs", "blog", nil, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Any lookup (even for the live token) sweeps every expired row.
	if _, err := s.StreamTokenByHash("live"); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneExpiredStreamTokens()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected the lazy prune in StreamTokenByHash to have already removed the expired row, %d still pruneable", n)
	}
}

// TestGitSourceRoundtrip proves UpsertGitSource/GitSourceByAppID/
// GitSourceByHookID round-trip every field, and that GitSourceByAppID
// reports ErrNotFound before any source is connected.
func TestGitSourceRoundtrip(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	if _, err := s.GitSourceByAppID(app.ID); err != ErrNotFound {
		t.Fatalf("GitSourceByAppID before connect = %v, want ErrNotFound", err)
	}

	got, err := s.UpsertGitSource(GitSource{
		AppID:           app.ID,
		URL:             "https://github.com/example/repo.git",
		Branch:          "main",
		Provider:        "github",
		HookID:          "hook-abc123",
		SecretEncrypted: "sealed-secret",
		TokenEncrypted:  "sealed-token",
	})
	if err != nil {
		t.Fatalf("UpsertGitSource: %v", err)
	}
	if got.AppID != app.ID || got.URL != "https://github.com/example/repo.git" || got.Branch != "main" ||
		got.Provider != "github" || got.HookID != "hook-abc123" ||
		got.SecretEncrypted != "sealed-secret" || got.TokenEncrypted != "sealed-token" {
		t.Fatalf("unexpected git source after insert: %+v", got)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("CreatedAt/UpdatedAt not set: %+v", got)
	}

	byApp, err := s.GitSourceByAppID(app.ID)
	if err != nil || byApp.HookID != "hook-abc123" {
		t.Fatalf("GitSourceByAppID: %+v, %v", byApp, err)
	}
	byHook, err := s.GitSourceByHookID("hook-abc123")
	if err != nil || byHook.AppID != app.ID {
		t.Fatalf("GitSourceByHookID: %+v, %v", byHook, err)
	}

	if _, err := s.GitSourceByHookID("no-such-hook"); err != ErrNotFound {
		t.Fatalf("GitSourceByHookID unknown = %v, want ErrNotFound", err)
	}
}

// TestGitSourceUpsertReplacesInPlace proves a second UpsertGitSource for
// the same app updates the existing row (keyed by the UNIQUE app_id
// column) rather than erroring or creating a duplicate, preserves
// created_at across the update, and advances updated_at.
func TestGitSourceUpsertReplacesInPlace(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	first, err := s.UpsertGitSource(GitSource{
		AppID: app.ID, URL: "https://github.com/example/repo.git", Branch: "main",
		HookID: "hook-1", SecretEncrypted: "s1",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, err := s.UpsertGitSource(GitSource{
		AppID: app.ID, URL: "https://github.com/example/repo.git", Branch: "develop",
		HookID: "hook-1", SecretEncrypted: "s1", TokenEncrypted: "t2",
	})
	if err != nil {
		t.Fatalf("second upsert (reconnect): %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("reconnect created a new row: first ID %d, second ID %d", first.ID, second.ID)
	}
	if second.Branch != "develop" || second.TokenEncrypted != "t2" {
		t.Fatalf("reconnect did not apply new fields: %+v", second)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at changed across reconnect: %q -> %q", first.CreatedAt, second.CreatedAt)
	}

	all, err := s.ListGitDeliveries(app.ID, 10)
	if err != nil || len(all) != 0 {
		t.Fatalf("expected no deliveries yet: %v, %v", all, err)
	}
}

// TestGitSourceUpsertUnknownApp proves UpsertGitSource maps a foreign-key
// violation (an app_id that doesn't exist) to ErrNotFound, mirroring
// AddDomain's behavior.
func TestGitSourceUpsertUnknownApp(t *testing.T) {
	s := open(t)
	if _, err := s.UpsertGitSource(GitSource{AppID: 999999, URL: "https://example.com/r.git", Branch: "main", HookID: "h"}); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// TestGitSourceHookIDUniqueAcrossApps proves hook_id is globally unique —
// two apps cannot end up with colliding webhook URLs.
func TestGitSourceHookIDUniqueAcrossApps(t *testing.T) {
	s := open(t)
	app1, _ := s.CreateApp("app1", "img:1.0", 8080)
	app2, _ := s.CreateApp("app2", "img:1.0", 8081)

	if _, err := s.UpsertGitSource(GitSource{AppID: app1.ID, URL: "https://example.com/a.git", Branch: "main", HookID: "shared-hook"}); err != nil {
		t.Fatalf("app1 upsert: %v", err)
	}
	if _, err := s.UpsertGitSource(GitSource{AppID: app2.ID, URL: "https://example.com/b.git", Branch: "main", HookID: "shared-hook"}); err == nil {
		t.Fatal("expected a UNIQUE constraint error for a colliding hook_id across two different apps, got nil")
	}
}

// TestGitSourceCascadeDelete proves deleting an app removes its git
// source row too (ON DELETE CASCADE).
func TestGitSourceCascadeDelete(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)
	if _, err := s.UpsertGitSource(GitSource{AppID: app.ID, URL: "https://example.com/r.git", Branch: "main", HookID: "hook-x"}); err != nil {
		t.Fatal(err)
	}

	s.DeleteApp(app.ID)

	if _, err := s.GitSourceByAppID(app.ID); err != ErrNotFound {
		t.Fatalf("git source survived app delete: err=%v", err)
	}
}

// TestDeleteGitSource proves DeleteGitSource disconnects a repo, and is a
// harmless no-op when none was connected.
func TestDeleteGitSource(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	if err := s.DeleteGitSource(app.ID); err != nil {
		t.Fatalf("DeleteGitSource with nothing connected: %v", err)
	}

	if _, err := s.UpsertGitSource(GitSource{AppID: app.ID, URL: "https://example.com/r.git", Branch: "main", HookID: "hook-x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGitSource(app.ID); err != nil {
		t.Fatalf("DeleteGitSource: %v", err)
	}
	if _, err := s.GitSourceByAppID(app.ID); err != ErrNotFound {
		t.Fatalf("git source still present after DeleteGitSource: err=%v", err)
	}
}

// TestGitDeliveryRoundtripAndOrdering proves InsertGitDelivery/
// ListGitDeliveries round-trip every field (including a nil vs. set
// DeploymentID) and list newest first.
func TestGitDeliveryRoundtripAndOrdering(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	if _, err := s.InsertGitDelivery(GitDelivery{
		AppID: app.ID, Provider: "github", Event: "push", Ref: "refs/heads/main",
		CommitSHA: "aaa111", Status: "ignored_branch", Detail: "branch mismatch",
	}); err != nil {
		t.Fatalf("insert #1: %v", err)
	}

	// deployment_id references a real deployments row (FOREIGN KEY), so
	// exercise the round-trip against one actually created.
	dep, err := s.CreateDeployment(app.ID, "img:2.0")
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got2, err := s.InsertGitDelivery(GitDelivery{
		AppID: app.ID, Provider: "github", Event: "push", Ref: "refs/heads/main",
		CommitSHA: "bbb222", Status: "deployed", Detail: "deployed as #3", DeploymentID: &dep.ID,
	})
	if err != nil {
		t.Fatalf("insert #2: %v", err)
	}
	if got2.DeploymentID == nil || *got2.DeploymentID != dep.ID {
		t.Fatalf("deployment_id not round-tripped: %+v", got2)
	}
	if got2.ReceivedAt == "" || got2.ID == 0 {
		t.Fatalf("id/received_at not populated: %+v", got2)
	}

	list, err := s.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatalf("ListGitDeliveries: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 deliveries, got %d: %+v", len(list), list)
	}
	// Newest first: the second insert (commit bbb222) must come before the
	// first (commit aaa111).
	if list[0].CommitSHA != "bbb222" || list[1].CommitSHA != "aaa111" {
		t.Fatalf("deliveries not newest-first: %+v", list)
	}
	if list[0].DeploymentID == nil || *list[0].DeploymentID != dep.ID {
		t.Fatalf("expected first entry's deployment_id round-tripped: %+v", list[0])
	}
	if list[1].DeploymentID != nil {
		t.Fatalf("expected second entry's deployment_id to be nil: %+v", list[1])
	}
}

// TestGitDeliveryCascadeDelete proves deleting an app removes its
// delivery log too.
func TestGitDeliveryCascadeDelete(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)
	if _, err := s.InsertGitDelivery(GitDelivery{AppID: app.ID, Status: "ignored_event"}); err != nil {
		t.Fatal(err)
	}

	s.DeleteApp(app.ID)

	list, err := s.ListGitDeliveries(app.ID, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("deliveries survived app delete: %+v, %v", list, err)
	}
}

// TestGitDeliveryPerAppIsolation proves ListGitDeliveries only returns
// the requested app's rows.
func TestGitDeliveryPerAppIsolation(t *testing.T) {
	s := open(t)
	app1, _ := s.CreateApp("app1", "img:1.0", 8080)
	app2, _ := s.CreateApp("app2", "img:1.0", 8081)

	if _, err := s.InsertGitDelivery(GitDelivery{AppID: app1.ID, CommitSHA: "a1", Status: "deployed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertGitDelivery(GitDelivery{AppID: app2.ID, CommitSHA: "a2", Status: "deployed"}); err != nil {
		t.Fatal(err)
	}

	list1, _ := s.ListGitDeliveries(app1.ID, 10)
	list2, _ := s.ListGitDeliveries(app2.ID, 10)
	if len(list1) != 1 || list1[0].CommitSHA != "a1" {
		t.Fatalf("app1 deliveries wrong: %+v", list1)
	}
	if len(list2) != 1 || list2[0].CommitSHA != "a2" {
		t.Fatalf("app2 deliveries wrong: %+v", list2)
	}
}

// TestPruneGitDeliveriesKeepsExactlyNNewest proves PruneGitDeliveries (and
// InsertGitDelivery's automatic call to it, using DefaultGitDeliveryKeep)
// leaves exactly the keep newest rows.
func TestPruneGitDeliveriesKeepsExactlyNNewest(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("myapp", "img:1.0", 8080)

	// Insert 5 deliveries, one at a time, in increasing "freshness" order
	// (commit_sha doubles as an ordinal so we can identify survivors
	// without depending on sub-second received_at resolution).
	for i := 0; i < 5; i++ {
		if _, err := s.InsertGitDelivery(GitDelivery{
			AppID: app.ID, CommitSHA: string(rune('a' + i)), Status: "ignored_event",
		}); err != nil {
			t.Fatalf("insert #%d: %v", i, err)
		}
	}

	n, err := s.PruneGitDeliveries(app.ID, 3)
	if err != nil {
		t.Fatalf("PruneGitDeliveries: %v", err)
	}
	if n != 2 {
		t.Fatalf("PruneGitDeliveries removed %d rows, want 2 (5 - keep 3)", n)
	}

	list, err := s.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatalf("ListGitDeliveries: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected exactly 3 deliveries left, got %d: %+v", len(list), list)
	}
	// The 3 newest (by insertion order/id, since received_at may tie at
	// second resolution) must be the last 3 inserted: c, d, e.
	got := map[string]bool{}
	for _, d := range list {
		got[d.CommitSHA] = true
	}
	for _, want := range []string{"c", "d", "e"} {
		if !got[want] {
			t.Fatalf("expected surviving delivery %q, got set %v (full list %+v)", want, got, list)
		}
	}
	for _, gone := range []string{"a", "b"} {
		if got[gone] {
			t.Fatalf("expected delivery %q pruned, but it survived: %+v", gone, list)
		}
	}
}

// ---------------------------------------------------------------------
// v0.5: named volumes + deploy_strategy (migration
// 00008_volumes_strategy.sql). Kept in its own block for the same reason
// as store.go's own block — see that file's comment.
// ---------------------------------------------------------------------

// TestCreateAppDefaultsDeployStrategy proves a brand-new app starts with
// DeployStrategy "zero-downtime" (the schema default) on both the
// returned App and what AppBySlug/ListApps read back — mirrors
// TestCreateAppDefaultsAliasScheme's shape for the sibling column this
// task adds.
func TestCreateAppDefaultsDeployStrategy(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if app.DeployStrategy != DeployStrategyZeroDowntime {
		t.Fatalf("app.DeployStrategy = %q, want %q", app.DeployStrategy, DeployStrategyZeroDowntime)
	}

	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployStrategy != DeployStrategyZeroDowntime {
		t.Fatalf("AppBySlug DeployStrategy = %q, want %q", got.DeployStrategy, DeployStrategyZeroDowntime)
	}

	apps, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].DeployStrategy != DeployStrategyZeroDowntime {
		t.Fatalf("ListApps DeployStrategy = %+v, want %q", apps, DeployStrategyZeroDowntime)
	}
}

// TestUpdateAppDeployStrategy proves UpdateAppDeployStrategy persists and
// round-trips through both values.
func TestUpdateAppDeployStrategy(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("blog", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateAppDeployStrategy(app.ID, DeployStrategyReplace); err != nil {
		t.Fatal(err)
	}
	got, err := s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployStrategy != DeployStrategyReplace {
		t.Fatalf("DeployStrategy after update = %q, want %q", got.DeployStrategy, DeployStrategyReplace)
	}

	if err := s.UpdateAppDeployStrategy(app.ID, DeployStrategyZeroDowntime); err != nil {
		t.Fatal(err)
	}
	got, err = s.AppBySlug("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployStrategy != DeployStrategyZeroDowntime {
		t.Fatalf("DeployStrategy after second update = %q, want %q", got.DeployStrategy, DeployStrategyZeroDowntime)
	}
}

// TestValidDeployStrategy covers the validator PATCH /api/v1/apps/{slug}
// relies on to 422 an unknown deploy_strategy value.
func TestValidDeployStrategy(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"zero-downtime", true},
		{"replace", true},
		{"", false},
		{"Replace", false},
		{"rolling", false},
	}
	for _, tc := range cases {
		if got := ValidDeployStrategy(tc.value); got != tc.want {
			t.Errorf("ValidDeployStrategy(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestUpsertVolumeCreatesAndUpdates proves UpsertVolume both inserts a new
// row and, on a second call for the same (app_id, name), updates
// container_path in place rather than erroring or duplicating — the
// upsert contract Task 8's compose apply (a re-appliable operation) relies
// on.
func TestUpsertVolumeCreatesAndUpdates(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}

	v, err := s.UpsertVolume(app.ID, "data", "/var/lib/postgresql/data")
	if err != nil {
		t.Fatal(err)
	}
	if v.AppID != app.ID || v.Name != "data" || v.ContainerPath != "/var/lib/postgresql/data" || v.ID == 0 || v.CreatedAt == "" {
		t.Fatalf("unexpected volume: %+v", v)
	}

	// Re-upsert with a different container_path: same row, updated path.
	v2, err := s.UpsertVolume(app.ID, "data", "/var/lib/postgresql/17/data")
	if err != nil {
		t.Fatal(err)
	}
	if v2.ID != v.ID {
		t.Fatalf("re-upsert created a new row: first ID %d, second ID %d", v.ID, v2.ID)
	}
	if v2.ContainerPath != "/var/lib/postgresql/17/data" {
		t.Fatalf("ContainerPath after re-upsert = %q, want updated path", v2.ContainerPath)
	}

	got, err := s.ListVolumes(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListVolumes = %+v, want exactly 1 (re-upsert must not duplicate)", got)
	}
}

// TestListVolumesOrderedByNameAndIsolatedPerApp proves ListVolumes returns
// only appID's own volumes, alphabetically by name.
func TestListVolumesOrderedByNameAndIsolatedPerApp(t *testing.T) {
	s := open(t)
	app1, err := s.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	app2, err := s.CreateApp("cache", "redis:7", 6379)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpsertVolume(app1.ID, "logs", "/var/log"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertVolume(app1.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertVolume(app2.ID, "data", "/data"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListVolumes(app1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "data" || got[1].Name != "logs" {
		t.Fatalf("ListVolumes(app1) = %+v, want [data, logs] in that order", got)
	}
	for _, v := range got {
		if v.AppID != app1.ID {
			t.Fatalf("ListVolumes(app1) returned a volume belonging to a different app: %+v", v)
		}
	}
}

// TestVolumeCascadeDeleteOnAppDelete proves the volumes TABLE ROW is
// removed when its owning app is deleted (ON DELETE CASCADE, migration
// 00008) — this is bookkeeping-row cleanup only, distinct from (and not a
// contradiction of) the "app delete keeps volumes" product rule, which is
// about the underlying libpod volume never being removed. That half lives
// in internal/api's handleDeleteApp (it calls ListVolumes BEFORE
// DeleteApp specifically to still be able to log the surviving libpod
// volume names afterward) and is covered by that package's own tests, not
// here — this test only proves the store-level cascade.
func TestVolumeCascadeDeleteOnAppDelete(t *testing.T) {
	s := open(t)
	app, err := s.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertVolume(app.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteApp(app.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListVolumes(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("volumes row(s) survived app delete: %+v", got)
	}
}

// TestUpsertVolumeUniquePerAppAllowsSameNameOnDifferentApps proves the
// UNIQUE(app_id, name) constraint is scoped per-app, not global — two
// different apps can each declare a volume named "data".
func TestUpsertVolumeUniquePerAppAllowsSameNameOnDifferentApps(t *testing.T) {
	s := open(t)
	app1, err := s.CreateApp("db", "postgres:16", 5432)
	if err != nil {
		t.Fatal(err)
	}
	app2, err := s.CreateApp("cache", "redis:7", 6379)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.UpsertVolume(app1.ID, "data", "/var/lib/postgresql/data"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertVolume(app2.ID, "data", "/data"); err != nil {
		t.Fatalf("expected same-name volume on a different app to succeed, got: %v", err)
	}
}

// TestMigrationVolumesStrategyDefaultsAppliesToExistingApp mirrors
// TestMigrationDefaultsApplyToExistingApp / …AliasSchemeDefaults…: an app
// row inserted on a pre-migration-8 data dir (before migration 00008 ever
// ran, which had no deploy_strategy column and no volumes table at all)
// must come out the other side of an upgrade with DeployStrategy
// "zero-downtime" — never NULL, never a migration failure — and the
// volumes table must be usable immediately afterward. Bypasses Store.Open
// (which always migrates straight to latest) to reproduce that exact
// sequence: migrate up to version 6 only (before either of the two
// concurrent v0.5 migrations, 00007 and 00008, ran), insert a row the way
// that schema would have, then migrate the rest of the way and read it
// back through the normal Store API.
func TestMigrationVolumesStrategyDefaultsAppliesToExistingApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 6); err != nil {
		t.Fatalf("migrate to version 6: %v", err)
	}

	// Insert exactly as version-6 schema's apps table would have accepted
	// — no deploy_strategy column exists yet at this point.
	if _, err := db.Exec(`INSERT INTO apps(slug, image_ref, port) VALUES(?, ?, ?)`, "preexisting", "nginx:alpine", 80); err != nil {
		t.Fatalf("insert pre-migration-8 row: %v", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		t.Fatalf("migrate to latest: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.AppBySlug("preexisting")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployStrategy != DeployStrategyZeroDowntime {
		t.Fatalf("pre-existing row's DeployStrategy = %q, want %q", got.DeployStrategy, DeployStrategyZeroDowntime)
	}

	// The volumes table must be immediately usable for a pre-existing app.
	if _, err := s.UpsertVolume(got.ID, "data", "/data"); err != nil {
		t.Fatalf("UpsertVolume against a migrated-in-place app: %v", err)
	}
	vols, err := s.ListVolumes(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 1 || vols[0].Name != "data" {
		t.Fatalf("ListVolumes after migration = %+v", vols)
	}
}

// TestSetDeploymentGitSHA proves git_sha persists and defaults to "" for
// every non-git deployment (migration 00007_git_sources.sql).
func TestSetDeploymentGitSHA(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)

	d, err := s.CreateDeploymentFull(app.ID, "", "git", "api")
	if err != nil {
		t.Fatal(err)
	}
	if d.GitSha != "" {
		t.Fatalf("GitSha = %q, want empty before SetDeploymentGitSHA", d.GitSha)
	}

	if err := s.SetDeploymentGitSHA(d.ID, "abc123def456"); err != nil {
		t.Fatal(err)
	}

	got, err := s.DeploymentByNumber(app.ID, d.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitSha != "abc123def456" {
		t.Fatalf("GitSha = %q, want abc123def456", got.GitSha)
	}
	if got.Source != "git" {
		t.Fatalf("Source = %q, want git", got.Source)
	}

	// A registry-image deployment never touches git_sha — it defaults to
	// "" rather than NULL or some other sentinel.
	imageDep, err := s.CreateDeployment(app.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}
	if imageDep.GitSha != "" {
		t.Fatalf("image-sourced deployment GitSha = %q, want empty", imageDep.GitSha)
	}
}

// TestDeploymentByID proves DeploymentByID resolves a deployment by its
// store row ID (as opposed to DeploymentByNumber's per-app Number), and
// returns ErrNotFound for an unknown ID.
func TestDeploymentByID(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d, err := s.CreateDeployment(app.ID, "nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.DeploymentByID(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != d.ID || got.Number != d.Number || got.AppID != app.ID {
		t.Fatalf("DeploymentByID = %+v, want matching %+v", got, d)
	}

	if _, err := s.DeploymentByID(999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestUpdateGitDeliveryStatus proves a delivery row inserted optimistically
// (as the webhook receiver does — see the v0.5 plan's Task 5) can later be
// flipped to a different status/detail/deployment_id, and that an unknown
// id is a harmless no-op rather than an error.
func TestUpdateGitDeliveryStatus(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)

	delivery, err := s.InsertGitDelivery(GitDelivery{
		AppID: app.ID, Provider: "github", Event: "push", Ref: "refs/heads/main",
		CommitSHA: "sha1", Status: "deployed", Detail: "build queued",
	})
	if err != nil {
		t.Fatal(err)
	}

	dep, err := s.CreateDeploymentFull(app.ID, "", "git", "webhook")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateGitDeliveryStatus(delivery.ID, "error", "clone failed: timed out", &dep.ID); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListGitDeliveries(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(list))
	}
	got := list[0]
	if got.Status != "error" || got.Detail != "clone failed: timed out" {
		t.Fatalf("delivery not updated: %+v", got)
	}
	if got.DeploymentID == nil || *got.DeploymentID != dep.ID {
		t.Fatalf("expected deployment_id %d, got %+v", dep.ID, got.DeploymentID)
	}

	// Unknown id: no error, no rows affected.
	if err := s.UpdateGitDeliveryStatus(999999, "error", "should not apply", nil); err != nil {
		t.Fatalf("update of unknown id should be a no-op, got err: %v", err)
	}
}
