#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENTRYPOINT="${REPO_ROOT}/openhuman/scripts/openhuman-worker-entrypoint.sh"
TMP_ROOT=$(mktemp -d)
trap 'rm -rf "${TMP_ROOT}"' EXIT

MOCK_BIN="${TMP_ROOT}/bin"
LOG_DIR="${TMP_ROOT}/logs"
WORKSPACE="${TMP_ROOT}/workspace"
mkdir -p "${MOCK_BIN}" "${LOG_DIR}" "${WORKSPACE}"
printf '# Agent instructions\n' > "${WORKSPACE}/AGENTS.md"
printf '# Worker identity\n' > "${WORKSPACE}/SOUL.md"

cat > "${MOCK_BIN}/mc" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${TEST_LOG_DIR}/mc.log"
EOF

cat > "${MOCK_BIN}/openhuman-core" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${TEST_LOG_DIR}/openhuman-core.log"
EOF

for command_name in curl hiclaw sleep; do
    cat > "${MOCK_BIN}/${command_name}" <<'EOF'
#!/bin/sh
exit 1
EOF
done
chmod +x "${MOCK_BIN}"/*

env -i \
    PATH="${MOCK_BIN}:${PATH}" \
    HOME="${TMP_ROOT}/home" \
    TEST_LOG_DIR="${LOG_DIR}" \
    OPENHUMAN_WORKSPACE="${WORKSPACE}" \
    AGENTTEAMS_WORKER_NAME="worker-a" \
    AGENTTEAMS_WORKER_CR_NAME="worker-cr-a" \
    AGENTTEAMS_RUNTIME="docker" \
    AGENTTEAMS_FS_ENDPOINT="http://minio:9000" \
    AGENTTEAMS_FS_ACCESS_KEY="worker-a" \
    AGENTTEAMS_FS_SECRET_KEY="minio-secret" \
    AGENTTEAMS_FS_BUCKET="agentteams-storage" \
    AGENTTEAMS_STORAGE_ALIAS="agentteams" \
    AGENTTEAMS_STORAGE_PREFIX="agentteams/agentteams-storage" \
    AGENTTEAMS_MATRIX_URL="http://matrix:8080" \
    AGENTTEAMS_MATRIX_DOMAIN="matrix.example" \
    AGENTTEAMS_WORKER_MATRIX_TOKEN="matrix-token" \
    AGENTTEAMS_WORKER_ROOM_ID="!worker-room:matrix.example" \
    AGENTTEAMS_AI_GATEWAY_URL="http://gateway:8080" \
    AGENTTEAMS_WORKER_GATEWAY_KEY="gateway-key" \
    AGENTTEAMS_DEFAULT_MODEL="qwen-test" \
    bash "${ENTRYPOINT}" > "${LOG_DIR}/entrypoint.log" 2>&1

grep -Fq 'alias set agentteams http://minio:9000 worker-a minio-secret' "${LOG_DIR}/mc.log"
grep -Fq 'mirror agentteams/agentteams-storage/agents/worker-a/' "${LOG_DIR}/mc.log"
grep -Fq 'config update_model_settings --inference_url http://gateway:8080/v1 --api_key gateway-key --default_model qwen-test' \
    "${LOG_DIR}/openhuman-core.log"
grep -Fq 'homeserver = "http://matrix:8080"' "${WORKSPACE}/config.toml"
grep -Fq 'access_token = "matrix-token"' "${WORKSPACE}/config.toml"
grep -Fq 'room_id = "!worker-room:matrix.example"' "${WORKSPACE}/config.toml"
grep -Fq 'user_id = "@worker-a:matrix.example"' "${WORKSPACE}/config.toml"
grep -Fq 'models.providers["agentteams-gateway"]' "${ENTRYPOINT}"

legacy_env_prefix="HICLAW""_"
if grep -q "${legacy_env_prefix}" "${ENTRYPOINT}"; then
    echo "entrypoint still references retired environment variables" >&2
    exit 1
fi

echo "OpenHuman AgentTeams entrypoint contract: PASS"
