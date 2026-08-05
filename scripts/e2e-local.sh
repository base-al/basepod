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
# (finite SSE fetch, query-token auth scoped to the logs route only), the
# dashboard's static asset pipeline (hashed asset + immutable
# Cache-Control), and (added in v0.3) the dashboard being served remotely
# through Caddy over HTTPS at basepod.<root-domain> (its own listener on
# the "basepod" network's gateway address — Linux-first, see README).
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
# containers labeled basepod.managed=true whose name matches bp-<SLUG>-* or
# bp-caddy, the basepod network, and this script's own mktemp dirs.
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
			"bp-${SLUG}-"* | bp-caddy)
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

	rm -rf "${CFG_DIR}" "${DATA_DIR}" "${BIN_DIR}" 2>/dev/null || true

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
# Dashboard — served automatically by Caddy at https://basepod.<root-domain>
# once the server discovers the "basepod" network's gateway IP and binds its
# second (gateway-facing) listener there at boot (see
# internal/server.Run and resolveDashboardDomain). This is Linux-first:
# this CI runner (ubuntu-24.04 rootless podman) can bind the gateway
# address directly, but macOS podman-machine cannot (the gateway lives
# inside the VM's own network namespace) — see README's Remote access
# section for the macOS fallback.
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
# Logs — a finite (follow=0) SSE stream over the query-token auth path
# must carry the app's own log output; query-token auth must NOT work on
# any other route.
# ---------------------------------------------------------------------------
log "curling the app to generate a log line..."
curl -sk --max-time 5 \
	--resolve "${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}:127.0.0.1" \
	"https://${SLUG}.${ROOT_DOMAIN}:${HTTPS_PORT}/" >/dev/null 2>&1 || true

log "fetching a finite (follow=0) log stream..."
logs_output=$(curl -sN --max-time 15 \
	"${API_BASE}/api/v1/apps/${SLUG}/logs?follow=0&tail=50&access_token=${TOKEN}")
printf '%s' "${logs_output}" | grep -q '^event: log$' || fail "logs stream missing an 'event: log' line: ${logs_output}"
printf '%s' "${logs_output}" | grep -q '"stream"' || fail "logs stream missing a data line with a stream field: ${logs_output}"

log "verifying query-token auth is rejected outside the logs route..."
apps_query_token_code=$(http_code "${API_BASE}/api/v1/apps?access_token=${TOKEN}")
[ "${apps_query_token_code}" = "401" ] || fail "expected /api/v1/apps with ?access_token= to be 401, got ${apps_query_token_code}"

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
