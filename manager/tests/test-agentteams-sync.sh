#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SYNC_SCRIPT="${REPO_ROOT}/manager/agent/worker-agent/skills/file-sync/scripts/agentteams-sync.sh"
MERGE_LIB="${REPO_ROOT}/shared/lib/merge-openclaw-config.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

WORKER_NAME="alice"
LOCAL_ROOT="${TMP_DIR}/local"
REMOTE_ROOT="${TMP_DIR}/remote"
LIB_DIR="${TMP_DIR}/lib"
BIN_DIR="${TMP_DIR}/bin"
OBSERVATION="${TMP_DIR}/mirror-observation"
REMOTE_TMP="${TMP_DIR}/openclaw-remote.json"

mkdir -p \
    "${LOCAL_ROOT}/agents/${WORKER_NAME}/skills" \
    "${LOCAL_ROOT}/shared" \
    "${REMOTE_ROOT}/agents/${WORKER_NAME}" \
    "${REMOTE_ROOT}/shared" \
    "${LIB_DIR}" \
    "${BIN_DIR}"

cat > "${LOCAL_ROOT}/agents/${WORKER_NAME}/openclaw.json" <<'EOF'
{
  "channels": {
    "matrix": {
      "accessToken": "fresh-token",
      "homeserver": "https://local.example"
    }
  },
  "tools": {
    "workerOwned": true
  }
}
EOF

cat > "${REMOTE_ROOT}/agents/${WORKER_NAME}/openclaw.json" <<'EOF'
{
  "models": {
    "default": "manager-model"
  },
  "channels": {
    "matrix": {
      "accessToken": "stale-token",
      "homeserver": "https://manager.example"
    }
  }
}
EOF

printf '%s\n' "updated instructions" > "${REMOTE_ROOT}/agents/${WORKER_NAME}/AGENTS.md"

cat > "${LIB_DIR}/agentteams-env.sh" <<'EOF'
ensure_mc_credentials() {
    :
}
EOF

cat > "${LIB_DIR}/worker-file-sync.sh" <<'EOF'
worker_sync_mark_remote_pull() {
    :
}
EOF
cp "${MERGE_LIB}" "${LIB_DIR}/merge-openclaw-config.sh"

cat > "${BIN_DIR}/mc" <<'EOF'
#!/bin/sh
set -eu

operation="$1"
shift

case "${operation}" in
    cp)
        source_path="$1"
        destination="$2"
        case "${source_path}" in
            */agents/"${AGENTTEAMS_WORKER_NAME}"/openclaw.json)
                cp "${FAKE_REMOTE_ROOT}/agents/${AGENTTEAMS_WORKER_NAME}/openclaw.json" "${destination}"
                ;;
            *)
                echo "unexpected mc cp source: ${source_path}" >&2
                exit 1
                ;;
        esac
        ;;
    mirror)
        source_path="$1"
        destination="$2"
        shift 2
        case "${source_path}" in
            */agents/"${AGENTTEAMS_WORKER_NAME}"/)
                excluded=false
                previous=""
                for argument in "$@"; do
                    if [ "${previous}" = "--exclude" ] && [ "${argument}" = "openclaw.json" ]; then
                        excluded=true
                    fi
                    previous="${argument}"
                done

                mkdir -p "${destination}"
                if [ "${excluded}" != "true" ]; then
                    cp "${FAKE_REMOTE_ROOT}/agents/${AGENTTEAMS_WORKER_NAME}/openclaw.json" \
                        "${destination}/openclaw.json"
                fi

                token="$(jq -r '.channels.matrix.accessToken' "${destination}/openclaw.json")"
                {
                    echo "excluded=${excluded}"
                    echo "mirror-token=${token}"
                } > "${FAKE_OBSERVATION}"
                cp "${FAKE_REMOTE_ROOT}/agents/${AGENTTEAMS_WORKER_NAME}/AGENTS.md" \
                    "${destination}/AGENTS.md"
                ;;
            */shared/)
                mkdir -p "${destination}"
                ;;
            *)
                echo "unexpected mc mirror source: ${source_path}" >&2
                exit 1
                ;;
        esac
        ;;
    *)
        echo "unexpected mc operation: ${operation}" >&2
        exit 1
        ;;
esac
EOF
chmod +x "${BIN_DIR}/mc"

PATH="${BIN_DIR}:${PATH}" \
AGENTTEAMS_WORKER_NAME="${WORKER_NAME}" \
AGENTTEAMS_STORAGE_PREFIX="fake/agentteams-storage" \
AGENTTEAMS_ROOT="${LOCAL_ROOT}" \
AGENTTEAMS_LIB_DIR="${LIB_DIR}" \
AGENTTEAMS_REMOTE_OPENCLAW_TMP="${REMOTE_TMP}" \
FAKE_REMOTE_ROOT="${REMOTE_ROOT}" \
FAKE_OBSERVATION="${OBSERVATION}" \
    /bin/sh "${SYNC_SCRIPT}" >/dev/null

grep -qx 'excluded=true' "${OBSERVATION}"
grep -qx 'mirror-token=fresh-token' "${OBSERVATION}"

LOCAL_OPENCLAW="${LOCAL_ROOT}/agents/${WORKER_NAME}/openclaw.json"
test "$(jq -r '.channels.matrix.accessToken' "${LOCAL_OPENCLAW}")" = "fresh-token"
test "$(jq -r '.channels.matrix.homeserver' "${LOCAL_OPENCLAW}")" = "https://manager.example"
test "$(jq -r '.models.default' "${LOCAL_OPENCLAW}")" = "manager-model"
test "$(jq -r '.tools.workerOwned' "${LOCAL_OPENCLAW}")" = "true"
test "$(cat "${LOCAL_ROOT}/agents/${WORKER_NAME}/AGENTS.md")" = "updated instructions"
test ! -e "${REMOTE_TMP}"

echo "agentteams-sync token safety test: PASS"
