package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/base-al/basepod/internal/gitsource"
	"github.com/base-al/basepod/internal/store"
)

// GitFetcher shallow-clones a configured git repo into a packed build
// context — the subset of *gitsource.Cloner this package needs, declared
// here (rather than depending on gitsource's concrete type) so handlers
// can be tested against a fake instead of shelling out to a real git
// binary. *gitsource.Cloner satisfies this; see the compile-time
// assertion in git_test.go.
type GitFetcher interface {
	Fetch(ctx context.Context, rawURL, branch, token string) (gzTar io.ReadCloser, headSHA string, err error)
}

// gitURLAllowedSchemes is the scheme allowlist handlePutGitSource enforces
// at connect time. Deliberately just "https": the URL is set by an
// authenticated operator who can already deploy arbitrary containers, so
// this is basic input hygiene, not the SSRF-relevant control (that lives
// in internal/gitsource.Cloner, which never lets the webhook payload
// choose a URL at all — see webhook.go).
var gitURLAllowedSchemes = map[string]bool{"https": true}

// gitCloneTimeout bounds a manual "deploy now" trigger's synchronous
// clone (handleDeployGit) — generous enough for a legitimate shallow
// clone, but bounded so a stalled remote can't hang the HTTP request
// forever (internal/gitsource.Cloner enforces its own, generally shorter,
// timeout internally; this is a backstop at the API layer).
const gitCloneTimeout = 5 * time.Minute

// newHookID returns a fresh, random, non-secret webhook path segment: 24
// random bytes, hex-encoded (48 hex characters) — see migration
// 00007_git_sources.sql's git_sources.hook_id column.
func newHookID() (string, error) {
	return randomHex(24)
}

// newGitSecret returns a fresh HMAC/shared-secret webhook secret: 16
// random bytes, hex-encoded (32 hex characters, per the v0.5 plan's Task
// 4: "the secret (32 hex chars)").
func newGitSecret() (string, error) {
	return randomHex(16)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: generate random value: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// detectProvider guesses which forge rawURL points at, from its hostname
// alone — informational only (stored in git_sources.provider, surfaced in
// the dashboard's connect-repo hints per Task 9), never used to change
// verification or clone behavior: the webhook receiver accepts whichever
// signature header is actually present (see webhook.go's
// verifyWebhookSignature), regardless of what this guessed.
func detectProvider(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "github"):
		return "github"
	case strings.Contains(host, "gitlab"):
		return "gitlab"
	case strings.Contains(host, "gitea"):
		return "gitea"
	default:
		return ""
	}
}

// gitSourceResponse is the wire shape of PUT/GET .../apps/{slug}/git and
// POST .../apps/{slug}/git/rotate-secret. Secret is write-only, matching
// every other secret in this product (env values, the deploy token, the
// audit's stated principle that secrets don't come back out — issue
// #13): it's empty/omitted on every response EXCEPT the one that just
// minted it — a first connect (handlePutGitSource, hadExisting == false)
// or an explicit rotation (handleRotateGitSecret) — where it carries the
// new plaintext exactly once, the operator's only chance to copy it
// before it's sealed at rest. GET and a steady-state re-PUT never
// populate it at all. Token is masked the same way, to "set" or "" — the
// deploy token is write-only and never round-trips.
type gitSourceResponse struct {
	URL        string   `json:"url"`
	Branch     string   `json:"branch"`
	Provider   string   `json:"provider"`
	HookID     string   `json:"hook_id"`
	Secret     string   `json:"secret,omitempty"`
	Token      string   `json:"token"`
	WebhookURL string   `json:"webhook_url"`
	Warnings   []string `json:"warnings,omitempty"`
}

// webhookURLFor builds the full webhook URL for hookID from the
// dashboard_domain setting (mirroring how handleCreateApp/handleAddDomain
// already read it), falling back to a relative path — with a warning
// entry explaining why — when no dashboard domain is configured.
func (a *api) webhookURLFor(hookID string) (webhookURL string, warnings []string, err error) {
	path := "/api/v1/webhooks/git/" + hookID
	domain, err := a.st.Setting("dashboard_domain")
	if err != nil {
		return "", nil, err
	}
	if domain == "" || domain == "off" {
		return path, []string{
			"no dashboard domain is configured, so webhook_url is a relative path — prefix it with this server's own https:// URL before pasting it into a forge's webhook settings",
		}, nil
	}
	return "https://" + domain + path, nil, nil
}

// gitSourceResponseFor builds the wire response for gs. revealSecret is
// "" for every caller except the one that just minted a fresh secret
// (handlePutGitSource on first connect, handleRotateGitSecret) — see
// gitSourceResponse's doc comment on why it's the sole exception to
// write-only. Passing "" never needs a decrypt: unlike the old
// always-readable design, a steady-state GET/PUT no longer opens the
// sealed secret at all.
func (a *api) gitSourceResponseFor(gs store.GitSource, revealSecret string) (gitSourceResponse, error) {
	tokenState := ""
	if gs.TokenEncrypted != "" {
		tokenState = "set"
	}
	webhookURL, warnings, err := a.webhookURLFor(gs.HookID)
	if err != nil {
		return gitSourceResponse{}, err
	}
	return gitSourceResponse{
		URL: gs.URL, Branch: gs.Branch, Provider: gs.Provider, HookID: gs.HookID,
		Secret: revealSecret, Token: tokenState, WebhookURL: webhookURL, Warnings: warnings,
	}, nil
}

// putGitSourceRequest is the body of PUT .../apps/{slug}/git. Token is
// write-only: omitted or "" leaves any already-stored token untouched
// (matching env var PUT's keep-on-empty-secret semantics — see
// handlePutEnv), never used to clear a token; disconnecting entirely goes
// through DELETE instead. There is no rotate flag here any more — minting
// a fresh webhook secret is POST .../apps/{slug}/git/rotate-secret's one
// job (issue #13: rotation is a distinct, deliberate action, not a
// side-effect flag on an otherwise-idempotent connection-details update).
type putGitSourceRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Token  string `json:"token"`
}

// handlePutGitSource connects (or reconnects) an app's git repo config.
// On first connect it mints a fresh hook_id and secret, returned in
// plaintext exactly this once (see gitSourceResponse's doc comment) — an
// operator who loses it rotates via POST .../git/rotate-secret, the same
// way a lost env secret or deploy token is handled: by replacing it, not
// by reading it back. A re-PUT of an already-connected repo keeps
// hook_id and secret untouched (so it can update just the branch or
// token without invalidating a webhook already configured on the forge
// side) and never needs to decrypt the stored secret to do so. See
// putGitSourceRequest's doc comment for the keep-on-empty-token
// semantics, and the v0.5 plan's Task 4 for the AAD binding (secret/token
// are sealed with AAD bound to (appID, "git_secret") / (appID,
// "git_token") — the same appID|key binding env var values use).
func (a *api) handlePutGitSource(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	var req putGitSourceRequest
	if !readJSON(w, r, &req) {
		return
	}

	if req.Branch == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "branch must not be empty")
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "url must be a valid absolute URL")
		return
	}
	if !gitURLAllowedSchemes[strings.ToLower(u.Scheme)] {
		writeError(w, http.StatusUnprocessableEntity, "validation", `url scheme must be "https"`)
		return
	}

	existing, lookupErr := a.st.GitSourceByAppID(app.ID)
	hadExisting := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal", "failed to look up git source")
		return
	}

	// revealSecret carries the new plaintext into the response ONLY on
	// first connect — a re-PUT reuses the existing sealed secret verbatim
	// (no decrypt, no re-seal) and never reveals it again.
	var hookID, secretEncrypted, revealSecret string
	if hadExisting {
		hookID = existing.HookID
		secretEncrypted = existing.SecretEncrypted
	} else {
		var secret string
		if hookID, err = newHookID(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to generate webhook id")
			return
		}
		if secret, err = newGitSecret(); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to generate webhook secret")
			return
		}
		if secretEncrypted, err = a.seal(app.ID, "git_secret", secret); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to save webhook secret")
			return
		}
		revealSecret = secret
	}

	tokenEncrypted := ""
	if hadExisting {
		tokenEncrypted = existing.TokenEncrypted
	}
	if req.Token != "" {
		if tokenEncrypted, err = a.seal(app.ID, "git_token", req.Token); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to save deploy token")
			return
		}
	}

	gs, err := a.st.UpsertGitSource(store.GitSource{
		AppID: app.ID, URL: req.URL, Branch: req.Branch, Provider: detectProvider(req.URL),
		HookID: hookID, SecretEncrypted: secretEncrypted, TokenEncrypted: tokenEncrypted,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to save git source")
		return
	}

	resp, err := a.gitSourceResponseFor(*gs, revealSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to build response")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetGitSource returns an app's connected git repo config, if any —
// 404 "git_not_connected" otherwise. The webhook secret is write-only
// (omitted here, same treatment as a secret env value and the deploy
// token — issue #13): this handler never even decrypts it, since nothing
// about a GET needs the plaintext.
func (a *api) handleGetGitSource(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	gs, err := a.st.GitSourceByAppID(app.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "git_not_connected", "no git repo connected for this app")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up git source")
		}
		return
	}

	resp, err := a.gitSourceResponseFor(*gs, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to build response")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRotateGitSecret mints a fresh webhook secret for an app's
// connected git repo, returned in plaintext exactly once (see
// gitSourceResponse's doc comment) — the one deliberate way to see the
// secret again after first connect, mirroring how a lost env secret or
// deploy token is recovered: by replacing it, never by reading it back
// (issue #13). hook_id, url, branch, and the deploy token are all left
// untouched — only the secret changes, so the webhook's payload URL
// stays valid; the operator updates just the secret value in their
// forge's webhook settings. 422 "git_not_connected" if no repo is
// connected (matching handleDeployGit's own deviation from a plain 404:
// the app exists, it's the git config that's missing).
func (a *api) handleRotateGitSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	existing, err := a.st.GitSourceByAppID(app.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "git_not_connected", "no git repo connected for this app")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up git source")
		}
		return
	}

	secret, err := newGitSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to generate webhook secret")
		return
	}
	secretEncrypted, err := a.seal(app.ID, "git_secret", secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to save webhook secret")
		return
	}

	gs, err := a.st.UpsertGitSource(store.GitSource{
		AppID: app.ID, URL: existing.URL, Branch: existing.Branch, Provider: existing.Provider,
		HookID: existing.HookID, SecretEncrypted: secretEncrypted, TokenEncrypted: existing.TokenEncrypted,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to save git source")
		return
	}

	resp, err := a.gitSourceResponseFor(*gs, secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to build response")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteGitSource disconnects an app's git repo config. Not an
// error if none was connected (matches store.DeleteGitSource's own
// idempotent contract).
func (a *api) handleDeleteGitSource(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}
	if err := a.st.DeleteGitSource(app.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to disconnect git source")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// defaultGitDeliveriesLimit and maxGitDeliveriesLimit bound the ?limit=
// query param on GET .../apps/{slug}/git/deliveries.
const (
	defaultGitDeliveriesLimit = 20
	maxGitDeliveriesLimit     = 200
)

// gitDeliveryResponse is one row of GET .../apps/{slug}/git/deliveries.
// DeploymentNumber is set only for a delivery that actually triggered a
// deploy (status "deployed") and whose deployment row still exists.
type gitDeliveryResponse struct {
	ID               int64  `json:"id"`
	ReceivedAt       string `json:"received_at"`
	Provider         string `json:"provider"`
	Event            string `json:"event"`
	Ref              string `json:"ref"`
	CommitSHA        string `json:"commit_sha"`
	Status           string `json:"status"`
	Detail           string `json:"detail"`
	DeploymentNumber *int   `json:"deployment_number,omitempty"`
}

// handleListGitDeliveries returns an app's most recent webhook deliveries,
// newest first (default limit 20, capped at 200).
func (a *api) handleListGitDeliveries(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	limit := defaultGitDeliveriesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxGitDeliveriesLimit {
			writeError(w, http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("limit must be an integer between 1 and %d", maxGitDeliveriesLimit))
			return
		}
		limit = n
	}

	deliveries, err := a.st.ListGitDeliveries(app.ID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list deliveries")
		return
	}

	out := make([]gitDeliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		item := gitDeliveryResponse{
			ID: d.ID, ReceivedAt: d.ReceivedAt, Provider: d.Provider, Event: d.Event,
			Ref: d.Ref, CommitSHA: d.CommitSHA, Status: d.Status, Detail: d.Detail,
		}
		if d.DeploymentID != nil {
			if dep, err := a.st.DeploymentByID(*d.DeploymentID); err == nil {
				n := dep.Number
				item.DeploymentNumber = &n
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// writeGitFetchError maps a GitFetcher.Fetch failure to the documented
// status/error code: gitsource.ErrGitUnavailable (no git binary at boot)
// is 503 "git_unavailable"; anything else — an invalid URL, an unreadable
// branch, a timed-out or oversized clone — is 502 "git_clone_failed" with
// gitsource's own (already sanitized — see gitsource.sanitize) error
// message, so an operator testing a fresh connection gets actionable
// feedback without ever risking a token leaking into the response.
func writeGitFetchError(w http.ResponseWriter, err error) {
	if errors.Is(err, gitsource.ErrGitUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "git_unavailable", "git is not available on this server")
		return
	}
	writeError(w, http.StatusBadGateway, "git_clone_failed", err.Error())
}

// handleDeployGit triggers a manual "deploy now" for an app's connected
// git repo: clone (synchronous — clone failures are reported as an
// actionable 502/503 while the operator is testing the connection, per
// the v0.5 plan's Task 4) → the existing async build pipeline (202 +
// deployment JSON, git_sha already recorded). Requires a connected repo
// (422 "git_not_connected" otherwise, matching a Task 4 deviation from a
// plain 404: the app exists, it's the git config that's missing).
func (a *api) handleDeployGit(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	gs, err := a.st.GitSourceByAppID(app.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "git_not_connected", "no git repo connected for this app")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", "failed to look up git source")
		}
		return
	}

	if a.gitFetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "git_unavailable", "git is not available on this server")
		return
	}

	token := ""
	if gs.TokenEncrypted != "" {
		if token, err = a.open(app.ID, "git_token", gs.TokenEncrypted); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to decrypt deploy token")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), gitCloneTimeout)
	defer cancel()

	gzTar, headSHA, err := a.gitFetcher.Fetch(ctx, gs.URL, gs.Branch, token)
	if err != nil {
		writeGitFetchError(w, err)
		return
	}
	defer gzTar.Close()

	prepared, err := a.builder.PrepareBuild(gzTar)
	if err != nil {
		writePrepareBuildError(w, err)
		return
	}

	// DeployGitAsync takes ownership of prepared from here on (success or
	// failure) — it always eventually closes it, so this handler must not.
	dep, err := a.dep.DeployGitAsync(r.Context(), app, prepared, a.builder, headSHA, "api")
	if err != nil {
		writeError(w, http.StatusBadGateway, "deploy_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, toDeploymentResponse(*dep))
}

// gitCoalescer bounds how many webhook-triggered git deploys can be
// in-flight or queued per app: at most one running, plus at most one
// pending — a flood of N rapid pushes to the same app therefore yields at
// most 2 actual deploys (the one already running when the flood starts,
// plus one follow-up for whatever the newest push turned out to be), not
// N. Pushes beyond that are still recorded as their own "coalesced"
// delivery (see webhook.go's handleGitWebhook) rather than silently
// dropped — see the v0.5 plan's Task 5.
//
// Keyed by app ID (a small int64 map, like the build pipeline's own
// per-app locking — internal/build.Builder.appLock) rather than by
// hook_id: coalescing is a property of "how many builds can run for this
// app at once", which is what actually contends for the build semaphore
// and holds the app's single deploy-in-progress state, not a property of
// which webhook happened to trigger it.
type gitCoalescer struct {
	mu      sync.Mutex
	running map[int64]bool
	pending map[int64]pendingGitPush
}

// pendingGitPush is the queued follow-up deploy for one app: the newest
// push's commit/ref, overwriting whatever was queued before it (see
// gitCoalescer.setPending) — "the newest SHA wins".
type pendingGitPush struct {
	sha string
	ref string
}

func newGitCoalescer() *gitCoalescer {
	return &gitCoalescer{running: make(map[int64]bool), pending: make(map[int64]pendingGitPush)}
}

// tryStart claims appID's single "running" slot, reporting true if the
// caller may start a deploy immediately. If a deploy is already running
// for appID, it reports false — the caller must queue its push instead
// (see setPending) rather than starting a second concurrent clone/build
// for the same app.
func (c *gitCoalescer) tryStart(appID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[appID] {
		return false
	}
	c.running[appID] = true
	return true
}

// setPending overwrites appID's queued follow-up push with sha/ref: if a
// push was already queued, it's replaced outright, never appended to, so
// a burst of N pushes arriving while one deploy runs collapses to at most
// one follow-up deploy for the newest of them, not N.
func (c *gitCoalescer) setPending(appID int64, sha, ref string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[appID] = pendingGitPush{sha: sha, ref: ref}
}

// finish releases appID's running slot and, if a push was queued while it
// ran, atomically reclaims the slot for that follow-up and returns it (ok
// == true) — so a third push arriving concurrently with this call still
// correctly observes the slot as taken, never a window where it looks
// free between "the old deploy finished" and "the follow-up started".
func (c *gitCoalescer) finish(appID int64) (pendingGitPush, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[appID]
	if ok {
		delete(c.pending, appID)
		return p, true
	}
	c.running[appID] = false
	return pendingGitPush{}, false
}

// gitDeliveryInput is the subset of store.GitDelivery's fields
// recordGitDelivery's callers vary per outcome — AppID is always the
// caller's, ReceivedAt is always "now" (set by the store).
type gitDeliveryInput struct {
	Provider  string
	Event     string
	Ref       string
	CommitSHA string
	Status    string
	Detail    string
}

// recordGitDelivery inserts a git_deliveries row for appID, logging (not
// failing the request over) a store error — a webhook caller has already
// gotten as far as it's going to for this request; a delivery-log write
// failure must never turn an otherwise-handled push into a 500.
func (a *api) recordGitDelivery(appID int64, in gitDeliveryInput) {
	if _, err := a.st.InsertGitDelivery(store.GitDelivery{
		AppID: appID, Provider: in.Provider, Event: in.Event, Ref: in.Ref,
		CommitSHA: in.CommitSHA, Status: in.Status, Detail: in.Detail,
	}); err != nil {
		log.Printf("api: git: record delivery for app %d: %v", appID, err)
	}
}

// triggerGitDeploy is the webhook receiver's shared trigger helper (see
// webhook.go's handleGitWebhook): it opens the repo's deploy token (if
// any), records an optimistic "deployed" delivery row up front (the
// deployment row it links to exists synchronously, but the clone/build
// that follows runs in the background — see
// Deployer.DeployGitCloneAsync's doc comment), starts that clone+build via
// the Deployer, and wires a completion callback that flips the delivery
// to "error" on failure and always releases this app's coalescer slot —
// chaining into a queued follow-up push, if the coalescer reports one, by
// calling itself again for that push.
//
// token/URL/branch come exclusively from gs (the stored,
// operator-configured source) — never from the webhook payload: this is
// the load-bearing half of "the payload can trigger and filter, never
// steer" (see webhook.go's doc comment and its hostile-payload test).
func (a *api) triggerGitDeploy(ctx context.Context, app *store.App, gs *store.GitSource, sha, provider, event, ref, triggerKind string) {
	token := ""
	if gs.TokenEncrypted != "" {
		if t, err := a.open(app.ID, "git_token", gs.TokenEncrypted); err == nil {
			token = t
		} else {
			log.Printf("api: git: decrypt deploy token for app %s: %v", app.Slug, err)
		}
	}

	delivery, err := a.st.InsertGitDelivery(store.GitDelivery{
		AppID: app.ID, Provider: provider, Event: event, Ref: ref, CommitSHA: sha,
		Status: "deployed", Detail: "build queued",
	})
	if err != nil {
		log.Printf("api: git: record delivery for app %s: %v", app.Slug, err)
	}

	fetch := func(fetchCtx context.Context) (io.ReadCloser, string, error) {
		return a.gitFetcher.Fetch(fetchCtx, gs.URL, gs.Branch, token)
	}

	onDone := func(dep *store.Deployment, deployErr error) {
		if deployErr != nil && delivery != nil {
			var depID *int64
			if dep != nil {
				depID = &dep.ID
			}
			if err := a.st.UpdateGitDeliveryStatus(delivery.ID, "error", deployErr.Error(), depID); err != nil {
				log.Printf("api: git: update delivery status for app %s: %v", app.Slug, err)
			}
		}
		a.finishGitCoalesce(ctx, app, gs, provider, event)
	}

	if _, err := a.dep.DeployGitCloneAsync(ctx, app, fetch, a.builder, triggerKind, sha, onDone); err != nil {
		if delivery != nil {
			if uerr := a.st.UpdateGitDeliveryStatus(delivery.ID, "error", "failed to start deploy: "+err.Error(), nil); uerr != nil {
				log.Printf("api: git: update delivery status for app %s: %v", app.Slug, uerr)
			}
		}
		a.finishGitCoalesce(ctx, app, gs, provider, event)
	}
}

// finishGitCoalesce releases app's coalescer slot and, if a push was
// queued while the just-finished deploy ran, immediately chains into a
// deploy for it — see gitCoalescer.finish's doc comment for why this is
// safe to call directly rather than needing its own lock/retry: finish
// itself atomically reclaims the slot for the chained call.
func (a *api) finishGitCoalesce(ctx context.Context, app *store.App, gs *store.GitSource, provider, event string) {
	next, ok := a.gitCoalescer.finish(app.ID)
	if !ok {
		return
	}
	a.triggerGitDeploy(ctx, app, gs, next.sha, provider, event, next.ref, "webhook")
}
