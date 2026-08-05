// Package store provides the SQLite-backed persistence layer for BasePod.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a lookup finds no matching row.
var ErrNotFound = errors.New("not found")

// timeFormat is the RFC3339 layout used to store timestamps (UTC).
const timeFormat = time.RFC3339

// User is a BasePod account.
type User struct {
	ID           int64
	Email        string
	Name         string
	PasswordHash string
	IsSuperadmin bool
}

// App is a deployed application.
type App struct {
	ID       int64
	Slug     string
	ImageRef string
	Port     int
	Status   string
}

// Deployment is a single deploy attempt for an App. StartedAt is always
// set (RFC3339 UTC); FinishedAt is "" until the deployment reaches a
// terminal status (see FinishDeployment). Source is "image" (a registry
// pull, the original v0.1 path) or "tarball" (Task 6's build-from-upload
// path). BuildLogPath is "" for image-sourced deployments, and the path
// to a build log file on disk for tarball-sourced ones (see
// SetDeploymentBuildLog). TriggerKind records what initiated the deploy
// ("api" today; named trigger_kind in the schema because "trigger" is a
// SQLite keyword) — reserved for a future non-API trigger (e.g. a cron
// redeploy).
type Deployment struct {
	ID           int64
	AppID        int64
	Number       int
	ImageRef     string
	Status       string
	Error        string
	StartedAt    string
	FinishedAt   string
	Source       string
	BuildLogPath string
	TriggerKind  string
}

// EnvVar is an environment variable for an App.
type EnvVar struct {
	ID             int64
	AppID          int64
	Key            string
	ValueEncrypted string
	IsSecret       bool
}

// Domain is a hostname mapped to an App.
type Domain struct {
	ID       int64
	AppID    int64
	Hostname string
}

// Store wraps a SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path, configures
// WAL mode, a busy timeout, and foreign keys, and runs pending migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-writer discipline

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		db.Close()
		return nil, err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SetSetting upserts a key/value pair in the settings table.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Setting returns the value for key, or "" if unset (no error in that case).
func (s *Store) Setting(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// CreateUser inserts a new user and returns its ID.
func (s *Store) CreateUser(email, name, hash string, super bool) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users(email, name, password_hash, is_superadmin) VALUES(?, ?, ?, ?)`,
		email, name, hash, super)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var isSuper int
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &isSuper)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.IsSuperadmin = isSuper != 0
	return &u, nil
}

// UserByEmail looks up a user by email. Returns ErrNotFound if none exists.
func (s *Store) UserByEmail(email string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, email, name, password_hash, is_superadmin FROM users WHERE email = ?`, email)
	return scanUser(row)
}

// CountUsers returns the total number of users in the database.
func (s *Store) CountUsers() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

// CreateSession inserts a new session for userID with the given token hash
// and expiry time (stored as RFC3339 UTC).
func (s *Store) CreateSession(userID int64, tokenHash string, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions(user_id, token_hash, expires_at) VALUES(?, ?, ?)`,
		userID, tokenHash, expires.UTC().Format(timeFormat))
	return err
}

// UserBySessionTokenHash looks up the user owning a non-expired session with
// the given token hash. Returns ErrNotFound if the session is missing or expired.
func (s *Store) UserBySessionTokenHash(hash string) (*User, error) {
	row := s.db.QueryRow(`SELECT u.id, u.email, u.name, u.password_hash, u.is_superadmin
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.token_hash = ? AND se.expires_at > ?`, hash, time.Now().UTC().Format(timeFormat))
	return scanUser(row)
}

// DeleteSessionByTokenHash removes the session with the given token hash, if
// any. It is a no-op success (nil error) if no session matches — logout is
// idempotent, not a lookup that must find something.
func (s *Store) DeleteSessionByTokenHash(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PruneExpiredSessions deletes every session whose expiry has passed and
// returns the number of rows removed.
func (s *Store) PruneExpiredSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CreateApp inserts a new app with status "created" and returns it.
func (s *Store) CreateApp(slug, imageRef string, port int) (*App, error) {
	res, err := s.db.Exec(`INSERT INTO apps(slug, image_ref, port) VALUES(?, ?, ?)`, slug, imageRef, port)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &App{ID: id, Slug: slug, ImageRef: imageRef, Port: port, Status: "created"}, nil
}

func scanApp(row *sql.Row) (*App, error) {
	var a App
	err := row.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AppBySlug looks up an app by slug. Returns ErrNotFound if none exists.
func (s *Store) AppBySlug(slug string) (*App, error) {
	row := s.db.QueryRow(`SELECT id, slug, image_ref, port, status FROM apps WHERE slug = ?`, slug)
	return scanApp(row)
}

// ListApps returns all apps ordered by ID.
func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT id, slug, image_ref, port, status FROM apps ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// UpdateAppStatus sets an app's status and bumps updated_at.
func (s *Store) UpdateAppStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE apps SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(timeFormat), id)
	return err
}

// UpdateAppImage sets an app's image_ref and bumps updated_at.
func (s *Store) UpdateAppImage(id int64, imageRef string) error {
	_, err := s.db.Exec(`UPDATE apps SET image_ref = ?, updated_at = ? WHERE id = ?`,
		imageRef, time.Now().UTC().Format(timeFormat), id)
	return err
}

// DeleteApp removes an app (and its deployments, via ON DELETE CASCADE).
func (s *Store) DeleteApp(id int64) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	return err
}

// CreateDeployment inserts a new deployment for appID with source "image"
// and trigger_kind "api" — the original v0.1 registry-pull path. It
// delegates to CreateDeploymentFull; see that doc comment for details.
func (s *Store) CreateDeployment(appID int64, imageRef string) (*Deployment, error) {
	return s.CreateDeploymentFull(appID, imageRef, "image", "api")
}

// CreateDeploymentFull inserts a new deployment for appID with an
// explicit source ("image" or "tarball") and triggerKind (what initiated
// the deploy; "api" today). The Number is per-app max+1 (starting at 1),
// and Status starts "deploying". started_at is set explicitly (rather
// than left to the column's SQL default) so the returned Deployment's
// StartedAt matches exactly what was persisted. imageRef may be ""
// (Task 6's tarball path: the row is created before the image tag exists,
// so the deployment number is addressable for the build log path; see
// SetDeploymentImage).
func (s *Store) CreateDeploymentFull(appID int64, imageRef, source, triggerKind string) (*Deployment, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM deployments WHERE app_id = ?`, appID).Scan(&n); err != nil {
		return nil, err
	}
	startedAt := time.Now().UTC().Format(timeFormat)
	res, err := s.db.Exec(`INSERT INTO deployments(app_id, number, image_ref, started_at, source, trigger_kind) VALUES(?, ?, ?, ?, ?, ?)`,
		appID, n, imageRef, startedAt, source, triggerKind)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Deployment{
		ID: id, AppID: appID, Number: n, ImageRef: imageRef, Status: "deploying", StartedAt: startedAt,
		Source: source, TriggerKind: triggerKind,
	}, nil
}

// FinishDeployment sets a deployment's terminal status and error message,
// and records finished_at.
func (s *Store) FinishDeployment(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, time.Now().UTC().Format(timeFormat), id)
	return err
}

// SetDeploymentImage sets a deployment's image_ref — used by the tarball
// build path (Task 6) once the local build produces an image tag, since
// CreateDeploymentFull creates the row with imageRef "" before the build
// even starts (see DeployBuild).
func (s *Store) SetDeploymentImage(id int64, imageRef string) error {
	_, err := s.db.Exec(`UPDATE deployments SET image_ref = ? WHERE id = ?`, imageRef, id)
	return err
}

// SetDeploymentBuildLog sets a deployment's build_log_path — used by the
// tarball build path (Task 6) to record where its build log was written,
// as soon as that path is known (before the build even finishes), so a
// failed build's log is still addressable.
func (s *Store) SetDeploymentBuildLog(id int64, path string) error {
	_, err := s.db.Exec(`UPDATE deployments SET build_log_path = ? WHERE id = ?`, path, id)
	return err
}

// scanDeploymentRows scans the common deployment column set (id, app_id,
// number, image_ref, status, error, started_at, finished_at, source,
// build_log_path, trigger_kind — in that order) shared by ListDeployments
// and DeploymentByNumber.
func scanDeploymentRow(scan func(...any) error) (Deployment, error) {
	var d Deployment
	var finishedAt sql.NullString
	err := scan(&d.ID, &d.AppID, &d.Number, &d.ImageRef, &d.Status, &d.Error, &d.StartedAt, &finishedAt,
		&d.Source, &d.BuildLogPath, &d.TriggerKind)
	if err != nil {
		return Deployment{}, err
	}
	d.FinishedAt = finishedAt.String
	return d, nil
}

const deploymentColumns = `id, app_id, number, image_ref, status, error, started_at, finished_at, source, build_log_path, trigger_kind`

// ListDeployments returns all deployments for appID, newest first.
func (s *Store) ListDeployments(appID int64) ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT `+deploymentColumns+`
		FROM deployments WHERE app_id = ? ORDER BY number DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []Deployment
	for rows.Next() {
		d, err := scanDeploymentRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}

// DeploymentByNumber looks up a single deployment by appID and its
// per-app Number. Returns ErrNotFound if none exists.
func (s *Store) DeploymentByNumber(appID int64, number int) (*Deployment, error) {
	row := s.db.QueryRow(`SELECT `+deploymentColumns+`
		FROM deployments WHERE app_id = ? AND number = ?`, appID, number)
	d, err := scanDeploymentRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertEnvVar inserts or updates an environment variable for appID.
func (s *Store) UpsertEnvVar(appID int64, key, valueEncrypted string, isSecret bool) error {
	_, err := s.db.Exec(`INSERT INTO env_vars(app_id, key, value_encrypted, is_secret)
		VALUES(?, ?, ?, ?) ON CONFLICT(app_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, is_secret = excluded.is_secret`,
		appID, key, valueEncrypted, isSecret)
	return err
}

// DeleteEnvVar removes an environment variable for appID by key.
func (s *Store) DeleteEnvVar(appID int64, key string) error {
	_, err := s.db.Exec(`DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, key)
	return err
}

// ReplaceEnvVars replaces appID's entire env var set in a single
// transaction: every entry in vars is upserted, and every existing key
// not present in vars is deleted. This is what backs a full-replace PUT
// — doing the upserts and prunes as individual statements outside a
// transaction would risk leaving a mix of old and new values if one of
// them failed partway through.
func (s *Store) ReplaceEnvVars(appID int64, vars []EnvVar) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT key FROM env_vars WHERE app_id = ?`, appID)
	if err != nil {
		return err
	}
	var existingKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		existingKeys = append(existingKeys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	keep := make(map[string]bool, len(vars))
	for _, v := range vars {
		keep[v.Key] = true
		if _, err := tx.Exec(`INSERT INTO env_vars(app_id, key, value_encrypted, is_secret)
			VALUES(?, ?, ?, ?) ON CONFLICT(app_id, key) DO UPDATE SET value_encrypted = excluded.value_encrypted, is_secret = excluded.is_secret`,
			appID, v.Key, v.ValueEncrypted, v.IsSecret); err != nil {
			return err
		}
	}

	for _, k := range existingKeys {
		if keep[k] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, k); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ListEnvVars returns all environment variables for appID, ordered by key.
func (s *Store) ListEnvVars(appID int64) ([]EnvVar, error) {
	rows, err := s.db.Query(`SELECT id, app_id, key, value_encrypted, is_secret
		FROM env_vars WHERE app_id = ? ORDER BY key`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var envVars []EnvVar
	for rows.Next() {
		var ev EnvVar
		var isSecret int
		if err := rows.Scan(&ev.ID, &ev.AppID, &ev.Key, &ev.ValueEncrypted, &isSecret); err != nil {
			return nil, err
		}
		ev.IsSecret = isSecret != 0
		envVars = append(envVars, ev)
	}
	return envVars, rows.Err()
}

func scanDomain(row *sql.Row) (*Domain, error) {
	var d Domain
	err := row.Scan(&d.ID, &d.AppID, &d.Hostname)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AddDomain adds a domain for appID. Returns ErrNotFound if appID does not exist.
// Returns an error if the hostname already exists (UNIQUE constraint).
func (s *Store) AddDomain(appID int64, hostname string) (*Domain, error) {
	res, err := s.db.Exec(`INSERT INTO domains(app_id, hostname) VALUES(?, ?)`, appID, hostname)
	if err != nil {
		// Map FOREIGN KEY constraint violation to ErrNotFound
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Domain{ID: id, AppID: appID, Hostname: hostname}, nil
}

// ListDomains returns all domains for appID.
func (s *Store) ListDomains(appID int64) ([]Domain, error) {
	rows, err := s.db.Query(`SELECT id, app_id, hostname FROM domains WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.AppID, &d.Hostname); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// ListAllDomains returns all domains across all apps.
func (s *Store) ListAllDomains() ([]Domain, error) {
	rows, err := s.db.Query(`SELECT id, app_id, hostname FROM domains`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.AppID, &d.Hostname); err != nil {
			return nil, err
		}
		domains = append(domains, d)
	}
	return domains, rows.Err()
}

// DeleteDomain removes a domain by ID.
func (s *Store) DeleteDomain(id int64) error {
	_, err := s.db.Exec(`DELETE FROM domains WHERE id = ?`, id)
	return err
}

// DomainByHostname looks up a domain by hostname. Returns ErrNotFound if none exists.
func (s *Store) DomainByHostname(hostname string) (*Domain, error) {
	row := s.db.QueryRow(`SELECT id, app_id, hostname FROM domains WHERE hostname = ?`, hostname)
	return scanDomain(row)
}
