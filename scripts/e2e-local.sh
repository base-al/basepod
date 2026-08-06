#!/usr/bin/env bash
# shellcheck disable=SC2329 # several functions below are only invoked
# indirectly, by name, via wait_for() or the EXIT trap.
#
# scripts/e2e-local.sh — BasePod end-to-end smoke test.
#
# Builds the basepod binary, runs `setup` + `server` against throwaway temp
# dirs and ports, then drives the full API flow (login -> create app ->
# deploy -> HTTPS fetch -> redeploy -> delete) as described in the v0.1
# walking-skeleton exit criteria, plus (added in v0.2) env vars (PUT/GET
# masking, redeploy-injects-into-container via podman inspect), custom
# domains (POST/DELETE against the rendered Caddy config), log streaming
# (finite SSE fetch, scoped to the logs route only via a short-lived
# stream token minted through POST /api/v1/stream-token — issue #4 — with
# the session token itself proven rejected on that same query param), the
# dashboard's static asset pipeline (hashed asset + immutable
# Cache-Control), and (added in v0.3) the dashboard being served remotely
# through Caddy over HTTPS at basepod.<root-domain> (proxied to a unix
# socket only the bp-caddy container can reach — Linux-first, see README),
# a tarball build-from-source deploy + its build-log endpoint (also
# exercised over both header session-token auth and a minted "build_log"
# stream token's ?access_token=), rollback (deploy v1, deploy v2, roll
# back to v1, confirm the content flips back without a rebuild), the
# basepod CLI binary itself driving that same flow
# (login/apps/deploy/rollback/env/logs) as a client against the server
# this script just started, and session logout invalidating its token.
# Safe to run repeatedly on a dev machine or in CI
# (GitHub Actions ubuntu-24.04 runner with rootless podman); requires a
# reachable podman socket (`podman machine start` on macOS,
# `systemctl --user enable --now podman.socket` on Linux).
#
# Compatible with bash 3.2 (macOS's default /bin/bash): no associative
# arrays, no mapfile, no `readlink -f`.
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
HTTP_PORT=18080
HTTPS_PORT=18443
LISTEN_ADDR=127.0.0.1:13080
API_BASE="http://${LISTEN_ADDR}"
ROOT_DOMAIN=apps.localhost
SLUG=hello
# BUILD_SLUG is a second, independent app used only by the tarball
# build/rollback/CLI section below — kept separate from SLUG so its
# deployment numbering starts clean at 1 (the rollback-to-1 assertions
# depend on that) and so it can be created/torn down without disturbing
# hello's own generation-counted assertions above.
BUILD_SLUG=buildtest
IMAGE=docker.io/traefik/whoami:latest
ADMIN_EMAIL=e2e@example.com
ADMIN_PASSWORD=e2e-testing-123
MAX_WAIT=60

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------
log() { printf '[e2e] %s\n' "$*"; }
fail() {
	printf '[e2e] FAIL: %s\n' "$*" >&2
	exit 1
}

# wait_for <description> <max-seconds> <check-function-name>
# Polls check_fn (a zero-arg function returning 0/1) once a second until it
# succeeds or max-seconds elapses. Never sleeps longer than needed.
wait_for() {
	description=$1
	max_seconds=$2
	check_fn=$3
	start_ts=$(date +%s)
	while ! "${check_fn}"; do
		now_ts=$(date +%s)
		if [ $((now_ts - start_ts)) -ge "${max_seconds}" ]; then
			return 1
		fi
		sleep 1
	done
	log "ready: ${description} ($(($(date +%s) - start_ts))s)"
	return 0
}

http_code() {
	# curl itself writes "000" via -w on connection failure (before
	# returning its own non-zero exit status), so the only job of the
	# trailing `|| true` is to stop that non-zero status from tripping
	# `set -e` — it must NOT also echo a fallback "000", or a failed
	# request prints "000" twice (curl's own output plus the fallback)
	# and the concatenated string never equals the literal "000".
	curl -s -o /dev/null --max-time 5 -w '%{http_code}' "$@" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Safety pre-check: refuse to run over a live deployment. This host may
# carry unrelated user containers (never touch those) but must have zero
# basepod.managed=true containers before we start, or we could be tearing
# down someone's real BasePod deployment in the cleanup trap below.
# ---------------------------------------------------------------------------
preexisting=$(podman ps -a --filter "label=basepod.managed=true" --format '{{.Names}}' 2>/dev/null || true)
if [ -n "${preexisting}" ]; then
	fail "pre-existing basepod.managed containers found — refusing to run (possible live deployment): ${preexisting}"
fi

preexisting_net=$(podman network ls --filter "name=^basepod$" --format '{{.Name}}' 2>/dev/null || true)
if [ -n "${preexisting_net}" ]; then
	fail "a 'basepod' podman network already exists — refusing to run (possible live deployment)"
fi

# ---------------------------------------------------------------------------
# Temp dirs / paths (mktemp, portable to macOS and Linux)
# ---------------------------------------------------------------------------
CFG_DIR=$(mktemp -d "${TMPDIR:-/tmp}/basepod-e2e-cfg.XXXXXX")
DATA_DIR=$(mktemp -d "${TMPDIR:-/tmp}/basepod-e2e-data.XXXXXX")
BIN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/basepod-e2e-bin.XXXXXX")
CFG_PATH="${CFG_DIR}/config.yaml"
BIN_PATH="${BIN_DIR}/basepod"
SERVER_LOG="${CFG_DIR}/server.log"

SERVER_PID=""
RESULT=FAIL

# ---------------------------------------------------------------------------
# Cleanup trap — runs on any exit (success, failure, or set -e abort).
# Only ever touches: the server process this script started, podman
# containers labeled basepod.managed=true whose name matches bp-<SLUG>-*,
# bp-<BUILD_SLUG>-*, or bp-caddy, the basepod network, and this script's
# own mktemp dirs. BUILD_SLUG's containers are normally already gone by
# this point (its own section deletes the app via the API before this
# trap ever runs) — this is a backstop for the case where an assertion
# for BUILD_SLUG fails partway through and the script aborts before
# reaching that delete, so a leftover container can't trip the
# preexisting-containers safety check on the next run.
# ---------------------------------------------------------------------------
cleanup() {
	exit_code=$?
	log "cleaning up..."

	if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi

	managed=$(podman ps -a --filter "label=basepod.managed=true" --format '{{.Names}}' 2>/dev/null || true)
	if [ -n "${managed}" ]; then
		printf '%s\n' "${managed}" | while IFS= read -r name; do
			case "${name}" in
			"bp-${SLUG}-"* | "bp-${BUILD_SLUG}-"* | bp-caddy)
				podman rm -f "${name}" >/dev/null 2>&1 || true
				;;
			esac
		done
	fi

	podman network rm basepod >/dev/null 2>&1 || true

	if [ "${RESULT}" = PASS ]; then
		echo "===== E2E RESULT: PASS ====="
	else
		echo "===== E2E RESULT: FAIL =====" >&2
		if [ -f "${SERVER_LOG}" ]; then
			echo "----- server log (tail) -----" >&2
			tail -n 60 "${SERVER_LOG}" >&2 2>/dev/null || true
		fi
	fi

	rm -rf "${CFG_DIR}" "${DATA_DIR}" "${BIN_DIR}" "${BUILD_DIR:-}" "${TARBALL:-}" 2>/dev/null || true

	exit "${exit_code}"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
log "building basepod binary..."
(cd "${REPO_ROOT}" && go build -o "${BIN_PATH}" ./cmd/basepod)

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------
log "running basepod setup..."
"${BIN_PATH}" setup \
	--config "${CFG_PATH}" \
	--data-dir "${DATA_DIR}" \
	--root-domain "${ROOT_DOMAIN}" \
	--admin-email "${ADMIN_EMAIL}" \
	--admin-password "${ADMIN_PASSWORD}"

# ---------------------------------------------------------------------------
# Server (background)
# ---------------------------------------------------------------------------
log "starting basepod server (listen=${LISTEN_ADDR}, http=${HTTP_PORT}, https=${HTTPS_PORT})..."
BASEPOD_LISTEN="${LISTEN_ADDR}" \
	BASEPOD_HTTP_PORT="${HTTP_PORT}" \
	BASEPOD_HTTPS_PORT="${HTTPS_PORT}" \
	"${BIN_PATH}" server --config "${CFG_PATH}" >"${SERVER_LOG}" 2>&1 &
SERVER_PID=$!

server_ready() {
	code=$(http_code -X POST "${API_BASE}/api/v1/auth/login" -d '{}')
	[ "${code}" != "000" ]
}
if ! wait_for "server accepting connections" "${MAX_WAIT}" server_ready; then
	fail "server did not start listening on ${LISTEN_ADDR} within ${MAX_WAIT}s"
fi
if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
	fail "server process exited during startup — see log above"
fi

index_html=$(curl -s --max-time 10 "${API_BASE}/")
printf '%s' "${index_html}" | grep -q '<div id="app"' || fail "dashboard shell not served at /"

# One hashed /assets/* file referenced by the served shell must exist and
# carry the long-lived immutable cache header (web/embed.go's
# setCacheHeaders) — the SPA-shell check above only proves index.html
# itself renders, not that its asset pipeline is wired up end-to-end.
asset_path=$(printf '%s' "${index_html}" | grep -o 'src="/assets/[^"]*"' | head -n1 | sed 's/^src="//;s/"$//')
[ -n "${asset_path}" ] || fail "could not find a hashed /assets/ path in served index.html"

asset_headers=$(curl -s -D - -o /dev/null --max-time 10 "${API_BASE}${asset_path}")
printf '%s' "${asset_headers}" | grep -qi '^HTTP/[0-9.]* 200' || fail "asset ${asset_path} did not return 200: ${asset_headers}"
printf '%s' "${asset_headers}" | grep -qi '^Cache-Control:.*immutable' || fail "asset ${asset_path} missing immutable Cache-Control: ${asset_headers}"

# ---------------------------------------------------------------------------
# Dashboard — served automatically by Caddy at https://basepod.<root-domain>,
# proxied to a unix socket (data_dir/caddy-sock/api.sock) that only the
# bp-caddy container can reach — bind-mounted into it alone, at
# DashboardSockMountDest (see internal/caddy.Manager and
# internal/server.Run/prepareDashboardListener). This works out of the box
# on Linux, rootless included (this CI runner: ubuntu-24.04 rootless
# podman); macOS podman-machine cannot, since its virtiofs bind mounts
# don't carry unix sockets across the VM boundary — see README's Remote
# access section for the macOS fallback.
# ---------------------------------------------------------------------------
DASHBOARD_DOMAIN="basepod.${ROOT_DOMAIN}"
CADDY_CONFIG="${DATA_DIR}/caddy/current.json"

log "verifying the dashboard route landed in caddy's config..."
[ -f "${CADDY_CONFIG}" ] || fail "caddy config not found at ${CADDY_CONFIG}"
grep -q "${DASHBOARD_DOMAIN}" "${CADDY_CONFIG}" || fail "caddy config does not contain the dashboard hostname ${DASHBOARD_DOMAIN}"

dashboard_up() {
	curl -sk --max-time 5 \
		--resolve "${DASHBOARD_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
		"https://${DASHBOARD_DOMAIN}:${HTTPS_PORT}/" 2>/dev/null | grep -q '<div id="app"'
}
if ! wait_for "dashboard reachable over HTTPS at ${DASHBOARD_DOMAIN}" "${MAX_WAIT}" dashboard_up; then
	fail "dashboard not reachable via HTTPS at ${DASHBOARD_DOMAIN} within ${MAX_WAIT}s"
fi

# ---------------------------------------------------------------------------
# Login
# ---------------------------------------------------------------------------
log "logging in..."
login_resp=$(curl -s --max-time 10 -X POST "${API_BASE}/api/v1/auth/login" \
	-d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PASSWORD}\"}")
TOKEN=$(printf '%s' "${login_resp}" | jq -r '.token // empty')
[ -n "${TOKEN}" ] || fail "login failed: ${login_resp}"

auth_curl() {
	curl -s --max-time 10 -H "Authorization: Bearer ${TOKEN}" "$@"
}

# deploy_curl is auth_curl's variant for /deploy calls specifically: a
# deploy pulls an image and polls health probes synchronously inside the
# handler (bounded server-side by deployTimeout, 5 minutes — see
# internal/api.deployTimeout), so it can legitimately take much longer
# than the 10s budget every other call in this script gets, especially on
# a loaded CI runner doing its second or third cutover in a row.
deploy_curl() {
	curl -s --max-time 90 -H "Authorization: Bearer ${TOKEN}" "$@"
}

# wait_for_deployment_healthy <slug> <number> — polls GET
# .../deployments/<number> (see internal/api's handleGetDeployment) once a
# second until it reports a terminal status, failing fast on "failed"
# (naming the deployment's own error) rather than waiting out the full
# MAX_WAIT budget, and failing on timeout otherwise. This is the async
# tarball-deploy counterpart to wait_for's generic polling loop: a tarball
# deploy's own POST .../deploy/tarball now returns 202 immediately (issue
# #2), well before the build+rollout finishes, so every caller must poll
# this route (not just check the initial response) to learn how it turns
# out.
wait_for_deployment_healthy() {
	slug=$1
	number=$2
	desc="${slug} deployment ${number} healthy"
	start_ts=$(date +%s)
	while :; do
		dep_resp=$(auth_curl "${API_BASE}/api/v1/apps/${slug}/deployments/${number}")
		dep_status=$(printf '%s' "${dep_resp}" | jq -r '.status // empty')
		if [ "${dep_status}" = "healthy" ]; then
			log "ready: ${desc} ($(($(date +%s) - start_ts))s)"
			return 0
		fi
		if [ "${dep_status}" = "failed" ]; then
			dep_error=$(printf '%s' "${dep_resp}" | jq -r '.error // empty')
			fail "${desc}: deployment failed: ${dep_error}"
		fi
		now_ts=$(date +%s)
		if [ $((now_ts - start_ts)) -ge "${MAX_WAIT}" ]; then
			fail "${desc}: timed out after ${MAX_WAIT}s (last status=${dep_status}, response=${dep_resp})"
		fi
		sleep 1
	done
}

# ---------------------------------------------------------------------------
# Create app
# ---------------------------------------------------------------------------
log "creating app '${SLUG}'..."
create_resp=$(auth_curl -X POST "${API_BASE}/api/v1/apps" \
	-d "{\"name\":\"${SLUG}\",\"image\":\"${IMAGE}\",\"port\":80}")
created_slug=$(printf '%s' "${create_resp}" | jq -r '.slug // empty')
[ "${created_slug}" = "${SLUG}" ] || fail "create app failed: ${create_resp}"

# ---------------------------------------------------------------------------
# First deploy
# ---------------------------------------------------------------------------
log "deploying (1st generation)..."
deploy1_resp=$(deploy_curl -X POST "${API_BASE}/api/v1/apps/${SLUG}/deploy")
deploy1_status=$(printf '%s' "${deploy1_resp}" | jq -r '.status // empty')
[ "${deploy1_status}" = "healthy" ] || fail "first deploy failed: ${deploy1_resp}"

whoami_up() {
	curl -sk --max-time 5 \
		--resolve "${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
		"https://${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" 2>/dev/null | grep -q "Hostname:"
}
if ! wait_for "whoami reachable over HTTPS" "${MAX_WAIT}" whoami_up; then
	fail "whoami app not reachable via HTTPS within ${MAX_WAIT}s"
fi

gen1_names=$(podman ps --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
[ "${gen1_names}" = "bp-${SLUG}-1" ] || fail "expected exactly bp-${SLUG}-1 running after first deploy, got: ${gen1_names}"

# ---------------------------------------------------------------------------
# Second deploy — same image, must cut over to bp-<slug>-2 and remove -1
# ---------------------------------------------------------------------------
log "deploying (2nd generation)..."
deploy2_resp=$(deploy_curl -X POST "${API_BASE}/api/v1/apps/${SLUG}/deploy")
deploy2_status=$(printf '%s' "${deploy2_resp}" | jq -r '.status // empty')
[ "${deploy2_status}" = "healthy" ] || fail "second deploy failed: ${deploy2_resp}"

only_gen2_running() {
	names=$(podman ps --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
	[ "${names}" = "bp-${SLUG}-2" ]
}
if ! wait_for "exactly bp-${SLUG}-2 running (bp-${SLUG}-1 removed)" "${MAX_WAIT}" only_gen2_running; then
	got=$(podman ps -a --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
	fail "expected exactly bp-${SLUG}-2 after second deploy, got: ${got}"
fi

if ! wait_for "whoami reachable over HTTPS after redeploy" "${MAX_WAIT}" whoami_up; then
	fail "whoami app not reachable via HTTPS after redeploy within ${MAX_WAIT}s"
fi

# ---------------------------------------------------------------------------
# Env vars — PUT masks secrets in its response, a redeploy actually injects
# them into the new container, and GET keeps masking afterward. Run before
# the app's final delete below (this section, domains, and logs all need a
# live app).
# ---------------------------------------------------------------------------
log "setting env vars for '${SLUG}'..."
env_put_resp=$(auth_curl -X PUT "${API_BASE}/api/v1/apps/${SLUG}/env" \
	-d '[{"key":"E2E_FOO","value":"bar123","is_secret":false},{"key":"E2E_SECRET","value":"shh","is_secret":true}]')

put_secret_value=$(printf '%s' "${env_put_resp}" | jq -r '.[] | select(.key=="E2E_SECRET") | .value')
[ "${put_secret_value}" = "" ] || fail "PUT env: E2E_SECRET value not masked in response: ${env_put_resp}"
put_foo_value=$(printf '%s' "${env_put_resp}" | jq -r '.[] | select(.key=="E2E_FOO") | .value')
[ "${put_foo_value}" = "bar123" ] || fail "PUT env: E2E_FOO value wrong in response: ${env_put_resp}"

log "redeploying to pick up env vars (3rd generation)..."
deploy3_resp=$(deploy_curl -X POST "${API_BASE}/api/v1/apps/${SLUG}/deploy")
deploy3_status=$(printf '%s' "${deploy3_resp}" | jq -r '.status // empty')
[ "${deploy3_status}" = "healthy" ] || fail "env redeploy failed: ${deploy3_resp}"

only_gen3_running() {
	names=$(podman ps --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
	[ "${names}" = "bp-${SLUG}-3" ]
}
if ! wait_for "exactly bp-${SLUG}-3 running (bp-${SLUG}-2 removed)" "${MAX_WAIT}" only_gen3_running; then
	got=$(podman ps -a --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
	fail "expected exactly bp-${SLUG}-3 after env redeploy, got: ${got}"
fi

log "verifying the new container's env via podman inspect..."
container_env=$(podman inspect "bp-${SLUG}-3" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true)
printf '%s\n' "${container_env}" | grep -qxF "E2E_FOO=bar123" || fail "bp-${SLUG}-3 env missing E2E_FOO=bar123: ${container_env}"
printf '%s\n' "${container_env}" | grep -qxF "E2E_SECRET=shh" || fail "bp-${SLUG}-3 env missing E2E_SECRET=shh: ${container_env}"

log "verifying GET env still masks the secret..."
env_get_resp=$(auth_curl "${API_BASE}/api/v1/apps/${SLUG}/env")
get_secret_value=$(printf '%s' "${env_get_resp}" | jq -r '.[] | select(.key=="E2E_SECRET") | .value')
[ "${get_secret_value}" = "" ] || fail "GET env: E2E_SECRET value not masked: ${env_get_resp}"

# ---------------------------------------------------------------------------
# Domains — a custom hostname must land in Caddy's rendered config on
# POST, and disappear from it on DELETE. (CADDY_CONFIG is set above, in the
# Dashboard section.)
# ---------------------------------------------------------------------------
CUSTOM_DOMAIN=e2e-custom.example.com

log "adding custom domain '${CUSTOM_DOMAIN}'..."
domain_raw=$(auth_curl -w '\n%{http_code}' -X POST "${API_BASE}/api/v1/apps/${SLUG}/domains" \
	-d "{\"hostname\":\"${CUSTOM_DOMAIN}\"}")
domain_code=$(printf '%s' "${domain_raw}" | tail -n1)
domain_body=$(printf '%s' "${domain_raw}" | sed '$d')
[ "${domain_code}" = "201" ] || fail "add domain: expected 201, got ${domain_code}: ${domain_body}"
domain_id=$(printf '%s' "${domain_body}" | jq -r '.id // empty')
[ -n "${domain_id}" ] || fail "add domain: no id in response: ${domain_body}"

[ -f "${CADDY_CONFIG}" ] || fail "caddy config not found at ${CADDY_CONFIG}"
grep -q "${CUSTOM_DOMAIN}" "${CADDY_CONFIG}" || fail "caddy config does not contain ${CUSTOM_DOMAIN} after POST"

log "deleting custom domain '${CUSTOM_DOMAIN}'..."
delete_domain_code=$(http_code -X DELETE -H "Authorization: Bearer ${TOKEN}" \
	"${API_BASE}/api/v1/apps/${SLUG}/domains/${domain_id}")
[ "${delete_domain_code}" = "204" ] || fail "delete domain: expected 204, got ${delete_domain_code}"

if grep -q "${CUSTOM_DOMAIN}" "${CADDY_CONFIG}"; then
	fail "caddy config still contains ${CUSTOM_DOMAIN} after DELETE"
fi

# ---------------------------------------------------------------------------
# Logs — the full session token is no longer accepted as a query-string
# ?access_token= on the logs route (issue #4: the SSE routes now demand a
# short-lived, single-purpose stream token minted via
# POST /api/v1/stream-token, scoped to exactly this app + scope). A finite
# (follow=0) SSE stream fetched with a freshly-minted "app_logs" stream
# token must carry the app's own log output; the session token itself must
# now be REJECTED (401) on that same query param — the whole point of this
# change — and query-token auth generally must still NOT work on any other
# route.
# ---------------------------------------------------------------------------
log "curling the app to generate a log line..."
curl -sk --max-time 5 \
	--resolve "${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
	"https://${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" >/dev/null 2>&1 || true

log "minting a stream token scoped to '${SLUG}' app logs..."
logs_stream_token_resp=$(auth_curl -X POST "${API_BASE}/api/v1/stream-token" \
	-d "{\"scope\":\"app_logs\",\"slug\":\"${SLUG}\"}")
LOGS_STREAM_TOKEN=$(printf '%s' "${logs_stream_token_resp}" | jq -r '.token // empty')
[ -n "${LOGS_STREAM_TOKEN}" ] || fail "mint app_logs stream token failed: ${logs_stream_token_resp}"

log "fetching a finite (follow=0) log stream using the minted stream token..."
logs_output=$(curl -sN --max-time 15 \
	"${API_BASE}/api/v1/apps/${SLUG}/logs?follow=0&tail=50&access_token=${LOGS_STREAM_TOKEN}")
printf '%s' "${logs_output}" | grep -q '^event: log$' || fail "logs stream missing an 'event: log' line: ${logs_output}"
printf '%s' "${logs_output}" | grep -q '"stream"' || fail "logs stream missing a data line with a stream field: ${logs_output}"

log "verifying the SESSION token is now rejected as ?access_token= on the logs route..."
session_token_on_logs_code=$(http_code "${API_BASE}/api/v1/apps/${SLUG}/logs?follow=0&tail=1&access_token=${TOKEN}")
[ "${session_token_on_logs_code}" = "401" ] || fail "expected the logs route with the session token as ?access_token= to be 401, got ${session_token_on_logs_code}"

log "verifying query-token auth is rejected outside the logs route..."
apps_query_token_code=$(http_code "${API_BASE}/api/v1/apps?access_token=${TOKEN}")
[ "${apps_query_token_code}" = "401" ] || fail "expected /api/v1/apps with ?access_token= to be 401, got ${apps_query_token_code}"

# ---------------------------------------------------------------------------
# Tarball build deploy — a fresh app (BUILD_SLUG, not SLUG — see its
# definition above), deployed from an uploaded gzipped tar build context
# (a Containerfile + one static file) rather than a registry image. This
# exercises the v0.3 build pipeline end to end: gzip decompress -> tar
# validate -> podman build -> the same probe-gated rollout every other
# deploy path in this script already uses.
# ---------------------------------------------------------------------------
log "creating app '${BUILD_SLUG}' for tarball/rollback/CLI coverage..."
build_create_resp=$(auth_curl -X POST "${API_BASE}/api/v1/apps" \
	-d "{\"name\":\"${BUILD_SLUG}\",\"image\":\"${IMAGE}\",\"port\":80}")
build_created_slug=$(printf '%s' "${build_create_resp}" | jq -r '.slug // empty')
[ "${build_created_slug}" = "${BUILD_SLUG}" ] || fail "create app ${BUILD_SLUG} failed: ${build_create_resp}"

BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/basepod-e2e-build.XXXXXX")
TARBALL="${BUILD_DIR}.tar.gz"

# busybox httpd: -f keeps it in the foreground (required — a daemonizing
# CMD would exit 0 immediately and the container would never pass its
# health probe), -v logs each request (so the CLI `logs` assertion further
# down has a line to read), -p is the listen port (must match the app's
# port set above, 80), -h is the document root COPY places the fixture
# under. The health probe (and every "does it serve the right content"
# assertion below) requests "/" — busybox httpd's directory handler only
# auto-serves a file named index.html, so the fixture must land at
# /www/index.html, not /www/index.txt, or every request for "/" 404s and
# the deploy never reaches "healthy".
cat >"${BUILD_DIR}/Containerfile" <<'EOF'
FROM docker.io/library/busybox:latest
COPY index.txt /www/index.html
CMD ["httpd", "-f", "-v", "-p", "80", "-h", "/www"]
EOF
printf '%s' "e2e-build-v1" >"${BUILD_DIR}/index.txt"
tar czf "${TARBALL}" -C "${BUILD_DIR}" .

# Issue #2: the tarball endpoint now spools+validates the upload
# synchronously but responds 202 Accepted the moment that succeeds — well
# before the build even starts — rather than blocking until the deploy is
# healthy. Assert the 202 + still-"deploying" shape here, then poll
# GET .../deployments/{number} (wait_for_deployment_healthy) for the real
# outcome before asserting anything about served content below.
log "deploying '${BUILD_SLUG}' from an uploaded tarball (v1)..."
build1_raw=$(deploy_curl -w '\n%{http_code}' -X POST -H "Content-Type: application/gzip" \
	--data-binary @"${TARBALL}" \
	"${API_BASE}/api/v1/apps/${BUILD_SLUG}/deploy/tarball")
build1_code=$(printf '%s' "${build1_raw}" | tail -n1)
build1_resp=$(printf '%s' "${build1_raw}" | sed '$d')
[ "${build1_code}" = "202" ] || fail "tarball deploy (v1): expected 202, got ${build1_code}: ${build1_resp}"
build1_number=$(printf '%s' "${build1_resp}" | jq -r '.number // empty')
build1_source=$(printf '%s' "${build1_resp}" | jq -r '.source // empty')
[ -n "${build1_number}" ] || fail "tarball deploy (v1): missing deployment number: ${build1_resp}"
[ "${build1_source}" = "tarball" ] || fail "tarball deploy (v1): expected source=tarball, got ${build1_source}: ${build1_resp}"

wait_for_deployment_healthy "${BUILD_SLUG}" "${build1_number}"

EXPECTED_CONTENT="e2e-build-v1"
buildtest_serves() {
	body=$(curl -sk --max-time 5 \
		--resolve "${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
		"https://${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" 2>/dev/null || true)
	printf '%s' "${body}" | grep -q "${EXPECTED_CONTENT}"
}
if ! wait_for "built app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "built app did not serve ${EXPECTED_CONTENT} via HTTPS within ${MAX_WAIT}s"
fi

log "fetching the build log for deployment 1..."
buildlog1_raw=$(auth_curl -w '\n%{http_code}' "${API_BASE}/api/v1/apps/${BUILD_SLUG}/deployments/1/log")
buildlog1_code=$(printf '%s' "${buildlog1_raw}" | tail -n1)
buildlog1_body=$(printf '%s' "${buildlog1_raw}" | sed '$d')
[ "${buildlog1_code}" = "200" ] || fail "build log: expected 200, got ${buildlog1_code}: ${buildlog1_body}"
[ -n "${buildlog1_body}" ] || fail "build log: unexpectedly empty"
printf '%s' "${buildlog1_body}" | grep -qE 'COMMIT|busybox' || fail "build log missing expected build output (COMMIT/busybox pull lines): ${buildlog1_body}"

log "minting a stream token scoped to '${BUILD_SLUG}' deployment 1's build log..."
buildlog_stream_token_resp=$(auth_curl -X POST "${API_BASE}/api/v1/stream-token" \
	-d "{\"scope\":\"build_log\",\"slug\":\"${BUILD_SLUG}\",\"deployment_number\":1}")
BUILDLOG_STREAM_TOKEN=$(printf '%s' "${buildlog_stream_token_resp}" | jq -r '.token // empty')
[ -n "${BUILDLOG_STREAM_TOKEN}" ] || fail "mint build_log stream token failed: ${buildlog_stream_token_resp}"

log "fetching the build log for deployment 1 via the minted stream token's ?access_token=..."
buildlog1_query_raw=$(curl -s -w '\n%{http_code}' --max-time 10 \
	"${API_BASE}/api/v1/apps/${BUILD_SLUG}/deployments/1/log?access_token=${BUILDLOG_STREAM_TOKEN}")
buildlog1_query_code=$(printf '%s' "${buildlog1_query_raw}" | tail -n1)
buildlog1_query_body=$(printf '%s' "${buildlog1_query_raw}" | sed '$d')
[ "${buildlog1_query_code}" = "200" ] || fail "build log via stream token: expected 200, got ${buildlog1_query_code}: ${buildlog1_query_body}"
[ "${buildlog1_query_body}" = "${buildlog1_body}" ] || fail "build log via stream token: body did not match the header-authed fetch"

log "verifying the SESSION token is rejected as ?access_token= on the build-log route..."
session_token_on_buildlog_code=$(http_code "${API_BASE}/api/v1/apps/${BUILD_SLUG}/deployments/1/log?access_token=${TOKEN}")
[ "${session_token_on_buildlog_code}" = "401" ] || fail "expected the build-log route with the session token as ?access_token= to be 401, got ${session_token_on_buildlog_code}"

# ---------------------------------------------------------------------------
# Rollback — a second tarball deploy (v2) must cut traffic over, and
# rolling back to deployment 1 must bring v1's content back by reusing its
# already-built local image tag, without rebuilding.
# ---------------------------------------------------------------------------
printf '%s' "e2e-build-v2" >"${BUILD_DIR}/index.txt"
tar czf "${TARBALL}" -C "${BUILD_DIR}" .

log "deploying '${BUILD_SLUG}' from an uploaded tarball (v2)..."
build2_raw=$(deploy_curl -w '\n%{http_code}' -X POST -H "Content-Type: application/gzip" \
	--data-binary @"${TARBALL}" \
	"${API_BASE}/api/v1/apps/${BUILD_SLUG}/deploy/tarball")
build2_code=$(printf '%s' "${build2_raw}" | tail -n1)
build2_resp=$(printf '%s' "${build2_raw}" | sed '$d')
[ "${build2_code}" = "202" ] || fail "tarball deploy (v2): expected 202, got ${build2_code}: ${build2_resp}"
build2_number=$(printf '%s' "${build2_resp}" | jq -r '.number // empty')
[ -n "${build2_number}" ] || fail "tarball deploy (v2): missing deployment number: ${build2_resp}"

wait_for_deployment_healthy "${BUILD_SLUG}" "${build2_number}"

EXPECTED_CONTENT="e2e-build-v2"
if ! wait_for "built app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "built app did not serve ${EXPECTED_CONTENT} via HTTPS within ${MAX_WAIT}s"
fi

log "rolling back '${BUILD_SLUG}' to deployment 1..."
rollback1_raw=$(deploy_curl -w '\n%{http_code}' -X POST "${API_BASE}/api/v1/apps/${BUILD_SLUG}/rollback" \
	-d '{"number":1}')
rollback1_code=$(printf '%s' "${rollback1_raw}" | tail -n1)
rollback1_body=$(printf '%s' "${rollback1_raw}" | sed '$d')
[ "${rollback1_code}" = "200" ] || fail "rollback: expected 200, got ${rollback1_code}: ${rollback1_body}"
rollback1_trigger=$(printf '%s' "${rollback1_body}" | jq -r '.trigger // empty')
[ "${rollback1_trigger}" = "rollback" ] || fail "rollback: expected trigger=rollback, got ${rollback1_trigger}: ${rollback1_body}"

EXPECTED_CONTENT="e2e-build-v1"
if ! wait_for "rolled-back app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "app did not serve ${EXPECTED_CONTENT} after rollback within ${MAX_WAIT}s"
fi

# ---------------------------------------------------------------------------
# CLI — drives the same running server through the basepod binary's own
# client commands rather than raw curl, against its own config file
# (BASEPOD_CLI_CONFIG, separate from any real ~/.config/basepod/cli.yaml
# on this machine — see internal/cli's configPathEnv).
# ---------------------------------------------------------------------------
CLI_CFG="${CFG_DIR}/cli.yaml"

log "cli: logging in..."
if ! printf '%s\n' "${ADMIN_PASSWORD}" |
	BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" login "${API_BASE}" --email "${ADMIN_EMAIL}"; then
	fail "cli login failed"
fi

log "cli: listing apps..."
cli_apps_json=$(BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" apps --json)
printf '%s' "${cli_apps_json}" | grep -q "\"${BUILD_SLUG}\"" || fail "cli apps --json missing ${BUILD_SLUG}: ${cli_apps_json}"

# Issue #2: `basepod deploy` now follows the async tarball deploy live —
# uploads, mints a build_log stream token, streams the build log to
# stdout as the build runs, then polls to a terminal status — rather than
# just blocking silently on the upload request. Capture its output (both
# streams, since the live build log and the final summary both print to
# stdout) and assert it actually contains real build output, not just a
# success message.
log "cli: deploying '${BUILD_SLUG}' from source (v2 content again)..."
cli_deploy_out=$(BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" deploy "${BUILD_DIR}" -a "${BUILD_SLUG}" 2>&1) ||
	fail "cli deploy failed: ${cli_deploy_out}"
printf '%s' "${cli_deploy_out}" | grep -qE 'COMMIT|busybox' ||
	fail "cli deploy: live build-log output missing expected build output (COMMIT/busybox pull lines): ${cli_deploy_out}"
EXPECTED_CONTENT="e2e-build-v2"
if ! wait_for "cli-deployed app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "cli deploy: app did not serve ${EXPECTED_CONTENT} within ${MAX_WAIT}s"
fi

log "cli: rolling back '${BUILD_SLUG}' to deployment 1..."
if ! BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" rollback "${BUILD_SLUG}" 1; then
	fail "cli rollback failed"
fi
EXPECTED_CONTENT="e2e-build-v1"
if ! wait_for "cli-rolled-back app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "cli rollback: app did not serve ${EXPECTED_CONTENT} within ${MAX_WAIT}s"
fi

log "cli: setting E2E_CLI env var and redeploying to pick it up..."
if ! BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" env set "${BUILD_SLUG}" E2E_CLI=1; then
	fail "cli env set failed"
fi
if ! BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" deploy "${BUILD_DIR}" -a "${BUILD_SLUG}"; then
	fail "cli redeploy (to pick up env) failed"
fi
EXPECTED_CONTENT="e2e-build-v2"
if ! wait_for "cli env-redeployed app serving ${EXPECTED_CONTENT}" "${MAX_WAIT}" buildtest_serves; then
	fail "cli env redeploy: app did not serve ${EXPECTED_CONTENT} within ${MAX_WAIT}s"
fi

log "cli: verifying E2E_CLI landed in the new container's env..."
latest_build_number=$(auth_curl "${API_BASE}/api/v1/apps/${BUILD_SLUG}" | jq -r '.deployments | map(.number) | max')
build_container_env=$(podman inspect "bp-${BUILD_SLUG}-${latest_build_number}" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true)
printf '%s\n' "${build_container_env}" | grep -qxF "E2E_CLI=1" || fail "bp-${BUILD_SLUG}-${latest_build_number} env missing E2E_CLI=1: ${build_container_env}"

log "cli: generating a log line and fetching it via 'basepod logs'..."
curl -sk --max-time 5 \
	--resolve "${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
	"https://${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" >/dev/null 2>&1 || true
cli_logs_out=$(BASEPOD_CLI_CONFIG="${CLI_CFG}" "${BIN_PATH}" logs "${BUILD_SLUG}" --tail 5)
[ -n "${cli_logs_out}" ] || fail "cli logs: expected at least one line, got none"

# ---------------------------------------------------------------------------
# Tear down the build/rollback/CLI app now, independently of hello's own
# delete section below — it must not linger and trip the safety pre-check
# (or need the cleanup trap's backstop) on the next run.
# ---------------------------------------------------------------------------
log "deleting app '${BUILD_SLUG}'..."
# Uses deploy_curl's generous budget, not http_code's fixed 5s: by this
# point in the script the runner has just finished six build+rollout
# cycles back to back, and RemoveApp's container-stop-and-remove plus a
# route reapply can occasionally run long on a loaded CI runner (observed
# 30-50s elsewhere in this same run) — a tight timeout here would read as
# a false failure ("000"), not an actual bug.
build_delete_raw=$(deploy_curl -w '\n%{http_code}' -X DELETE "${API_BASE}/api/v1/apps/${BUILD_SLUG}" || true)
build_delete_code=$(printf '%s' "${build_delete_raw}" | tail -n1)
build_delete_body=$(printf '%s' "${build_delete_raw}" | sed '$d')
[ "${build_delete_code}" = "204" ] || fail "delete app ${BUILD_SLUG}: expected 204, got ${build_delete_code}: ${build_delete_body}"

buildtest_route_gone() {
	body=$(curl -sk --max-time 5 \
		--resolve "${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
		"https://${BUILD_SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" 2>/dev/null || true)
	! printf '%s' "${body}" | grep -q "e2e-build-"
}
if ! wait_for "route removed from Caddy for ${BUILD_SLUG}" "${MAX_WAIT}" buildtest_route_gone; then
	fail "${BUILD_SLUG} still reachable via HTTPS after DELETE — route was not dropped"
fi

# ---------------------------------------------------------------------------
# Logout — POST /auth/logout must invalidate the session token it's given.
# Verified against a freshly-issued token (not $TOKEN, still needed below
# for hello's own delete) so this can run as the very last piece of API
# work before the final cleanup, per the task brief's own ordering (a
# logout test that killed $TOKEN any earlier would break every assertion
# after it).
# ---------------------------------------------------------------------------
log "logging in a throwaway session to exercise logout..."
logout_login_resp=$(curl -s --max-time 10 -X POST "${API_BASE}/api/v1/auth/login" \
	-d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PASSWORD}\"}")
LOGOUT_TOKEN=$(printf '%s' "${logout_login_resp}" | jq -r '.token // empty')
[ -n "${LOGOUT_TOKEN}" ] || fail "logout: throwaway login failed: ${logout_login_resp}"

logout_code=$(http_code -X POST -H "Authorization: Bearer ${LOGOUT_TOKEN}" "${API_BASE}/api/v1/auth/logout")
[ "${logout_code}" = "204" ] || fail "logout: expected 204, got ${logout_code}"

me_after_logout_code=$(http_code -H "Authorization: Bearer ${LOGOUT_TOKEN}" "${API_BASE}/api/v1/auth/me")
[ "${me_after_logout_code}" = "401" ] || fail "logout: /auth/me expected 401 after logout, got ${me_after_logout_code}"

# ---------------------------------------------------------------------------
# Delete app — route must be dropped from Caddy
# ---------------------------------------------------------------------------
log "deleting app '${SLUG}'..."
delete_raw=$(auth_curl -w '\n%{http_code}' -X DELETE "${API_BASE}/api/v1/apps/${SLUG}" || true)
delete_code=$(printf '%s' "${delete_raw}" | tail -n1)
delete_body=$(printf '%s' "${delete_raw}" | sed '$d')
[ "${delete_code}" = "204" ] || fail "delete app: expected 204, got ${delete_code}: ${delete_body}"

route_gone() {
	body=$(curl -sk --max-time 5 \
		--resolve "${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
		"https://${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" 2>/dev/null || true)
	! printf '%s' "${body}" | grep -q "Hostname:"
}
if ! wait_for "route removed from Caddy" "${MAX_WAIT}" route_gone; then
	fail "whoami still reachable via HTTPS after DELETE — route was not dropped"
fi

remaining=$(podman ps -a --filter "label=basepod.app=${SLUG}" --format '{{.Names}}' 2>/dev/null || true)
[ -z "${remaining}" ] || fail "expected no containers left for ${SLUG} after delete, got: ${remaining}"

# ---------------------------------------------------------------------------
# All assertions passed.
# ---------------------------------------------------------------------------
RESULT=PASS
log "all checks passed"
exit 0
