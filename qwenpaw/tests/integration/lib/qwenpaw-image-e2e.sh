#!/usr/bin/env bash
# Shared helpers for QwenPaw worker image integration tests.

qwenpaw_e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
qwenpaw_e2e_repo_root="$(cd "${qwenpaw_e2e_dir}/../../.." && pwd)"

qwenpaw_e2e_log() {
    echo "[qwenpaw-image-e2e] $*" >&2
}

qwenpaw_e2e_fail() {
    qwenpaw_e2e_log "ERROR: $*"
    exit 1
}

qwenpaw_e2e_require_enabled() {
    if [ "${AGENTTEAMS_QWENPAW_IMAGE_E2E:-}" != "1" ]; then
        qwenpaw_e2e_log "SKIP: set AGENTTEAMS_QWENPAW_IMAGE_E2E=1 to run QwenPaw image integration tests"
        exit 0
    fi
}

qwenpaw_e2e_require_docker() {
    command -v docker >/dev/null 2>&1 || qwenpaw_e2e_fail "docker is required"
    docker info >/dev/null 2>&1 || qwenpaw_e2e_fail "docker daemon is not reachable"
}

qwenpaw_e2e_init() {
    local name="$1"
    QWENPAW_E2E_NAME="${name}"
    QWENPAW_E2E_VERSION="${AGENTTEAMS_QWENPAW_IMAGE_E2E_VERSION:-${name}-$(date +%s)-$$}"
    QWENPAW_E2E_IMAGE="${AGENTTEAMS_QWENPAW_IMAGE:-agentteams/qwenpaw-worker:${QWENPAW_E2E_VERSION}}"
    QWENPAW_E2E_MINIO_IMAGE="${AGENTTEAMS_QWENPAW_IMAGE_E2E_MINIO_IMAGE:-minio/minio:latest}"
    QWENPAW_E2E_BUCKET="${AGENTTEAMS_QWENPAW_IMAGE_E2E_BUCKET:-agentteams-storage}"
    QWENPAW_E2E_WORKER_NAME="${AGENTTEAMS_QWENPAW_IMAGE_E2E_WORKER_NAME:-${name}-worker-$$}"
    QWENPAW_E2E_WORKER_HOME="/root/agentteams-fs/agents/${QWENPAW_E2E_WORKER_NAME}"
    QWENPAW_E2E_WORKING_DIR="${QWENPAW_E2E_WORKER_HOME}/.qwenpaw"
    QWENPAW_E2E_SECRET_DIR="${QWENPAW_E2E_WORKING_DIR}.secret"
    QWENPAW_E2E_SHARED_DIR="/root/agentteams-fs/shared"
    QWENPAW_E2E_TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/qwenpaw-${name}.XXXXXX")"
    QWENPAW_E2E_NETWORK="qwenpaw-${name}-$$"
    QWENPAW_E2E_MINIO_CONTAINER="qwenpaw-${name}-minio-$$"
    QWENPAW_E2E_WORKER_CONTAINER="qwenpaw-${name}-worker-$$"
    mkdir -p "${QWENPAW_E2E_TMP_DIR}/seed"
    trap qwenpaw_e2e_cleanup EXIT
}

qwenpaw_e2e_cleanup() {
    local status=$?
    if [ "${status}" -ne 0 ] && [ "${AGENTTEAMS_QWENPAW_IMAGE_E2E_KEEP:-}" = "1" ]; then
        qwenpaw_e2e_log "preserving resources for debugging"
        qwenpaw_e2e_log "  worker container: ${QWENPAW_E2E_WORKER_CONTAINER}"
        qwenpaw_e2e_log "  minio container: ${QWENPAW_E2E_MINIO_CONTAINER}"
        qwenpaw_e2e_log "  docker network: ${QWENPAW_E2E_NETWORK}"
        qwenpaw_e2e_log "  temp dir: ${QWENPAW_E2E_TMP_DIR}"
        exit "${status}"
    fi
    docker rm -f "${QWENPAW_E2E_WORKER_CONTAINER}" "${QWENPAW_E2E_MINIO_CONTAINER}" >/dev/null 2>&1 || true
    docker network rm "${QWENPAW_E2E_NETWORK}" >/dev/null 2>&1 || true
    rm -rf "${QWENPAW_E2E_TMP_DIR}"
}

qwenpaw_e2e_build_or_use_image() {
    if [ -n "${AGENTTEAMS_QWENPAW_IMAGE:-}" ]; then
        qwenpaw_e2e_log "using existing QwenPaw worker image: ${QWENPAW_E2E_IMAGE}"
        docker image inspect "${QWENPAW_E2E_IMAGE}" >/dev/null 2>&1 || qwenpaw_e2e_fail "image not found: ${QWENPAW_E2E_IMAGE}"
        return
    fi
    if [ "${AGENTTEAMS_QWENPAW_IMAGE_E2E_SKIP_BUILD:-}" = "1" ]; then
        docker image inspect "${QWENPAW_E2E_IMAGE}" >/dev/null 2>&1 || qwenpaw_e2e_fail "image not found: ${QWENPAW_E2E_IMAGE}"
        return
    fi
    qwenpaw_e2e_log "building QwenPaw worker image: ${QWENPAW_E2E_IMAGE}"
    make -C "${qwenpaw_e2e_repo_root}" build-qwenpaw-worker VERSION="${QWENPAW_E2E_VERSION}"
}

qwenpaw_e2e_create_network() {
    qwenpaw_e2e_log "creating docker network: ${QWENPAW_E2E_NETWORK}"
    docker network create "${QWENPAW_E2E_NETWORK}" >/dev/null
}

qwenpaw_e2e_start_minio() {
    qwenpaw_e2e_log "starting MinIO: ${QWENPAW_E2E_MINIO_CONTAINER}"
    docker run -d \
        --name "${QWENPAW_E2E_MINIO_CONTAINER}" \
        --network "${QWENPAW_E2E_NETWORK}" \
        --network-alias minio \
        -e MINIO_ROOT_USER=minioadmin \
        -e MINIO_ROOT_PASSWORD=minioadmin \
        "${QWENPAW_E2E_MINIO_IMAGE}" \
        server /data >/dev/null
}

qwenpaw_e2e_run_mc() {
    docker run --rm \
        --network "${QWENPAW_E2E_NETWORK}" \
        -v "${QWENPAW_E2E_TMP_DIR}/seed:/tmp/seed:ro" \
        --entrypoint sh \
        -e MC_HOST_local="http://minioadmin:minioadmin@minio:9000" \
        -e AGENTTEAMS_FS_BUCKET="${QWENPAW_E2E_BUCKET}" \
        -e AGENTTEAMS_WORKER_NAME="${QWENPAW_E2E_WORKER_NAME}" \
        "${QWENPAW_E2E_IMAGE}" \
        -lc "$*"
}

qwenpaw_e2e_wait_for_minio() {
    local i
    for i in $(seq 1 60); do
        if qwenpaw_e2e_run_mc '/usr/local/bin/mc.bin mb --ignore-existing "local/${AGENTTEAMS_FS_BUCKET}" >/dev/null 2>&1'; then
            return 0
        fi
        sleep 2
    done
    docker logs "${QWENPAW_E2E_MINIO_CONTAINER}" 2>&1 || true
    qwenpaw_e2e_fail "minio did not become ready"
}

qwenpaw_e2e_seed_storage() {
    qwenpaw_e2e_log "seeding object storage"
    qwenpaw_e2e_run_mc '/usr/local/bin/mc.bin cp --recursive /tmp/seed/ "local/${AGENTTEAMS_FS_BUCKET}/"' >/dev/null
}

qwenpaw_e2e_start_worker() {
    qwenpaw_e2e_log "starting qwenpaw-worker container: ${QWENPAW_E2E_WORKER_CONTAINER}"
    docker run -d \
        --name "${QWENPAW_E2E_WORKER_CONTAINER}" \
        --network "${QWENPAW_E2E_NETWORK}" \
        -e AGENTTEAMS_WORKER_NAME="${QWENPAW_E2E_WORKER_NAME}" \
        -e AGENTTEAMS_WORKER_CR_NAME="${QWENPAW_E2E_WORKER_NAME}" \
        -e AGENTTEAMS_FS_ENDPOINT="http://minio:9000" \
        -e AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
        -e AGENTTEAMS_FS_SECRET_KEY=minioadmin \
        -e AGENTTEAMS_FS_BUCKET="${QWENPAW_E2E_BUCKET}" \
        -e QWENPAW_LOG_LEVEL="${QWENPAW_LOG_LEVEL:-info}" \
        "$@" \
        "${QWENPAW_E2E_IMAGE}" >/dev/null
}

qwenpaw_e2e_wait_worker_http() {
    local path="$1"
    local timeout_seconds="${2:-240}"
    local elapsed=0
    while [ "${elapsed}" -lt "${timeout_seconds}" ]; do
        if docker exec "${QWENPAW_E2E_WORKER_CONTAINER}" curl -sf "http://127.0.0.1:8088${path}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    docker logs --tail 180 "${QWENPAW_E2E_WORKER_CONTAINER}" 2>&1 || true
    qwenpaw_e2e_fail "worker endpoint did not become ready: ${path}"
}

qwenpaw_e2e_exec() {
    docker exec -i \
        -e HOME="${QWENPAW_E2E_WORKER_HOME}" \
        -e QWENPAW_WORKING_DIR="${QWENPAW_E2E_WORKING_DIR}" \
        -e QWENPAW_SECRET_DIR="${QWENPAW_E2E_SECRET_DIR}" \
        "${QWENPAW_E2E_WORKER_CONTAINER}" \
        "$@"
}

qwenpaw_e2e_write_runtime_yaml() {
    local generation="$1"
    local target="${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/runtime/runtime.yaml"
    mkdir -p "$(dirname "${target}")"
    cat >"${target}" <<EOF
metadata:
  generation: "${generation}"
team:
  name: qwenpaw-image-e2e
member:
  name: ${QWENPAW_E2E_WORKER_NAME}
  runtimeName: ${QWENPAW_E2E_WORKER_NAME}
  runtime: qwenpaw
  role: worker
desired:
  mcpServers: {}
  channelPolicy: {}
EOF
}

qwenpaw_e2e_put_runtime_yaml() {
    qwenpaw_e2e_run_mc "
        /usr/local/bin/mc.bin cp \
            '/tmp/seed/agents/${QWENPAW_E2E_WORKER_NAME}/runtime/runtime.yaml' \
            'local/${QWENPAW_E2E_BUCKET}/agents/${QWENPAW_E2E_WORKER_NAME}/runtime/runtime.yaml'
    " >/dev/null
}
