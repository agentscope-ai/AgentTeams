#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT_DIR}/manager/agent/skills/project-management/scripts/create-project.sh"
REFERENCE="${ROOT_DIR}/manager/agent/skills/project-management/references/create-project.md"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

[ -f "${SCRIPT}" ] || fail "create-project.sh not found at ${SCRIPT}"
[ -f "${REFERENCE}" ] || fail "create-project.md reference not found at ${REFERENCE}"

# 1. The script must parse the --grant-admin flag.
grep -q -- "--grant-admin) GRANT_ADMIN_CSV" "${SCRIPT}" ||
    fail "create-project.sh must parse --grant-admin into GRANT_ADMIN_CSV"

# 2. The usage line must document the new optional flag.
grep -Fq -- "[--grant-admin <u1,u2,...>]" "${SCRIPT}" ||
    fail "create-project.sh usage line must document [--grant-admin <u1,u2,...>]"

# 3. Granted users must land in the room-creation power level override at
#    level 100 (co-owner), next to the admin and worker entries.
grep -Fq -- 'GRANT_ADMIN_LEVELS="${GRANT_ADMIN_LEVELS},\"${grant_id}\": 100"' "${SCRIPT}" ||
    fail "create-project.sh must add each --grant-admin user at power level 100"
grep -F -- '${WORKER_POWER_LEVELS}' "${SCRIPT}" | grep -Fq -- '${GRANT_ADMIN_LEVELS}' ||
    fail "power_level_content_override must splice in the granted admin levels"

# 4. The runtime-facing project-management reference must instruct the
#    Manager to pass --grant-admin — the script flag alone is dead code if
#    the Manager is never told about it.
grep -Fq -- "--grant-admin" "${REFERENCE}" ||
    fail "create-project.md must document --grant-admin for the Manager"
grep -Fq -- "--grant-admin \"sunzong\"" "${REFERENCE}" ||
    fail "create-project.md must show a concrete --grant-admin usage example"
grep -qi -- "co-owns the project room" "${REFERENCE}" ||
    fail "create-project.md must explain what --grant-admin grants (level 100 co-ownership)"

echo "PASS: create-project.sh --grant-admin is implemented and wired into the runtime reference"
