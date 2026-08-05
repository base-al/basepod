#!/usr/bin/env bash
# shellcheck disable=SC2329 # several functions below are only invoked
# indirectly, by name, via wait_for() or the EXIT trap.
#
# scripts/e2e-local.sh — BasePod v0.1 end-to-end smoke test.
#
# Builds the basepod binary, runs `setup` + `server` against throwaway temp
# dirs and ports, then drives the full API flow (login -> create app ->
# deploy -> HTTPS fetch -> redeploy -> delete) exactly as described in the
# v0.1 walking-skeleton exit criteria. Safe to run repeatedly on a dev
# machine or in CI (GitHub Actions ubuntu-24.04 runner with rootless
# podman); requires a reachable podman socket (`podman machine start` on
# macOS, `systemctl --user enable --now podman.socket` on Linux).
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

curl -s --max-time 10 "${API_BASE}/" | grep -q '<div id="app"' || fail "dashboard shell not served at /"

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
deploy1_resp=$(auth_curl -X POST "${API_BASE}/api/v1/apps/${SLUG}/deploy")
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
deploy2_resp=$(auth_curl -X POST "${API_BASE}/api/v1/apps/${SLUG}/deploy")
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
