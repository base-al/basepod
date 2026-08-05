package store

import (
	"path/filepath"
	"testing"
	"time"
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

func TestDeploymentNumbering(t *testing.T) {
	s := open(t)
	app, _ := s.CreateApp("blog", "nginx:alpine", 80)
	d1, _ := s.CreateDeployment(app.ID, "nginx:alpine")
	d2, _ := s.CreateDeployment(app.ID, "nginx:alpine")
	if d1.Number != 1 || d2.Number != 2 {
		t.Fatalf("numbers: %d, %d", d1.Number, d2.Number)
	}
	s.FinishDeployment(d2.ID, "healthy", "")
	ds, _ := s.ListDeployments(app.ID)
	if len(ds) != 2 || ds[0].Number != 2 || ds[0].Status != "healthy" {
		t.Fatalf("list wrong: %+v", ds)
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
