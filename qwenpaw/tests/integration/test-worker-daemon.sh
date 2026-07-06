#!/usr/bin/env bash
# QwenPaw worker daemon image integration test.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/qwenpaw-image-e2e.sh"

qwenpaw_e2e_require_enabled
qwenpaw_e2e_require_docker
qwenpaw_e2e_init "worker-daemon"
qwenpaw_e2e_build_or_use_image

qwenpaw_e2e_write_runtime_yaml "1"
mkdir -p "${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}"
cat >"${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/SOUL.md" <<EOF
# QwenPaw Worker Daemon E2E

Start the worker daemon and expose QwenPaw APIs.
EOF

qwenpaw_e2e_create_network
qwenpaw_e2e_start_minio
qwenpaw_e2e_wait_for_minio
qwenpaw_e2e_seed_storage
qwenpaw_e2e_start_worker

qwenpaw_e2e_wait_worker_http /api/version 240
qwenpaw_e2e_wait_worker_http /api/teamharness/health 240

qwenpaw_e2e_exec sh -lc '
    test -d "$HOME"
    test -d "$QWENPAW_WORKING_DIR"
    test -d "$QWENPAW_WORKING_DIR/workspaces/default"
    test -f "$QWENPAW_WORKING_DIR/heartbeat.json"
'

for _ in $(seq 1 30); do
    status="$(
        qwenpaw_e2e_exec /opt/venv/qwenpaw/bin/python - <<'PY'
import json
import os
from pathlib import Path

path = Path(os.environ["QWENPAW_WORKING_DIR"]) / "heartbeat.json"
print(json.loads(path.read_text(encoding="utf-8")).get("status", ""))
PY
    )"
    if [ "${status}" = "ready" ]; then
        break
    fi
    sleep 2
done
[ "${status}" = "ready" ] || qwenpaw_e2e_fail "worker heartbeat did not become ready"

running="$(docker inspect -f '{{.State.Running}}' "${QWENPAW_E2E_WORKER_CONTAINER}")"
[ "${running}" = "true" ] || qwenpaw_e2e_fail "worker container is not running"

qwenpaw_e2e_log "PASS: worker daemon started, APIs ready, heartbeat ready"
