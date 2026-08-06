package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ApiError mirrors the server's {"error":{code,message}} envelope (see
// internal/api's errorResponse) as a Go error: Status is the HTTP status
// code, Code the machine-readable error code, and Message the
// human-readable text every command prints verbatim.
type ApiError struct {
	Status  int
	Code    string
	Message string
}

func (e *ApiError) Error() string { return e.Message }

// Client is a thin wrapper over net/http for BasePod's REST API v1: a base
// URL (the server's root, e.g. "https://basepod.example.com" — "/api/v1"
// is added per-request) and a bearer session token. The embedded
// http.Client is left at its zero value (no Timeout) — a deploy/build or a
// follow=1 log stream can legitimately run far longer than any fixed
// client-side timeout would allow, and every server route already bounds
// itself server-side (deployTimeout, buildTimeout).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a Client for baseURL (e.g. a context's stored URL) and
// token (its stored session token, "" for the unauthenticated login call).
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{},
	}
}

// errorResponse mirrors internal/api's wire shape for decoding.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// newRequest builds a request against path (e.g. "/apps") under
// BaseURL+"/api/v1", attaching the bearer token and Content-Type when set.
func (c *Client) newRequest(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// do sends req and decodes a JSON response into out (skipped if out is
// nil, e.g. for a 204 No Content response). A non-2xx response is decoded
// as the server's error envelope and returned as *ApiError; a body that
// isn't valid JSON at all (a proxy's HTML error page, a dropped
// connection) falls back to a generic message carrying the raw status.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var er errorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
			return &ApiError{Status: resp.StatusCode, Code: er.Error.Code, Message: er.Error.Message}
		}
		return &ApiError{Status: resp.StatusCode, Message: fmt.Sprintf("unexpected response: %s", resp.Status)}
	}

	if out == nil {
		return nil
	}
	if resp.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// LoginResult is the wire shape of a successful POST /auth/login.
type LoginResult struct {
	Token string `json:"token"`
	User  struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
}

// Login exchanges email/password for a session token. Unlike every other
// Client method, this one never sends an Authorization header (c.Token is
// typically empty at this point — the caller doesn't have one yet).
func (c *Client) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out LoginResult
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Logout revokes the current session token server-side (POST
// /auth/logout). It does not touch the CLI's local config — the caller
// (basepod logout) is responsible for clearing the saved token afterward.
func (c *Client) Logout(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodPost, "/auth/logout", "", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// AppInfo is the wire shape of one app in a list/create response.
type AppInfo struct {
	Slug   string `json:"slug"`
	Image  string `json:"image"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// Deployment is the wire shape of one deployment (returned from a deploy,
// rollback, or as part of an AppDetail's history).
type Deployment struct {
	Number      int    `json:"number"`
	Image       string `json:"image"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	Source      string `json:"source"`
	Trigger     string `json:"trigger"`
	HasBuildLog bool   `json:"has_build_log"`
	// GitSha is the resolved commit a "git"-sourced deployment built —
	// "" for every other Source (see the v0.5 plan's Task 4/5 and
	// store.Deployment.GitSha's doc comment).
	GitSha string `json:"git_sha"`
}

// AppDetail is the wire shape of GET /apps/{slug}: an app plus its full
// deployment history, newest first (matching store.ListDeployments' own
// ORDER BY number DESC), so Deployments[0] is always the most recent.
type AppDetail struct {
	AppInfo
	Deployments []Deployment `json:"deployments"`
}

// EnvVar is the wire shape of one environment variable, for both GET and
// PUT .../env. IsSecret entries always report Value "" from a GET; sending
// one back with Value "" on a PUT keeps its previously stored value
// unchanged (the server's "keep-on-empty-secret" contract — see
// internal/api/env.go's handlePutEnv).
type EnvVar struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
}

// SystemInfo is the wire shape of GET /system.
type SystemInfo struct {
	Version string `json:"version"`
	Podman  string `json:"podman"`
	Apps    int    `json:"apps"`
}

// ListApps returns every app.
func (c *Client) ListApps(ctx context.Context) ([]AppInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps", "", nil)
	if err != nil {
		return nil, err
	}
	var out []AppInfo
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetApp returns an app's details plus its deployment history.
func (c *Client) GetApp(ctx context.Context, slug string) (*AppDetail, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+slug, "", nil)
	if err != nil {
		return nil, err
	}
	var out AppDetail
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Deploy triggers an image-sourced deploy: image "" redeploys the app's
// current image.
func (c *Client) Deploy(ctx context.Context, slug, image string) (*Deployment, error) {
	body, err := json.Marshal(map[string]string{"image": image})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/apps/"+slug+"/deploy", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out Deployment
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeployTarball uploads a gzipped tar build context (body) for a
// build-from-source deploy. size sets Content-Length up front (the caller
// packs to a temp file first specifically so this can avoid chunked
// transfer encoding).
//
// The server spools and validates the upload synchronously — so a bad
// upload (413/422) still fails fast, surfacing here as an *ApiError from
// c.do exactly like before — but responds 202 Accepted immediately once
// that succeeds, before the build+rollout even starts (see
// internal/api.handleDeployTarball). The returned Deployment is therefore
// always still "deploying": callers follow up with CreateStreamToken +
// OpenDeploymentLog (to watch it build) and GetDeployment (to poll for a
// terminal status), rather than trusting this call's own return value —
// see followDeployment in commands.go, the only caller.
func (c *Client) DeployTarball(ctx context.Context, slug string, body io.Reader, size int64) (*Deployment, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/apps/"+slug+"/deploy/tarball", "application/gzip", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	var out Deployment
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ComposeService is the wire shape of one service entry in a compose
// apply/dry-run response (mirrors internal/api's composeServiceResponse).
type ComposeService struct {
	Name             string   `json:"name"`
	Slug             string   `json:"slug"`
	Action           string   `json:"action"`
	Internal         bool     `json:"internal"`
	Port             int      `json:"port"`
	Alias            string   `json:"alias"`
	DeployStrategy   string   `json:"deploy_strategy"`
	DeploymentNumber int      `json:"deployment_number"`
	Warnings         []string `json:"warnings"`
}

// ComposeResult is the wire shape of POST /compose/up's response, for
// both a dry run (DryRun true, nothing changed, every
// DeploymentNumber 0) and a real apply (202 — every service's deployment
// already created and pollable, in dependency order).
type ComposeResult struct {
	Project  string           `json:"project"`
	DryRun   bool             `json:"dry_run"`
	Services []ComposeService `json:"services"`
	// Orphans lists the slugs of apps that belong to this compose
	// project from a prior apply but have no corresponding service in
	// the file just applied — never deleted automatically, only
	// reported; see internal/api/compose.go's package doc comment.
	Orphans  []string `json:"orphans"`
	Warnings []string `json:"warnings"`
}

// ComposeUp uploads a gzipped tar (body, size bytes — packed via
// packToTempFile the same way deployFromSource packs a build context)
// carrying compose.yaml (or compose.yml/docker-compose.yml) at its root
// plus any per-service build contexts, to
// POST /compose/up?project=<project>&dry_run=<dryRun>. project may be ""
// to fall back to the compose file's own top-level `name:` (the server
// 422s if neither is present).
func (c *Client) ComposeUp(ctx context.Context, project string, dryRun bool, body io.Reader, size int64) (*ComposeResult, error) {
	path := "/compose/up?dry_run=" + strconv.FormatBool(dryRun)
	if project != "" {
		path += "&project=" + url.QueryEscape(project)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, "application/gzip", body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	var out ComposeResult
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDeployment fetches a single deployment by number (GET
// .../deployments/{number}) — the polling half of the async tarball deploy
// flow: followDeployment (commands.go) calls this in a loop until Status
// is terminal.
func (c *Client) GetDeployment(ctx context.Context, slug string, number int) (*Deployment, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+slug+"/deployments/"+strconv.Itoa(number), "", nil)
	if err != nil {
		return nil, err
	}
	var out Deployment
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamToken is the wire shape of a successful POST /stream-token
// response — see internal/api/stream_token.go's streamTokenResponse.
type StreamToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// CreateBuildLogStreamToken mints a short-lived, single-purpose stream
// token scoped to exactly one deployment's build log (scope "build_log") —
// the credential OpenDeploymentLog's ?access_token= needs, matching the
// same stream-token model the dashboard's SSE connections use (see
// internal/api/stream_token.go) rather than putting the CLI's own
// full-authority session token in a URL query string.
func (c *Client) CreateBuildLogStreamToken(ctx context.Context, slug string, number int) (*StreamToken, error) {
	n := number
	body, err := json.Marshal(map[string]any{"scope": "build_log", "slug": slug, "deployment_number": &n})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/stream-token", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out StreamToken
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpenDeploymentLog opens GET .../deployments/{number}/log, authenticated
// via a stream token minted by CreateBuildLogStreamToken (as ?access_token=
// — this route also accepts a bearer session token, but the CLI
// deliberately uses the same scoped-token model the dashboard does rather
// than putting its full-authority session token in a URL). The caller owns
// closing the returned response body and reading it as either an SSE
// stream (while the deployment is still "deploying" — Content-Type
// text/event-stream) or a single plain-text body (once it's terminal —
// Content-Type text/plain): see followDeployment in commands.go, the only
// caller, for how it tells the two apart.
func (c *Client) OpenDeploymentLog(ctx context.Context, slug string, number int, streamToken string) (*http.Response, error) {
	path := "/apps/" + slug + "/deployments/" + strconv.Itoa(number) + "/log?access_token=" + url.QueryEscape(streamToken)
	req, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var er errorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
			return nil, &ApiError{Status: resp.StatusCode, Code: er.Error.Code, Message: er.Error.Message}
		}
		return nil, &ApiError{Status: resp.StatusCode, Message: fmt.Sprintf("unexpected response: %s", resp.Status)}
	}
	return resp, nil
}

// Rollback triggers a rollback to an earlier deployment's exact image.
func (c *Client) Rollback(ctx context.Context, slug string, number int) (*Deployment, error) {
	body, err := json.Marshal(map[string]int{"number": number})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/apps/"+slug+"/rollback", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out Deployment
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEnv returns an app's environment variables (secrets masked to "").
func (c *Client) GetEnv(ctx context.Context, slug string) ([]EnvVar, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+slug+"/env", "", nil)
	if err != nil {
		return nil, err
	}
	var out []EnvVar
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutEnv replaces an app's entire environment variable set and returns the
// new set as the server sees it (secrets masked to "" again).
func (c *Client) PutEnv(ctx context.Context, slug string, vars []EnvVar) ([]EnvVar, error) {
	if vars == nil {
		vars = []EnvVar{}
	}
	body, err := json.Marshal(vars)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/apps/"+slug+"/env", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out []EnvVar
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GitSource is the wire shape of PUT/GET .../apps/{slug}/git and POST
// .../apps/{slug}/git/rotate-secret — see internal/api/git.go's
// gitSourceResponse. Secret is write-only: empty except on the one
// response that just minted it (PutGitSource on first connect, or
// RotateGitSecret) — matching Token, which is masked to "set"/"" like a
// secret env value and never round-trips either.
type GitSource struct {
	URL        string   `json:"url"`
	Branch     string   `json:"branch"`
	Provider   string   `json:"provider"`
	HookID     string   `json:"hook_id"`
	Secret     string   `json:"secret,omitempty"`
	Token      string   `json:"token"`
	WebhookURL string   `json:"webhook_url"`
	Warnings   []string `json:"warnings,omitempty"`
}

// GitDelivery is the wire shape of one row from GET
// .../apps/{slug}/git/deliveries — see internal/api/git.go's
// gitDeliveryResponse.
type GitDelivery struct {
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

// putGitSourceRequest mirrors internal/api/git.go's own request type —
// kept private since callers use PutGitSource's named parameters instead.
type putGitSourceRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Token  string `json:"token"`
}

// PutGitSource connects (or reconnects) an app's git repo config. token
// "" leaves any already-stored deploy token untouched (see
// putGitSourceRequest's server-side counterpart). On first connect, the
// returned GitSource's Secret carries the fresh webhook secret in
// plaintext exactly once; a re-connect's Secret is always "" — use
// RotateGitSecret to mint (and see) a new one.
func (c *Client) PutGitSource(ctx context.Context, slug, url, branch, token string) (*GitSource, error) {
	body, err := json.Marshal(putGitSourceRequest{URL: url, Branch: branch, Token: token})
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/apps/"+slug+"/git", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out GitSource
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RotateGitSecret mints a fresh webhook secret for an app's connected git
// source, returned in the response's Secret field in plaintext exactly
// once — the only way to see the secret again after first connect.
// hook_id, the URL, branch, and deploy token are all left untouched.
func (c *Client) RotateGitSecret(ctx context.Context, slug string) (*GitSource, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/apps/"+slug+"/git/rotate-secret", "", nil)
	if err != nil {
		return nil, err
	}
	var out GitSource
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetGitSource returns an app's connected git repo config. Returns
// *ApiError with Code "git_not_connected" (Status 404) if none is
// connected.
func (c *Client) GetGitSource(ctx context.Context, slug string) (*GitSource, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+slug+"/git", "", nil)
	if err != nil {
		return nil, err
	}
	var out GitSource
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteGitSource disconnects an app's git repo config.
func (c *Client) DeleteGitSource(ctx context.Context, slug string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/apps/"+slug+"/git", "", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// ListGitDeliveries returns an app's most recent webhook deliveries,
// newest first (server default limit 20 if limit <= 0).
func (c *Client) ListGitDeliveries(ctx context.Context, slug string, limit int) ([]GitDelivery, error) {
	path := "/apps/" + slug + "/git/deliveries"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var out []GitDelivery
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeployGit triggers a manual "deploy now" of an app's connected git
// repo: the server clones synchronously (so a clone failure surfaces here
// as an *ApiError right away) and hands the result to the same async
// build pipeline DeployTarball uses — the returned Deployment is always
// still "deploying"; see followDeployment in commands.go for the
// SSE-follow + poll-to-terminal flow both this and DeployTarball share.
func (c *Client) DeployGit(ctx context.Context, slug string) (*Deployment, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/apps/"+slug+"/deploy/git", "", nil)
	if err != nil {
		return nil, err
	}
	var out Deployment
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// System returns the control plane's version/runtime/app-count summary.
func (c *Client) System(ctx context.Context) (*SystemInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/system", "", nil)
	if err != nil {
		return nil, err
	}
	var out SystemInfo
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeploymentLog fetches a deployment's build log as plain text. It is only
// ever called (by the deploy command) after a deploy has already reached a
// terminal state, at which point the server always serves the log as a
// plain-text body rather than an SSE stream (see
// internal/api.handleDeploymentLog) — so no SSE parsing is needed here.
func (c *Client) DeploymentLog(ctx context.Context, slug string, number int) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+slug+"/deployments/"+strconv.Itoa(number)+"/log", "", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		var er errorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
			return "", &ApiError{Status: resp.StatusCode, Code: er.Error.Code, Message: er.Error.Message}
		}
		return "", &ApiError{Status: resp.StatusCode, Message: fmt.Sprintf("unexpected response: %s", resp.Status)}
	}
	return string(body), nil
}

// LogsStream opens GET .../logs as a live request, returning the raw
// response body for the caller to read as Server-Sent Events (see sse.go).
// Unlike a browser's EventSource, this sets a real Authorization header —
// no ?access_token= query-string fallback needed.
func (c *Client) LogsStream(ctx context.Context, slug string, follow bool, tail int) (io.ReadCloser, error) {
	path := "/apps/" + slug + "/logs?"
	q := make([]string, 0, 2)
	if follow {
		q = append(q, "follow=1")
	}
	if tail > 0 {
		q = append(q, "tail="+strconv.Itoa(tail))
	}
	path += strings.Join(q, "&")

	req, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var er errorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error.Message != "" {
			return nil, &ApiError{Status: resp.StatusCode, Code: er.Error.Code, Message: er.Error.Message}
		}
		return nil, &ApiError{Status: resp.StatusCode, Message: fmt.Sprintf("unexpected response: %s", resp.Status)}
	}
	return resp.Body, nil
}
