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
