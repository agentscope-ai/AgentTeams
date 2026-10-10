#!/bin/bash
set -euo pipefail

# AgentTeams managed worker entrypoint for the qwen-code runtime.
#
# Contract: the controller projects a MemberRuntimeConfig document at
# agents/<worker>/runtime/runtime.yaml (pulled below with mc). The entrypoint
# starts `qwen serve` (the CLI's HTTP session daemon) with model credentials
# from that projection, then runs serve_bridge.py — the Matrix room loop that
# binds serve sessions to the worker's team/personal rooms.
#
# Deliberate scope (four-piece-set boundary): no Matrix client, file sync,
# policy, or session layer is implemented here beyond what the reference
# implementation reuses — the matrix room loop and session state are the same
# reference scripts the deepseek-harness runtime ships; the execution layer is
# the qwen-code serve HTTP API.

if [ "${AGENTTEAMS_MATRIX_E2EE:-0}" = "1" ] || [ "${AGENTTEAMS_MATRIX_E2EE:-}" = "true" ]; then
    echo "[agentteams-qc-worker] ERROR: qwen-code runtime does not support Matrix E2EE; disable AGENTTEAMS_MATRIX_E2EE or choose another runtime" >&2
    exit 1
fi

source /opt/agentteams/scripts/lib/agentteams-env.sh

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
WORKER_HOME="${AGENTTEAMS_WORKER_HOME:-/root/agentteams-fs/agents/${WORKER_NAME}}"
RUNTIME_DIR="${WORKER_HOME}/runtime"
RUNTIME_CONFIG="${RUNTIME_DIR}/runtime.yaml"
REMOTE_WORKER="${AGENTTEAMS_STORAGE_PREFIX%/}/agents/${WORKER_NAME}"

log() {
    echo "[agentteams-qc-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

if ensure_mc_credentials && agentteams_mc_host_configured; then
    log "Using controller-issued storage credentials"
else
    if [ "${AGENTTEAMS_STORAGE_PROVIDER:-minio}" = "oss" ]; then
        log "ERROR: OSS storage credentials are unavailable"
        exit 1
    fi
    mc alias set "${AGENTTEAMS_STORAGE_ALIAS}" \
        "${AGENTTEAMS_FS_ENDPOINT:?AGENTTEAMS_FS_ENDPOINT is required}" \
        "${AGENTTEAMS_FS_ACCESS_KEY:?AGENTTEAMS_FS_ACCESS_KEY is required}" \
        "${AGENTTEAMS_FS_SECRET_KEY:?AGENTTEAMS_FS_SECRET_KEY is required}" >/dev/null
fi

mkdir -p "${WORKER_HOME}" "${RUNTIME_DIR}"
export HOME="${WORKER_HOME}"
export QWEN_HOME="${WORKER_HOME}/.qwen"
export TEAMHARNESS_RUNTIME_CONFIG="${RUNTIME_CONFIG}"
export TEAMHARNESS_WORKSPACE="${WORKER_HOME}/workspace"
export AGENTTEAMS_MATRIX_USER_ID="@${WORKER_NAME}:${AGENTTEAMS_MATRIX_DOMAIN}"
mkdir -p "${QWEN_HOME}" "${TEAMHARNESS_WORKSPACE}"

log "Pulling controller-projected runtime state"
RETRY=0
until mc mirror "${REMOTE_WORKER}/runtime/" "${RUNTIME_DIR}/" --overwrite >/dev/null 2>&1; do
    RETRY=$((RETRY + 1))
    if [ "${RETRY}" -ge 12 ]; then
        log "ERROR: runtime state is unavailable after ${RETRY} attempts"
        exit 1
    fi
    sleep 5
done
if [ ! -s "${RUNTIME_CONFIG}" ]; then
    log "ERROR: ${RUNTIME_CONFIG} is missing"
    exit 1
fi

# --- model projection: runtime.yaml desired.model -> ~/.qwen/settings.json ---
MODEL="$(python3 - "$RUNTIME_CONFIG" <<'PY'
import sys
from pathlib import Path
def _unq(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in {"'", '"'}:
        return v[1:-1]
    return v
model, url = "", ""
in_desired = in_model = False
for raw in Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    if raw == "desired:":
        in_desired, in_model = True, False
        continue
    if in_desired and raw and not raw.startswith(" "):
        break
    if not in_desired:
        continue
    if raw == "  model:":
        in_model = True
        continue
    if in_model and raw.startswith("  ") and not raw.startswith("    "):
        in_model = False
    if not in_model or not raw.startswith("    "):
        continue
    k, sep, v = raw.strip().partition(":")
    if not sep:
        continue
    if k == "model":
        model = _unq(v)
    elif k == "gatewayUrl":
        url = _unq(v)
print(model)
PY
)"
GATEWAY_URL="$(python3 - "$RUNTIME_CONFIG" <<'PY'
import sys
from pathlib import Path
def _unq(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in {"'", '"'}:
        return v[1:-1]
    return v
url = ""
in_desired = in_model = False
for raw in Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    if raw == "desired:":
        in_desired, in_model = True, False
        continue
    if in_desired and raw and not raw.startswith(" "):
        break
    if not in_desired:
        continue
    if raw == "  model:":
        in_model = True
        continue
    if in_model and raw.startswith("  ") and not raw.startswith("    "):
        in_model = False
    if not in_model or not raw.startswith("    "):
        continue
    k, sep, v = raw.strip().partition(":")
    if sep and k == "gatewayUrl":
        url = _unq(v)
print(url)
PY
)"
API_KEY="${AGENTTEAMS_WORKER_GATEWAY_KEY:-}"
if [ -z "${API_KEY}" ]; then
    log "ERROR: no model credential available; the controller projects AGENTTEAMS_WORKER_GATEWAY_KEY"
    exit 1
fi
BASE_URL="${GATEWAY_URL:-${AGENTTEAMS_AI_GATEWAY_URL:-}}"
if [ -z "${BASE_URL}" ]; then
    log "ERROR: no model gateway URL (runtime.yaml desired.model.gatewayUrl or AGENTTEAMS_AI_GATEWAY_URL)"
    exit 1
fi
case "${BASE_URL%/}" in
    */v1) : ;;
    *) BASE_URL="${BASE_URL%/}/v1" ;;
esac

python3 - "${QWEN_HOME}/settings.json" "${MODEL}" "${API_KEY}" "${BASE_URL}" <<'PY'
import json, os, sys
path, model, api_key, base_url = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
doc = {
    "security": {"auth": {"selectedType": "openai", "apiKey": api_key, "baseUrl": base_url}},
    "model": {"name": model},
}
with open(path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
PY
log "Model projected: ${MODEL} via gateway (credentials in ${QWEN_HOME}/settings.json)"

SERVE_PORT="${AGENTTEAMS_QWEN_SERVE_PORT:-8088}"
SERVE_TOKEN="${AGENTTEAMS_QWEN_SERVE_TOKEN:-agentteams-worker}"
export AGENTTEAMS_QWEN_SERVE_BASE="http://127.0.0.1:${SERVE_PORT}"

log "Starting qwen serve (port ${SERVE_PORT})"
nohup qwen serve \
    --port "${SERVE_PORT}" \
    --hostname 127.0.0.1 \
    --token "${SERVE_TOKEN}" \
    --workspace "${TEAMHARNESS_WORKSPACE}" \
    > "${WORKER_HOME}/qwen-serve.log" 2>&1 &
SERVE_PID=$!

# serve must be answering before the room loop starts using it
RETRY=0
until curl -sf -m 2 -H "Authorization: Bearer ${SERVE_TOKEN}" "http://127.0.0.1:${SERVE_PORT}/health" >/dev/null 2>&1; do
    RETRY=$((RETRY + 1))
    if [ "${RETRY}" -ge 24 ]; then
        log "ERROR: qwen serve did not become healthy after ${RETRY} attempts"
        tail -20 "${WORKER_HOME}/qwen-serve.log" >&2 || true
        exit 1
    fi
    if ! kill -0 "${SERVE_PID}" 2>/dev/null; then
        log "ERROR: qwen serve process exited during startup"
        tail -20 "${WORKER_HOME}/qwen-serve.log" >&2 || true
        exit 1
    fi
    sleep 5
done
log "qwen serve ready (pid ${SERVE_PID})"

log "Starting serve bridge (Matrix room loop)"
exec python3 /opt/agentteams/scripts/serve_bridge.py
