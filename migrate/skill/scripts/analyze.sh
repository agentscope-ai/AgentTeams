#!/bin/bash
# analyze.sh - Analyze current OpenClaw environment for HiClaw migration
#
# Scans the OpenClaw state directory, workspace files, skills, cron jobs,
# and system tool dependencies to produce a tool-analysis.json report.
#
# Usage:
#   analyze.sh [--state-dir <path>] [--output <dir>]

set -e

# ============================================================
# Defaults
# ============================================================
STATE_DIR="${HOME}/.openclaw"
OUTPUT_DIR="/tmp/hiclaw-migration"

while [ $# -gt 0 ]; do
    case "$1" in
        --state-dir) STATE_DIR="$2"; shift 2 ;;
        --output)    OUTPUT_DIR="$2"; shift 2 ;;
        *)           echo "Unknown option: $1"; exit 1 ;;
    esac
done

mkdir -p "${OUTPUT_DIR}"

# ============================================================
# Helpers
# ============================================================
log() { echo "[hiclaw-import $(date '+%H:%M:%S')] $1"; }

# Known commands that are pre-installed in the HiClaw worker base image
# (from openclaw-base Dockerfile: git, python3, make, g++, curl, jq, nginx,
#  gettext-base, openssh-client, ca-certificates, procps, tzdata, nodejs, pnpm, npm)
BUILTIN_CMDS="bash sh cat ls cp mv rm mkdir rmdir chmod chown ln touch head tail tee wc sort uniq tr cut paste comm diff grep sed awk find xargs echo printf date sleep test expr env export read source true false cd pwd which whoami hostname uname id groups stat file du df free top ps kill pkill pgrep nohup timeout nice tar gzip gunzip bzip2 xz zip unzip less more strings od xxd base64 md5sum sha256sum openssl ssh ssh-keygen scp git python3 make g++ gcc curl wget jq nginx envsubst node npm npx pnpm mc openclaw mcporter skills"

is_builtin_cmd() {
    local cmd="$1"
    echo " ${BUILTIN_CMDS} " | grep -qw "${cmd}"
}

# ============================================================
# Step 1: Detect OpenClaw state directory
# ============================================================
log "Analyzing OpenClaw state directory: ${STATE_DIR}"

if [ ! -d "${STATE_DIR}" ]; then
    echo "ERROR: State directory not found: ${STATE_DIR}"
    echo "Try: analyze.sh --state-dir /path/to/.openclaw"
    exit 1
fi

CONFIG_FILE="${STATE_DIR}/openclaw.json"
if [ ! -f "${CONFIG_FILE}" ]; then
    echo "ERROR: openclaw.json not found in ${STATE_DIR}"
    exit 1
fi

# Detect workspace directory from config
WORKSPACE_DIR=""
if command -v jq &>/dev/null; then
    WORKSPACE_DIR=$(jq -r '.agents.defaults.workspace // empty' "${CONFIG_FILE}" 2>/dev/null || true)
fi
if [ -z "${WORKSPACE_DIR}" ]; then
    WORKSPACE_DIR="${STATE_DIR}/workspace"
fi
log "Workspace directory: ${WORKSPACE_DIR}"

# ============================================================
# Step 2: Scan skill scripts for command usage
# ============================================================
log "Step 2: Scanning skill scripts for tool dependencies..."

SKILL_CMDS_FILE="${OUTPUT_DIR}/skill-commands.txt"
> "${SKILL_CMDS_FILE}"

# Scan workspace skills directory
SKILLS_DIRS=()
if [ -d "${WORKSPACE_DIR}/skills" ]; then
    SKILLS_DIRS+=("${WORKSPACE_DIR}/skills")
fi
if [ -d "${STATE_DIR}/extensions/skills" ]; then
    SKILLS_DIRS+=("${STATE_DIR}/extensions/skills")
fi

for skills_dir in "${SKILLS_DIRS[@]}"; do
    log "  Scanning: ${skills_dir}"
    # Find all shell and python scripts
    find "${skills_dir}" -type f \( -name "*.sh" -o -name "*.py" -o -name "*.bash" \) 2>/dev/null | while read -r script; do
        # Extract command names from shell scripts
        if [[ "${script}" == *.sh ]] || [[ "${script}" == *.bash ]]; then
            # Look for command invocations: lines starting with a command, or after pipe/semicolon/&&/||
            grep -oE '^\s*[a-zA-Z_][a-zA-Z0-9_-]*' "${script}" 2>/dev/null | sed 's/^[[:space:]]*//' >> "${SKILL_CMDS_FILE}" || true
            grep -oE '\|\s*[a-zA-Z_][a-zA-Z0-9_-]*' "${script}" 2>/dev/null | sed 's/^|[[:space:]]*//' >> "${SKILL_CMDS_FILE}" || true
            # Look for $(command) and `command` patterns
            grep -oE '\$\([a-zA-Z_][a-zA-Z0-9_-]*' "${script}" 2>/dev/null | sed 's/^\$(//' >> "${SKILL_CMDS_FILE}" || true
        fi
        # Extract imports from python scripts
        if [[ "${script}" == *.py ]]; then
            grep -oE '^\s*(import|from)\s+[a-zA-Z_][a-zA-Z0-9_]*' "${script}" 2>/dev/null | \
                sed 's/^\s*import\s\+//; s/^\s*from\s\+//' >> "${OUTPUT_DIR}/python-imports.txt" || true
        fi
    done
done

# ============================================================
# Step 3: Analyze shell history
# ============================================================
log "Step 3: Analyzing shell history..."

HISTORY_CMDS_FILE="${OUTPUT_DIR}/history-commands.txt"
> "${HISTORY_CMDS_FILE}"

for hist_file in "${HOME}/.bash_history" "${HOME}/.zsh_history"; do
    if [ -f "${hist_file}" ]; then
        log "  Reading: ${hist_file}"
        # Extract first word of each command (the command name)
        # zsh history has `: timestamp:0;command` format
        sed 's/^: [0-9]*:[0-9]*;//' "${hist_file}" 2>/dev/null | \
            grep -oE '^\s*[a-zA-Z_][a-zA-Z0-9_.-]*' | \
            sed 's/^[[:space:]]*//' >> "${HISTORY_CMDS_FILE}" || true
    fi
done

# ============================================================
# Step 4: Analyze cron job payloads
# ============================================================
log "Step 4: Analyzing cron job payloads..."

CRON_CMDS_FILE="${OUTPUT_DIR}/cron-commands.txt"
> "${CRON_CMDS_FILE}"

CRON_FILE="${STATE_DIR}/cron/jobs.json"
if [ -f "${CRON_FILE}" ] && command -v jq &>/dev/null; then
    # Extract text from agentTurn payloads and scan for command references
    jq -r '.[].payload.agentTurn.parts[]?.text // empty' "${CRON_FILE}" 2>/dev/null | \
        grep -oE '`[a-zA-Z_][a-zA-Z0-9_-]*`' | tr -d '`' >> "${CRON_CMDS_FILE}" || true
    jq -r '.[].payload.agentTurn.parts[]?.text // empty' "${CRON_FILE}" 2>/dev/null | \
        grep -oE '^\s*[a-zA-Z_][a-zA-Z0-9_-]*' | sed 's/^[[:space:]]*//' >> "${CRON_CMDS_FILE}" || true
fi

# ============================================================
# Step 5: Scan AGENTS.md code blocks
# ============================================================
log "Step 5: Scanning AGENTS.md code blocks..."

AGENTS_CMDS_FILE="${OUTPUT_DIR}/agents-commands.txt"
> "${AGENTS_CMDS_FILE}"

AGENTS_MD="${WORKSPACE_DIR}/AGENTS.md"
if [ -f "${AGENTS_MD}" ]; then
    # Extract commands from code blocks (```bash ... ```)
    awk '/^```(bash|sh|shell)?$/,/^```$/' "${AGENTS_MD}" 2>/dev/null | \
        grep -v '^```' | \
        grep -oE '^\s*[a-zA-Z_][a-zA-Z0-9_.-]*' | \
        sed 's/^[[:space:]]*//' >> "${AGENTS_CMDS_FILE}" || true
fi

# Also scan TOOLS.md if present
TOOLS_MD="${WORKSPACE_DIR}/TOOLS.md"
if [ -f "${TOOLS_MD}" ]; then
    awk '/^```(bash|sh|shell)?$/,/^```$/' "${TOOLS_MD}" 2>/dev/null | \
        grep -v '^```' | \
        grep -oE '^\s*[a-zA-Z_][a-zA-Z0-9_.-]*' | \
        sed 's/^[[:space:]]*//' >> "${AGENTS_CMDS_FILE}" || true
fi

# ============================================================
# Step 6: Aggregate and classify commands
# ============================================================
log "Step 6: Aggregating and classifying tool dependencies..."

ALL_CMDS_FILE="${OUTPUT_DIR}/all-commands-raw.txt"
cat "${SKILL_CMDS_FILE}" "${HISTORY_CMDS_FILE}" "${CRON_CMDS_FILE}" "${AGENTS_CMDS_FILE}" 2>/dev/null | \
    sort | uniq -c | sort -rn > "${ALL_CMDS_FILE}"

# Filter: keep only commands that exist on the system but are NOT in the base image
APT_PACKAGES=()
PIP_PACKAGES=()
NPM_PACKAGES=()
UNKNOWN_BINARIES=()

# Track sources for each command
declare -A CMD_SOURCES

while read -r count cmd; do
    [ -z "${cmd}" ] && continue
    # Skip shell builtins and keywords
    case "${cmd}" in
        if|then|else|elif|fi|for|do|done|while|until|case|esac|function|return|exit|local|export|declare|readonly|set|unset|shift|break|continue|trap|eval|exec|source|true|false|in|select) continue ;;
    esac
    # Skip if it's a builtin command in the worker image
    if is_builtin_cmd "${cmd}"; then
        continue
    fi
    # Check if the command exists on this system
    if command -v "${cmd}" &>/dev/null; then
        cmd_path=$(command -v "${cmd}" 2>/dev/null)
        # Try to find which apt package provides it
        if command -v dpkg &>/dev/null; then
            pkg=$(dpkg -S "${cmd_path}" 2>/dev/null | head -1 | cut -d: -f1 || true)
            if [ -n "${pkg}" ]; then
                # Deduplicate
                if ! printf '%s\n' "${APT_PACKAGES[@]}" | grep -qx "${pkg}" 2>/dev/null; then
                    APT_PACKAGES+=("${pkg}")
                fi
                continue
            fi
        fi
        # Check if it's an npm global package
        if [[ "${cmd_path}" == */node_modules/* ]] || [[ "${cmd_path}" == */npm/* ]]; then
            if ! printf '%s\n' "${NPM_PACKAGES[@]}" | grep -qx "${cmd}" 2>/dev/null; then
                NPM_PACKAGES+=("${cmd}")
            fi
            continue
        fi
        # Check if it's a pip-installed command
        if [[ "${cmd_path}" == */pip* ]] || [[ "${cmd_path}" == */.local/bin/* ]] || [[ "${cmd_path}" == */python* ]]; then
            if ! printf '%s\n' "${PIP_PACKAGES[@]}" | grep -qx "${cmd}" 2>/dev/null; then
                PIP_PACKAGES+=("${cmd}")
            fi
            continue
        fi
        # Unknown binary
        if ! printf '%s\n' "${UNKNOWN_BINARIES[@]}" | grep -qx "${cmd}" 2>/dev/null; then
            UNKNOWN_BINARIES+=("${cmd}")
        fi
    fi
done < <(awk '{print $1, $2}' "${ALL_CMDS_FILE}")

# Also check python imports for pip packages
if [ -f "${OUTPUT_DIR}/python-imports.txt" ]; then
    while read -r mod; do
        [ -z "${mod}" ] && continue
        # Common stdlib modules to skip
        case "${mod}" in
            os|sys|re|json|math|time|datetime|collections|itertools|functools|pathlib|subprocess|shutil|tempfile|io|typing|abc|enum|dataclasses|argparse|logging|unittest|copy|glob|hashlib|base64|urllib|http|socket|threading|multiprocessing|signal|struct|csv|xml|html|email|string|textwrap|random|secrets|uuid|pprint|traceback|inspect|contextlib|operator|bisect|heapq|array|queue|weakref|types|importlib|pkgutil|platform|ctypes|codecs|locale|gettext|unicodedata|difflib|fnmatch|stat|posixpath|ntpath|linecache|tokenize|keyword|ast|dis|compileall|zipfile|tarfile|gzip|bz2|lzma|zlib|configparser|tomllib|sqlite3|dbm|pickle|shelve|marshal|warnings|atexit|gc|resource|sysconfig|builtins|__future__) continue ;;
        esac
        if ! printf '%s\n' "${PIP_PACKAGES[@]}" | grep -qx "${mod}" 2>/dev/null; then
            PIP_PACKAGES+=("${mod}")
        fi
    done < "${OUTPUT_DIR}/python-imports.txt"
fi

# ============================================================
# Step 7: Generate tool-analysis.json
# ============================================================
log "Step 7: Generating tool-analysis.json..."

# Build JSON using jq
APT_JSON=$(printf '%s\n' "${APT_PACKAGES[@]}" 2>/dev/null | jq -R . | jq -s . 2>/dev/null || echo '[]')
PIP_JSON=$(printf '%s\n' "${PIP_PACKAGES[@]}" 2>/dev/null | jq -R . | jq -s . 2>/dev/null || echo '[]')
NPM_JSON=$(printf '%s\n' "${NPM_PACKAGES[@]}" 2>/dev/null | jq -R . | jq -s . 2>/dev/null || echo '[]')
UNKNOWN_JSON=$(printf '%s\n' "${UNKNOWN_BINARIES[@]}" 2>/dev/null | jq -R . | jq -s . 2>/dev/null || echo '[]')

# Count commands per source
SKILL_COUNT=$(wc -l < "${SKILL_CMDS_FILE}" 2>/dev/null | tr -d ' ')
HISTORY_COUNT=$(wc -l < "${HISTORY_CMDS_FILE}" 2>/dev/null | tr -d ' ')
CRON_COUNT=$(wc -l < "${CRON_CMDS_FILE}" 2>/dev/null | tr -d ' ')
AGENTS_COUNT=$(wc -l < "${AGENTS_CMDS_FILE}" 2>/dev/null | tr -d ' ')

jq -n \
    --argjson apt "${APT_JSON}" \
    --argjson pip "${PIP_JSON}" \
    --argjson npm "${NPM_JSON}" \
    --argjson unknown "${UNKNOWN_JSON}" \
    --arg skill_count "${SKILL_COUNT}" \
    --arg history_count "${HISTORY_COUNT}" \
    --arg cron_count "${CRON_COUNT}" \
    --arg agents_count "${AGENTS_COUNT}" \
    '{
        "apt_packages": $apt,
        "pip_packages": $pip,
        "npm_packages": $npm,
        "unknown_binaries": $unknown,
        "analysis_sources": {
            "skill_scripts_commands": ($skill_count | tonumber),
            "shell_history_commands": ($history_count | tonumber),
            "cron_payload_commands": ($cron_count | tonumber),
            "agents_md_commands": ($agents_count | tonumber)
        }
    }' > "${OUTPUT_DIR}/tool-analysis.json"

log "Analysis complete!"
log "  APT packages: ${#APT_PACKAGES[@]}"
log "  pip packages: ${#PIP_PACKAGES[@]}"
log "  npm packages: ${#NPM_PACKAGES[@]}"
log "  Unknown binaries: ${#UNKNOWN_BINARIES[@]}"
log "  Output: ${OUTPUT_DIR}/tool-analysis.json"

# Clean up intermediate files
rm -f "${SKILL_CMDS_FILE}" "${HISTORY_CMDS_FILE}" "${CRON_CMDS_FILE}" "${AGENTS_CMDS_FILE}" "${ALL_CMDS_FILE}" "${OUTPUT_DIR}/python-imports.txt"

cat "${OUTPUT_DIR}/tool-analysis.json"
