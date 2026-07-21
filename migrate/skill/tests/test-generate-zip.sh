#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GENERATOR="${GENERATOR:-${SCRIPT_DIR}/../scripts/generate-zip.sh}"

for command in jq zip; do
    if ! command -v "${command}" >/dev/null 2>&1; then
        echo "SKIP: ${command} is required"
        exit 0
    fi
done

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

STATE_DIR="${TMP_DIR}/state"
WORKSPACE_DIR="${STATE_DIR}/workspace"
OUTPUT_DIR="${TMP_DIR}/output"
ANALYSIS_FILE="${TMP_DIR}/tool-analysis.json"
mkdir -p "${WORKSPACE_DIR}" "${OUTPUT_DIR}"

jq -n --arg workspace "${WORKSPACE_DIR}" '{
    meta: {lastTouchedVersion: "test"},
    agents: {defaults: {workspace: $workspace}}
}' > "${STATE_DIR}/openclaw.json"

jq -n '{
    apt_packages: [],
    pip_packages: [],
    npm_packages: [],
    unknown_binaries: []
}' > "${ANALYSIS_FILE}"

output=$(bash "${GENERATOR}" \
    --name test-worker \
    --state-dir "${STATE_DIR}" \
    --analysis "${ANALYSIS_FILE}" \
    --output "${OUTPUT_DIR}")

zip_path=$(printf '%s\n' "${output}" | tail -n 1)
zip_name=$(basename "${zip_path}")
expected="bash agentteams-import.sh worker --name test-worker --zip ${zip_name}"

if [ ! -f "${zip_path}" ]; then
    echo "FAIL: migration ZIP was not created: ${zip_path}" >&2
    exit 1
fi

if ! printf '%s\n' "${output}" | grep -Fq "${expected}"; then
    echo "FAIL: generated instructions did not include the runnable import command" >&2
    echo "Expected: ${expected}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
fi

for invalid_name in '!!!' '-worker'; do
    if invalid_output=$(bash "${GENERATOR}" \
        --name "${invalid_name}" \
        --state-dir "${STATE_DIR}" \
        --analysis "${ANALYSIS_FILE}" \
        --output "${OUTPUT_DIR}" 2>&1); then
        echo "FAIL: invalid worker name was accepted: ${invalid_name}" >&2
        exit 1
    fi

    if ! printf '%s\n' "${invalid_output}" | grep -Fq 'ERROR: Invalid worker name'; then
        echo "FAIL: invalid worker name did not produce a clear error: ${invalid_name}" >&2
        printf '%s\n' "${invalid_output}" >&2
        exit 1
    fi
done

echo "PASS: generated instructions are runnable and invalid worker names are rejected"
