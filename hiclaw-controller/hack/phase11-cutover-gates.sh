#!/usr/bin/env bash
# Phase 11 cutover gate checks (read-only). See design/phase11-cutover-gates.md.
set -euo pipefail

NAMESPACE="${NAMESPACE:-agentteams}"
FAIL=0

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 2
  }
}

need kubectl
need jq

section() {
  printf '\n== %s ==\n' "$1"
}

check_empty() {
  local label="$1"
  local json="$2"
  local count
  count="$(printf '%s' "$json" | jq 'length')"
  if [[ "$count" -eq 0 ]]; then
    echo "PASS: $label (0)"
  else
    echo "FAIL: $label ($count)"
    printf '%s\n' "$json" | jq .
    FAIL=1
  fi
}

section "1.1 Legacy Teams (empty workerMembers + inline leader/workers)"
LEGACY="$(kubectl get teams -n "$NAMESPACE" -o json | jq '
  [.items[]
   | select((.spec.workerMembers // []) | length == 0)
           and ((.spec.leader.name // "") != "" or ((.spec.workers // []) | length > 0))
   | .metadata.name]')"
check_empty "legacy teams" "$LEGACY"

section "1.2 Teams with empty workerMembers (non-deleting)"
EMPTY_MEMBERS="$(kubectl get teams -n "$NAMESPACE" -o json | jq '
  [.items[]
   | select(.metadata.deletionTimestamp == null)
   | select((.spec.workerMembers // []) | length == 0)
   | {name: .metadata.name, leader: .spec.leader.name, workers: (.spec.workers | length)}]')"
check_empty "active teams without workerMembers" "$EMPTY_MEMBERS"

section "1.3 Teams with workerMembers AND leftover inline spec"
DUAL_SPEC="$(kubectl get teams -n "$NAMESPACE" -o json | jq '
  [.items[]
   | select((.spec.workerMembers // []) | length > 0)
   | select((.spec.leader.name // "") != "" or ((.spec.workers // []) | length > 0))
   | .metadata.name]')"
check_empty "dual-spec teams" "$DUAL_SPEC"

section "1.4 Pre-refactor annotated child Worker CRs"
CHILD_WORKERS="$(kubectl get workers -n "$NAMESPACE" -o json | jq '
  [.items[]
   | select(.metadata.annotations["agentteams.io/team"] != null
           or .metadata.annotations["agentteams.io/team-leader"] != null
           or .metadata.annotations["agentteams.io/role"] == "team_leader")
   | .metadata.name]')"
check_empty "annotated team child workers" "$CHILD_WORKERS"

printf '\n'
if [[ "$FAIL" -eq 0 ]]; then
  echo "All kubectl gates PASS for namespace ${NAMESPACE}."
  echo "Also run registry/API checks in design/phase11-cutover-gates.md §1.5–§1.7."
  exit 0
fi

echo "One or more gates FAILED. Do not proceed with Phase 11 hard cutover."
exit 1
