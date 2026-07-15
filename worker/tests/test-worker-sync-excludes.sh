#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
ENTRYPOINT="${REPO_ROOT}/worker/scripts/worker-entrypoint.sh"

bash -n "${ENTRYPOINT}"

grep -Fq 'BULK_CHANGED=$(find "${WORKSPACE}/"' "${ENTRYPOINT}"
grep -Fq -- '-path "${WORKSPACE}/credentials"' "${ENTRYPOINT}"
grep -Fq -- '! -name "HEARTBEAT.md"' "${ENTRYPOINT}"
grep -Fq 'for _mf in SOUL.md AGENTS.md; do' "${ENTRYPOINT}"

if grep -Eq 'for _mf in .*HEARTBEAT\.md' "${ENTRYPOINT}"; then
    echo "HEARTBEAT.md must not be pushed by the Worker local-to-remote loop" >&2
    exit 1
fi

echo "worker sync excludes are configured"
