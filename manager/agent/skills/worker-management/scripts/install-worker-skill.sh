#!/bin/bash
# install-worker-skill.sh - Safely import an attached Worker Skill and assign it.

set -euo pipefail

WORKER_NAME=""
ARCHIVE_PATH=""
REPLACE=false
NOTIFY=true

usage() {
    echo "Usage: $0 --worker NAME --archive FILE.zip [--replace] [--no-notify]" >&2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --worker) WORKER_NAME="${2:-}"; shift 2 ;;
        --archive) ARCHIVE_PATH="${2:-}"; shift 2 ;;
        --replace) REPLACE=true; shift ;;
        --no-notify) NOTIFY=false; shift ;;
        *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

if [ -z "${WORKER_NAME}" ] || [ -z "${ARCHIVE_PATH}" ]; then
    usage
    exit 2
fi
if [ ! -f "${ARCHIVE_PATH}" ]; then
    echo "Worker Skill archive not found: ${ARCHIVE_PATH}" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXTRACTOR="${SCRIPT_DIR}/safe-extract-worker-skill.py"
PUSH_SCRIPT="${SCRIPT_DIR}/push-worker-skills.sh"
WORK_DIR="$(mktemp -d)"
STAGED_DIR=""
BACKUP_DIR=""

cleanup() {
    rm -rf "${WORK_DIR}"
    if [ -n "${STAGED_DIR}" ] && [ -d "${STAGED_DIR}" ]; then
        rm -rf "${STAGED_DIR}"
    fi
    if [ -n "${BACKUP_DIR}" ] && [ -d "${BACKUP_DIR}" ]; then
        rm -rf "${BACKUP_DIR}"
    fi
}
trap cleanup EXIT

metadata=$(python3 "${EXTRACTOR}" \
    --archive "${ARCHIVE_PATH}" \
    --output-dir "${WORK_DIR}/extracted")
skill_name=$(echo "${metadata}" | jq -r '.name')
skill_root=$(echo "${metadata}" | jq -r '.skillRoot')
has_assign_when=$(echo "${metadata}" | jq -r '.hasAssignWhen')

worker_json=$(agt get workers "${WORKER_NAME}" -o json)
if ! echo "${worker_json}" | jq -e --arg worker "${WORKER_NAME}" \
    '(.name // .workerName // "") == $worker' >/dev/null; then
    echo "Worker not found: ${WORKER_NAME}" >&2
    exit 1
fi

skill_parent="${HOME}/worker-skills"
target_dir="${skill_parent}/${skill_name}"
mkdir -p "${skill_parent}"

if [ -e "${target_dir}" ] && [ "${REPLACE}" != true ]; then
    echo "Worker Skill already exists: ${target_dir} (use --replace to update it)" >&2
    exit 1
fi

STAGED_DIR="${skill_parent}/.${skill_name}.install.$$"
mkdir -p "${STAGED_DIR}"
cp -a "${skill_root}/." "${STAGED_DIR}/"

if [ "${REPLACE}" = true ] && [ -e "${target_dir}" ]; then
    BACKUP_DIR="${skill_parent}/.${skill_name}.backup.$$"
    mv "${target_dir}" "${BACKUP_DIR}"
    if ! mv "${STAGED_DIR}" "${target_dir}"; then
        mv "${BACKUP_DIR}" "${target_dir}"
        BACKUP_DIR=""
        exit 1
    fi
    STAGED_DIR=""
else
    mv "${STAGED_DIR}" "${target_dir}"
    STAGED_DIR=""
fi

push_args=(--worker "${WORKER_NAME}" --add-skill "${skill_name}")
if [ "${NOTIFY}" = false ]; then
    push_args+=(--no-notify)
fi
if ! push_output=$(bash "${PUSH_SCRIPT}" "${push_args[@]}" 2>&1); then
    # A failed replacement must restore the previous canonical version even
    # when the Worker CR already lists the Skill. For a first-time install,
    # preserve the files if the CR accepted the assignment so reconciliation
    # never points at a missing definition.
    assigned_after_failure=false
    rollback_failed=false
    rollback_output=""
    if worker_after_failure=$(agt get workers "${WORKER_NAME}" -o json 2>/dev/null); then
        if echo "${worker_after_failure}" | jq -e --arg skill "${skill_name}" \
            '(.skills // []) | index($skill)' >/dev/null; then
            assigned_after_failure=true
        fi
    fi
    if [ -n "${BACKUP_DIR}" ] && [ -d "${BACKUP_DIR}" ]; then
        rm -rf "${target_dir}"
        mv "${BACKUP_DIR}" "${target_dir}"
        BACKUP_DIR=""
        if ! rollback_output=$(bash "${PUSH_SCRIPT}" \
            --worker "${WORKER_NAME}" --skill "${skill_name}" --no-notify 2>&1); then
            rollback_failed=true
        fi
    elif [ "${assigned_after_failure}" != true ]; then
        rm -rf "${target_dir}"
    fi
    echo "${push_output}" >&2
    if [ "${rollback_failed}" = true ]; then
        echo "Worker Skill remote rollback failed for ${skill_name}; canonical files were restored locally but Worker storage may be inconsistent:" >&2
        [ -z "${rollback_output}" ] || echo "${rollback_output}" >&2
    fi
    exit 1
fi
if [ -n "${push_output}" ]; then
    echo "${push_output}" >&2
fi
if [ -n "${BACKUP_DIR}" ] && [ -d "${BACKUP_DIR}" ]; then
    rm -rf "${BACKUP_DIR}"
    BACKUP_DIR=""
fi

jq -nc \
    --arg worker "${WORKER_NAME}" \
    --arg skill "${skill_name}" \
    --argjson has_assign_when "${has_assign_when}" \
    '{ok:true, worker:$worker, skill:$skill, assigned:true, hasAssignWhen:$has_assign_when}'
