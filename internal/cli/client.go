package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// transfer encoding). This call blocks for as long as the server takes to
// build and roll out the new image — c.HTTP has no client-side timeout of
// its own, matching the server's own long buildTimeout.
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
