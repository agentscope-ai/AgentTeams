#!/bin/bash
# Parity test for the two openclaw.json migrators.
#
# The upstream v2026.10.9 matrix room schema rejects the fork-only
# ``groups.<room>.allow`` switch. Both the shell migrator (used by the Manager
# entrypoint and the Worker entrypoint) and the Python migrator (used by the
# Manager sync merge) run on *persisted* configs, so they must agree and must
# keep the deny semantics: allow=true was "the default", allow=false meant
# "do not answer in this room" and maps to enabled=false.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SHELL_LIB="${ROOT}/shared/lib/migrate-openclaw-config.sh"
PY_SRC="${ROOT}/manager/src/agentteams_manager"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

cat > "${TMP}/legacy.json" <<'JSON'
{
  "channels": {
    "matrix": {
      "homeserver": "http://matrix:6167",
      "groups": {
        "*": { "allow": true, "requireMention": true },
        "!room:example.org": { "allow": false, "requireMention": true },
        "!explicit:example.org": { "enabled": true, "allow": false, "requireMention": false },
        "!plain:example.org": { "requireMention": false }
      }
    }
  }
}
JSON

cp "${TMP}/legacy.json" "${TMP}/shell.json"
# shellcheck disable=SC1090
. "${SHELL_LIB}"
migrate_openclaw_config "${TMP}/shell.json"

shell_groups="$(jq -S '.channels.matrix.groups' "${TMP}/shell.json")"

if jq -e '.channels.matrix.groups | .. | objects | has("allow")' "${TMP}/shell.json" >/dev/null; then
  echo "${shell_groups}" >&2
  fail "shell migrator left a groups.*.allow property behind"
fi

# allow=false must stay a deny.
if [ "$(jq -r '.channels.matrix.groups["!room:example.org"].enabled' "${TMP}/shell.json")" != "false" ]; then
  fail "shell migrator turned an explicit deny into a default (allow=false)"
fi
# An explicit enabled=true must win over a legacy allow=false.
if [ "$(jq -r '.channels.matrix.groups["!explicit:example.org"].enabled' "${TMP}/shell.json")" != "true" ]; then
  fail "shell migrator overrode an explicit enabled"
fi
# requireMention is preserved everywhere.
if [ "$(jq -r '.channels.matrix.groups["*"].requireMention' "${TMP}/shell.json")" != "true" ]; then
  fail "shell migrator dropped requireMention"
fi

# Python parity (skipped when the interpreter is unavailable).
if command -v python3 >/dev/null 2>&1; then
  PYTHONPATH="${PY_SRC}" python3 - "${TMP}/legacy.json" "${TMP}/python.json" <<'PY'
import json
import sys

from agentteams_manager.sync import _migrate_openclaw_config

src, dst = sys.argv[1], sys.argv[2]
with open(src, encoding="utf-8") as handle:
    cfg = json.load(handle)
with open(dst, "w", encoding="utf-8") as handle:
    json.dump(_migrate_openclaw_config(cfg), handle)
PY
  python_groups="$(jq -S '.channels.matrix.groups' "${TMP}/python.json")"
  if [ "${shell_groups}" != "${python_groups}" ]; then
    echo "--- shell" >&2; echo "${shell_groups}" >&2
    echo "--- python" >&2; echo "${python_groups}" >&2
    fail "shell and python migrators disagree"
  fi
  echo "ok: shell/python parity"
else
  echo "skip: python3 not available, parity not checked"
fi

# The persisted boot paths must actually run the migrator: the Manager on its
# existing workspace config, and the Worker after pulling the stored config.
for script in \
  "${ROOT}/manager/scripts/init/start-manager-agent.sh" \
  "${ROOT}/worker/scripts/worker-entrypoint.sh"; do
  if ! grep -q 'migrate_openclaw_config' "${script}"; then
    fail "$(basename "${script}") does not run migrate_openclaw_config on the persisted config"
  fi
done
echo "ok: both boot paths run the migrator"

# If the OpenClaw CLI is available, the migrated config must still parse.
if command -v openclaw >/dev/null 2>&1; then
  mkdir -p "${TMP}/home"
  if ! HOME="${TMP}/home" OPENCLAW_CONFIG_PATH="${TMP}/shell.json" openclaw config validate >/dev/null 2>&1; then
    fail "openclaw config validate rejected the migrated config"
  fi
  echo "ok: openclaw config validate"
else
  echo "skip: openclaw CLI not available"
fi

echo "PASS: openclaw config migration"
