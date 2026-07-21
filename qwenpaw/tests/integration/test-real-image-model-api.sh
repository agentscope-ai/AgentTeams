#!/usr/bin/env bash
# Real QwenPaw worker image + real model API tracer bullet.
#
# This test is opt-in because it builds a Docker image and calls a paid model API.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

log() {
    echo "[qwenpaw-real-e2e] $*" >&2
}

fail() {
    log "ERROR: $*"
    exit 1
}

if [ "${AGENTTEAMS_QWENPAW_REAL_MODEL_E2E:-}" != "1" ]; then
    log "SKIP: set AGENTTEAMS_QWENPAW_REAL_MODEL_E2E=1 to build the image and call a real model API"
    exit 0
fi

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "docker daemon is not reachable"

ENV_FILE="${AGENTTEAMS_ENV_FILE:-${HOME}/agentteams-manager.env}"

read_env_file() {
    key="$1"
    [ -f "${ENV_FILE}" ] || return 0
    grep "^${key}=" "${ENV_FILE}" 2>/dev/null | head -1 | cut -d= -f2- | tr -d '\r'
}

API_KEY="${AGENTTEAMS_QWENPAW_REAL_MODEL_API_KEY:-$(read_env_file AGENTTEAMS_LLM_API_KEY)}"
[ -n "${API_KEY}" ] || fail "AGENTTEAMS_LLM_API_KEY is required in ${ENV_FILE}"

MODEL="${AGENTTEAMS_QWENPAW_REAL_MODEL:-$(read_env_file AGENTTEAMS_DEFAULT_MODEL)}"
MODEL="${MODEL:-qwen3.6-plus}"
BASE_URL="${AGENTTEAMS_QWENPAW_REAL_MODEL_BASE_URL:-$(read_env_file AGENTTEAMS_OPENAI_BASE_URL)}"
BASE_URL="${BASE_URL:-https://dashscope.aliyuncs.com/compatible-mode/v1}"
PROVIDER_ID="${AGENTTEAMS_QWENPAW_REAL_MODEL_PROVIDER_ID:-hiclaw-real-model}"
MINIO_IMAGE="${AGENTTEAMS_QWENPAW_REAL_MODEL_MINIO_IMAGE:-minio/minio:latest}"
VERSION="${AGENTTEAMS_QWENPAW_REAL_MODEL_IMAGE_VERSION:-qwenpaw-real-e2e-$(date +%s)-$$}"
WORKER_NAME="${AGENTTEAMS_QWENPAW_REAL_MODEL_WORKER_NAME:-qwenpaw-real-worker-$$}"
WORKER_HOME="/root/agentteams-fs/agents/${WORKER_NAME}"
QWENPAW_WORKING_DIR="${WORKER_HOME}/.qwenpaw"
QWENPAW_SECRET_DIR="${QWENPAW_WORKING_DIR}.secret"
BUCKET="${AGENTTEAMS_QWENPAW_REAL_MODEL_BUCKET:-agentteams-storage}"

if [ -n "${AGENTTEAMS_QWENPAW_REAL_MODEL_IMAGE:-}" ]; then
    IMAGE="${AGENTTEAMS_QWENPAW_REAL_MODEL_IMAGE}"
else
    IMAGE="agentteams/qwenpaw-worker:${VERSION}"
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/qwenpaw-real-e2e.XXXXXX")"
NETWORK="qwenpaw-real-e2e-$$"
MINIO_CONTAINER="qwenpaw-real-minio-$$"
WORKER_CONTAINER="qwenpaw-real-worker-$$"

cleanup() {
    status=$?
    if [ "${status}" -ne 0 ] && [ "${AGENTTEAMS_QWENPAW_REAL_MODEL_KEEP:-}" = "1" ]; then
        log "preserving resources for debugging"
        log "  worker container: ${WORKER_CONTAINER}"
        log "  minio container: ${MINIO_CONTAINER}"
        log "  docker network: ${NETWORK}"
        log "  temp dir: ${TMP_DIR}"
        exit "${status}"
    fi
    docker rm -f "${WORKER_CONTAINER}" "${MINIO_CONTAINER}" >/dev/null 2>&1 || true
    docker network rm "${NETWORK}" >/dev/null 2>&1 || true
    rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

run_mc() {
    docker run --rm \
        --network "${NETWORK}" \
        -v "${TMP_DIR}/seed:/tmp/seed:ro" \
        --entrypoint sh \
        -e MC_HOST_local="http://minioadmin:minioadmin@minio:9000" \
        -e AGENTTEAMS_FS_BUCKET="${BUCKET}" \
        -e AGENTTEAMS_WORKER_NAME="${WORKER_NAME}" \
        "${IMAGE}" \
        -lc "$*"
}

wait_for_minio() {
    for _ in $(seq 1 60); do
        if run_mc '/usr/local/bin/mc.bin mb --ignore-existing "local/${AGENTTEAMS_FS_BUCKET}" >/dev/null 2>&1'; then
            return 0
        fi
        sleep 2
    done
    docker logs "${MINIO_CONTAINER}" 2>&1 || true
    fail "minio did not become ready"
}

wait_worker_http() {
    path="$1"
    timeout_seconds="${2:-180}"
    elapsed=0
    while [ "${elapsed}" -lt "${timeout_seconds}" ]; do
        if docker exec "${WORKER_CONTAINER}" curl -sf "http://127.0.0.1:8088${path}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    docker logs --tail 160 "${WORKER_CONTAINER}" 2>&1 || true
    fail "worker endpoint did not become ready: ${path}"
}

if [ -z "${AGENTTEAMS_QWENPAW_REAL_MODEL_IMAGE:-}" ] && [ "${AGENTTEAMS_QWENPAW_REAL_MODEL_SKIP_BUILD:-}" != "1" ]; then
    log "building QwenPaw worker image: ${IMAGE}"
    make -C "${REPO_ROOT}" build-qwenpaw-worker VERSION="${VERSION}"
else
    log "using existing QwenPaw worker image: ${IMAGE}"
    docker image inspect "${IMAGE}" >/dev/null 2>&1 || fail "image not found: ${IMAGE}"
fi

log "creating docker network: ${NETWORK}"
docker network create "${NETWORK}" >/dev/null

log "starting MinIO: ${MINIO_CONTAINER}"
docker run -d \
    --name "${MINIO_CONTAINER}" \
    --network "${NETWORK}" \
    --network-alias minio \
    -e MINIO_ROOT_USER=minioadmin \
    -e MINIO_ROOT_PASSWORD=minioadmin \
    "${MINIO_IMAGE}" \
    server /data >/dev/null

mkdir -p "${TMP_DIR}/seed/agents/${WORKER_NAME}/runtime" "${TMP_DIR}/seed/agents/${WORKER_NAME}"
cat >"${TMP_DIR}/seed/agents/${WORKER_NAME}/runtime/runtime.yaml" <<EOF
metadata:
  generation: "1"
team:
  name: qwenpaw-real-model-e2e
member:
  name: ${WORKER_NAME}
  runtimeName: ${WORKER_NAME}
  runtime: qwenpaw
  role: worker
desired:
  model:
    providerId: ${PROVIDER_ID}
    providerName: AgentTeams Real Model E2E
    model: ${MODEL}
    baseUrl: ${BASE_URL}
    apiKeyEnv: AGENTTEAMS_QWENPAW_REAL_MODEL_API_KEY
  mcpServers: {}
  channelPolicy: {}
credentials:
  gatewayKeyEnv: AGENTTEAMS_QWENPAW_REAL_MODEL_API_KEY
EOF
cat >"${TMP_DIR}/seed/agents/${WORKER_NAME}/SOUL.md" <<EOF
# QwenPaw Real Model E2E

Use the configured real model provider.
EOF

wait_for_minio
log "seeding runtime.yaml into object storage"
run_mc "
    /usr/local/bin/mc.bin cp \
        '/tmp/seed/agents/${WORKER_NAME}/runtime/runtime.yaml' \
        'local/${BUCKET}/agents/${WORKER_NAME}/runtime/runtime.yaml' &&
    /usr/local/bin/mc.bin cp \
        '/tmp/seed/agents/${WORKER_NAME}/SOUL.md' \
        'local/${BUCKET}/agents/${WORKER_NAME}/SOUL.md'
" >/dev/null

log "starting qwenpaw-worker container: ${WORKER_CONTAINER}"
docker run -d \
    --name "${WORKER_CONTAINER}" \
    --network "${NETWORK}" \
    -e AGENTTEAMS_WORKER_NAME="${WORKER_NAME}" \
    -e AGENTTEAMS_WORKER_CR_NAME="${WORKER_NAME}" \
    -e AGENTTEAMS_FS_ENDPOINT="http://minio:9000" \
    -e AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
    -e AGENTTEAMS_FS_SECRET_KEY=minioadmin \
    -e AGENTTEAMS_FS_BUCKET="${BUCKET}" \
    -e AGENTTEAMS_QWENPAW_REAL_MODEL_API_KEY="${API_KEY}" \
    -e QWENPAW_LOG_LEVEL="${QWENPAW_LOG_LEVEL:-info}" \
    "${IMAGE}" >/dev/null

wait_worker_http /api/version 240
wait_worker_http /api/teamharness/health 240

log "verifying provider config from runtime.yaml"
docker exec -i \
    -e HOME="${WORKER_HOME}" \
    -e QWENPAW_WORKING_DIR="${QWENPAW_WORKING_DIR}" \
    -e QWENPAW_SECRET_DIR="${QWENPAW_SECRET_DIR}" \
    -e EXPECTED_PROVIDER="${PROVIDER_ID}" \
    -e EXPECTED_MODEL="${MODEL}" \
    -e EXPECTED_BASE_URL="${BASE_URL}" \
    "${WORKER_CONTAINER}" \
    /opt/venv/qwenpaw/bin/python - <<'PY'
import os

from qwenpaw.providers.provider_manager import ProviderManager

expected_provider = os.environ["EXPECTED_PROVIDER"]
expected_model = os.environ["EXPECTED_MODEL"]
expected_base = os.environ["EXPECTED_BASE_URL"].rstrip("/")
if not expected_base.endswith("/v1"):
    expected_base = f"{expected_base}/v1"

manager = ProviderManager.get_instance()
active = manager.active_model
provider = manager.custom_providers.get(expected_provider)

assert active.provider_id == expected_provider, active
assert active.model == expected_model, active
assert provider is not None, expected_provider
assert provider.base_url == expected_base, provider.base_url
assert bool(provider.api_key), "provider api key missing"
PY

MARKER="AGENTTEAMS_QWENPAW_REAL_MODEL_E2E_$$_$(date +%s)"
log "calling real model through qwenpaw agents chat"
docker exec \
    -e HOME="${WORKER_HOME}" \
    -e QWENPAW_WORKING_DIR="${QWENPAW_WORKING_DIR}" \
    -e QWENPAW_SECRET_DIR="${QWENPAW_SECRET_DIR}" \
    "${WORKER_CONTAINER}" \
    /opt/venv/qwenpaw/bin/qwenpaw agents chat \
    --from-agent tester \
    --to-agent default \
    --text "Return exactly this token and no other words: ${MARKER}" \
    --mode final \
    --json-output \
    --base-url http://127.0.0.1:8088 \
    >"${TMP_DIR}/chat.out" 2>"${TMP_DIR}/chat.err" || {
        docker logs --tail 160 "${WORKER_CONTAINER}" 2>&1 || true
        cat "${TMP_DIR}/chat.err" >&2 || true
        cat "${TMP_DIR}/chat.out" >&2 || true
        fail "qwenpaw real model chat failed"
    }

if ! grep -q "${MARKER}" "${TMP_DIR}/chat.out" "${TMP_DIR}/chat.err"; then
    cat "${TMP_DIR}/chat.err" >&2 || true
    cat "${TMP_DIR}/chat.out" >&2 || true
    fail "real model response did not contain marker ${MARKER}"
fi

log "PASS: image built, worker started, provider configured, real model responded"
