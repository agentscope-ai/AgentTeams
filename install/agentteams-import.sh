#!/bin/bash
# agentteams-import.sh - Import Worker/Team/Human resources into AgentTeams
#
# Thin shell that delegates to the `agt` CLI inside the Manager container.
# Supports ZIP packages, remote packages (nacos://, http://), and YAML files.
#
# Usage:
#   ./agentteams-import.sh worker --name <name> --zip <path-or-url> [--yes]
#   ./agentteams-import.sh worker --name <name> --package <nacos://...> [--model MODEL]
#   ./agentteams-import.sh worker --name <name>                         # auto-imports package <name>
#   ./agentteams-import.sh worker --name <name> --model MODEL [--skills s1,s2] [--mcp-servers m1,m2]
#   ./agentteams-import.sh -f <resource.yaml> [--prune] [--dry-run]
#
# Environment variables (for automation):
#   AGENTTEAMS_NON_INTERACTIVE       Skip all prompts (same as --yes)

set -e

# ============================================================
# Detect container runtime
# ============================================================
CONTAINER_CMD=""
if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
    CONTAINER_CMD="docker"
elif command -v podman &>/dev/null && podman info &>/dev/null 2>&1; then
    CONTAINER_CMD="podman"
fi
if [ -z "${CONTAINER_CMD}" ]; then
    echo "ERROR: Neither docker nor podman found." >&2
    echo "" >&2
    echo "Docker is required to run AgentTeams. Install Docker first, then install AgentTeams:" >&2
    echo "  bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)" >&2
    exit 1
fi

# Verify Manager container
if ! ${CONTAINER_CMD} ps --filter name=agentteams-manager --format '{{.Names}}' 2>/dev/null | grep -q 'agentteams-manager'; then
    echo "ERROR: agentteams-manager container is not running." >&2
    echo "" >&2
    # Check if the container exists but is stopped
    if ${CONTAINER_CMD} ps -a --filter name=agentteams-manager --format '{{.Names}}' 2>/dev/null | grep -q 'agentteams-manager'; then
        echo "The agentteams-manager container exists but is stopped. Start it with:" >&2
        echo "  ${CONTAINER_CMD} start agentteams-manager" >&2
    else
        echo "AgentTeams does not appear to be installed. Install it first:" >&2
        echo "  bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)" >&2
    fi
    exit 1
fi

# Ensure /tmp/import exists in container
${CONTAINER_CMD} exec agentteams-manager mkdir -p /tmp/import 2>/dev/null || true

# ============================================================
# Parse first argument to determine mode
# ============================================================

# YAML mode: -f / --file
if [ "${1}" = "-f" ] || [ "${1}" = "--file" ]; then
    SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    exec bash "${SCRIPT_DIR}/agentteams-apply.sh" "$@"
fi

# Resource subcommand mode: worker / team / human
RESOURCE_TYPE="${1:-}"
shift 2>/dev/null || true

case "${RESOURCE_TYPE}" in
    worker)
        # Parse worker-specific arguments
        AGENTTEAMS_ARGS=("apply" "worker")
        ZIP_FILE=""
        WORKER_NAME=""
        PACKAGE_URI=""
        while [ $# -gt 0 ]; do
            case "$1" in
                --zip)
                    ZIP_FILE="$2"; shift 2 ;;
                --name)
                    WORKER_NAME="$2"
                    AGENTTEAMS_ARGS+=("$1" "$2")
                    shift 2 ;;
                --package)
                    PACKAGE_URI="$2"
                    AGENTTEAMS_ARGS+=("$1" "$2")
                    shift 2 ;;
                --model|--skills|--mcp-servers|--runtime)
                    AGENTTEAMS_ARGS+=("$1" "$2"); shift 2 ;;
                --dry-run)
                    AGENTTEAMS_ARGS+=("$1"); shift ;;
                --yes)
                    shift ;;
                *) echo "Unknown option: $1"; exit 1 ;;
            esac
        done

        # Handle ZIP: download URL if needed, then docker cp into container
        if [ -n "${ZIP_FILE}" ]; then
            if echo "${ZIP_FILE}" | grep -qE '^https?://'; then
                echo "[AgentTeams Import] Downloading ${ZIP_FILE}..."
                DOWNLOADED_ZIP=$(mktemp /tmp/agentteams-import-XXXXXX.zip)
                curl -fSL -o "${DOWNLOADED_ZIP}" "${ZIP_FILE}" || { echo "ERROR: Download failed"; exit 1; }
                ZIP_FILE="${DOWNLOADED_ZIP}"
                trap 'rm -f "${DOWNLOADED_ZIP}"' EXIT
            fi

            ZIP_BASENAME=$(basename "${ZIP_FILE}")
            ${CONTAINER_CMD} cp "${ZIP_FILE}" "agentteams-manager:/tmp/import/${ZIP_BASENAME}"
            echo "[AgentTeams Import] Copied ${ZIP_BASENAME} → container:/tmp/import/"
            AGENTTEAMS_ARGS+=("--zip" "/tmp/import/${ZIP_BASENAME}")
        fi

        if [ -z "${ZIP_FILE}" ] && [ -n "${WORKER_NAME}" ] && [ -z "${PACKAGE_URI}" ]; then
            AGENTTEAMS_ARGS+=("--package" "${WORKER_NAME}")
        fi

        # `agentteams-import.sh` accepts `--yes` for backward compatibility, but the
        # container-internal `agt apply worker` CLI does not support it.
        # Swallow the flag here instead of forwarding it and breaking imports.
        exec ${CONTAINER_CMD} exec agentteams-manager agt "${AGENTTEAMS_ARGS[@]}"
        ;;

    -h|--help|"")
        echo "Usage:"
        echo "  $0 worker --name <name> --zip <path-or-url>"
        echo "  $0 worker --name <name> --package <nacos://...> [--model MODEL]"
        echo "  $0 worker --name <name>                         # auto-import package <name>"
        echo "  $0 worker --name <name> --model MODEL [--skills s1,s2] [--mcp-servers m1,m2]"
        echo "  $0 -f <resource.yaml> [--prune] [--dry-run]"
        exit 0
        ;;

    *)
        echo "Unknown resource type: ${RESOURCE_TYPE}"
        echo "Supported: worker"
        echo "For YAML mode: $0 -f <resource.yaml>"
        exit 1
        ;;
esac
