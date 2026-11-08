#!/usr/bin/env bash
# Execute the *actual* pre-download Node guard from the four Dockerfiles.
#
# The maintainer's #1421 constraint is Node 24 LTS: the guard must validate
# both the requested major and the 24.22.1 floor, so that NODE_VERSION=25/26
# cannot slip through with NODE_MAJOR=24. This test extracts the guard text
# from each Dockerfile (no network, no image build) and runs it in Linux sh.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

files=(
  openclaw-base/Dockerfile
  qwenpaw/Dockerfile
  manager/Dockerfile.qwenpaw
  hermes/Dockerfile
)

# Extract `required_major="24";` ... through the floor `fi;` (before tarball).
extract_guard() {
  awk '
    /^[[:space:]]*required_major="24"; \\$/ { active=1 }
    active { buf = buf $0 "\n" }
    active && /older than required floor/ { seen_floor=1 }
    active && seen_floor && /^[[:space:]]*fi; \\$/ { print buf; exit }
  ' "$1" | sed -e ':a' -e '/\\$/N; s/\\\n//; ta'
}

fail=0
run_case() {
  local file="$1" name="$2" node_major="$3" node_version="$4" want="$5" resolved="${6:-}"
  local guard script rc
  guard="$(extract_guard "$file")"
  if [ -z "$guard" ]; then
    echo "ERROR: $file: could not extract the Node guard" >&2
    fail=1
    return
  fi
  script="$(mktemp)"
  {
    echo '#!/bin/sh'
    echo 'set -eu'
    echo "NODE_MAJOR='${node_major}'"
    echo "NODE_MIN_VERSION='24.22.1'"
    echo "NODE_VERSION='${node_version}'"
    if [ -n "$resolved" ]; then
      # Simulate the dist-index resolution path without network access.
      echo "curl() { :; }"
      echo "jq() { printf 'v${resolved}\\n'; }"
    fi
    printf '%s\n' "$guard"
  } > "$script"
  chmod +x "$script"
  set +e
  sh "$script" >/tmp/node-guard.out 2>&1
  rc=$?
  set -e
  rm -f "$script"
  if { [ "$want" = "accept" ] && [ "$rc" -ne 0 ]; } || { [ "$want" = "reject" ] && [ "$rc" -eq 0 ]; }; then
    echo "ERROR: $file [$name] expected $want, rc=$rc" >&2
    sed 's/^/    /' /tmp/node-guard.out >&2
    fail=1
  else
    echo "ok: $file [$name] -> $want"
  fi
}

for f in "${files[@]}"; do
  run_case "$f" "24.22.1 accepted"            24 "24.22.1" accept
  run_case "$f" "24.21.0 rejected"            24 "24.21.0" reject
  run_case "$f" "25.0.0 rejected"             24 "25.0.0"  reject
  run_case "$f" "26.12.1 rejected"            24 "26.12.1" reject
  run_case "$f" "24.23.0 accepted"            24 "24.23.0" accept
  run_case "$f" "23.9.9 rejected"             24 "23.9.9"  reject
  run_case "$f" "v-prefixed 24.22.1 accepted" 24 "v24.22.1" accept
  run_case "$f" "NODE_MAJOR=25 rejected"      25 "25.0.0"  reject
  run_case "$f" "resolved 25 rejected"        24 ""        reject "25.0.0"
  run_case "$f" "resolved 24.22.1 accepted"   24 ""        accept "24.22.1"
done

if [ "$fail" -ne 0 ]; then
  echo "Node guard regression failed." >&2
  exit 1
fi
echo "Node guards OK (major 24 + floor 24.22.1, four Dockerfiles)."
