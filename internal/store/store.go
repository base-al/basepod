// Package store provides the SQLite-backed persistence layer for BasePod.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"os"
	"path/filepath"
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

// Deployment is a single deploy attempt for an App.
type Deployment struct {
	ID       int64
	AppID    int64
	Number   int
	ImageRef string
	Status   string
	Error    string
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

// CreateDeployment inserts a new deployment for appID. The Number is
// per-app max+1 (starting at 1), and Status starts "deploying".
func (s *Store) CreateDeployment(appID int64, imageRef string) (*Deployment, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(number), 0) + 1 FROM deployments WHERE app_id = ?`, appID).Scan(&n); err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`INSERT INTO deployments(app_id, number, image_ref) VALUES(?, ?, ?)`, appID, n, imageRef)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Deployment{ID: id, AppID: appID, Number: n, ImageRef: imageRef, Status: "deploying"}, nil
}

// FinishDeployment sets a deployment's terminal status and error message,
// and records finished_at.
func (s *Store) FinishDeployment(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, time.Now().UTC().Format(timeFormat), id)
	return err
}

// ListDeployments returns all deployments for appID, newest first.
func (s *Store) ListDeployments(appID int64) ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT id, app_id, number, image_ref, status, error
		FROM deployments WHERE app_id = ? ORDER BY number DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.AppID, &d.Number, &d.ImageRef, &d.Status, &d.Error); err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}
