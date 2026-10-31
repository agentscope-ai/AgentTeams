#!/usr/bin/env bash
# Control UI device-identity regression for the existing HTTP / LAN console
# entries (#1460).
#
# Runs a real OpenClaw gateway from the openclaw-base image and probes it the
# way a browser does over plain HTTP:
#
#   1. policy pin      - a device-less Control UI connection over HTTP origin is
#                        rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED;
#   2. host-localhost  - with an auto-provisioned device identity (the same
#                        Ed25519 identity the SDK/browser provisions) the
#                        loopback entry returns hello-ok with no approval step;
#   3. LAN entry       - the same client connecting over a non-loopback
#                        address gets a pending pairing request, `openclaw
#                        devices approve --latest` covers it once, and the
#                        re-connect returns hello-ok.
#
# Requires docker and a locally available image (OPENCLAW_BASE_IMAGE, default
# agentteams/openclaw-base:latest). Without either it SKIPs (exit 0) with a
# reason — it must never report a false pass.
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
HOST_PORT="${CONTROL_UI_TEST_PORT:-19799}"
TOKEN="control-ui-test-token"
WORKDIR="$(mktemp -d)"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }
skip() { echo "  SKIP: $1"; SKIP=$((SKIP + 1)); }

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
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
  skip "image ${IMAGE} is not present locally (CI loads it from the build artifacts)"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 0
fi
if ! docker network inspect host >/dev/null 2>&1; then
  skip "docker host networking is unavailable; the LAN entry cannot be exercised"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 0
fi

# The gateway state (config + device pairing DB) lives in HOME; the probe
# identity file is shared with the host through the same mount.
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

echo "--- starting gateway (${IMAGE})"
if ! docker run -d --name "${CONTAINER}" --network host \
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

run_probe() { # $1=url $2=origin $3=device (0|1) ; prints JSON
  docker exec "${CONTAINER}" env \
    PROBE_URL="$1" PROBE_ORIGIN="$2" PROBE_TOKEN="${TOKEN}" \
    PROBE_DEVICE="$3" PROBE_IDENTITY_FILE=/probe/identity.json \
    node /probe/probe.mjs 2>/dev/null
}

json_field() { # $1=json $2=field
  printf '%s' "$1" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1]) or "")' "$2" 2>/dev/null
}

# --- readiness: a device-less probe answering with a policy code proves the
# gateway is up (any answer, including the expected rejection, means serving).
READY=0
for _ in $(seq 1 60); do
  OUT="$(run_probe "ws://127.0.0.1:${HOST_PORT}" "http://127.0.0.1:${HOST_PORT}" 0)"
  if [ -n "$(json_field "$OUT" code)" ] || [ -n "$(json_field "$OUT" result)" ]; then
    READY=1
    break
  fi
  sleep 1
done
if [ "${READY}" -ne 1 ]; then
  echo "--- gateway log (tail)"
  docker logs "${CONTAINER}" 2>&1 | tail -30
  fail "gateway became ready" "no handshake answer within 60s"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
  exit 1
fi
pass "gateway answered on 127.0.0.1:${HOST_PORT}"

# --- case 1: device-less Control UI over HTTP origin -> explicit policy code.
CODE="$(json_field "$OUT" code)"
if [ "${CODE}" = "CONTROL_UI_DEVICE_IDENTITY_REQUIRED" ]; then
  pass "device-less Control UI is rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED"
else
  fail "device-less Control UI is rejected with CONTROL_UI_DEVICE_IDENTITY_REQUIRED" \
       "got code=${CODE:-<none>} output=${OUT}"
fi

# --- case 2: device identity over the loopback entry -> hello-ok, no approval.
OUT="$(run_probe "ws://127.0.0.1:${HOST_PORT}" "http://127.0.0.1:${HOST_PORT}" 1)"
RESULT="$(json_field "$OUT" result)"
CODE="$(json_field "$OUT" code)"
if [ "${RESULT}" = "hello-ok" ]; then
  pass "loopback entry with an auto-provisioned device identity returns hello-ok (no approval)"
else
  fail "loopback entry with an auto-provisioned device identity returns hello-ok" \
       "got result=${RESULT:-<none>} code=${CODE:-<none>} output=${OUT}"
fi

# --- case 3: LAN entry -> pending pairing, approve once, reconnect -> hello-ok.
LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
if [ -z "${LAN_IP}" ]; then
  skip "LAN entry: no non-loopback address on this host"
else
  OUT="$(run_probe "ws://${LAN_IP}:${HOST_PORT}" "http://${LAN_IP}:${HOST_PORT}" 1)"
  CODE="$(json_field "$OUT" code)"
  if [ "${CODE}" = "DEVICE_IDENTITY_REQUIRED" ] || [ "${CODE}" = "NOT_PAIRED" ]; then
    pass "LAN entry asks for a one-time device approval (code=${CODE})"
  else
    fail "LAN entry asks for a one-time device approval" \
         "got code=${CODE:-<none>} output=${OUT}"
  fi

  if docker exec "${CONTAINER}" sh -c "openclaw devices approve --latest" >/dev/null 2>&1; then
    pass "openclaw devices approve --latest approved the pending device"
  else
    fail "openclaw devices approve --latest approved the pending device" \
         "command failed; see the gateway log"
  fi

  OUT="$(run_probe "ws://${LAN_IP}:${HOST_PORT}" "http://${LAN_IP}:${HOST_PORT}" 1)"
  RESULT="$(json_field "$OUT" result)"
  CODE="$(json_field "$OUT" code)"
  if [ "${RESULT}" = "hello-ok" ]; then
    pass "LAN entry returns hello-ok after the one-time approval"
  else
    fail "LAN entry returns hello-ok after the one-time approval" \
         "got result=${RESULT:-<none>} code=${CODE:-<none>} output=${OUT}"
  fi
fi

echo "SUMMARY: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped"
if [ "${FAIL}" -ne 0 ]; then
  echo "--- gateway log (tail)"
  docker logs "${CONTAINER}" 2>&1 | tail -30
  exit 1
fi
exit 0
