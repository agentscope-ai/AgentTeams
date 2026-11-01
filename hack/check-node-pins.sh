#!/usr/bin/env bash
# Enforce a pinned Node.js 24.22.1+ (Node 24 LTS) across AgentTeams images.
#
# Fails when:
#   - a Dockerfile uses a floating Node 24 reference (node:24, node:24-*), or NodeSource setup_24.x
#   - a Node-bearing Dockerfile does not pin 24.22.1
#   - the DeepSeek Harness base image is not the pinned tag
#   - the live SHASUMS256.txt extraction yields no valid checksum for x64/arm64 (set SKIP_NODE_LIVE=1 to skip in offline environments)
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
  if ! grep -q 'required_major="24"' "$f"; then
    echo "ERROR: $f must validate the Node 24 major (required_major=24)" >&2
    fail=1
  fi
  if ! grep -q 'major.*required_major' "$f"; then
    echo "ERROR: $f must reject a non-24 resolved/explicit major" >&2
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
if ! grep -q 'requiredMajor=24' deepseek-harness/Dockerfile; then
  echo "ERROR: deepseek-harness/Dockerfile must reject a non-24 major" >&2
  fail=1
fi

if ! grep -q 'node:24-bookworm-slim' hack/mirror-images.sh; then
  echo "ERROR: hack/mirror-images.sh must mirror node:24-bookworm-slim" >&2
  fail=1
fi

# Live extraction check: resolve the version and pull the tarball checksum
# from the official SHASUMS256.txt exactly as the Dockerfiles do. The text
# checks above cannot catch an extraction that fails at build time.
if [ "${SKIP_NODE_LIVE:-0}" != "1" ]; then
  live_ver="$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false) | select(.version | startswith("v24."))][0].version' | sed 's/^v//')"
  if [ -z "$live_ver" ]; then
    echo "ERROR: could not resolve latest Node 24 LTS from dist index" >&2
    fail=1
  else
    for node_arch in x64 arm64; do
      tarball="node-v${live_ver}-linux-${node_arch}.tar.xz"
      sha="$(curl -fsSL "https://nodejs.org/dist/v${live_ver}/SHASUMS256.txt" | awk -v f="$tarball" '$2 == f {print $1}')"
      if printf '%s' "$sha" | grep -qE '^[0-9a-f]{64}$'; then
        echo "OK: $tarball sha256=$sha"
      else
        echo "ERROR: no valid SHASUMS256.txt entry for $tarball" >&2
        fail=1
      fi
    done
    # Missing-entry failure path: an unknown filename must extract to empty.
    bogus="node-v${live_ver}-linux-sparc64.tar.xz"
    if [ -n "$(curl -fsSL "https://nodejs.org/dist/v${live_ver}/SHASUMS256.txt" | awk -v f="$bogus" '$2 == f {print $1}')" ]; then
      echo "ERROR: checksum extraction is not exact-filename (matched $bogus)" >&2
      fail=1
    fi
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo "Node pin check failed." >&2
  exit 1
fi
echo "Node pins OK (24.22.1)."
