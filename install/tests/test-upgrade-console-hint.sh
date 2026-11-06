#!/usr/bin/env bash
# Regression for the console-approval hint printed after a manager upgrade
# (#1460).
#
# The hint tells the operator which commands to run for a one-time device
# approval. Those commands must be printed literally: the message is a
# double-quoted string, so backticks (or any unquoted `$(...)`) would make the
# shell *execute* them while printing, and a literal `<requestId>` would turn
# into a redirection and break the message. This test extracts the hint lines
# from install/agentteams-upgrade.sh, evaluates them with a recording `docker`
# stub, and asserts:
#
#   - the printed text contains the commands verbatim (both languages);
#   - the stub `docker` is never invoked;
#   - no backtick survives in the script's log messages.
#
# It never runs real Docker.

set -uo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${AGENTTEAMS_UPGRADE_SCRIPT:-${SCRIPT_DIR}/../agentteams-upgrade.sh}"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }

echo "=== upgrade console hint prints literally (no execution) ==="

if [ ! -f "${INSTALLER}" ]; then
  fail "installer exists" "${INSTALLER} not found"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
  exit 1
fi

# 1) No backticks anywhere in the script's log messages: backticks inside a
#    double-quoted string are command substitution.
BACKTICK_LINES="$(grep -n 'log .*`' "${INSTALLER}" || true)"
if [ -z "${BACKTICK_LINES}" ]; then
  pass "no backticks in log messages"
else
  fail "no backticks in log messages" "found: ${BACKTICK_LINES}"
fi

# 2) Evaluate each hint line with recording stubs. The lines are extracted
#    verbatim (leading whitespace and the surrounding if-blocks are not
#    executed), so this exercises the exact quoting.
HINT_LINES="$(grep -n 'one-time device approval\|一次性设备审批' "${INSTALLER}" | grep 'log "' || true)"
if [ -n "${HINT_LINES}" ]; then
  pass "hint lines found in the installer"
else
  fail "hint lines found in the installer" "no log line matches the hint text"
fi

RUNNER="$(mktemp)"
trap 'rm -f "${RUNNER}"' EXIT
while IFS= read -r entry; do
  [ -n "${entry}" ] || continue
  lineno="${entry%%:*}"
  statement="${entry#*:}"
  cat > "${RUNNER}" <<HARNESS
set -u
docker() { printf 'DOCKER-WAS-CALLED\n' >&2; return 1; }
log() { printf '%s\n' "\$*"; }
${statement}
HARNESS
  out="$(bash "${RUNNER}" 2>"${RUNNER}.err")"
  err="$(cat "${RUNNER}.err")"
  rm -f "${RUNNER}.err"
  if printf '%s' "${out}" | grep -q 'docker exec agentteams-manager openclaw devices list'; then
    pass "line ${lineno}: hint prints the 'devices list' command literally"
  else
    fail "line ${lineno}: hint prints the 'devices list' command literally" "output=${out}"
  fi
  if printf '%s' "${out}" | grep -q 'docker exec agentteams-manager openclaw devices approve <requestId>'; then
    pass "line ${lineno}: hint prints the 'approve <requestId>' command literally"
  else
    fail "line ${lineno}: hint prints the 'approve <requestId>' command literally" "output=${out}"
  fi
  if printf '%s' "${err}" | grep -q 'DOCKER-WAS-CALLED'; then
    fail "line ${lineno}: printing the hint must not execute docker" "stderr=${err}"
  else
    pass "line ${lineno}: printing the hint does not execute docker"
  fi
done <<< "${HINT_LINES}"

echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
