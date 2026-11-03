#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APPLY_WRAPPER="${ROOT_DIR}/install/agentteams-apply.sh"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

for unsupported in --prune --dry-run --watch; do
    output="$(
        AGENTTEAMS_CONTAINER_CMD=false \
            bash "${APPLY_WRAPPER}" -f missing.yaml "${unsupported}" 2>&1 || true
    )"
    grep -Fq -- "${unsupported} is not supported" <<<"${output}" ||
        fail "wrapper must reject ${unsupported} before contacting the container runtime"
done

if grep -Eq '^#[[:space:]]+.*agentteams-apply\.sh.*--(prune|dry-run|watch)' "${APPLY_WRAPPER}"; then
    fail "usage examples must not advertise unsupported apply flags"
fi

echo "PASS: apply wrapper rejects unsupported flags explicitly"
