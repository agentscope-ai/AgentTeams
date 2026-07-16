#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ANALYZER="${ANALYZER:-${SCRIPT_DIR}/../scripts/analyze.sh}"

if ! command -v jq >/dev/null 2>&1; then
    echo "SKIP: jq is required"
    exit 0
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

HOME_DIR="${TMP_DIR}/home"
STATE_DIR="${TMP_DIR}/state"
WORKSPACE_DIR="${STATE_DIR}/workspace"
OUTPUT_DIR="${TMP_DIR}/output"
BIN_DIR="${TMP_DIR}/bin"
mkdir -p "${HOME_DIR}" "${WORKSPACE_DIR}" "${STATE_DIR}/cron" "${OUTPUT_DIR}" "${BIN_DIR}"

jq -n --arg workspace "${WORKSPACE_DIR}" '{
    agents: {defaults: {workspace: $workspace}}
}' > "${STATE_DIR}/openclaw.json"

jq -n '{
    version: 1,
    jobs: [{
        payload: {
            agentTurn: {
                parts: [{text: "`customcron`"}]
            }
        }
    }]
}' > "${STATE_DIR}/cron/jobs.json"

printf '%s\n' '#!/bin/sh' 'exit 0' > "${BIN_DIR}/customcron"
printf '%s\n' '#!/bin/sh' 'exit 0' > "${BIN_DIR}/legacycron"
chmod +x "${BIN_DIR}/customcron"
chmod +x "${BIN_DIR}/legacycron"

HOME="${HOME_DIR}" PATH="${BIN_DIR}:${PATH}" bash "${ANALYZER}" \
    --state-dir "${STATE_DIR}" \
    --output "${OUTPUT_DIR}" >/dev/null

if ! jq -e '
    .unknown_binaries == ["customcron"] and
    .analysis_sources.cron_payload_commands == 1
' "${OUTPUT_DIR}/tool-analysis.json" >/dev/null; then
    echo "FAIL: current-format cron dependency was not analyzed" >&2
    cat "${OUTPUT_DIR}/tool-analysis.json" >&2
    exit 1
fi

jq -n '[{
    payload: {
        agentTurn: {
            parts: [{text: "`legacycron`"}]
        }
    }
}]' > "${STATE_DIR}/cron/jobs.json"

HOME="${HOME_DIR}" PATH="${BIN_DIR}:${PATH}" bash "${ANALYZER}" \
    --state-dir "${STATE_DIR}" \
    --output "${OUTPUT_DIR}" >/dev/null

if ! jq -e '
    .unknown_binaries == ["legacycron"] and
    .analysis_sources.cron_payload_commands == 1
' "${OUTPUT_DIR}/tool-analysis.json" >/dev/null; then
    echo "FAIL: legacy-format cron dependency was not analyzed" >&2
    cat "${OUTPUT_DIR}/tool-analysis.json" >&2
    exit 1
fi

jq -n '{version: 1, jobs: []}' > "${STATE_DIR}/cron/jobs.json"

HOME="${HOME_DIR}" PATH="${BIN_DIR}:${PATH}" bash "${ANALYZER}" \
    --state-dir "${STATE_DIR}" \
    --output "${OUTPUT_DIR}" >/dev/null

if ! jq -e '
    .apt_packages == [] and
    .pip_packages == [] and
    .npm_packages == [] and
    .unknown_binaries == [] and
    .analysis_sources.cron_payload_commands == 0
' "${OUTPUT_DIR}/tool-analysis.json" >/dev/null; then
    echo "FAIL: empty dependency categories were not emitted as empty arrays" >&2
    cat "${OUTPUT_DIR}/tool-analysis.json" >&2
    exit 1
fi

echo "PASS: cron formats and empty dependency arrays are analyzed correctly"
