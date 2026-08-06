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

// App is a deployed application. MemoryLimitMB, CPULimit (in cores, e.g.
// 1.5), and PidsLimit are the container resource limits applied to every
// container this app deploys (see internal/podman.CreateSpec and
// internal/deploy's runRollout) — 0 means unlimited for any of the three.
// New apps get the schema defaults (512 MiB / 1.0 cores / 512 pids, see
// migration 00004_resource_limits.sql), which also apply retroactively to
// every app that existed before that migration ran.
//
// AliasScheme records which network-alias namespace this app's current
// (or most recent) container generation actually carries — "legacy" for
// the pre-fix "bp-<slug>" alias, or "v2" for the collision-safe
// "app-<slug>" alias (audit finding M1; see migration
// 00005_alias_scheme.sql and internal/deploy's Routes()/runRollout). It
// starts "legacy" (the schema default) for every app, including brand-new
// ones, and is flipped to "v2" only once a rollout has actually created a
// container with the new alias — Routes() must render whichever alias the
// app's currently-running container really has, not whichever scheme is
// "newest".
//
// DeployStrategy selects how internal/deploy's runRollout cuts over an
// app's container generation on every deploy/rollback — see migration
// 00008_volumes_strategy.sql's doc comment for the full rationale.
// DeployStrategyZeroDowntime (the schema default) creates+starts+probes
// the new container before tearing down the old one; DeployStrategyReplace
// tears the old one down FIRST, which volume-backed (SQLite/Postgres-style)
// apps require to avoid two containers writing one named volume at once,
// at the cost of a failed deploy leaving the app down with no old
// container to fall back to.
// ComposeProject/ComposeService key an app back to the compose.yaml
// service it was created from (both "" for a hand-created app — see
// migration 00009_compose.sql). Internal is true for a service that had no
// `expose:` entries: no Caddy route (internal/deploy's Routes() skips it)
// and no HTTP health probe target (runRollout skips probing it) — see the
// migration's doc comment. Only internal/api's compose apply ever sets any
// of the three.
type App struct {
	ID             int64
	Slug           string
	ImageRef       string
	Port           int
	Status         string
	MemoryLimitMB  int64
	CPULimit       float64
	PidsLimit      int64
	AliasScheme    string
	DeployStrategy string
	ComposeProject string
	ComposeService string
	Internal       bool
}

// AliasSchemeLegacy and AliasSchemeV2 are the two values App.AliasScheme
// (and the apps.alias_scheme column) can hold — see App's doc comment.
const (
	AliasSchemeLegacy = "legacy"
	AliasSchemeV2     = "v2"
)

// DeployStrategyZeroDowntime and DeployStrategyReplace are the two values
// App.DeployStrategy (and the apps.deploy_strategy column) can hold — see
// App's doc comment and migration 00008_volumes_strategy.sql.
const (
	DeployStrategyZeroDowntime = "zero-downtime"
	DeployStrategyReplace      = "replace"
)

// ValidDeployStrategy reports whether s is one of the two recognized
// App.DeployStrategy values — used by the API layer (PATCH
// /api/v1/apps/{slug}) to 422 on an unknown value rather than silently
// persisting garbage that internal/deploy's runRollout would fall through
// to zero-downtime behavior for.
func ValidDeployStrategy(s string) bool {
	return s == DeployStrategyZeroDowntime || s == DeployStrategyReplace
}

// Deployment is a single deploy attempt for an App. StartedAt is always
// set (RFC3339 UTC); FinishedAt is "" until the deployment reaches a
// terminal status (see FinishDeployment). Source is "image" (a registry
// pull, the original v0.1 path), "tarball" (Task 6's build-from-upload
// path), or "git" (the v0.5 git+compose plan's Task 4/5 clone-and-build
// path — manual POST .../deploy/git or a webhook push). BuildLogPath is
// "" for image-sourced deployments, and the path to a build log file on
// disk for tarball- and git-sourced ones (see SetDeploymentBuildLog).
// TriggerKind records what initiated the deploy — "api" for a normal
// deploy, "rollback" for one created by Rollback, and "webhook" for one
// created by the git webhook receiver; named trigger_kind in the schema
// because "trigger" is a SQLite keyword. GitSha is the resolved commit a
// git-sourced deployment built (see migration 00007_git_sources.sql and
// SetDeploymentGitSHA) — "" for every non-git deployment.
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
	GitSha       string
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

// GitSource is an app's connected git repo (push-to-deploy) configuration
// — see migration 00007_git_sources.sql. SecretEncrypted and
// TokenEncrypted are XChaCha20-Poly1305 ciphertexts (see
// internal/crypto.SealAAD/OpenAAD); the caller (internal/api, Task 4 of
// the v0.5 git+compose plan) is responsible for sealing/opening them with
// AAD bound to (AppID, "git_secret") and (AppID, "git_token") respectively
// — the same appID|key binding env var values use (see
// internal/crypto.AAD) — so a ciphertext relocated onto a different app's
// row fails to decrypt there. The store itself never seals or opens
// anything; it persists exactly the ciphertext strings it's given.
// TokenEncrypted is "" for a public repo needing no credential. HookID is
// a random, non-secret URL path segment identifying this app's webhook
// endpoint — never the secret itself.
type GitSource struct {
	ID              int64
	AppID           int64
	URL             string
	Branch          string
	Provider        string
	HookID          string
	SecretEncrypted string
	TokenEncrypted  string
	CreatedAt       string
	UpdatedAt       string
}

// GitDelivery is one received (or attempted) webhook event, recorded for
// the dashboard's "recent deliveries" view (see migration
// 00007_git_sources.sql). Detail is a short human-readable string only —
// NEVER a raw payload body or a secret. DeploymentID is nil unless Status
// is "deployed" (the only outcome that actually triggers one).
type GitDelivery struct {
	ID           int64
	AppID        int64
	ReceivedAt   string
	Provider     string
	Event        string
	Ref          string
	CommitSHA    string
	Status       string
	Detail       string
	DeploymentID *int64
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

// UserByID looks up a user by primary key. Returns ErrNotFound if none
// exists. Used by stream-token validation (see StreamTokenByHash and
// internal/api/stream_token.go) to resolve a stream token's owning user,
// the way UserBySessionTokenHash resolves a session's in one query — a
// stream token row only carries user_id, not the joined user columns.
func (s *Store) UserByID(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, email, name, password_hash, is_superadmin FROM users WHERE id = ?`, id)
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

// StreamToken is a short-lived, single-purpose token minted via
// POST /api/v1/stream-token (see internal/api/stream_token.go) so a
// browser EventSource — which cannot set an Authorization header — never
// has to carry the caller's full-authority session token in a URL query
// string the way the pre-v0.4 SSE routes required (audit finding M4).
// Unlike a session, a stream token authenticates exactly one (Scope,
// Slug, DeploymentNumber) triple: a token minted for one app's
// container-log stream cannot open a different app's, or a build-log
// stream, even before it expires — see requireAuthAppLogs/
// requireAuthBuildLog in internal/api/stream_token.go, which check the
// token's triple against the route actually being requested.
// DeploymentNumber is nil for Scope "app_logs" (which streams by slug
// alone) and non-nil for "build_log" (one specific deployment's build
// log).
type StreamToken struct {
	ID               int64
	UserID           int64
	Scope            string
	Slug             string
	DeploymentNumber *int64
	ExpiresAt        string
}

// CreateStreamToken inserts a new stream token row for userID, scoped to
// (scope, slug, deploymentNumber) and expiring at expires. The caller
// (internal/api's handleCreateStreamToken) owns validating scope and the
// slug/deploymentNumber combination before calling this — the store
// applies them as given.
func (s *Store) CreateStreamToken(userID int64, tokenHash, scope, slug string, deploymentNumber *int64, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO stream_tokens(token_hash, user_id, scope, slug, deployment_number, expires_at) VALUES(?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, scope, slug, nullableInt64(deploymentNumber), expires.UTC().Format(timeFormat))
	return err
}

// StreamTokenByHash looks up a non-expired stream token by its hash.
// Returns ErrNotFound if the token is missing or expired.
//
// It also opportunistically deletes every expired stream token row (not
// just the one being looked up) before the lookup — see
// PruneExpiredStreamTokens. Stream tokens are not pruned by
// internal/server's hourly session-prune ticker: that ticker lives
// outside this milestone's file-ownership split (internal/api,
// internal/store, internal/auth, web/src/lib/sse.ts — see the v0.4 plan's
// Task 5 self-review note), so wiring a second call into it isn't safe to
// do from here. Pruning lazily here instead is an acceptable substitute:
// every stream token is looked up at least once (the SSE route it
// authenticates) within its 5-minute lifetime in the common case, and in
// the worst case (a minted-but-never-used token) it is still bounded
// garbage — 5 minutes' worth of rows, not 30 days' — until the next
// lookup or store restart sweeps it.
func (s *Store) StreamTokenByHash(hash string) (*StreamToken, error) {
	if _, err := s.PruneExpiredStreamTokens(); err != nil {
		return nil, err
	}

	var st StreamToken
	var deploymentNumber sql.NullInt64
	row := s.db.QueryRow(`SELECT user_id, scope, slug, deployment_number, expires_at
		FROM stream_tokens WHERE token_hash = ?`, hash)
	err := row.Scan(&st.UserID, &st.Scope, &st.Slug, &deploymentNumber, &st.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// ID is intentionally left zero: no caller needs the row's primary
	// key, only the (Scope, Slug, DeploymentNumber) triple below, so it's
	// not part of this SELECT.
	if deploymentNumber.Valid {
		n := deploymentNumber.Int64
		st.DeploymentNumber = &n
	}
	return &st, nil
}

// PruneExpiredStreamTokens deletes every stream token whose expiry has
// passed and returns the number of rows removed. Mirrors
// PruneExpiredSessions; see StreamTokenByHash's doc comment for why this
// is invoked lazily from there rather than from internal/server's hourly
// ticker.
func (s *Store) PruneExpiredStreamTokens() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM stream_tokens WHERE expires_at <= ?`, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nullableInt64 converts v into a driver value suitable for a nullable
// INTEGER column: nil becomes SQL NULL, otherwise the pointed-to value.
func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// Session is one saved login session, as returned by ListSessions. TokenHash
// is carried here only so the API layer can compare it against the
// caller's own (already-hashed) bearer token to flag "current" — it must
// never be serialized back to a client.
//
// The sessions table has no user_agent/ip columns (see migration
// 00001_init.sql) — this milestone's session list identifies sessions by
// creation/expiry time only, not "which device", so no additive migration
// was needed to support it.
type Session struct {
	ID        int64
	CreatedAt string
	ExpiresAt string
	TokenHash string
}

// ListSessions returns every session belonging to userID, newest first.
func (s *Store) ListSessions(userID int64) ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, token_hash, created_at, expires_at
		FROM sessions WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.TokenHash, &sess.CreatedAt, &sess.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// DeleteSession removes session id, scoped to userID so one user can never
// revoke another user's session by guessing/enumerating ids. Returns
// ErrNotFound if no session with that id belongs to userID (whether it
// doesn't exist at all, or exists but belongs to someone else — the two are
// indistinguishable to the caller, matching DeleteDomain's cross-owner
// behavior).
func (s *Store) DeleteSession(id, userID int64) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSessionsExcept removes every session belonging to userID EXCEPT the
// one whose token hash is keepTokenHash, returning the number of rows
// removed. Used by a password change to revoke every other session while
// keeping the caller's own alive (see internal/api/auth.go's
// handleChangePassword) — getting this backwards either locks the user out
// of the session they're using right now, or leaves every other
// session (e.g. an attacker's) alive.
func (s *Store) DeleteSessionsExcept(userID int64, keepTokenHash string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keepTokenHash)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdatePassword sets userID's password_hash to hash (an already-computed
// argon2id encoding — see auth.HashPassword). The caller owns verifying the
// current password and the new password's strength before calling this.
func (s *Store) UpdatePassword(userID int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

// DefaultMemoryLimitMB, DefaultCPULimit, and DefaultPidsLimit mirror the
// column defaults migration 00004_resource_limits.sql sets on the apps
// table (memory_limit_mb, cpu_limit, pids_limit) — kept here as named
// constants so CreateApp's returned App (built directly, not re-SELECTed
// after INSERT) reports exactly what the row actually holds, without
// hardcoding the same magic numbers twice. Exported so callers outside
// this package (internal/deploy's applyManifestDefaults, resolving
// issue #1) can tell "an app is still at the schema default" apart from
// "an operator explicitly set this value" without duplicating these
// numbers themselves.
const (
	DefaultMemoryLimitMB int64   = 512
	DefaultCPULimit      float64 = 1.0
	DefaultPidsLimit     int64   = 512
)

// appColumns is the column list (in scan order) shared by CreateApp's
// implicit row shape, scanApp, and ListApps.
const appColumns = `id, slug, image_ref, port, status, memory_limit_mb, cpu_limit, pids_limit, alias_scheme, deploy_strategy, compose_project, compose_service, internal`

// CreateApp inserts a new app with status "created", the schema's default
// resource limits (see defaultMemoryLimitMB etc.), the schema's default
// alias_scheme ("legacy" — see App.AliasScheme's doc comment: it only
// becomes "v2" once a rollout actually creates a container with the new
// alias), and the schema's default deploy_strategy ("zero-downtime" — see
// App.DeployStrategy's doc comment), and returns it.
func (s *Store) CreateApp(slug, imageRef string, port int) (*App, error) {
	res, err := s.db.Exec(`INSERT INTO apps(slug, image_ref, port) VALUES(?, ?, ?)`, slug, imageRef, port)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &App{
		ID: id, Slug: slug, ImageRef: imageRef, Port: port, Status: "created",
		MemoryLimitMB: DefaultMemoryLimitMB, CPULimit: DefaultCPULimit, PidsLimit: DefaultPidsLimit,
		AliasScheme:    AliasSchemeLegacy,
		DeployStrategy: DeployStrategyZeroDowntime,
	}, nil
}

// CreateComposeApp is CreateApp's compose-aware twin (v0.5 Task 8): it
// additionally sets compose_project/compose_service (the (project,
// service) key a re-apply looks the app up by — see migration
// 00009_compose.sql) and internal (no Caddy route, no HTTP probe — see the
// App.Internal doc comment) at insert time, since a compose-created app
// must carry this metadata from the moment it exists, not as a later
// PATCH. imageRef may be "" for a build-sourced service (no image tag
// exists until the first build completes). Only internal/api's compose
// apply calls this; every other app-creation path (handleCreateApp) still
// goes through CreateApp, which leaves all three at their schema defaults
// ("", "", false).
func (s *Store) CreateComposeApp(slug, imageRef string, port int, internal bool, project, service string) (*App, error) {
	res, err := s.db.Exec(`INSERT INTO apps(slug, image_ref, port, compose_project, compose_service, internal) VALUES(?, ?, ?, ?, ?, ?)`,
		slug, imageRef, port, project, service, internal)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &App{
		ID: id, Slug: slug, ImageRef: imageRef, Port: port, Status: "created",
		MemoryLimitMB: DefaultMemoryLimitMB, CPULimit: DefaultCPULimit, PidsLimit: DefaultPidsLimit,
		AliasScheme:    AliasSchemeLegacy,
		DeployStrategy: DeployStrategyZeroDowntime,
		ComposeProject: project,
		ComposeService: service,
		Internal:       internal,
	}, nil
}

func scanApp(row *sql.Row) (*App, error) {
	var a App
	var internal int
	err := row.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status, &a.MemoryLimitMB, &a.CPULimit, &a.PidsLimit, &a.AliasScheme, &a.DeployStrategy, &a.ComposeProject, &a.ComposeService, &internal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Internal = internal != 0
	return &a, nil
}

// AppBySlug looks up an app by slug. Returns ErrNotFound if none exists.
func (s *Store) AppBySlug(slug string) (*App, error) {
	row := s.db.QueryRow(`SELECT `+appColumns+` FROM apps WHERE slug = ?`, slug)
	return scanApp(row)
}

// AppByID looks up an app by primary key. Returns ErrNotFound if none
// exists. Used by internal/deploy's SweepStuckDeployments to resolve a
// stuck deployment row's owning app (a Deployment only carries AppID, not
// the app's slug — see Deployment's doc comment) at boot.
func (s *Store) AppByID(id int64) (*App, error) {
	row := s.db.QueryRow(`SELECT `+appColumns+` FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

// ListApps returns all apps ordered by ID.
func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT ` + appColumns + ` FROM apps ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		var internal int
		if err := rows.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status, &a.MemoryLimitMB, &a.CPULimit, &a.PidsLimit, &a.AliasScheme, &a.DeployStrategy, &a.ComposeProject, &a.ComposeService, &internal); err != nil {
			return nil, err
		}
		a.Internal = internal != 0
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// UpdateAppAliasScheme sets an app's alias_scheme (see App.AliasScheme's
// doc comment) and bumps updated_at. Called by internal/deploy's
// runRollout immediately after a rollout creates a container with the
// new "app-<slug>" alias, so the very next Routes() computation (run
// before that same rollout's cutover — see runRollout) already renders
// the new upstream for this app.
func (s *Store) UpdateAppAliasScheme(id int64, scheme string) error {
	_, err := s.db.Exec(`UPDATE apps SET alias_scheme = ?, updated_at = ? WHERE id = ?`,
		scheme, time.Now().UTC().Format(timeFormat), id)
	return err
}

// UpdateAppDeployStrategy sets an app's deploy_strategy (see App's doc
// comment) and bumps updated_at. The caller (API layer) owns validating
// the value against ValidDeployStrategy before calling this — the store
// applies it as given.
func (s *Store) UpdateAppDeployStrategy(id int64, strategy string) error {
	_, err := s.db.Exec(`UPDATE apps SET deploy_strategy = ?, updated_at = ? WHERE id = ?`,
		strategy, time.Now().UTC().Format(timeFormat), id)
	return err
}

// UpdateAppLimits sets an app's container resource limits (see the App
// struct doc comment for the zero-means-unlimited convention) and bumps
// updated_at. The caller (API layer) owns validating the values before
// calling this — the store applies them as given.
func (s *Store) UpdateAppLimits(id int64, memoryLimitMB int64, cpuLimit float64, pidsLimit int64) error {
	_, err := s.db.Exec(`UPDATE apps SET memory_limit_mb = ?, cpu_limit = ?, pids_limit = ?, updated_at = ? WHERE id = ?`,
		memoryLimitMB, cpuLimit, pidsLimit, time.Now().UTC().Format(timeFormat), id)
	return err
}

// UpdateAppPort sets an app's container port and bumps updated_at — used
// by internal/deploy.applyManifestDefaults to apply a basepod.yaml
// manifest's `port` onto an app that hasn't been explicitly configured
// with one yet (see that function's doc comment for the precedence
// rule). The caller owns deciding whether overwriting the app's current
// port is appropriate; the store applies it as given.
func (s *Store) UpdateAppPort(id int64, port int) error {
	_, err := s.db.Exec(`UPDATE apps SET port = ?, updated_at = ? WHERE id = ?`,
		port, time.Now().UTC().Format(timeFormat), id)
	return err
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

// DeleteApp removes an app (and its deployments and declared-volume
// bookkeeping rows, via ON DELETE CASCADE — see migration
// 00008_volumes_strategy.sql). This only ever removes the volumes TABLE
// row (metadata: logical name + container path); it never removes the
// underlying libpod volume the data actually lives in — see
// internal/api's handleDeleteApp, which lists this app's volumes via
// ListVolumes BEFORE calling this, precisely so it can still log their
// (surviving) libpod names afterward.
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

// SetDeploymentGitSHA sets a deployment's git_sha — used by the git
// deploy paths (v0.5 plan Tasks 4/5) once the commit a clone actually
// checked out is known: a manual POST .../deploy/git deploy resolves it
// before the deployment row is even created (the clone runs
// synchronously there), while a webhook-triggered deploy resolves it only
// once its background clone completes (see
// deploy.Engine.DeployGitCloneAsync) — so this is called at a different
// point in each path, unlike SetDeploymentImage/SetDeploymentBuildLog
// which both tarball paths call at the same point.
func (s *Store) SetDeploymentGitSHA(id int64, sha string) error {
	_, err := s.db.Exec(`UPDATE deployments SET git_sha = ? WHERE id = ?`, sha, id)
	return err
}

// scanDeploymentRows scans the common deployment column set (id, app_id,
// number, image_ref, status, error, started_at, finished_at, source,
// build_log_path, trigger_kind, git_sha — in that order) shared by
// ListDeployments, DeploymentByNumber, and DeploymentByID.
func scanDeploymentRow(scan func(...any) error) (Deployment, error) {
	var d Deployment
	var finishedAt sql.NullString
	err := scan(&d.ID, &d.AppID, &d.Number, &d.ImageRef, &d.Status, &d.Error, &d.StartedAt, &finishedAt,
		&d.Source, &d.BuildLogPath, &d.TriggerKind, &d.GitSha)
	if err != nil {
		return Deployment{}, err
	}
	d.FinishedAt = finishedAt.String
	return d, nil
}

const deploymentColumns = `id, app_id, number, image_ref, status, error, started_at, finished_at, source, build_log_path, trigger_kind, git_sha`

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

// DeploymentByID looks up a single deployment by its store row ID, as
// opposed to DeploymentByNumber's per-app Number — used by
// internal/api's git delivery listing (Task 4/5) to resolve a
// GitDelivery.DeploymentID into a displayable deployment number. Returns
// ErrNotFound if no such deployment exists.
func (s *Store) DeploymentByID(id int64) (*Deployment, error) {
	row := s.db.QueryRow(`SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, id)
	d, err := scanDeploymentRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDeployingDeployments returns every deployment across every app whose
// Status is "deploying", ordered by app_id then number. Used at boot (see
// internal/deploy.Engine.SweepStuckDeployments) to find deployments whose
// background goroutine never reached a terminal status because the
// process died mid-deploy.
func (s *Store) ListDeployingDeployments() ([]Deployment, error) {
	rows, err := s.db.Query(`SELECT ` + deploymentColumns + `
		FROM deployments WHERE status = 'deploying' ORDER BY app_id, number`)
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

// DefaultGitDeliveryKeep is how many of an app's most recent git
// deliveries InsertGitDelivery retains — see PruneGitDeliveries.
const DefaultGitDeliveryKeep = 50

const gitSourceColumns = `id, app_id, url, branch, provider, hook_id, secret_encrypted, token_encrypted, created_at, updated_at`

func scanGitSource(scan func(...any) error) (GitSource, error) {
	var g GitSource
	err := scan(&g.ID, &g.AppID, &g.URL, &g.Branch, &g.Provider, &g.HookID,
		&g.SecretEncrypted, &g.TokenEncrypted, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// UpsertGitSource inserts appID's git source config, or replaces it in
// place (keyed by the UNIQUE app_id column — an app has at most one
// connected repo) if one already exists. created_at is preserved across
// an update; updated_at always advances to now. The caller owns deciding
// whether hook_id/secret should stay stable across a reconnect (Task 4's
// PUT handler, which re-PUTs the existing values unless the operator
// asked to rotate) or change; this method persists exactly the row it's
// given. Returns ErrNotFound if appID doesn't reference an existing app;
// returns the underlying error if hook_id collides with a different row
// (UNIQUE constraint — hook_id values are 24 random bytes hex-encoded by
// the caller, so a collision should never happen in practice).
func (s *Store) UpsertGitSource(g GitSource) (*GitSource, error) {
	now := time.Now().UTC().Format(timeFormat)
	_, err := s.db.Exec(`INSERT INTO git_sources(app_id, url, branch, provider, hook_id, secret_encrypted, token_encrypted, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET
			url = excluded.url,
			branch = excluded.branch,
			provider = excluded.provider,
			hook_id = excluded.hook_id,
			secret_encrypted = excluded.secret_encrypted,
			token_encrypted = excluded.token_encrypted,
			updated_at = excluded.updated_at`,
		g.AppID, g.URL, g.Branch, g.Provider, g.HookID, g.SecretEncrypted, g.TokenEncrypted, now, now)
	if err != nil {
		// Map FOREIGN KEY constraint violation to ErrNotFound (mirrors
		// AddDomain).
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GitSourceByAppID(g.AppID)
}

// GitSourceByAppID looks up an app's git source config. Returns
// ErrNotFound if the app has none connected.
func (s *Store) GitSourceByAppID(appID int64) (*GitSource, error) {
	row := s.db.QueryRow(`SELECT `+gitSourceColumns+` FROM git_sources WHERE app_id = ?`, appID)
	g, err := scanGitSource(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GitSourceByHookID looks up a git source by its webhook path segment —
// the unauthenticated webhook receiver's only way to resolve which app a
// POST /api/v1/webhooks/git/{hookID} request is for. Returns ErrNotFound
// if no source has that hook_id, which the receiver must map to the same
// generic 404 an unknown route gets — no oracle for whether a hook ID was
// ever valid (see the v0.5 git+compose plan's Task 5).
func (s *Store) GitSourceByHookID(hookID string) (*GitSource, error) {
	row := s.db.QueryRow(`SELECT `+gitSourceColumns+` FROM git_sources WHERE hook_id = ?`, hookID)
	g, err := scanGitSource(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// DeleteGitSource disconnects appID's git repo, if any — not an error if
// none was connected.
func (s *Store) DeleteGitSource(appID int64) error {
	_, err := s.db.Exec(`DELETE FROM git_sources WHERE app_id = ?`, appID)
	return err
}

const gitDeliveryColumns = `id, app_id, received_at, provider, event, ref, commit_sha, status, detail, deployment_id`

func scanGitDelivery(scan func(...any) error) (GitDelivery, error) {
	var d GitDelivery
	var deploymentID sql.NullInt64
	err := scan(&d.ID, &d.AppID, &d.ReceivedAt, &d.Provider, &d.Event, &d.Ref,
		&d.CommitSHA, &d.Status, &d.Detail, &deploymentID)
	if err != nil {
		return GitDelivery{}, err
	}
	if deploymentID.Valid {
		v := deploymentID.Int64
		d.DeploymentID = &v
	}
	return d, nil
}

// InsertGitDelivery records one received (or attempted) webhook event for
// appID (ReceivedAt is always set to now, overriding whatever the caller
// passed in d), then prunes that app's delivery log down to the
// DefaultGitDeliveryKeep most recent rows (see PruneGitDeliveries) — so a
// flood of webhook traffic, forged or otherwise, can never grow this
// table unboundedly. Returns ErrNotFound if appID doesn't reference an
// existing app.
func (s *Store) InsertGitDelivery(d GitDelivery) (*GitDelivery, error) {
	now := time.Now().UTC().Format(timeFormat)
	res, err := s.db.Exec(`INSERT INTO git_deliveries(app_id, received_at, provider, event, ref, commit_sha, status, detail, deployment_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.AppID, now, d.Provider, d.Event, d.Ref, d.CommitSHA, d.Status, d.Detail, nullableInt64(d.DeploymentID))
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := s.PruneGitDeliveries(d.AppID, DefaultGitDeliveryKeep); err != nil {
		return nil, err
	}
	d.ID = id
	d.ReceivedAt = now
	return &d, nil
}

// UpdateGitDeliveryStatus updates a previously recorded delivery's status,
// detail, and deployment_id — used by the webhook receiver (v0.5 plan
// Task 5) when a delivery's true outcome is only known after the row was
// already inserted optimistically: an accepted push is recorded
// status="deployed" as soon as its build is queued (the deployment row
// exists, but its clone/build runs in the background — see
// deploy.Engine.DeployGitCloneAsync), and this flips it to "error" if
// that clone or build actually fails. deploymentID may be nil to leave
// the column NULL. Not an error if id doesn't exist — best-effort
// bookkeeping, mirroring FinishDeployment's own shape.
func (s *Store) UpdateGitDeliveryStatus(id int64, status, detail string, deploymentID *int64) error {
	_, err := s.db.Exec(`UPDATE git_deliveries SET status = ?, detail = ?, deployment_id = ? WHERE id = ?`,
		status, detail, nullableInt64(deploymentID), id)
	return err
}

// ListGitDeliveries returns appID's most recent git deliveries, newest
// first, capped at limit rows.
func (s *Store) ListGitDeliveries(appID int64, limit int) ([]GitDelivery, error) {
	rows, err := s.db.Query(`SELECT `+gitDeliveryColumns+`
		FROM git_deliveries WHERE app_id = ? ORDER BY received_at DESC, id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deliveries []GitDelivery
	for rows.Next() {
		d, err := scanGitDelivery(rows.Scan)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

// PruneGitDeliveries deletes every one of appID's git deliveries except
// the keep most recent (ordered newest-first by received_at, ties broken
// by id — matching ListGitDeliveries' ordering), returning the number of
// rows removed. Called automatically by InsertGitDelivery after every
// insert; exposed separately so it's independently testable and callable
// with a non-default keep.
func (s *Store) PruneGitDeliveries(appID int64, keep int) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM git_deliveries WHERE app_id = ? AND id NOT IN (
		SELECT id FROM git_deliveries WHERE app_id = ? ORDER BY received_at DESC, id DESC LIMIT ?
	)`, appID, appID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------
// v0.5: named volumes + deploy_strategy (migration
// 00008_volumes_strategy.sql). Kept in its own block for the same reason
// as the git sources/deliveries block above it — see that block's
// comment; this was originally appended at the end of the file by a
// concurrent v0.5 branch, which is what produced the merge conflict this
// section's resolution untangled. Future additions here should live near
// related code rather than repeat that pattern.
// ---------------------------------------------------------------------

// Volume is a named volume declared for an App (see migration
// 00008_volumes_strategy.sql). Name is the short logical name an operator
// (or Task 8's compose apply) assigns; the actual libpod volume a rollout
// creates/mounts is derived from it as "bp-<app.Slug>-<name>" (see
// internal/deploy.VolumeName) rather than stored here, so there is only
// one source of truth for that derivation. ContainerPath is the absolute
// path inside the container the volume is mounted at.
type Volume struct {
	ID            int64
	AppID         int64
	Name          string
	ContainerPath string
	CreatedAt     string
}

const volumeColumns = `id, app_id, name, container_path, created_at`

func scanVolume(scan func(...any) error) (Volume, error) {
	var v Volume
	err := scan(&v.ID, &v.AppID, &v.Name, &v.ContainerPath, &v.CreatedAt)
	return v, err
}

// UpsertVolume declares a named volume for appID, or updates its
// container_path if one with this name already exists (UNIQUE(app_id,
// name) — see the migration). Manual volume create/delete is out of scope
// this milestone (v0.6+): Task 8's compose apply is this method's only
// caller in production, deliberately upserting rather than failing on a
// re-apply of an unchanged compose file.
func (s *Store) UpsertVolume(appID int64, name, containerPath string) (*Volume, error) {
	now := time.Now().UTC().Format(timeFormat)
	if _, err := s.db.Exec(`
		INSERT INTO volumes(app_id, name, container_path, created_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(app_id, name) DO UPDATE SET container_path = excluded.container_path`,
		appID, name, containerPath, now); err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`SELECT `+volumeColumns+` FROM volumes WHERE app_id = ? AND name = ?`, appID, name)
	v, err := scanVolume(row.Scan)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVolumes returns appID's declared volumes, ordered by name. Backs
// GET /api/v1/apps/{slug}/volumes and internal/deploy's runRollout (which
// mounts every one of them into the app's next container).
func (s *Store) ListVolumes(appID int64) ([]Volume, error) {
	rows, err := s.db.Query(`SELECT `+volumeColumns+` FROM volumes WHERE app_id = ? ORDER BY name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var volumes []Volume
	for rows.Next() {
		v, err := scanVolume(rows.Scan)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, v)
	}
	return volumes, rows.Err()
}

// ---------------------------------------------------------------------
// v0.5 Task 8: compose apply (migration 00009_compose.sql) — kept in its
// own block for the same reason as the git sources/deliveries and
// volumes/deploy_strategy blocks above it.
// ---------------------------------------------------------------------

// UpdateAppInternal sets an app's internal flag (see App's doc comment)
// and bumps updated_at. Compose re-applies this on every apply (a service
// can gain or lose its `expose:` entry between compose.yaml revisions), so
// this is idempotent by design rather than a one-time creation-only
// setting.
func (s *Store) UpdateAppInternal(id int64, internal bool) error {
	_, err := s.db.Exec(`UPDATE apps SET internal = ?, updated_at = ? WHERE id = ?`,
		internal, time.Now().UTC().Format(timeFormat), id)
	return err
}

// ListAppsByComposeProject returns every app whose compose_project equals
// project, ordered by id. Used by internal/api/compose.go to build
// compose.PlanContext.ExistingApps (this project's apps from a prior
// apply, so BuildPlan can tell create from update and detect orphans — a
// service present in the store for this project but absent from the
// compose file being applied now) and to compute an app slug's remaining
// membership when reporting orphans.
func (s *Store) ListAppsByComposeProject(project string) ([]App, error) {
	rows, err := s.db.Query(`SELECT `+appColumns+` FROM apps WHERE compose_project = ? ORDER BY id`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		var internal int
		if err := rows.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status, &a.MemoryLimitMB, &a.CPULimit, &a.PidsLimit, &a.AliasScheme, &a.DeployStrategy, &a.ComposeProject, &a.ComposeService, &internal); err != nil {
			return nil, err
		}
		a.Internal = internal != 0
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// ListComposeApps returns every app with a non-empty compose_service —
// i.e. every service, across every compose project, that some prior
// compose apply created — ordered by id. Used by internal/api/compose.go
// to build compose.PlanContext.TakenAliases: only a compose service's
// container ever carries a bare-service-name network alias (see
// internal/deploy's runRollout), so this (not every app) is the full set
// of existing bare-alias claims a new apply's aliases must be checked
// against (audit finding M1's regression risk — see the v0.5 plan's
// self-review notes).
func (s *Store) ListComposeApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT ` + appColumns + ` FROM apps WHERE compose_service != '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var a App
		var internal int
		if err := rows.Scan(&a.ID, &a.Slug, &a.ImageRef, &a.Port, &a.Status, &a.MemoryLimitMB, &a.CPULimit, &a.PidsLimit, &a.AliasScheme, &a.DeployStrategy, &a.ComposeProject, &a.ComposeService, &internal); err != nil {
			return nil, err
		}
		a.Internal = internal != 0
		apps = append(apps, a)
	}
	return apps, rows.Err()
}
