#!/usr/bin/env bash
# Control UI device-identity regression for the existing HTTP / LAN console
# entries (#1460).
#
# Runs a real OpenClaw gateway from the openclaw-base image on an isolated
# Docker network and probes it with the real gateway client the way a browser
# does over plain HTTP:
#
#   1. policy pin      - a device-less Control UI connection over an HTTP origin
#                        is rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED;
#   2. host-localhost  - a fresh device identity over the loopback entry returns
#                        hello-ok with no approval step;
#   3. LAN entry       - a *second, fresh* identity connecting over a
#                        non-loopback address gets PAIRING_REQUIRED with a
#                        requestId; `openclaw devices approve <requestId>`
#                        covers it once and the reconnect returns hello-ok.
#
# The probe supplies the standalone gateway client's device-signing hostDeps
# and the gateway's own Control UI build id (read from the served HTML), which
# the real same-origin UI also sends.
#
# Requirements: docker plus a locally available image (OPENCLAW_BASE_IMAGE,
# default agentteams/openclaw-base:latest). With docker present but the image
# missing the test FAILS (set CONTROL_UI_TEST_ALLOW_MISSING_IMAGE=1 to skip for
# local development); without docker it skips with a reason — never a false
# pass.
#
# Example:
#   OPENCLAW_BASE_IMAGE=agentteams/openclaw-base:latest \
#     bash manager/tests/test-control-ui-device-identity.sh

set -uo pipefail

PASS=0
FAIL=0
SKIP=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE_SRC="${SCRIPT_DIR}/control-ui-device-identity-probe.mjs"
IMAGE="${OPENCLAW_BASE_IMAGE:-agentteams/openclaw-base:latest}"
CONTAINER="control-ui-device-identity-$$"
NETWORK="control-ui-test-net-$$"
HOST_PORT="${CONTROL_UI_TEST_PORT:-19799}"
TOKEN="control-ui-test-token"
WORKDIR="$(mktemp -d)"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }
skip() { echo "  SKIP: $1"; SKIP=$((SKIP + 1)); }
note() { echo "  NOTE: $1"; }

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  docker network rm "${NETWORK}" >/dev/null 2>&1 || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

echo "=== Control UI device-identity policy (existing HTTP / LAN entries) ==="

if ! command -v docker >/dev/null 2>&1; then
  skip "docker is not available in this environment (CI runs this in the integration job)"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 0
fi
if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  if [ "${CONTROL_UI_TEST_ALLOW_MISSING_IMAGE:-0}" = "1" ]; then
    skip "image ${IMAGE} is not present locally (local development)"
    echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
    exit 0
  fi
  fail "image ${IMAGE} is available" \
       "not found; CI loads it from the build artifacts (set CONTROL_UI_TEST_ALLOW_MISSING_IMAGE=1 to skip locally)"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi

# The gateway state (config + device pairing DB) lives in HOME; identities are
# kept on the shared /probe mount so each case can present its own device.
mkdir -p "${WORKDIR}/home/.openclaw" "${WORKDIR}/probe"
cat > "${WORKDIR}/home/.openclaw/openclaw.json" <<JSON
{
  "gateway": {
    "mode": "local",
    "port": ${HOST_PORT},
    "bind": "lan",
    "auth": { "token": "${TOKEN}" },
    "controlUi": { "allowedOrigins": ["*"] }
  }
}
JSON

echo "--- starting gateway (${IMAGE}) on an isolated network"
if ! docker network create "${NETWORK}" >/dev/null; then
  fail "isolated docker network created" "docker network create failed"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi
if ! docker run -d --name "${CONTAINER}" --network "${NETWORK}" \
    -e HOME=/root \
    -v "${WORKDIR}/home:/root" \
    -v "${WORKDIR}/probe:/probe" \
    -v "${PROBE_SRC}:/probe/probe.mjs:ro" \
    "${IMAGE}" \
    sh -c "openclaw gateway run --port ${HOST_PORT} --bind lan --auth token --token ${TOKEN} > /tmp/gateway.log 2>&1" \
    >/dev/null; then
  fail "gateway container started" "docker run failed"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi

CONTAINER_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CONTAINER}" 2>/dev/null)"
if [ -z "${CONTAINER_IP}" ]; then
  fail "gateway container has an address on the isolated network" "docker inspect returned no IP"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi

run_probe() { # $1=url $2=origin $3=device(0|1) $4=identity-file ; prints JSON
  docker exec "${CONTAINER}" env \
    PROBE_URL="$1" PROBE_ORIGIN="$2" PROBE_TOKEN="${TOKEN}" \
    PROBE_DEVICE="$3" PROBE_IDENTITY_FILE="$4" \
    PROBE_BUILD_ID="${BUILD_ID:-}" \
    PROBE_RAW_BUILD_ID="${RAW_BUILD_ID:-}" \
    node /probe/probe.mjs 2>/dev/null
}

json_field() { # $1=json $2=field (supports dot paths under details.)
  printf '%s' "$1" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
cur=d
for part in sys.argv[1].split("."):
    if isinstance(cur,dict) and part in cur:
        cur=cur[part]
    else:
        print(""); raise SystemExit
print(cur if cur is not None else "")
' "$2" 2>/dev/null
}

# --- readiness comes from the HTTP surface first, because the Control UI build
# id must be known *before* the first WebSocket probe: a same-origin control-ui
# connection without it is rejected with PROTOCOL_MISMATCH before the identity
# policy is even evaluated.
READY=0
for _ in $(seq 1 60); do
  if docker exec "${CONTAINER}" sh -c "curl -fsS http://127.0.0.1:${HOST_PORT}/ >/tmp/control-ui.html" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 1
done

BUILD_ID=""
RAW_BUILD_ID=""
if [ "${READY}" -eq 1 ]; then
  RAW_BUILD_ID="$(docker exec "${CONTAINER}" sh -c \
    "grep -o 'data-openclaw-control-ui-build-id=\"[^\"]*\"' /tmp/control-ui.html | head -n1 | sed 's/.*=\"//; s/\"$//'" 2>/dev/null)"
  pass "gateway served the Control UI page on 127.0.0.1:${HOST_PORT}"
else
  # No HTML surface: fall back to a handshake to prove liveness, and take the
  # build id from a PROTOCOL_MISMATCH rejection if the gateway advertises one.
  OUT="$(run_probe "ws://127.0.0.1:${HOST_PORT}" "http://127.0.0.1:${HOST_PORT}" 0 "")"
  if [ -n "$(json_field "$OUT" code)" ] || [ -n "$(json_field "$OUT" result)" ]; then
    READY=1
    BUILD_ID="$(json_field "$OUT" details.gatewayBuildId)"
    pass "gateway answered a handshake on 127.0.0.1:${HOST_PORT}"
  fi
fi

if [ "${READY}" -ne 1 ]; then
  echo "--- gateway log (tail)"
  docker logs "${CONTAINER}" 2>&1 | tail -30
  fail "gateway became ready" "no HTTP page and no handshake answer within 60s"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi

if [ -n "${RAW_BUILD_ID}" ]; then
  pass "gateway advertises its Control UI artifact stamp (${#RAW_BUILD_ID} chars; the probe normalizes it like the UI before sending)"
else
  note "gateway serves no Control UI build id; the build check stays exempt"
fi

# --- case 1: device-less Control UI over an HTTP origin -> explicit policy code.
OUT="$(run_probe "ws://127.0.0.1:${HOST_PORT}" "http://127.0.0.1:${HOST_PORT}" 0 "")"
CODE="$(json_field "$OUT" code)"
if [ "${CODE}" = "CONTROL_UI_DEVICE_IDENTITY_REQUIRED" ]; then
  pass "device-less Control UI is rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED"
else
  fail "device-less Control UI is rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED" \
       "got code=${CODE:-<none>} output=${OUT}"
fi

# --- case 2: a fresh identity over the loopback entry -> hello-ok, no approval.
OUT="$(run_probe "ws://127.0.0.1:${HOST_PORT}" "http://127.0.0.1:${HOST_PORT}" 1 /probe/identity-loopback.json)"
RESULT="$(json_field "$OUT" result)"
CODE="$(json_field "$OUT" code)"
if [ "${RESULT}" = "hello-ok" ]; then
  pass "loopback entry with a fresh device identity returns hello-ok (no approval)"
else
  fail "loopback entry with a fresh device identity returns hello-ok" \
       "got result=${RESULT:-<none>} code=${CODE:-<none>} output=${OUT}"
fi

# --- case 3: a *second, fresh* identity over the LAN entry -> PAIRING_REQUIRED,
# explicit approval, reconnect -> hello-ok.
OUT="$(run_probe "ws://${CONTAINER_IP}:${HOST_PORT}" "http://${CONTAINER_IP}:${HOST_PORT}" 1 /probe/identity-lan.json)"
CODE="$(json_field "$OUT" code)"
REQUEST_ID="$(json_field "$OUT" details.requestId)"
if [ "${CODE}" = "PAIRING_REQUIRED" ]; then
  pass "a fresh LAN identity gets PAIRING_REQUIRED (expected: a different device than the loopback one)"
else
  fail "a fresh LAN identity gets PAIRING_REQUIRED" \
       "got code=${CODE:-<none>} output=${OUT}"
fi

if [ -n "${REQUEST_ID}" ]; then
  pass "the pairing rejection carries the requestId (${REQUEST_ID})"
else
  fail "the pairing rejection carries the requestId" "no details.requestId in output=${OUT}"
fi

if [ -n "${REQUEST_ID}" ] && docker exec "${CONTAINER}" sh -c "openclaw devices approve ${REQUEST_ID} --url ws://127.0.0.1:${HOST_PORT} --token ${TOKEN}" >/dev/null 2>&1; then
  pass "openclaw devices approve ${REQUEST_ID} approved the pending request"
else
  fail "openclaw devices approve <requestId> approved the pending request" \
       "command failed (requestId=${REQUEST_ID:-<none>})"
fi

OUT="$(run_probe "ws://${CONTAINER_IP}:${HOST_PORT}" "http://${CONTAINER_IP}:${HOST_PORT}" 1 /probe/identity-lan.json)"
RESULT="$(json_field "$OUT" result)"
CODE="$(json_field "$OUT" code)"
if [ "${RESULT}" = "hello-ok" ]; then
  pass "the approved LAN identity reconnects with hello-ok"
else
  fail "the approved LAN identity reconnects with hello-ok" \
       "got result=${RESULT:-<none>} code=${CODE:-<none>} output=${OUT}"
fi

echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
if [ "${FAIL}" -ne 0 ]; then
  echo "--- gateway log (tail)"
  docker logs "${CONTAINER}" 2>&1 | tail -30
  exit 1
fi
exit 0
