#!/bin/bash
# generate-zip.sh - Generate a HiClaw migration ZIP package
#
# Takes the analysis results and OpenClaw config, produces a ZIP file
# that can be fed to hiclaw-import.sh on the HiClaw host.
#
# Usage:
#   generate-zip.sh --name <worker-name> [--state-dir <path>] [--analysis <path>] [--output <dir>] [--base-image <image>]

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# ============================================================
# Defaults
# ============================================================
WORKER_NAME=""
STATE_DIR="${HOME}/.openclaw"
ANALYSIS_FILE=""
OUTPUT_DIR="/tmp/hiclaw-migration"
BASE_IMAGE="hiclaw/worker-agent:latest"

while [ $# -gt 0 ]; do
    case "$1" in
        --name)       WORKER_NAME="$2"; shift 2 ;;
        --state-dir)  STATE_DIR="$2"; shift 2 ;;
        --analysis)   ANALYSIS_FILE="$2"; shift 2 ;;
        --output)     OUTPUT_DIR="$2"; shift 2 ;;
        --base-image) BASE_IMAGE="$2"; shift 2 ;;
        *)            echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    WORKER_NAME=$(hostname -s 2>/dev/null | tr 'A-Z' 'a-z' | tr -cd 'a-z0-9-' || echo "migrated-worker")
fi

# Normalize name
WORKER_NAME=$(echo "${WORKER_NAME}" | tr 'A-Z' 'a-z' | tr -cd 'a-z0-9-')

mkdir -p "${OUTPUT_DIR}"

log() { echo "[hiclaw-import $(date '+%H:%M:%S')] $1"; }

# ============================================================
# Validate inputs
# ============================================================
if [ ! -d "${STATE_DIR}" ]; then
    echo "ERROR: State directory not found: ${STATE_DIR}"
    exit 1
fi

CONFIG_FILE="${STATE_DIR}/openclaw.json"
if [ ! -f "${CONFIG_FILE}" ]; then
    echo "ERROR: openclaw.json not found in ${STATE_DIR}"
    exit 1
fi

# Run analysis if not provided
if [ -z "${ANALYSIS_FILE}" ]; then
    ANALYSIS_FILE="${OUTPUT_DIR}/tool-analysis.json"
    if [ ! -f "${ANALYSIS_FILE}" ]; then
        log "Running analysis first..."
        bash "${SCRIPT_DIR}/analyze.sh" --state-dir "${STATE_DIR}" --output "${OUTPUT_DIR}"
    fi
fi

if [ ! -f "${ANALYSIS_FILE}" ]; then
    echo "ERROR: Analysis file not found: ${ANALYSIS_FILE}"
    exit 1
fi

# Detect workspace directory
WORKSPACE_DIR=""
if command -v jq &>/dev/null; then
    WORKSPACE_DIR=$(jq -r '.agents.defaults.workspace // empty' "${CONFIG_FILE}" 2>/dev/null || true)
fi
# Fallback: try grep if jq failed (e.g. JSON parse errors)
if [ -z "${WORKSPACE_DIR}" ]; then
    WORKSPACE_DIR=$(grep -oP '"workspace"\s*:\s*"\K[^"]+' "${CONFIG_FILE}" 2>/dev/null || true)
fi
if [ -z "${WORKSPACE_DIR}" ]; then
    WORKSPACE_DIR="${STATE_DIR}/workspace"
fi
log "Workspace directory: ${WORKSPACE_DIR}"

# ============================================================
# Prepare staging directory
# ============================================================
TIMESTAMP=$(date '+%Y%m%d-%H%M%S')
STAGING="${OUTPUT_DIR}/staging-${TIMESTAMP}"
mkdir -p "${STAGING}/config" "${STAGING}/skills" "${STAGING}/crons"

log "Staging migration package in: ${STAGING}"

# ============================================================
# Step 1: Generate manifest.json
# ============================================================
log "Step 1: Generating manifest.json..."

OPENCLAW_VERSION=$(jq -r '.meta.lastTouchedVersion // "unknown"' "${CONFIG_FILE}" 2>/dev/null || echo "unknown")
OS_INFO=$(uname -s -r 2>/dev/null || echo "unknown")
HOSTNAME_VAL=$(hostname 2>/dev/null || echo "unknown")

# Read package counts from analysis
APT_PKGS=$(jq -r '.apt_packages // []' "${ANALYSIS_FILE}")
PIP_PKGS=$(jq -r '.pip_packages // []' "${ANALYSIS_FILE}")
NPM_PKGS=$(jq -r '.npm_packages // []' "${ANALYSIS_FILE}")

# Detect if China timezone for proxy suggestion
PROXY_SUGGESTED=false
PROXY_REASON=""
TZ_CURRENT=$(cat /etc/timezone 2>/dev/null || readlink /etc/localtime 2>/dev/null | sed 's|.*/zoneinfo/||' || echo "")
case "${TZ_CURRENT}" in
    Asia/Shanghai|Asia/Chongqing|Asia/Harbin|Asia/Urumqi|Asia/Hong_Kong|Asia/Macau|Asia/Taipei)
        PROXY_SUGGESTED=true
        PROXY_REASON="Detected China timezone: ${TZ_CURRENT}"
        ;;
esac

jq -n \
    --arg version "1.0" \
    --arg oc_version "${OPENCLAW_VERSION}" \
    --arg hostname "${HOSTNAME_VAL}" \
    --arg os "${OS_INFO}" \
    --arg created_at "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    --arg name "${WORKER_NAME}" \
    --arg base_image "${BASE_IMAGE}" \
    --argjson apt_packages "${APT_PKGS}" \
    --argjson pip_packages "${PIP_PKGS}" \
    --argjson npm_packages "${NPM_PKGS}" \
    --argjson proxy_suggested "${PROXY_SUGGESTED}" \
    --arg proxy_reason "${PROXY_REASON}" \
    '{
        "version": $version,
        "source": {
            "openclaw_version": $oc_version,
            "hostname": $hostname,
            "os": $os,
            "created_at": $created_at
        },
        "worker": {
            "suggested_name": $name,
            "base_image": $base_image,
            "apt_packages": $apt_packages,
            "pip_packages": $pip_packages,
            "npm_packages": $npm_packages
        },
        "proxy": {
            "suggested": $proxy_suggested,
            "reason": $proxy_reason
        }
    }' > "${STAGING}/manifest.json"

# ============================================================
# Step 2: Generate Dockerfile
# ============================================================
log "Step 2: Generating Dockerfile..."

TEMPLATE="${SCRIPT_DIR}/templates/Dockerfile.tmpl"
if [ ! -f "${TEMPLATE}" ]; then
    echo "ERROR: Dockerfile template not found: ${TEMPLATE}"
    exit 1
fi

# Read packages from analysis
APT_LIST=$(jq -r '.apt_packages[]' "${ANALYSIS_FILE}" 2>/dev/null | tr '\n' ' ')
PIP_LIST=$(jq -r '.pip_packages[]' "${ANALYSIS_FILE}" 2>/dev/null | tr '\n' ' ')
NPM_LIST=$(jq -r '.npm_packages[]' "${ANALYSIS_FILE}" 2>/dev/null | tr '\n' ' ')

# Build Dockerfile from template
DOCKERFILE="${STAGING}/Dockerfile"

# Start with ARG and FROM
cat > "${DOCKERFILE}" <<DEOF
ARG BASE_IMAGE=${BASE_IMAGE}
FROM \${BASE_IMAGE}

ARG APT_MIRROR=mirrors.aliyun.com
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG http_proxy
ARG https_proxy

# Set proxy for build
ENV HTTP_PROXY=\${HTTP_PROXY} \\
    HTTPS_PROXY=\${HTTPS_PROXY} \\
    http_proxy=\${http_proxy} \\
    https_proxy=\${https_proxy}

# Configure APT mirror (China acceleration)
RUN if [ -n "\${APT_MIRROR}" ]; then \\
        sed -i "s|archive.ubuntu.com|\${APT_MIRROR}|g; s|security.ubuntu.com|\${APT_MIRROR}|g" \\
            /etc/apt/sources.list 2>/dev/null || true; \\
    fi
DEOF

# APT packages
if [ -n "${APT_LIST}" ] && [ "${APT_LIST}" != " " ]; then
    cat >> "${DOCKERFILE}" <<DEOF

# Install migrated system tools
RUN apt-get update && apt-get install -y \\
    ${APT_LIST}\\
    && rm -rf /var/lib/apt/lists/*
DEOF
fi

# pip packages
if [ -n "${PIP_LIST}" ] && [ "${PIP_LIST}" != " " ]; then
    cat >> "${DOCKERFILE}" <<DEOF

# Install Python packages
RUN pip3 install --no-cache-dir ${PIP_LIST}
DEOF
fi

# npm packages
if [ -n "${NPM_LIST}" ] && [ "${NPM_LIST}" != " " ]; then
    cat >> "${DOCKERFILE}" <<DEOF

# Install Node.js global packages
RUN npm install -g ${NPM_LIST}&& rm -rf /root/.npm
DEOF
fi

# Clear proxy
cat >> "${DOCKERFILE}" <<'DEOF'

# Clear proxy env vars (runtime should not use build-time proxy)
ENV HTTP_PROXY= \
    HTTPS_PROXY= \
    http_proxy= \
    https_proxy=
DEOF

log "  Dockerfile generated with: apt=[${APT_LIST}] pip=[${PIP_LIST}] npm=[${NPM_LIST}]"

# ============================================================
# Step 3: Copy SOUL.md
# ============================================================
log "Step 3: Copying workspace files..."

# SOUL.md
if [ -f "${WORKSPACE_DIR}/SOUL.md" ]; then
    cp "${WORKSPACE_DIR}/SOUL.md" "${STAGING}/config/SOUL.md"
    log "  Copied SOUL.md"
else
    # Generate a default SOUL.md
    cat > "${STAGING}/config/SOUL.md" <<SEOF
# ${WORKER_NAME} - Worker Agent

## AI Identity

**You are an AI Agent, not a human.**

- Both you and the Manager are AI agents that can work 24/7
- You do not need rest, sleep, or "off-hours"
- You can immediately start the next task after completing one
- Your time units are **minutes and hours**, not "days"

## Role
- Name: ${WORKER_NAME}
- Role: Migrated from standalone OpenClaw instance

## Security Rules
- Never reveal API keys, passwords, or credentials
- Only access files and tools necessary for your assigned tasks
SEOF
    log "  Generated default SOUL.md"
fi

# ============================================================
# Step 4: Generate adapted AGENTS.md
# ============================================================
log "Step 4: Generating adapted AGENTS.md..."

# The user-custom content goes AFTER the hiclaw-builtin-end marker.
# The migration script on the HiClaw host will handle injecting the builtin section.
# Here we just prepare the user content portion.

USER_AGENTS="${STAGING}/config/AGENTS.md"
{
    echo ""
    echo "# Migrated OpenClaw Configuration"
    echo ""
    echo "> This content was migrated from a standalone OpenClaw instance on $(date -u '+%Y-%m-%d')."
    echo ""

    # Copy original AGENTS.md content if it exists
    if [ -f "${WORKSPACE_DIR}/AGENTS.md" ]; then
        echo "## Original Agent Configuration"
        echo ""
        cat "${WORKSPACE_DIR}/AGENTS.md"
        echo ""
    fi

    # Copy TOOLS.md content if it exists
    if [ -f "${WORKSPACE_DIR}/TOOLS.md" ]; then
        echo "## Tools Configuration"
        echo ""
        cat "${WORKSPACE_DIR}/TOOLS.md"
        echo ""
    fi

    # Note about installed tools
    if [ -n "${APT_LIST}" ] || [ -n "${PIP_LIST}" ] || [ -n "${NPM_LIST}" ]; then
        echo "## Installed Tools (Custom Image)"
        echo ""
        echo "The following tools are installed in this Worker's custom Docker image:"
        echo ""
        if [ -n "${APT_LIST}" ] && [ "${APT_LIST}" != " " ]; then
            echo "- APT packages: \`${APT_LIST}\`"
        fi
        if [ -n "${PIP_LIST}" ] && [ "${PIP_LIST}" != " " ]; then
            echo "- pip packages: \`${PIP_LIST}\`"
        fi
        if [ -n "${NPM_LIST}" ] && [ "${NPM_LIST}" != " " ]; then
            echo "- npm packages: \`${NPM_LIST}\`"
        fi
        echo ""
    fi
} > "${USER_AGENTS}"

log "  AGENTS.md user content generated"

# ============================================================
# Step 5: Copy MEMORY.md and memory files
# ============================================================
log "Step 5: Copying memory files..."

if [ -f "${WORKSPACE_DIR}/MEMORY.md" ]; then
    cp "${WORKSPACE_DIR}/MEMORY.md" "${STAGING}/config/MEMORY.md"
    log "  Copied MEMORY.md"
fi

if [ -d "${WORKSPACE_DIR}/memory" ]; then
    mkdir -p "${STAGING}/config/memory"
    # Copy markdown memory files (skip sqlite databases which are session-specific)
    find "${WORKSPACE_DIR}/memory" -name "*.md" -exec cp {} "${STAGING}/config/memory/" \; 2>/dev/null || true
    MEMORY_COUNT=$(find "${STAGING}/config/memory" -name "*.md" 2>/dev/null | wc -l | tr -d ' ')
    log "  Copied ${MEMORY_COUNT} memory files"
fi

# ============================================================
# Step 6: Copy custom skills
# ============================================================
log "Step 6: Copying custom skills..."

SKILL_COUNT=0
for skills_dir in "${WORKSPACE_DIR}/skills" "${STATE_DIR}/extensions/skills"; do
    if [ -d "${skills_dir}" ]; then
        for skill_dir in "${skills_dir}"/*/; do
            [ -d "${skill_dir}" ] || continue
            # Check if this is a skill directory (has SKILL.md) or a sub-group (e.g. skills/public/)
            if [ -f "${skill_dir}SKILL.md" ]; then
                skill_name=$(basename "${skill_dir}")
                # Skip HiClaw built-in skills
                case "${skill_name}" in
                    file-sync|mcporter|find-skills) continue ;;
                esac
                mkdir -p "${STAGING}/skills/${skill_name}"
                cp -r "${skill_dir}"* "${STAGING}/skills/${skill_name}/" 2>/dev/null || true
                SKILL_COUNT=$((SKILL_COUNT + 1))
            else
                # Nested skill group (e.g. skills/public/*/), recurse one level
                for nested_dir in "${skill_dir}"*/; do
                    [ -d "${nested_dir}" ] || continue
                    [ -f "${nested_dir}SKILL.md" ] || continue
                    skill_name=$(basename "${nested_dir}")
                    case "${skill_name}" in
                        file-sync|mcporter|find-skills) continue ;;
                    esac
                    mkdir -p "${STAGING}/skills/${skill_name}"
                    cp -r "${nested_dir}"* "${STAGING}/skills/${skill_name}/" 2>/dev/null || true
                    SKILL_COUNT=$((SKILL_COUNT + 1))
                done
            fi
        done
    fi
done
log "  Copied ${SKILL_COUNT} custom skills"

# ============================================================
# Step 7: Adapt cron jobs
# ============================================================
log "Step 7: Adapting cron jobs..."

CRON_FILE="${STATE_DIR}/cron/jobs.json"
if [ -f "${CRON_FILE}" ] && command -v jq &>/dev/null; then
    # The cron file is {"version":1,"jobs":[...]}, extract the jobs array
    # Try .jobs[] first (new format), fallback to .[] (legacy format)
    JOBS_COUNT=$(jq -r 'if .jobs then (.jobs | length) else length end' "${CRON_FILE}" 2>/dev/null || echo "0")
    if [ "${JOBS_COUNT}" != "0" ] && [ -n "${JOBS_COUNT}" ]; then
        # Remove channel-specific delivery config (Discord/Slack), keep schedule and payload
        jq 'if .jobs then .jobs else . end | [.[] | {
            id: .id,
            name: .name,
            description: .description,
            schedule: .schedule,
            payload: {
                agentTurn: .payload.agentTurn
            },
            state: {
                enabled: (if .state.enabled == false then false else true end)
            }
        } | del(.payload.agentTurn | nulls)]' "${CRON_FILE}" > "${STAGING}/crons/jobs.json" 2>/dev/null || echo "[]" > "${STAGING}/crons/jobs.json"
        CRON_COUNT=$(jq 'length' "${STAGING}/crons/jobs.json" 2>/dev/null || echo "0")
        log "  Adapted ${CRON_COUNT} cron jobs"
    else
        echo "[]" > "${STAGING}/crons/jobs.json"
        log "  No cron jobs found"
    fi
else
    echo "[]" > "${STAGING}/crons/jobs.json"
    log "  No cron jobs found"
fi

# ============================================================
# Step 8: Copy tool analysis report
# ============================================================
cp "${ANALYSIS_FILE}" "${STAGING}/tool-analysis.json"

# ============================================================
# Step 9: Create ZIP
# ============================================================
log "Step 9: Creating ZIP package..."

ZIP_NAME="migration-${WORKER_NAME}-${TIMESTAMP}.zip"
ZIP_PATH="${OUTPUT_DIR}/${ZIP_NAME}"

(cd "${STAGING}" && zip -r "${ZIP_PATH}" . -x "*.DS_Store")

# Clean up staging
rm -rf "${STAGING}"

ZIP_SIZE=$(du -h "${ZIP_PATH}" 2>/dev/null | cut -f1)
log ""
log "=========================================="
log "Migration package created successfully!"
log "=========================================="
log "  File: ${ZIP_PATH}"
log "  Size: ${ZIP_SIZE}"
log "  Worker name: ${WORKER_NAME}"
log ""
log "Transfer this file to the HiClaw Manager host and run:"
log "  bash hiclaw-import.sh --zip ${ZIP_NAME}"
log ""

echo "${ZIP_PATH}"
