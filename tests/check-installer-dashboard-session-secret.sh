#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"

# Regression for issue #1311: the installer never generated, persisted, or
# passed DASHBOARD_SESSION_SECRET to the dashboard container, so the
# dashboard fail-closed on the login page ("Session store unavailable").

# ── Section 1: static contract of _start_dashboard ─────────────────────────

# The dashboard container must receive the session secret as an env var.
grep -q 'env_args+=(-e DASHBOARD_SESSION_SECRET="${DASHBOARD_SESSION_SECRET}")' "${INSTALLER}" || {
    echo "FAIL: _start_dashboard must pass DASHBOARD_SESSION_SECRET to the dashboard container" >&2
    exit 1
}

# A fresh secret must be generated with openssl and persisted to the env file
# so container rebuilds keep existing sessions valid.
grep -q 'DASHBOARD_SESSION_SECRET="$(openssl rand -hex 32)"' "${INSTALLER}" || {
    echo "FAIL: _start_dashboard must generate a fresh session secret with openssl" >&2
    exit 1
}
grep -q "printf 'DASHBOARD_SESSION_SECRET=%s\\\\n'" "${INSTALLER}" || {
    echo "FAIL: _start_dashboard must persist the generated session secret to the env file" >&2
    exit 1
}

# A persisted secret must be picked up again instead of being regenerated.
grep -q "grep '^DASHBOARD_SESSION_SECRET='" "${INSTALLER}" || {
    echo "FAIL: _start_dashboard must reuse a session secret persisted in the env file" >&2
    exit 1
}

# ── Section 2: load_current_params_from_env readback ───────────────────────

eval "$(sed -n '/^load_current_params_from_env() {/,/^}/p' "${INSTALLER}")"

if ! type load_current_params_from_env >/dev/null 2>&1; then
    echo "FAIL: could not extract load_current_params_from_env from the installer" >&2
    exit 1
fi

# The installer runs this function under plain `set -e` (no pipefail): a grep
# miss on a field absent from the env file must stay harmless, as in
# production. Match those semantics before exercising the function.
set +o pipefail

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
env_file="${workdir}/agentteams-manager.env"

cat > "${env_file}" << 'EOF'
AGENTTEAMS_DASHBOARD=1
DASHBOARD_SESSION_SECRET=persisted-secret-from-env-file
EOF

# The `dashboard` subcommand path: nothing exported, the secret must come
# from the env file so a manual rebuild reuses the persisted value.
unset DASHBOARD_SESSION_SECRET || true
AGENTTEAMS_ENV_FILE="${env_file}"
load_current_params_from_env
if [ "${DASHBOARD_SESSION_SECRET:-}" != "persisted-secret-from-env-file" ]; then
    echo "FAIL: expected DASHBOARD_SESSION_SECRET to read back from the env file, got '${DASHBOARD_SESSION_SECRET:-}'" >&2
    exit 1
fi

# An exported value must win over the env file (e.g. step_existing exports
# all env keys on upgrade).
DASHBOARD_SESSION_SECRET="exported-secret"
load_current_params_from_env
if [ "${DASHBOARD_SESSION_SECRET}" != "exported-secret" ]; then
    echo "FAIL: exported DASHBOARD_SESSION_SECRET must not be overwritten by the env file" >&2
    exit 1
fi

# Env files written before the field existed must read back empty and stay
# harmless under the installer's set -e (fresh-install path then generates).
grep -v '^DASHBOARD_SESSION_SECRET=' "${env_file}" > "${env_file}.old"
unset DASHBOARD_SESSION_SECRET
AGENTTEAMS_ENV_FILE="${env_file}.old"
load_current_params_from_env
if [ -n "${DASHBOARD_SESSION_SECRET:-}" ]; then
    echo "FAIL: expected empty DASHBOARD_SESSION_SECRET when the env file predates the field" >&2
    exit 1
fi

echo "PASS: installer persists and reuses DASHBOARD_SESSION_SECRET for the dashboard"
