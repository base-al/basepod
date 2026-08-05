// Package podman implements a minimal libpod REST client over a Unix
// domain socket. It talks directly to the Podman API (no shelling out,
// except for socket auto-detection via `podman system connection list`)
// and exposes just the operations BasePod's deploy engine and Caddy
// manager need: pull, create, start, stop, remove, inspect, list, and a
// shared "basepod" network.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// apiVersion is the libpod API version segment requests are made against.
// Real podman daemons (verified against podman 6.0.1 client / 5.7.1
// server) only serve the unversioned "/libpod/..." path for "/_ping";
// every other libpod route requires a versioned prefix
// ("/v{version}/libpod/..."), so requests must include one. v5.0.0 is the
// floor of libpod's stable v5 API and works against this server.
const apiVersion = "v5.0.0"

// baseURL is the libpod API base path. All requests are made against this
// fixed virtual host ("d" for "daemon") over the Unix socket transport;
// only the path after /libpod varies per call.
const baseURL = "http://d/" + apiVersion + "/libpod"

// Client talks to a local Podman daemon over its libpod REST API.
type Client struct {
	http *http.Client
}

// New creates a Client bound to a Unix socket. socketURI is a "unix://"
// URI; an empty string triggers auto-detection via DetectSocket. Any
// other scheme (e.g. "ssh://", as produced by a remote podman machine
// connection) is rejected: v0.1 requires a local Unix socket.
func New(socketURI string) (*Client, error) {
	if socketURI == "" {
		detected, err := DetectSocket()
		if err != nil {
			return nil, err
		}
		socketURI = detected
	}
	path, err := socketPath(socketURI)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}, nil
}

// socketPath validates uri as a "unix://" URI and returns the filesystem
// path to the socket.
func socketPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("podman: invalid socket URI %q: %w", uri, err)
	}
	if u.Scheme != "unix" {
		return "", fmt.Errorf(
			"podman: socket URI %q uses unsupported scheme %q; basepod v0.1 requires a local unix socket — "+
				"run `podman machine start` and ensure a local forwarded socket is available, "+
				"or set podman_socket in the basepod config to a local unix:// path",
			uri, u.Scheme,
		)
	}
	p := u.Path
	if p == "" {
		p = u.Opaque
	}
	if p == "" {
		return "", fmt.Errorf("podman: socket URI %q has no path", uri)
	}
	return p, nil
}

// connectionEntry mirrors one entry of
// `podman system connection list --format json`.
type connectionEntry struct {
	Name    string `json:"Name"`
	URI     string `json:"URI"`
	Default bool   `json:"Default"`
}

// DetectSocket locates a local Podman Unix socket without any explicit
// configuration. Order:
//  1. `podman system connection list --format json`'s default entry, if
//     its URI is a "unix://" socket.
//  2. $XDG_RUNTIME_DIR/podman/podman.sock (Linux rootless).
//  3. /run/podman/podman.sock (Linux root).
//  4. `podman machine inspect`'s forwarded local socket path (macOS,
//     where the default connection is typically "ssh://" but podman
//     machine also exposes a local unix socket for direct API access).
//
// If the default connection exists but uses a non-unix scheme (e.g.
// "ssh://", the common shape for a macOS `podman machine`) and none of
// the fallback paths exist, DetectSocket returns an actionable error
// rather than silently failing.
func DetectSocket() (string, error) {
	defaultURI, _ := defaultConnectionURI() // best-effort; absence is not fatal
	if defaultURI != "" {
		if u, err := url.Parse(defaultURI); err == nil && u.Scheme == "unix" {
			return defaultURI, nil
		}
	}

	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		p := filepath.Join(xdg, "podman", "podman.sock")
		if fileExists(p) {
			return "unix://" + p, nil
		}
	}

	if fileExists("/run/podman/podman.sock") {
		return "unix:///run/podman/podman.sock", nil
	}

	if p, err := machineForwardedSocket(); err == nil && p != "" && fileExists(p) {
		return "unix://" + p, nil
	}

	if defaultURI != "" {
		if u, err := url.Parse(defaultURI); err == nil && u.Scheme != "" && u.Scheme != "unix" {
			return "", fmt.Errorf(
				"podman: default connection %q uses scheme %q (remote); basepod v0.1 requires a local unix socket — "+
					"run `podman machine start` and ensure a local forwarded socket is available, "+
					"or set podman_socket in the basepod config to a local unix:// path",
				defaultURI, u.Scheme,
			)
		}
	}

	return "", fmt.Errorf(
		"podman: no local podman unix socket found (checked $XDG_RUNTIME_DIR/podman/podman.sock, /run/podman/podman.sock, " +
			"and `podman machine inspect`) — run `podman machine start` or set podman_socket in the basepod config",
	)
}

// machineForwardedSocket runs
// `podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}'`
// to find the local Unix socket a `podman machine` (macOS/Windows)
// forwards API traffic to. Returns "" if no machine is configured or the
// command fails.
func machineForwardedSocket() (string, error) {
	out, err := exec.Command("podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultConnectionURI runs `podman system connection list --format json`
// and returns the URI of the entry marked Default.
func defaultConnectionURI() (string, error) {
	out, err := exec.Command("podman", "system", "connection", "list", "--format", "json").Output()
	if err != nil {
		return "", err
	}
	var entries []connectionEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Default {
			return e.URI, nil
		}
	}
	return "", nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// request performs an HTTP call against the libpod API and returns the
// response status code and raw body. body, if non-nil, is JSON-marshaled
// as the request payload.
func (c *Client) request(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// apiErrorBody is libpod's standard error envelope, e.g.
// {"cause":"no such container","message":"...","response":404}.
type apiErrorBody struct {
	Cause   string `json:"cause"`
	Message string `json:"message"`
}

// apiError builds an error from a non-2xx libpod response.
func apiError(status int, data []byte) error {
	var eb apiErrorBody
	msg := ""
	if json.Unmarshal(data, &eb) == nil {
		switch {
		case eb.Message != "":
			msg = eb.Message
		case eb.Cause != "":
			msg = eb.Cause
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("podman: %s (status %d)", msg, status)
}

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	status, data, err := c.request(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return fmt.Errorf("podman: ping: %w", err)
	}
	if status != http.StatusOK {
		return apiError(status, data)
	}
	return nil
}

// versionResponse is the subset of libpod's GET /version payload Version
// cares about — the top-level daemon version string (e.g. "4.9.2").
type versionResponse struct {
	Version string `json:"Version"`
}

// Version reports the connected daemon's version string (e.g. "4.9.2"),
// read from libpod's GET /version endpoint. Callers use this at boot to
// enforce a minimum podman version (see server.checkPodmanVersion) —
// BasePod relies on libpod API behavior (e.g. explicit bridge-mode netns,
// see CreateContainer) that isn't reliably present on older daemons.
func (c *Client) Version(ctx context.Context) (string, error) {
	status, data, err := c.request(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", fmt.Errorf("podman: version: %w", err)
	}
	if status != http.StatusOK {
		return "", apiError(status, data)
	}
	var v versionResponse
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("podman: decoding version response: %w", err)
	}
	return v.Version, nil
}

// pullStreamLine is one line of the newline-delimited JSON stream libpod
// sends back from POST /images/pull. Errors that occur mid-pull (e.g. a
// missing manifest) are reported *inside* this stream with a 200 status,
// not as an HTTP error status.
type pullStreamLine struct {
	Stream string `json:"stream,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PullImage pulls ref, draining the daemon's progress stream. libpod
// reports pull failures (bad tag, auth, missing manifest, ...) as
// {"error": "..."} lines within an otherwise-200 stream, so the whole
// body must be read and inspected rather than relying on the HTTP status.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	if ref == "" {
		return fmt.Errorf("podman: PullImage: ref is required")
	}
	q := url.Values{}
	q.Set("reference", ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/images/pull?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("podman: pull %s: %w", ref, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return apiError(resp.StatusCode, data)
	}

	dec := json.NewDecoder(resp.Body)
	var streamErr error
	for {
		var line pullStreamLine
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			// A stream-reported error (libpod's normal way of surfacing
			// pull failures inside an HTTP-200 body) takes precedence
			// over anything that goes wrong decoding the rest of the
			// stream afterwards — e.g. a trailing non-JSON line — since
			// the error line is the actionable signal and the decode
			// failure is just noise past the point of failure.
			if streamErr != nil {
				break
			}
			return fmt.Errorf("podman: pull %s: reading stream: %w", ref, err)
		}
		if line.Error != "" {
			streamErr = fmt.Errorf("podman: pull %s: %s", ref, line.Error)
		}
	}
	return streamErr
}

// buildStreamLine is one line of the newline-delimited JSON stream
// libpod's POST /build sends back — mirrors pullStreamLine, but /build's
// error shape can additionally carry a nested "errorDetail":{"message":
// "..."} object instead of (or alongside) the flat "error" string; when
// both are present ErrorDetail.Message is preferred as the more specific
// message.
type buildStreamLine struct {
	Stream      string `json:"stream,omitempty"`
	Error       string `json:"error,omitempty"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail,omitempty"`
}

// BuildImage builds an image tagged tag from contextTar — a raw,
// *uncompressed* tar stream (the caller decompresses any gzip wrapper
// first; see internal/build.Builder) — via libpod's POST /build.
// dockerfile names the in-context path to the Containerfile/Dockerfile to
// build (libpod's build endpoint accepts either name, but does not try
// both itself — the caller must pass whichever one its context actually
// contains); "" defaults to "Containerfile".
//
// Like PullImage, build progress and failures both arrive as
// newline-delimited JSON within an otherwise-200 body: {"stream":"..."}
// lines are progress text, written verbatim to logSink, while
// {"error":"..."} (optionally with a more specific nested
// "errorDetail":{"message":"..."}) reports a failed build. The whole
// stream is drained before returning an error, mirroring PullImage's
// reasoning: a stream-reported error is the actionable signal and must
// not be discarded by a decode failure on a later (or trailing garbage)
// line.
func (c *Client) BuildImage(ctx context.Context, tag, dockerfile string, contextTar io.Reader, logSink io.Writer) error {
	if tag == "" {
		return fmt.Errorf("podman: BuildImage: tag is required")
	}
	if dockerfile == "" {
		dockerfile = "Containerfile"
	}
	q := url.Values{}
	q.Set("dockerfile", dockerfile)
	q.Set("t", tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/build?"+q.Encode(), contextTar)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("podman: build %s: %w", tag, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return apiError(resp.StatusCode, data)
	}

	dec := json.NewDecoder(resp.Body)
	var streamErr error
	for {
		var line buildStreamLine
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			// See PullImage's identical reasoning: a captured stream error
			// takes precedence over a decode failure on whatever comes
			// after it.
			if streamErr != nil {
				break
			}
			return fmt.Errorf("podman: build %s: reading stream: %w", tag, err)
		}
		if line.Stream != "" {
			io.WriteString(logSink, line.Stream)
		}
		if line.Error != "" {
			msg := line.Error
			if line.ErrorDetail != nil && line.ErrorDetail.Message != "" {
				msg = line.ErrorDetail.Message
			}
			streamErr = fmt.Errorf("podman: build %s: %s", tag, msg)
		}
	}
	return streamErr
}

// EnsureNetwork makes sure a network named name exists, creating it
// (labeled basepod.managed=true) if it doesn't. If it already exists, it
// self-heals a DNS-disabled network (see selfHealNetworkDNS) — a state
// that leaves every app container unable to resolve another by name/alias
// ("bp-<slug>"), and has been observed to arise from an out-of-band
// `podman network create basepod` (whose CLI default differs from the raw
// API's, see networkCreate) or a partially-applied config from an older
// basepod version.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	status, data, err := c.request(ctx, http.MethodGet, "/networks/"+url.PathEscape(name)+"/exists", nil)
	if err != nil {
		return fmt.Errorf("podman: checking network %q: %w", name, err)
	}
	if status < 300 {
		return c.selfHealNetworkDNS(ctx, name)
	}
	if status != http.StatusNotFound {
		return apiError(status, data)
	}

	body := networkCreate{
		Name:       name,
		Labels:     map[string]string{"basepod.managed": "true"},
		DNSEnabled: true,
	}
	status, data, err = c.request(ctx, http.MethodPost, "/networks/create", body)
	if err != nil {
		return fmt.Errorf("podman: creating network %q: %w", name, err)
	}
	if status >= 300 {
		return apiError(status, data)
	}
	return nil
}

// selfHealNetworkDNS inspects an already-existing network and, if it has
// DNS resolution disabled, recreates it with DNS enabled — but ONLY when
// no basepod-managed container is currently attached to it: recreating a
// network out from under a running container would break its networking
// outright, so in that case this just logs a loud warning with manual
// remediation steps instead. Containers are identified broadly (every
// basepod.managed=true container, not just ones actually attached to
// name) since v0.3 has exactly one shared network and every managed
// container joins it.
func (c *Client) selfHealNetworkDNS(ctx context.Context, name string) error {
	info, err := c.InspectNetwork(ctx, name)
	if err != nil {
		return fmt.Errorf("podman: inspecting network %q: %w", name, err)
	}
	if info.DNSEnabled {
		return nil
	}

	containers, err := c.ListContainers(ctx, map[string]string{"basepod.managed": "true"})
	if err != nil {
		return fmt.Errorf("podman: listing containers to self-heal network %q: %w", name, err)
	}
	if len(containers) > 0 {
		log.Printf(
			"podman: WARNING: network %q has DNS resolution disabled but %d basepod container(s) are still attached — "+
				"refusing to recreate it automatically. Containers won't be able to resolve each other by name. "+
				"Remediation: stop all basepod apps, run `podman network rm %s`, then restart basepod.",
			name, len(containers), name,
		)
		return nil
	}

	log.Printf("podman: network %q has DNS resolution disabled and no containers are attached — recreating it with DNS enabled", name)
	if err := c.RemoveNetwork(ctx, name); err != nil {
		return fmt.Errorf("podman: removing network %q to self-heal DNS: %w", name, err)
	}
	body := networkCreate{
		Name:       name,
		Labels:     map[string]string{"basepod.managed": "true"},
		DNSEnabled: true,
	}
	status, data, err := c.request(ctx, http.MethodPost, "/networks/create", body)
	if err != nil {
		return fmt.Errorf("podman: recreating network %q: %w", name, err)
	}
	if status >= 300 {
		return apiError(status, data)
	}
	return nil
}

// networkInspectResponse is the subset of libpod's network inspect
// payload InspectNetwork cares about.
type networkInspectResponse struct {
	Name       string `json:"name"`
	DNSEnabled bool   `json:"dns_enabled"`
	Subnets    []struct {
		Gateway string `json:"gateway"`
	} `json:"subnets"`
}

// InspectNetwork fetches details for a network by name. Returns
// ErrNotFound if it doesn't exist.
func (c *Client) InspectNetwork(ctx context.Context, name string) (*NetworkInfo, error) {
	status, data, err := c.request(ctx, http.MethodGet, "/networks/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return nil, fmt.Errorf("podman: inspecting network %q: %w", name, err)
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status >= 300 {
		return nil, apiError(status, data)
	}

	var raw networkInspectResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("podman: decoding network inspect response for %q: %w", name, err)
	}
	gateway := ""
	if len(raw.Subnets) > 0 {
		gateway = raw.Subnets[0].Gateway
	}
	return &NetworkInfo{Name: raw.Name, DNSEnabled: raw.DNSEnabled, Gateway: gateway}, nil
}

// RemoveNetwork removes a network by name. Removing an already-gone
// network (404) is treated as success.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	status, data, err := c.request(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return fmt.Errorf("podman: removing network %q: %w", name, err)
	}
	if status == http.StatusNotFound || status < 300 {
		return nil
	}
	return apiError(status, data)
}

// CreateContainer creates a container from spec and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, spec CreateSpec) (string, error) {
	sg := specGen{
		Name:          spec.Name,
		Image:         spec.Image,
		Labels:        spec.Labels,
		Env:           spec.Env,
		Command:       spec.Command,
		PortMappings:  spec.PortMappings,
		RestartPolicy: spec.RestartPolicy,
	}
	if spec.NetworkName != "" {
		sg.Networks = map[string]perNetworkOptions{
			spec.NetworkName: {Aliases: spec.NetworkAliases},
		}
		// Must be explicit: see the comment on the namespace type in
		// types.go for why the API doesn't infer this the way the CLI does.
		sg.NetNS = &namespace{NSMode: "bridge"}
	}
	for _, m := range spec.Mounts {
		sm := specMount{
			Destination: m.Dest,
			Source:      m.Source,
			Type:        "bind",
		}
		if m.ReadOnly {
			sm.Options = []string{"ro"}
		}
		sg.Mounts = append(sg.Mounts, sm)
	}

	status, data, err := c.request(ctx, http.MethodPost, "/containers/create", sg)
	if err != nil {
		return "", fmt.Errorf("podman: creating container %q: %w", spec.Name, err)
	}
	if status >= 300 {
		return "", apiError(status, data)
	}

	var out struct {
		Id       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("podman: decoding create response for %q: %w", spec.Name, err)
	}
	return out.Id, nil
}

// StartContainer starts an existing container. Starting an
// already-running container (304) is treated as success.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	status, data, err := c.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
	if err != nil {
		return fmt.Errorf("podman: starting container %q: %w", id, err)
	}
	if status == http.StatusNotModified || status < 300 {
		return nil
	}
	return apiError(status, data)
}

// StopContainer stops a container, waiting up to timeoutSec before
// killing it (0 uses the daemon default). Stopping an already-stopped
// container (304) is treated as success.
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	path := "/containers/" + url.PathEscape(id) + "/stop"
	if timeoutSec > 0 {
		path += "?t=" + strconv.Itoa(timeoutSec)
	}
	status, data, err := c.request(ctx, http.MethodPost, path, nil)
	if err != nil {
		return fmt.Errorf("podman: stopping container %q: %w", id, err)
	}
	if status == http.StatusNotModified || status < 300 {
		return nil
	}
	return apiError(status, data)
}

// RemoveContainer removes a container. force also removes a running
// container. Removing an already-gone container (404) is treated as
// success.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	path := "/containers/" + url.PathEscape(id)
	if force {
		path += "?force=true"
	}
	status, data, err := c.request(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("podman: removing container %q: %w", id, err)
	}
	if status == http.StatusNotFound || status < 300 {
		return nil
	}
	return apiError(status, data)
}

// inspectResponse is the subset of libpod's container inspect payload
// InspectContainer cares about. ImageName is the human-readable image
// reference (e.g. "docker.io/library/caddy:2.10-alpine"), as opposed to
// the top-level "Image" field (an ID digest) — verified against a live
// libpod 5.7.1 daemon. HostConfig.PortBindings is keyed
// "<containerPort>/<protocol>" (e.g. "80/tcp"); HostPort arrives as a
// JSON string, not a number.
type inspectResponse struct {
	Id        string `json:"Id"`
	Name      string `json:"Name"`
	ImageName string `json:"ImageName"`
	State     struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []mountInspectEntry `json:"Mounts"`
}

// mountInspectEntry is one element of libpod inspect's top-level "Mounts"
// array. Only bind mounts (Type == "bind") are meaningful to BasePod —
// every mount it creates is one (see podman.CreateSpec.Mounts) — so
// parseBindMounts filters to those and drops everything else (e.g. any
// mount unrelated to basepod that happens to exist).
type mountInspectEntry struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// parseBindMounts converts libpod inspect's "Mounts" array into
// []BindMount, keeping only bind-type entries. Callers (caddy.driftReason)
// compare mounts only as a set of Source→Destination pairs, so ReadOnly is
// left at its zero value here rather than parsed from the (unused) "RW"
// field. Sorted by Destination then Source for a deterministic,
// comparable order, since libpod's Mounts array has no guaranteed
// ordering.
func parseBindMounts(raw []mountInspectEntry) []BindMount {
	var out []BindMount
	for _, m := range raw {
		if m.Type != "bind" {
			continue
		}
		out = append(out, BindMount{Source: m.Source, Dest: m.Destination})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dest != out[j].Dest {
			return out[i].Dest < out[j].Dest
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// InspectContainer fetches details for a container by name or ID.
// Returns ErrNotFound if it doesn't exist.
func (c *Client) InspectContainer(ctx context.Context, nameOrID string) (*ContainerInfo, error) {
	status, data, err := c.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(nameOrID)+"/json", nil)
	if err != nil {
		return nil, fmt.Errorf("podman: inspecting container %q: %w", nameOrID, err)
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status >= 300 {
		return nil, apiError(status, data)
	}

	var raw inspectResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("podman: decoding inspect response for %q: %w", nameOrID, err)
	}
	return &ContainerInfo{
		ID:     raw.Id,
		Name:   strings.TrimPrefix(raw.Name, "/"),
		State:  raw.State.Status,
		Labels: raw.Config.Labels,
		Image:  raw.ImageName,
		Ports:  parsePortBindings(raw.HostConfig.PortBindings),
		Mounts: parseBindMounts(raw.Mounts),
	}, nil
}

// parsePortBindings converts libpod inspect's HostConfig.PortBindings map
// into []PortMapping. Only TCP bindings are parsed (v0.3 scope — see
// PortMapping); any entry with an unparseable port number is skipped
// rather than failing the whole inspect. The result is sorted by
// ContainerPort (then HostPort) for a deterministic, comparable order,
// since PortBindings is a map with no inherent ordering.
func parsePortBindings(bindings map[string][]struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}) []PortMapping {
	var out []PortMapping
	for key, entries := range bindings {
		port, proto, ok := strings.Cut(key, "/")
		if !ok || proto != "tcp" {
			continue
		}
		containerPort, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		for _, e := range entries {
			hostPort, err := strconv.Atoi(e.HostPort)
			if err != nil {
				continue
			}
			out = append(out, PortMapping{ContainerPort: uint16(containerPort), HostPort: uint16(hostPort)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContainerPort != out[j].ContainerPort {
			return out[i].ContainerPort < out[j].ContainerPort
		}
		return out[i].HostPort < out[j].HostPort
	})
	return out
}

// listEntry is one element of the libpod container list response.
type listEntry struct {
	Id     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// ListContainers lists all containers (running or not) whose labels
// match every key/value pair in labelFilters.
func (c *Client) ListContainers(ctx context.Context, labelFilters map[string]string) ([]ContainerInfo, error) {
	q := url.Values{}
	q.Set("all", "true")
	if len(labelFilters) > 0 {
		labels := make([]string, 0, len(labelFilters))
		for k, v := range labelFilters {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels) // deterministic query string
		filters, err := json.Marshal(map[string][]string{"label": labels})
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(filters))
	}

	status, data, err := c.request(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("podman: listing containers: %w", err)
	}
	if status >= 300 {
		return nil, apiError(status, data)
	}

	var raw []listEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("podman: decoding list response: %w", err)
	}

	out := make([]ContainerInfo, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, ContainerInfo{ID: r.Id, Name: name, State: r.State, Labels: r.Labels})
	}
	return out, nil
}

// ContainerLogs streams a container's stdout+stderr log data. The
// returned ReadCloser is the raw HTTP response body — libpod's multiplex
// framing, unparsed (see DemuxLogs) — rather than something this method
// JSON-decodes itself, since the whole point is to let the caller stream
// it (potentially indefinitely, with follow=true) instead of buffering it
// all in memory first. The caller owns closing it.
//
// follow keeps the connection open and streams new log data as the
// container produces it, so it must never complete on its own; this is
// safe because c.http's transport has no request Timeout configured (see
// New) — only ctx bounds the call. tail limits the response to the last N
// lines; tail<=0 requests the daemon's default (all logs).
//
// Returns ErrNotFound if the container doesn't exist.
func (c *Client) ContainerLogs(ctx context.Context, nameOrID string, follow bool, tail int) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("follow", strconv.FormatBool(follow))
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/containers/"+url.PathEscape(nameOrID)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman: logs %q: %w", nameOrID, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiError(resp.StatusCode, data)
	}
	return resp.Body, nil
}
