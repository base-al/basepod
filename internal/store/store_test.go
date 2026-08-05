package store

import (
	"errors"
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
