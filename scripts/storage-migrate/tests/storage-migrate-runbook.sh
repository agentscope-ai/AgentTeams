#!/usr/bin/env bash
# Read-only assertions for the cutover operator runbook (the 9-section
# checklist). No mc, no network, no state: --print-checklist prints the
# runbook for each deploy mode, and this script verifies the per-mode
# command blocks, the writer-class coverage, the section tagging, and the
# design-doc/README consistency (drift guard).
#
# Usage: bash tests/storage-migrate-runbook.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="$HERE/../storage-migrate.sh"
README="$HERE/../README.md"
DOC="$(cd "$HERE/../../.." && pwd)/docs/design/storage-external-s3-backend.md"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sm-runbook.XXXXXX")"
FAILS=0
pass() { echo "PASS: $*"; }
fail() { echo "[FAIL] $*"; FAILS=$((FAILS + 1)); }

# print the runbook for one deploy mode (stdout only; run in a clean CWD)
print_checklist() {
  ( cd "$WORK" && bash "$TOOL" --print-checklist --deploy-mode "$1" 2>/dev/null )
}

have() { # <haystack> <needle>
  printf '%s\n' "$1" | grep -qF -- "$2"
}

echo "=== storage-migrate runbook tests (read-only, no mc) ==="

# 0) the preview is side-effect free: no state directory, no files in the CWD
EMBEDDED="$(print_checklist embedded)"
HELM="$(print_checklist helm)"
BOTH="$(print_checklist both)"
if [ -z "$(ls -A "$WORK" 2>/dev/null)" ]; then
  pass "--print-checklist writes nothing into the CWD (no state dir, no files)"
else
  fail "--print-checklist writes nothing into the CWD: $(ls -A "$WORK" | tr '\n' ' ')"
fi

# 1) embedded mode carries the embedded command block
for k in "agt update worker" "--state Stopped" "docker rm -f agentteams-controller" \
         "AGENTTEAMS_STORAGE_PROVIDER=s3" "docker rm -f" "env | grep AGENTTEAMS_FS_" \
         "403" "mc mirror --overwrite --remove" "ACKNOWLEDGEMENT"; do
  if have "$EMBEDDED" "$k"; then pass "embedded contains $k"; else fail "embedded contains $k"; fi
done

# 2) helm mode carries the helm command block
for k in "kubectl -n" "--type merge" "helm upgrade" "storage.provider=s3" \
         "kubectl -n <ns> delete pod" "mc mirror --overwrite --remove" "ACKNOWLEDGEMENT"; do
  if have "$HELM" "$k"; then pass "helm contains $k"; else fail "helm contains $k"; fi
done

# 3) both = the union of the two command blocks
for k in "agt update worker" "--state Stopped" "docker rm -f agentteams-controller" \
         "AGENTTEAMS_STORAGE_PROVIDER=s3" "kubectl -n <ns> patch" "--type merge" \
         "helm upgrade" "storage.provider=s3" "kubectl -n <ns> delete pod" \
         "env | grep AGENTTEAMS_FS_" "mc mirror --overwrite --remove" "ACKNOWLEDGEMENT"; do
  if have "$BOTH" "$k"; then pass "both contains $k"; else fail "both contains $k"; fi
done

# 4) the retired claim ("workers untouched") must not survive in any output
for m in embedded helm both; do
  case "$m" in
    embedded) out="$EMBEDDED" ;;
    helm) out="$HELM" ;;
    *) out="$BOTH" ;;
  esac
  if have "$out" "workers untouched"; then
    fail "$m must not contain 'workers untouched'"
  else
    pass "$m must not contain 'workers untouched'"
  fi
done

# 5) every output covers the four writer classes (external clients,
#    controller, manager, worker)
for m in embedded helm both; do
  case "$m" in
    embedded) out="$EMBEDDED" ;;
    helm) out="$HELM" ;;
    *) out="$BOTH" ;;
  esac
  for k in "external clients" "controller" "manager" "worker"; do
    if have "$out" "$k"; then pass "$m covers writer class: $k"; else fail "$m covers writer class: $k"; fi
  done
done

# 6) doc + README drift guard: the key command strings live in both
CONSISTENCY_KEYS=(
  "agt update worker"
  "--state Stopped"
  "kubectl -n <ns> patch"
  "--type merge"
  "docker rm -f agentteams-controller"
  "AGENTTEAMS_STORAGE_PROVIDER=s3"
  "helm upgrade"
  "storage.provider=s3"
  "kubectl -n <ns> delete pod"
  "env | grep AGENTTEAMS_FS_"
  "mc mirror --overwrite --remove"
  "ACKNOWLEDGEMENT"
  "403"
)
for k in "${CONSISTENCY_KEYS[@]}"; do
  for f in "$DOC" "$README"; do
    if [ -f "$f" ] && grep -qF -- "$k" "$f"; then
      pass "$(basename "$f") contains $k"
    else
      fail "$(basename "$f") contains $k"
    fi
  done
done
for f in "$DOC" "$README"; do
  if [ -f "$f" ] && grep -qF "through MinIO" "$f"; then
    fail "$(basename "$f") must not claim the post-cutover round trip goes through MinIO"
  else
    pass "$(basename "$f") must not claim the post-cutover round trip goes through MinIO"
  fi
done

# 7) the runbook has exactly 9 numbered sections, each tagged [tool]/[manual]
for m in embedded helm both; do
  case "$m" in
    embedded) out="$EMBEDDED" ;;
    helm) out="$HELM" ;;
    *) out="$BOTH" ;;
  esac
  n=$(printf '%s\n' "$out" | grep -cE '^# \[[1-9]\] \[(tool|manual)\]')
  if [ "$n" -eq 9 ]; then
    pass "$m has 9 numbered [tool]/[manual] section headers (got $n)"
  else
    fail "$m has 9 numbered [tool]/[manual] section headers (got $n)"
  fi
done

echo
if [ "$FAILS" -eq 0 ]; then
  echo "RUNBOOK RESULT: PASS (all assertions green)"
  exit 0
fi
echo "RUNBOOK RESULT: FAIL ($FAILS assertion(s) failed)"
exit 1
