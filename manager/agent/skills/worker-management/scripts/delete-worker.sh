#!/bin/bash
# delete-worker.sh - Delete a Worker through the hiclaw CLI and clean local state.
#
# Usage:
#   delete-worker.sh --worker <name> [--cleanup-records]
#   delete-worker.sh --worker <name> --records-only

set -euo pipefail

WORKER_NAME=""
CLEANUP_RECORDS=false
RECORDS_ONLY=false

usage() {
    cat >&2 <<'EOF'
Usage:
  delete-worker.sh --worker <name> [--cleanup-records]
  delete-worker.sh --worker <name> --records-only

Options:
  --worker, -w        Worker name to delete or clean up.
  --cleanup-records   After successful delete, remove local state entries for this Worker.
  --records-only      Do not call hiclaw delete; only remove local state entries.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --worker|-w)
            WORKER_NAME="${2:-}"
            shift 2
            ;;
        --cleanup-records)
            CLEANUP_RECORDS=true
            shift
            ;;
        --records-only)
            RECORDS_ONLY=true
            CLEANUP_RECORDS=true
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "Unknown argument: $1" >&2
            usage
            exit 2
            ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    echo "Missing required --worker <name>" >&2
    usage
    exit 2
fi

cleanup_records() {
    local worker="$1"
    local cleaned_state=false
    local cleaned_lifecycle=false
    local cleaned_registry=false
    local tmp

    if [ -f "${HOME}/state.json" ]; then
        tmp=$(mktemp)
        jq --arg w "${worker}" '
          .active_tasks = [
            (.active_tasks // [])[]
            | select((.assigned_to // "") != $w and (.worker // "") != $w)
          ]
          | .updated_at = (now | strftime("%Y-%m-%dT%H:%M:%SZ"))
        ' "${HOME}/state.json" > "${tmp}" && mv "${tmp}" "${HOME}/state.json"
        cleaned_state=true
    fi

    if [ -f "${HOME}/worker-lifecycle.json" ]; then
        tmp=$(mktemp)
        jq --arg w "${worker}" '
          if (.workers // null) != null then
            del(.workers[$w]) | .updated_at = (now | strftime("%Y-%m-%dT%H:%M:%SZ"))
          else . end
        ' "${HOME}/worker-lifecycle.json" > "${tmp}" && mv "${tmp}" "${HOME}/worker-lifecycle.json"
        cleaned_lifecycle=true
    fi

    if [ -f "${HOME}/workers-registry.json" ]; then
        tmp=$(mktemp)
        jq --arg w "${worker}" '
          if (.workers // null) != null then
            del(.workers[$w]) | .updated_at = (now | strftime("%Y-%m-%dT%H:%M:%SZ"))
          else . end
        ' "${HOME}/workers-registry.json" > "${tmp}" && mv "${tmp}" "${HOME}/workers-registry.json"
        cleaned_registry=true
    fi

    jq -cn \
      --argjson state "${cleaned_state}" \
      --argjson lifecycle "${cleaned_lifecycle}" \
      --argjson registry "${cleaned_registry}" \
      '{state_json: $state, worker_lifecycle_json: $lifecycle, workers_registry_json: $registry}'
}

delete_output=""
delete_status="skipped"
delete_exit=0

if [ "${RECORDS_ONLY}" != true ]; then
    set +e
    delete_output=$(hiclaw delete worker "${WORKER_NAME}" 2>&1)
    delete_exit=$?
    set -e
    if [ "${delete_exit}" -ne 0 ]; then
        jq -cn \
          --arg worker "${WORKER_NAME}" \
          --arg output "${delete_output}" \
          --argjson exit_code "${delete_exit}" \
          '{worker: $worker, status: "failed", deleted: false, exit_code: $exit_code, output: $output}'
        exit "${delete_exit}"
    fi
    delete_status="deleted"
fi

cleanup_json='{}'
if [ "${CLEANUP_RECORDS}" = true ]; then
    cleanup_json=$(cleanup_records "${WORKER_NAME}")
fi

jq -cn \
  --arg worker "${WORKER_NAME}" \
  --arg status "${delete_status}" \
  --arg output "${delete_output}" \
  --argjson records "${cleanup_json}" \
  '{worker: $worker, status: $status, deleted: ($status == "deleted"), output: $output, records: $records}'
