#!/bin/bash
# provision-worker-gitea.sh - Operator helper: per-worker Gitea identity + registration
#
# Creates a dedicated Gitea user + scoped PAT for a worker, registers that
# worker's own gitea-mcp Higress MCP server (mcp-gitea-<worker>) gated to
# ONLY that worker's consumer, and sets repo-collaborator roles (ro->read,
# rw->write) from a Project manifest. This is an OPERATOR-RUN script — the
# controller makes NO Gitea calls and holds NO PATs (decision #12).
#
# Usage:
#   provision-worker-gitea.sh <worker> --project <id> [--rotate] [--deprovision <id>]
#
# Modes:
#   provision-worker-gitea.sh <worker> --project <id>
#       Create (if needed) the worker's Gitea user + PAT, register
#       mcp-gitea-<worker> (single-consumer, #14), and set repo-collaborator
#       roles from shared/projects/<id>/manifest.json (#13).
#
#   provision-worker-gitea.sh <worker> --rotate --project <id>
#       Re-mint the worker's PAT and re-register mcp-gitea-<worker> with the
#       new credential (Higress holds the new PAT; nothing else changes).
#
#   provision-worker-gitea.sh <worker> --deprovision <id>
#       Reverse the Project's repo-collaborator grants for the worker and
#       remove its per-worker mcp server registration (#18). Does not delete
#       the Gitea user itself unless --delete-user is also given.
#
# Prerequisites (env):
#   GITEA_URL            Gitea base URL, e.g. https://git.pawcommit.com
#   GITEA_ADMIN_TOKEN    Gitea admin API token (this script's own credential)
#   HICLAW_AI_GATEWAY_DOMAIN, HIGRESS_COOKIE_FILE  (forwarded to setup-mcp-proxy.sh)
#
# Decisions honored:
#   #12 — all Gitea-admin + gateway calls live in THIS script, never the controller.
#   #13 — repo access (ro|ro) is enforced via Gitea repo-collaborator role, not advisory.
#   #14 — mcp-gitea-<worker> is registered single-consumer (["worker-<name>"]) only;
#          the Step-5 all-workers broadcast in setup-mcp-proxy.sh is never reached.
#   #18 — --deprovision reverses grants/registration on operator-set project completion.

set -euo pipefail
source /opt/hiclaw/scripts/lib/hiclaw-env.sh 2>/dev/null || true

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_MCP_PROXY="${SETUP_MCP_PROXY_SCRIPT:-/opt/hiclaw/scripts/skills/mcp-server-management/setup-mcp-proxy.sh}"
# Repo-local fallback so this script also works from a checked-out worktree.
if [ ! -f "${SETUP_MCP_PROXY}" ]; then
    SETUP_MCP_PROXY="${SCRIPT_DIR}/../manager/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh"
fi

log() { echo "[provision-worker-gitea] $*" >&2; }

usage() {
    echo "Usage: $0 <worker> --project <id> [--rotate]"
    echo "       $0 <worker> --deprovision <id> [--delete-user]"
    echo ""
    echo "  <worker>            Worker name (registry key, e.g. 'alice')"
    echo "  --project <id>      Project id — reads shared/projects/<id>/manifest.json"
    echo "  --rotate            Re-mint the worker's PAT and re-register the mcp server"
    echo "  --deprovision <id>  Reverse collaborator grants + remove the registration for <id>"
    echo "  --delete-user       (with --deprovision) also delete the Gitea user"
    exit 1
}

WORKER=""
PROJECT_ID=""
ROTATE="false"
DEPROVISION_ID=""
DELETE_USER="false"

POSITIONAL=()
while [ $# -gt 0 ]; do
    case "$1" in
        --project)      PROJECT_ID="${2:-}"; shift 2 ;;
        --rotate)       ROTATE="true"; shift ;;
        --deprovision)  DEPROVISION_ID="${2:-}"; shift 2 ;;
        --delete-user)  DELETE_USER="true"; shift ;;
        -h|--help)      usage ;;
        *) POSITIONAL+=("$1"); shift ;;
    esac
done

WORKER="${POSITIONAL[0]:-}"
[ -z "${WORKER}" ] && usage
if [ -z "${PROJECT_ID}" ] && [ -z "${DEPROVISION_ID}" ]; then
    usage
fi

: "${GITEA_URL:?GITEA_URL must be set}"
: "${GITEA_ADMIN_TOKEN:?GITEA_ADMIN_TOKEN must be set}"

GITEA_USER="worker-${WORKER}"
MCP_SERVER_SHORT_NAME="gitea-${WORKER}"     # setup-mcp-proxy.sh prefixes "mcp-"
MCP_SERVER_NAME="mcp-${MCP_SERVER_SHORT_NAME}"
WORKER_CONSUMER="worker-${WORKER}"

# ============================================================
# Gitea admin API helpers
# ============================================================
gitea_api() {
    local method="$1" path="$2"
    shift 2
    curl -sf -X "${method}" "${GITEA_URL}${path}" \
        -H "Authorization: token ${GITEA_ADMIN_TOKEN}" \
        -H 'Content-Type: application/json' \
        "$@"
}

ensure_gitea_user() {
    log "Ensuring Gitea user ${GITEA_USER}..."
    if gitea_api GET "/api/v1/users/${GITEA_USER}" > /dev/null 2>&1; then
        log "  user ${GITEA_USER} already exists"
        return 0
    fi
    local rand_pass
    rand_pass=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)
    gitea_api POST "/api/v1/admin/users" -d '{
        "username": "'"${GITEA_USER}"'",
        "email": "'"${GITEA_USER}"'@workers.local",
        "password": "'"${rand_pass}"'",
        "must_change_password": false
    }' > /dev/null
    log "  created Gitea user ${GITEA_USER}"
}

mint_pat() {
    # Emits the PAT on stdout only; never logged.
    local token_name="worker-token-$(date -u +%Y%m%d%H%M%S)"
    gitea_api POST "/api/v1/users/${GITEA_USER}/tokens" -d '{
        "name": "'"${token_name}"'",
        "scopes": ["write:repository", "read:repository", "read:user"]
    }' | jq -r '.sha1 // empty'
}

set_collaborator_role() {
    # set_collaborator_role <owner/repo-or-full-url> <access: rw|ro>
    local repo_ref="$1" access="$2"
    local owner_repo permission
    owner_repo=$(echo "${repo_ref}" | sed -E 's#^https?://[^/]+/##; s/\.git$//')
    if [ "${access}" = "rw" ]; then
        permission="write"
    else
        permission="read"
    fi
    log "  setting ${GITEA_USER} -> ${owner_repo} (${access} -> ${permission})"
    gitea_api PUT "/api/v1/repos/${owner_repo}/collaborators/${GITEA_USER}" -d '{
        "permission": "'"${permission}"'"
    }' > /dev/null || log "  WARNING: failed to set collaborator role on ${owner_repo}"
}

remove_collaborator_role() {
    local repo_ref="$1"
    local owner_repo
    owner_repo=$(echo "${repo_ref}" | sed -E 's#^https?://[^/]+/##; s/\.git$//')
    log "  removing ${GITEA_USER} from ${owner_repo}"
    gitea_api DELETE "/api/v1/repos/${owner_repo}/collaborators/${GITEA_USER}" > /dev/null 2>&1 \
        || log "  WARNING: failed to remove collaborator role on ${owner_repo} (may already be absent)"
}

# ============================================================
# Per-worker mcp-gitea-<worker> registration (#14: single consumer only)
# ============================================================
register_worker_mcp_server() {
    local pat="$1"
    log "Registering ${MCP_SERVER_NAME} (single-consumer: ${WORKER_CONSUMER})..."
    bash "${SETUP_MCP_PROXY}" "${MCP_SERVER_SHORT_NAME}" "${GITEA_MCP_URL:-http://gitea-mcp:8080}" http \
        --header "Authorization: Bearer ${pat}" \
        --skip-worker-broadcast

    # setup-mcp-proxy.sh's own Step 3 authorizes "manager" only; #14 requires
    # this worker's OWN consumer be authorized instead/additionally, and
    # NOTHING else — this is the single source-of-truth PUT for this server's
    # consumer allowlist.
    log "  Authorizing single consumer [\"${WORKER_CONSUMER}\"] for ${MCP_SERVER_NAME}..."
    higress_put_consumers "${MCP_SERVER_NAME}" "${WORKER_CONSUMER}"
}

higress_put_consumers() {
    local mcp_server_name="$1" consumer="$2"
    local console_url="http://127.0.0.1:8001"
    curl -sf -X PUT "${console_url}/v1/mcpServer/consumers" \
        -b "${HIGRESS_COOKIE_FILE}" \
        -H 'Content-Type: application/json' \
        -d '{"mcpServerName":"'"${mcp_server_name}"'","consumers":["'"${consumer}"'"]}' > /dev/null \
        || log "  WARNING: failed to set consumer allowlist for ${mcp_server_name}"
}

deregister_worker_mcp_server() {
    local console_url="http://127.0.0.1:8001"
    log "Removing ${MCP_SERVER_NAME} registration..."
    curl -sf -X DELETE "${console_url}/v1/mcpServer?name=${MCP_SERVER_NAME}" \
        -b "${HIGRESS_COOKIE_FILE}" > /dev/null 2>&1 \
        || log "  WARNING: failed to delete ${MCP_SERVER_NAME} (may already be absent)"
}

# ============================================================
# Manifest read (Step-1 CRD projection shape)
# ============================================================
read_manifest_repos() {
    # read_manifest_repos <project-id>
    # Prints "url|access" lines to stdout.
    local pid="$1"
    local manifest_path="${TMPDIR:-/tmp}/provision-worker-gitea-manifest-${pid}.json"
    ensure_mc_credentials 2>/dev/null || true
    if command -v mc > /dev/null 2>&1; then
        mc cat "${HICLAW_STORAGE_PREFIX:-hiclaw/hiclaw-storage}/shared/projects/${pid}/manifest.json" > "${manifest_path}" 2>/dev/null || true
    fi
    if [ ! -s "${manifest_path}" ] && [ -f "/root/hiclaw-fs/shared/projects/${pid}/manifest.json" ]; then
        cp "/root/hiclaw-fs/shared/projects/${pid}/manifest.json" "${manifest_path}"
    fi
    if [ ! -s "${manifest_path}" ]; then
        log "ERROR: could not read manifest.json for project ${pid}"
        return 1
    fi
    jq -r '.repos[] | "\(.url)|\(.access)"' "${manifest_path}"
    rm -f "${manifest_path}"
}

# ============================================================
# Update ONLY this worker's mcporter.json (+ MinIO push) — never broadcast.
# ============================================================
update_worker_mcporter() {
    # NOTE: deliberately takes no PAT — mcporter.json carries only the worker's
    # gateway key; the Gitea PAT lives solely in the Higress server registration.
    local worker_agent_dir="/root/hiclaw-fs/agents/${WORKER}"
    local mcporter_dir="${worker_agent_dir}/config"
    local mcporter_file="${mcporter_dir}/mcporter.json"
    local domain="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
    local worker_creds="/data/worker-creds/${WORKER}.env"
    local worker_key=""
    if [ -f "${worker_creds}" ]; then
        worker_key=$(grep '^WORKER_GATEWAY_KEY=' "${worker_creds}" | sed 's/^WORKER_GATEWAY_KEY="//;s/"$//')
    fi
    if [ -z "${worker_key}" ]; then
        log "  WARNING: no gateway key for ${WORKER} (creds file missing), skipping mcporter update"
        return 0
    fi
    mkdir -p "${mcporter_dir}"
    if [ -f "${mcporter_file}" ]; then
        UPDATED=$(jq --arg name "${MCP_SERVER_NAME}" --arg domain "${domain}" --arg key "${worker_key}" \
            '.mcpServers[$name] = {
                url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                transport: "http",
                headers: {Authorization: ("Bearer " + $key)}
            }' "${mcporter_file}" 2>/dev/null) || UPDATED=""
        if [ -n "${UPDATED}" ]; then
            echo "${UPDATED}" | jq . > "${mcporter_file}"
        else
            log "  WARNING: mcporter merge failed — keeping existing config/mcporter.json for ${WORKER}"
        fi
    else
        jq -n --arg name "${MCP_SERVER_NAME}" --arg domain "${domain}" --arg key "${worker_key}" \
            '{mcpServers: {($name): {
                url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                transport: "http",
                headers: {Authorization: ("Bearer " + $key)}
            }}}' > "${mcporter_file}"
    fi
    ln -sfn "${mcporter_file}" "${worker_agent_dir}/mcporter-servers.json"
    log "  Updated config/mcporter.json for ${WORKER} (this worker ONLY)"
    ensure_mc_credentials 2>/dev/null || true
    mc cp "${mcporter_file}" "${HICLAW_STORAGE_PREFIX:-hiclaw/hiclaw-storage}/agents/${WORKER}/config/mcporter.json" 2>/dev/null \
        && log "  Pushed config/mcporter.json to MinIO for ${WORKER}" \
        || log "  WARNING: Failed to push config/mcporter.json to MinIO for ${WORKER}"
}

# ============================================================
# Modes
# ============================================================
do_provision() {
    ensure_gitea_user

    local pat=""
    if [ "${ROTATE}" = "true" ]; then
        log "Rotating PAT for ${GITEA_USER}..."
        pat=$(mint_pat)
    else
        pat=$(mint_pat)
    fi
    if [ -z "${pat}" ]; then
        log "ERROR: failed to mint PAT for ${GITEA_USER}"
        exit 1
    fi

    register_worker_mcp_server "${pat}"
    update_worker_mcporter

    log "Applying repo-collaborator roles from project ${PROJECT_ID} manifest..."
    local repos_out
    if ! repos_out=$(read_manifest_repos "${PROJECT_ID}"); then
        log "ERROR: could not read manifest for project ${PROJECT_ID}; aborting (no repo grants set)"
        exit 1
    fi
    while IFS='|' read -r url access; do
        [ -z "${url}" ] && continue
        set_collaborator_role "${url}" "${access}"
    done <<< "${repos_out}"

    log "Provisioning complete for worker=${WORKER} project=${PROJECT_ID}"
    log "NOTE: PAT was never written to disk/log by this script beyond Higress's credential store."
}

do_deprovision() {
    log "Deprovisioning worker=${WORKER} project=${DEPROVISION_ID}..."
    local repos_out
    if ! repos_out=$(read_manifest_repos "${DEPROVISION_ID}"); then
        log "ERROR: could not read manifest for project ${DEPROVISION_ID}; aborting (no repo grants reversed, registration untouched)"
        exit 1
    fi
    while IFS='|' read -r url access; do
        [ -z "${url}" ] && continue
        remove_collaborator_role "${url}"
    done <<< "${repos_out}"

    deregister_worker_mcp_server

    if [ "${DELETE_USER}" = "true" ]; then
        log "Deleting Gitea user ${GITEA_USER}..."
        gitea_api DELETE "/api/v1/admin/users/${GITEA_USER}" > /dev/null 2>&1 \
            || log "  WARNING: failed to delete Gitea user ${GITEA_USER}"
    fi

    log "Deprovisioning complete for worker=${WORKER} project=${DEPROVISION_ID}"
}

if [ -n "${DEPROVISION_ID}" ]; then
    do_deprovision
else
    do_provision
fi
