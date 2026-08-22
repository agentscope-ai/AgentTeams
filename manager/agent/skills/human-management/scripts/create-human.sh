#!/bin/bash
# create-human.sh - Import a human user into HiClaw
#
# Registers a Matrix account, configures permissions based on level,
# and optionally sends a welcome email.
#
# Usage:
#   create-human.sh --matrix-id <@user:domain> --name <display_name> --level <1|2|3> \
#     [--teams t1,t2] [--workers w1,w2] [--email user@example.com] [--note "..."]

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

log() {
    local msg="[hiclaw $(date '+%Y-%m-%d %H:%M:%S')] $1"
    echo "${msg}"
    if [ -w /proc/1/fd/1 ]; then
        echo "${msg}" > /proc/1/fd/1
    fi
}

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# ============================================================
# Parse arguments
# ============================================================
MATRIX_ID=""
DISPLAY_NAME=""
LEVEL=""
TEAMS_CSV=""
WORKERS_CSV=""
EMAIL=""
NOTE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --matrix-id)    MATRIX_ID="$2"; shift 2 ;;
        --name)         DISPLAY_NAME="$2"; shift 2 ;;
        --level)        LEVEL="$2"; shift 2 ;;
        --teams)        TEAMS_CSV="$2"; shift 2 ;;
        --workers)      WORKERS_CSV="$2"; shift 2 ;;
        --email)        EMAIL="$2"; shift 2 ;;
        --note)         NOTE="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${MATRIX_ID}" ] || [ -z "${DISPLAY_NAME}" ] || [ -z "${LEVEL}" ]; then
    echo "Usage: create-human.sh --matrix-id <@user:domain> --name <name> --level <1|2|3> [--teams t1,t2] [--workers w1,w2] [--email addr] [--note text]"
    exit 1
fi

# Extract username from Matrix ID (@username:domain → username)
HUMAN_USERNAME=$(echo "${MATRIX_ID}" | sed 's/^@//' | cut -d: -f1)
MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
ADMIN_USER="${HICLAW_ADMIN_USER:-admin}"

log "=== Importing Human: ${DISPLAY_NAME} (${MATRIX_ID}) ==="
log "  Level: ${LEVEL}"
log "  Teams: ${TEAMS_CSV:-none}"
log "  Workers: ${WORKERS_CSV:-none}"

# ============================================================
# Ensure credentials
# ============================================================
SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi

if [ -z "${MANAGER_MATRIX_TOKEN:-}" ]; then
    MANAGER_PASSWORD="${HICLAW_MANAGER_PASSWORD:-}"
    if [ -z "${MANAGER_PASSWORD}" ]; then
        _fail "MANAGER_MATRIX_TOKEN not set"
    fi
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
    [ -z "${MANAGER_MATRIX_TOKEN}" ] && _fail "Failed to obtain Manager Matrix token"
fi

# ============================================================
# Step 1: Register Matrix account
# ============================================================
log "Step 1: Registering Matrix account..."
HUMAN_PASSWORD=$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | xxd -p)

REG_RESP=$(curl -s -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/register \
    -H 'Content-Type: application/json' \
    -d '{
        "username": "'"${HUMAN_USERNAME}"'",
        "password": "'"${HUMAN_PASSWORD}"'",
        "auth": {
            "type": "m.login.registration_token",
            "token": "'"${HICLAW_REGISTRATION_TOKEN}"'"
        }
    }' 2>/dev/null) || true

if echo "${REG_RESP}" | jq -e '.access_token' > /dev/null 2>&1; then
    log "  Registered new account: ${MATRIX_ID}"
else
    log "  Account may already exist (registration response: ${REG_RESP:0:100})"
    log "  Proceeding with permission configuration..."
fi

# ============================================================
# Step 2: Configure permissions based on level
# ============================================================
log "Step 2: Configuring permissions (level ${LEVEL})..."

REGISTRIES_DIR="${HOME}"
WORKERS_REGISTRY="${REGISTRIES_DIR}/workers-registry.json"
TEAMS_REGISTRY="${REGISTRIES_DIR}/teams-registry.json"
ROOMS_INVITED=()

ensure_mc_credentials 2>/dev/null || true

# Helper: add human to an agent's groupAllowFrom and push to MinIO
_add_to_group_allow() {
    local agent_name="$1"
    local config_path="/root/hiclaw-fs/agents/${agent_name}/openclaw.json"

    if [ ! -f "${config_path}" ]; then
        log "    WARNING: ${config_path} not found, skipping"
        return
    fi

    local already
    already=$(jq -r --arg h "${MATRIX_ID}" \
        '.channels.matrix.groupAllowFrom // [] | map(select(. == $h)) | length' \
        "${config_path}" 2>/dev/null || echo "0")

    if [ "${already}" = "0" ]; then
        jq --arg h "${MATRIX_ID}" \
            '.channels.matrix.groupAllowFrom += [$h]' \
            "${config_path}" > /tmp/human-config-tmp.json
        mv /tmp/human-config-tmp.json "${config_path}"
        mc cp "${config_path}" "${HICLAW_STORAGE_PREFIX}/agents/${agent_name}/openclaw.json" 2>/dev/null \
            || log "    WARNING: Failed to push ${agent_name} config to MinIO"
        log "    Added to ${agent_name}'s groupAllowFrom"
    else
        log "    Already in ${agent_name}'s groupAllowFrom"
    fi
}

# Helper: add human to Manager's DM allowFrom + groupAllowFrom
_add_to_manager() {
    local mgr_config="${HOME}/openclaw.json"
    if [ ! -f "${mgr_config}" ]; then return; fi

    jq --arg h "${MATRIX_ID}" \
        'if (.channels.matrix.groupAllowFrom | index($h)) then .
         else .channels.matrix.groupAllowFrom += [$h]
         end
         | if (.channels.matrix.dm.allowFrom | index($h)) then .
           else .channels.matrix.dm.allowFrom += [$h]
           end' \
        "${mgr_config}" > /tmp/mgr-config-tmp.json
    mv /tmp/mgr-config-tmp.json "${mgr_config}"
    log "    Added to Manager's groupAllowFrom + dm.allowFrom"
}

# Helper: invite human to a Matrix room
_invite_to_room() {
    local room_id="$1"
    [ -z "${room_id}" ] || [ "${room_id}" = "null" ] && return

    curl -sf -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${room_id}/invite" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"user_id": "'"${MATRIX_ID}"'"}' 2>/dev/null || true
    ROOMS_INVITED+=("${room_id}")
}

# --- Level 1: Admin equivalent ---
if [ "${LEVEL}" = "1" ]; then
    log "  Level 1: Adding to Manager + all agents..."
    _add_to_manager

    # All workers (team leaders + workers + standalone)
    if [ -f "${WORKERS_REGISTRY}" ]; then
        for agent in $(jq -r '.workers | keys[]' "${WORKERS_REGISTRY}" 2>/dev/null); do
            _add_to_group_allow "${agent}"
            room=$(jq -r --arg w "${agent}" '.workers[$w].room_id // empty' "${WORKERS_REGISTRY}")
            _invite_to_room "${room}"
        done
    fi

    # All team rooms
    if [ -f "${TEAMS_REGISTRY}" ]; then
        for team in $(jq -r '.teams | keys[]' "${TEAMS_REGISTRY}" 2>/dev/null); do
            team_room=$(jq -r --arg t "${team}" '.teams[$t].team_room_id // empty' "${TEAMS_REGISTRY}")
            _invite_to_room "${team_room}"
        done
    fi
fi

# --- Level 2: Team-scoped ---
if [ "${LEVEL}" = "2" ]; then
    log "  Level 2: Adding to specified teams + workers..."

    # Specified teams
    if [ -n "${TEAMS_CSV}" ]; then
        IFS=',' read -ra TEAM_ARR <<< "${TEAMS_CSV}"
        for team in "${TEAM_ARR[@]}"; do
            team=$(echo "${team}" | tr -d ' ')
            [ -z "${team}" ] && continue

            if [ ! -f "${TEAMS_REGISTRY}" ]; then
                log "    Team registry not found, skipping team ${team} (will be configured when team is created)"
                continue
            fi

            # Check if team exists in registry
            team_exists=$(jq -r --arg t "${team}" '.teams[$t] // empty' "${TEAMS_REGISTRY}" 2>/dev/null)
            if [ -z "${team_exists}" ]; then
                log "    Team ${team} not yet created, skipping permissions (will be configured when team is created)"
                continue
            fi

            # Add to team leader
            leader=$(jq -r --arg t "${team}" '.teams[$t].leader // empty' "${TEAMS_REGISTRY}")
            if [ -n "${leader}" ]; then
                _add_to_group_allow "${leader}"
                leader_room=$(jq -r --arg w "${leader}" '.workers[$w].room_id // empty' "${WORKERS_REGISTRY}" 2>/dev/null)
                _invite_to_room "${leader_room}"
            fi

            # Add to all team workers
            for tw in $(jq -r --arg t "${team}" '.teams[$t].workers // [] | .[]' "${TEAMS_REGISTRY}" 2>/dev/null); do
                _add_to_group_allow "${tw}"
                tw_room=$(jq -r --arg w "${tw}" '.workers[$w].room_id // empty' "${WORKERS_REGISTRY}" 2>/dev/null)
                _invite_to_room "${tw_room}"
            done

            # Invite to team room
            team_room=$(jq -r --arg t "${team}" '.teams[$t].team_room_id // empty' "${TEAMS_REGISTRY}")
            _invite_to_room "${team_room}"
        done
    fi

    # Specified standalone workers
    if [ -n "${WORKERS_CSV}" ]; then
        IFS=',' read -ra W_ARR <<< "${WORKERS_CSV}"
        for w in "${W_ARR[@]}"; do
            w=$(echo "${w}" | tr -d ' ')
            [ -z "${w}" ] && continue
            _add_to_group_allow "${w}"
            w_room=$(jq -r --arg w "${w}" '.workers[$w].room_id // empty' "${WORKERS_REGISTRY}" 2>/dev/null)
            _invite_to_room "${w_room}"
        done
    fi
fi

# --- Level 3: Worker-only ---
if [ "${LEVEL}" = "3" ]; then
    log "  Level 3: Adding to specified workers only..."

    if [ -n "${WORKERS_CSV}" ]; then
        IFS=',' read -ra W_ARR <<< "${WORKERS_CSV}"
        for w in "${W_ARR[@]}"; do
            w=$(echo "${w}" | tr -d ' ')
            [ -z "${w}" ] && continue
            _add_to_group_allow "${w}"
            w_room=$(jq -r --arg w "${w}" '.workers[$w].room_id // empty' "${WORKERS_REGISTRY}" 2>/dev/null)
            _invite_to_room "${w_room}"
        done
    fi
fi

# ============================================================
# Step 3: Update humans-registry.json
# ============================================================
log "Step 3: Updating humans-registry.json..."
bash /opt/hiclaw/agent/skills/human-management/scripts/manage-humans-registry.sh \
    --action add \
    --name "${HUMAN_USERNAME}" \
    --matrix-id "${MATRIX_ID}" \
    --display-name "${DISPLAY_NAME}" \
    --level "${LEVEL}" \
    --teams "${TEAMS_CSV:-}" \
    --workers "${WORKERS_CSV:-}" \
    --note "${NOTE:-}"

# ============================================================
# Step 4: Send welcome email
# ============================================================
EMAIL_SENT=false
if [ -n "${EMAIL}" ] && [ -n "${HICLAW_SMTP_HOST:-}" ]; then
    log "Step 4: Sending welcome email to ${EMAIL}..."

    ELEMENT_URL="${HICLAW_ELEMENT_URL:-http://localhost:18080}"

    EMAIL_BODY="Hi ${DISPLAY_NAME},

Your HiClaw account has been created:

  Username: ${MATRIX_ID}
  Password: ${HUMAN_PASSWORD}
  Login URL: ${ELEMENT_URL}

Please log in using Element Web and change your password immediately.

— HiClaw"

    # Try sending via msmtp or sendmail
    if command -v msmtp > /dev/null 2>&1; then
        echo -e "Subject: Welcome to HiClaw - Your Account Details\nFrom: ${HICLAW_SMTP_FROM:-noreply@hiclaw.io}\nTo: ${EMAIL}\n\n${EMAIL_BODY}" | \
            msmtp --host="${HICLAW_SMTP_HOST}" --port="${HICLAW_SMTP_PORT:-465}" \
                  --auth=on --user="${HICLAW_SMTP_USER}" --password="${HICLAW_SMTP_PASS}" \
                  --tls=on --from="${HICLAW_SMTP_FROM:-noreply@hiclaw.io}" \
                  "${EMAIL}" 2>/dev/null && EMAIL_SENT=true
    elif command -v sendmail > /dev/null 2>&1; then
        echo -e "Subject: Welcome to HiClaw - Your Account Details\nFrom: ${HICLAW_SMTP_FROM:-noreply@hiclaw.io}\nTo: ${EMAIL}\n\n${EMAIL_BODY}" | \
            sendmail "${EMAIL}" 2>/dev/null && EMAIL_SENT=true
    fi

    if [ "${EMAIL_SENT}" = true ]; then
        log "  Welcome email sent to ${EMAIL}"
    else
        log "  WARNING: Failed to send email (SMTP may not be configured)"
    fi
elif [ -n "${EMAIL}" ]; then
    log "Step 4: Skipped email (HICLAW_SMTP_HOST not configured)"
else
    log "Step 4: Skipped email (no --email provided)"
fi

# ============================================================
# Output JSON result
# ============================================================
ROOMS_JSON="[]"
for r in "${ROOMS_INVITED[@]}"; do
    ROOMS_JSON=$(echo "${ROOMS_JSON}" | jq --arg r "$r" '. += [$r]')
done

RESULT=$(jq -n \
    --arg name "${HUMAN_USERNAME}" \
    --arg matrix_id "${MATRIX_ID}" \
    --arg display_name "${DISPLAY_NAME}" \
    --argjson level "${LEVEL}" \
    --arg email "${EMAIL:-}" \
    --argjson email_sent "${EMAIL_SENT}" \
    --arg password "${HUMAN_PASSWORD}" \
    --argjson rooms "${ROOMS_JSON}" \
    '{
        human_name: $name,
        matrix_user_id: $matrix_id,
        display_name: $display_name,
        permission_level: $level,
        password: $password,
        email: (if $email == "" then null else $email end),
        email_sent: $email_sent,
        rooms_invited: $rooms
    }')

echo "---RESULT---"
echo "${RESULT}"
