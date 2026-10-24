#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"

# Regression for issue #1311: the installer never generated, persisted, or
# passed DASHBOARD_SESSION_SECRET to the dashboard container, so the
# dashboard fail-closed on the login page ("Session store unavailable").
# Also covers the upgrade persistence gap: step_existing exports the secret,
# then `cat > ENV_FILE` must rewrite it, and a later rebuild in a new process
# must reuse the same value.

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

# The installer env-file rewrite (cat > ENV_FILE) must include the field so
# an upgrade cannot drop a secret that only lived in memory.
grep -Fq 'DASHBOARD_SESSION_SECRET=${DASHBOARD_SESSION_SECRET' "${INSTALLER}" || {
    echo "FAIL: installer env-file writer must include DASHBOARD_SESSION_SECRET" >&2
    exit 1
}

# Secrets generation must keep an existing secret (upgrade) or generate one.
grep -Fq 'DASHBOARD_SESSION_SECRET="${DASHBOARD_SESSION_SECRET:-$(generate_key)}"' "${INSTALLER}" || {
    echo "FAIL: secrets generation must retain or generate DASHBOARD_SESSION_SECRET" >&2
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

# ── Section 3: executable fresh-install -> upgrade -> later rebuild ────────
# Extract the real env-file writer and the full _start_dashboard, then run
# them with stubbed Docker/curl. No live deployment is touched.

extract_function() {
    local func_name="$1"
    local file="$2"
    local start_line
    start_line=$(grep -n "^${func_name}() {" "${file}" | head -1 | cut -d: -f1)
    if [ -z "${start_line}" ]; then
        return 1
    fi
    local depth=0
    local line_num=0
    local end_line=0
    while IFS= read -r line; do
        line_num=$((line_num + 1))
        if [ "${line_num}" -lt "${start_line}" ]; then
            continue
        fi
        local opens closes
        opens=$(printf '%s' "${line}" | tr -cd '{' | wc -c)
        closes=$(printf '%s' "${line}" | tr -cd '}' | wc -c)
        depth=$((depth + opens - closes))
        if [ "${depth}" -eq 0 ] && [ "${line_num}" -gt "${start_line}" ]; then
            end_line="${line_num}"
            break
        fi
    done < "${file}"
    if [ "${end_line}" -eq 0 ]; then
        return 1
    fi
    sed -n "${start_line},${end_line}p" "${file}"
}

env_writer="$(sed -n '/# Write \.env file/,/chmod 600 "\${ENV_FILE}"/p' "${INSTALLER}")"
if ! printf '%s\n' "${env_writer}" | grep -q 'cat > "${ENV_FILE}"'; then
    echo "FAIL: could not extract the installer env-file writer" >&2
    exit 1
fi

start_dashboard_src="${workdir}/_start_dashboard.sh"
if ! extract_function "_start_dashboard" "${INSTALLER}" > "${start_dashboard_src}"; then
    echo "FAIL: could not extract _start_dashboard from the installer" >&2
    exit 1
fi
if ! grep -q '_start_dashboard() {' "${start_dashboard_src}"; then
    echo "FAIL: extracted _start_dashboard is empty" >&2
    exit 1
fi

seq_env="${workdir}/seq.env"
secret_capture="${workdir}/docker-secret"
run_log="${workdir}/docker-run.log"

# Minimal values so the unquoted heredoc expands without `set -u` noise.
write_env_file() {
    set +u
    AGENTTEAMS_ENV_FILE="${seq_env}"
    ENV_FILE="${seq_env}"
    AGENTTEAMS_LANGUAGE="${AGENTTEAMS_LANGUAGE:-en}"
    AGENTTEAMS_LLM_PROVIDER="${AGENTTEAMS_LLM_PROVIDER:-}"
    AGENTTEAMS_DEFAULT_MODEL="${AGENTTEAMS_DEFAULT_MODEL:-}"
    AGENTTEAMS_LLM_API_KEY="${AGENTTEAMS_LLM_API_KEY:-}"
    AGENTTEAMS_ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"
    AGENTTEAMS_ADMIN_PASSWORD="${AGENTTEAMS_ADMIN_PASSWORD:-password12}"
    AGENTTEAMS_LOCAL_ONLY="${AGENTTEAMS_LOCAL_ONLY:-1}"
    AGENTTEAMS_PORT_GATEWAY="${AGENTTEAMS_PORT_GATEWAY:-18080}"
    AGENTTEAMS_PORT_CONSOLE="${AGENTTEAMS_PORT_CONSOLE:-18001}"
    AGENTTEAMS_PORT_ELEMENT_WEB="${AGENTTEAMS_PORT_ELEMENT_WEB:-18088}"
    AGENTTEAMS_MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:18080}"
    AGENTTEAMS_MATRIX_CLIENT_DOMAIN="${AGENTTEAMS_MATRIX_CLIENT_DOMAIN:-}"
    AGENTTEAMS_AI_GATEWAY_DOMAIN="${AGENTTEAMS_AI_GATEWAY_DOMAIN:-}"
    AGENTTEAMS_MANAGER_GATEWAY_KEY="${AGENTTEAMS_MANAGER_GATEWAY_KEY:-gwkey}"
    AGENTTEAMS_FS_DOMAIN="${AGENTTEAMS_FS_DOMAIN:-}"
    AGENTTEAMS_CONSOLE_DOMAIN="${AGENTTEAMS_CONSOLE_DOMAIN:-}"
    AGENTTEAMS_MINIO_USER="${AGENTTEAMS_MINIO_USER:-admin}"
    AGENTTEAMS_MINIO_PASSWORD="${AGENTTEAMS_MINIO_PASSWORD:-password12}"
    AGENTTEAMS_MANAGER_PASSWORD="${AGENTTEAMS_MANAGER_PASSWORD:-mpw}"
    AGENTTEAMS_REGISTRATION_TOKEN="${AGENTTEAMS_REGISTRATION_TOKEN:-rtok}"
    AGENTTEAMS_EMBEDDING_MODEL="${AGENTTEAMS_EMBEDDING_MODEL:-}"
    WORKER_IMAGE="${WORKER_IMAGE:-}"
    COPAW_WORKER_IMAGE="${COPAW_WORKER_IMAGE:-}"
    QWENPAW_WORKER_IMAGE="${QWENPAW_WORKER_IMAGE:-}"
    HERMES_WORKER_IMAGE="${HERMES_WORKER_IMAGE:-}"
    DEEPSEEK_HARNESS_WORKER_IMAGE="${DEEPSEEK_HARNESS_WORKER_IMAGE:-}"
    CLI_HARNESS_WORKER_IMAGE="${CLI_HARNESS_WORKER_IMAGE:-}"
    AGENTTEAMS_REGISTRY="${AGENTTEAMS_REGISTRY:-ghcr.io/agentteams-group}"
    eval "${env_writer}"
    set -u
}

read_file_secret() {
    grep '^DASHBOARD_SESSION_SECRET=' "${seq_env}" 2>/dev/null | cut -d= -f2- | tr -d '\r'
}

# Fake docker/curl: capture the session secret passed to `docker run`.
# shellcheck disable=SC2329
fake_docker() {
    if [ "$1" = "run" ]; then
        printf '%s\n' "$*" >> "${run_log}"
        : > "${secret_capture}"
        local arg
        for arg in "$@"; do
            case "${arg}" in
                DASHBOARD_SESSION_SECRET=*)
                    printf '%s\n' "${arg#DASHBOARD_SESSION_SECRET=}" > "${secret_capture}"
                    ;;
            esac
        done
        return 0
    fi
    return 0
}

run_start_dashboard() {
    : > "${secret_capture}"
    (
        set +e
        set +u
        log() { :; }
        msg() { echo "$*"; }
        curl() { return 0; }
        sleep() { :; }
        DOCKER_CMD="fake_docker"
        fake_docker() {
            if [ "$1" = "run" ]; then
                printf '%s\n' "$*" >> "${run_log}"
                : > "${secret_capture}"
                local arg
                for arg in "$@"; do
                    case "${arg}" in
                        DASHBOARD_SESSION_SECRET=*)
                            printf '%s\n' "${arg#DASHBOARD_SESSION_SECRET=}" > "${secret_capture}"
                            ;;
                    esac
                done
                return 0
            fi
            return 0
        }
        AGENTTEAMS_DASHBOARD=1
        AGENTTEAMS_USE_EMBEDDED=1
        AGENTTEAMS_REGISTRY="ghcr.io/agentteams-group"
        AGENTTEAMS_PORT_DASHBOARD="13000"
        AGENTTEAMS_DASHBOARD_VERSION="v1.2.4.9"
        AGENTTEAMS_DASHBOARD_IMAGE="ghcr.io/agentteams-group/agentteams/agentteams-dashboard:v1.2.4.9"
        AGENTTEAMS_LOCAL_ONLY=1
        AGENTTEAMS_AI_GATEWAY_ADMIN_URL=""
        AGENTTEAMS_ENV_FILE="${seq_env}"
        # shellcheck disable=SC1090
        source "${start_dashboard_src}"
        _start_dashboard
    )
}

read_captured_secret() {
    if [ -s "${secret_capture}" ]; then
        tr -d '\n\r' < "${secret_capture}"
    fi
}

# (1) Fresh install: generate a secret, write the env file, start dashboard.
unset DASHBOARD_SESSION_SECRET || true
set +u
DASHBOARD_SESSION_SECRET="${DASHBOARD_SESSION_SECRET:-$(openssl rand -hex 32)}"
set -u
fresh_secret="${DASHBOARD_SESSION_SECRET}"
if [ "${#fresh_secret}" -lt 64 ]; then
    echo "FAIL: generated session secret is too short (${#fresh_secret} chars)" >&2
    exit 1
fi
write_env_file
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: fresh-install env writer did not persist DASHBOARD_SESSION_SECRET (got '${file_secret}')" >&2
    exit 1
fi
run_start_dashboard
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: fresh-install _start_dashboard passed '${passed_secret}', expected '${fresh_secret}'" >&2
    exit 1
fi
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: fresh-install _start_dashboard changed the persisted secret to '${file_secret}'" >&2
    exit 1
fi

# (2) Immediate rebuild in the same process: reuse the exported secret.
run_start_dashboard
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: same-process rebuild passed '${passed_secret}', expected '${fresh_secret}'" >&2
    exit 1
fi

# (3) Upgrade rewrite: step_existing exports the secret, then cat > ENV_FILE.
# The generated block must keep the field.
export DASHBOARD_SESSION_SECRET="${fresh_secret}"
write_env_file
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: upgrade env rewrite dropped DASHBOARD_SESSION_SECRET (got '${file_secret}')" >&2
    exit 1
fi
run_start_dashboard
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: upgrade _start_dashboard passed '${passed_secret}', expected '${fresh_secret}'" >&2
    exit 1
fi
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: upgrade _start_dashboard did not keep the secret in the env file (got '${file_secret}')" >&2
    exit 1
fi

# Simulate the pre-fix upgrade rewrite that omitted the field, then start
# dashboard with the exported secret still in memory. _start_dashboard must
# write the exported value back so a later process can reuse it.
grep -v '^DASHBOARD_SESSION_SECRET=' "${seq_env}" > "${seq_env}.dropped"
mv "${seq_env}.dropped" "${seq_env}"
export DASHBOARD_SESSION_SECRET="${fresh_secret}"
run_start_dashboard
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: _start_dashboard must persist an exported secret after env rewrite dropped the field (got '${file_secret}')" >&2
    exit 1
fi
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: _start_dashboard must pass the exported secret after env rewrite dropped the field (got '${passed_secret}')" >&2
    exit 1
fi

# (4) Later rebuild in a new process: nothing exported, secret must come
# from the env file (dashboard subcommand / post-upgrade restart).
unset DASHBOARD_SESSION_SECRET || true
run_start_dashboard
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: later-process rebuild passed '${passed_secret}', expected '${fresh_secret}'" >&2
    exit 1
fi
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${fresh_secret}" ]; then
    echo "FAIL: later-process rebuild changed the persisted secret to '${file_secret}'" >&2
    exit 1
fi

# (5) The env file keeps the field but with an empty value: a new process
# must treat it as missing, generate a secret, and overwrite the empty
# field instead of skipping the writeback because the key already exists.
awk '/^DASHBOARD_SESSION_SECRET=/ && !done { print "DASHBOARD_SESSION_SECRET="; done = 1; next } { print }' \
    "${seq_env}" > "${seq_env}.empty"
mv "${seq_env}.empty" "${seq_env}"
if [ -n "$(read_file_secret)" ]; then
    echo "FAIL: could not empty the persisted session secret for the empty-field case" >&2
    exit 1
fi
unset DASHBOARD_SESSION_SECRET || true
run_start_dashboard
empty_field_secret="$(read_captured_secret)"
if [ "${#empty_field_secret}" -lt 64 ]; then
    echo "FAIL: empty-field start did not generate a session secret (got '${empty_field_secret}')" >&2
    exit 1
fi
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${empty_field_secret}" ]; then
    echo "FAIL: empty DASHBOARD_SESSION_SECRET field was not overwritten (got '${file_secret}')" >&2
    exit 1
fi
if [ "$(grep -c '^DASHBOARD_SESSION_SECRET=' "${seq_env}")" -ne 1 ]; then
    echo "FAIL: empty-field writeback must keep exactly one DASHBOARD_SESSION_SECRET field" >&2
    exit 1
fi
run_start_dashboard
if [ "$(read_captured_secret)" != "${empty_field_secret}" ]; then
    echo "FAIL: second process after empty-field writeback generated a different secret" >&2
    exit 1
fi

# (6) An explicit environment override must be persisted so a later process
# reuses the overridden value instead of reverting to the stale file value.
override_secret="$(openssl rand -hex 32)"
export DASHBOARD_SESSION_SECRET="${override_secret}"
run_start_dashboard
passed_secret="$(read_captured_secret)"
if [ "${passed_secret}" != "${override_secret}" ]; then
    echo "FAIL: override start passed '${passed_secret}', expected '${override_secret}'" >&2
    exit 1
fi
file_secret="$(read_file_secret)"
if [ "${file_secret}" != "${override_secret}" ]; then
    echo "FAIL: explicit override was not persisted to the env file (got '${file_secret}')" >&2
    exit 1
fi
if [ "$(grep -c '^DASHBOARD_SESSION_SECRET=' "${seq_env}")" -ne 1 ]; then
    echo "FAIL: override writeback must keep exactly one DASHBOARD_SESSION_SECRET field" >&2
    exit 1
fi
unset DASHBOARD_SESSION_SECRET || true
run_start_dashboard
if [ "$(read_captured_secret)" != "${override_secret}" ]; then
    echo "FAIL: later process reverted to the stale secret instead of the override" >&2
    exit 1
fi

echo "PASS: installer persists and reuses DASHBOARD_SESSION_SECRET for the dashboard"
