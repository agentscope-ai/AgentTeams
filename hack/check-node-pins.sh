#!/usr/bin/env bash
# Enforce a pinned Node.js 24.22.1+ (Node 24 LTS) across AgentTeams images.
#
# Fails when:
#   - a Dockerfile uses a floating Node 24 reference (node:24, node:24-*), or NodeSource setup_24.x
#   - a Node-bearing Dockerfile does not pin 24.22.1
#   - the DeepSeek Harness base image is not the pinned tag
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
fail=0

while IFS= read -r f; do
  if grep -nE 'node:24([^-.]|$)|setup_24\.x' "$f"; then
    echo "ERROR: floating Node 24 reference in $f (pin 24.22.1)" >&2
    fail=1
  fi
done < <(git ls-files '*Dockerfile*' 'hack/mirror-images.sh')

for f in openclaw-base/Dockerfile qwenpaw/Dockerfile hermes/Dockerfile manager/Dockerfile.qwenpaw; do
  if ! grep -q 'NODE_MIN_VERSION=24\.22\.1' "$f"; then
    echo "ERROR: $f must set NODE_MIN_VERSION=24.22.1" >&2
    fail=1
  fi
  if ! grep -q 'nodejs.org/dist/index.json' "$f"; then
    echo "ERROR: $f must resolve Node 24 LTS from nodejs.org dist index" >&2
    fail=1
  fi
done

if ! grep -q 'FROM node:24-bookworm-slim' deepseek-harness/Dockerfile; then
  echo "ERROR: deepseek-harness/Dockerfile must use node:24-bookworm-slim" >&2
  fail=1
fi
if ! grep -q 'NODE_MIN_VERSION=24\.22\.1' deepseek-harness/Dockerfile; then
  echo "ERROR: deepseek-harness/Dockerfile must enforce NODE_MIN_VERSION=24.22.1" >&2
  fail=1
fi

if ! grep -q 'node:24-bookworm-slim' hack/mirror-images.sh; then
  echo "ERROR: hack/mirror-images.sh must mirror node:24-bookworm-slim" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "Node pin check failed." >&2
  exit 1
fi
echo "Node pins OK (24.22.1)."
