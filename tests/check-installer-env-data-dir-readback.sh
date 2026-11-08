#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"

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
AGENTTEAMS_LLM_PROVIDER=deepseek
AGENTTEAMS_DEFAULT_MODEL=deepseek-chat
AGENTTEAMS_WORKSPACE_DIR=/opt/agentteams-workspace
AGENTTEAMS_DATA_DIR=custom-vol
AGENTTEAMS_DASHBOARD=1
EOF

# Simulate a fresh upgrade run: nothing exported, values must come from the
# env file. AGENTTEAMS_DATA_DIR is the field that was historically missing
# from the readback; losing it silently re-creates a fresh data volume.
unset AGENTTEAMS_DATA_DIR AGENTTEAMS_LLM_PROVIDER AGENTTEAMS_DEFAULT_MODEL \
    AGENTTEAMS_WORKSPACE_DIR AGENTTEAMS_DASHBOARD

AGENTTEAMS_ENV_FILE="${env_file}"
load_current_params_from_env

for pair in \
    "AGENTTEAMS_DATA_DIR=custom-vol" \
    "AGENTTEAMS_LLM_PROVIDER=deepseek" \
    "AGENTTEAMS_DEFAULT_MODEL=deepseek-chat" \
    "AGENTTEAMS_WORKSPACE_DIR=/opt/agentteams-workspace" \
    "AGENTTEAMS_DASHBOARD=1"
do
    var="${pair%%=*}"
    expected="${pair#*=}"
    actual="${!var:-}"
    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected ${var} to read back as ${expected}, got '${actual}'" >&2
        exit 1
    fi
done

# An exported value must win over the env file (and must not break the
# function's exit status under the installer's set -e).
AGENTTEAMS_DATA_DIR="exported-vol"
load_current_params_from_env
if [ "${AGENTTEAMS_DATA_DIR}" != "exported-vol" ]; then
    echo "FAIL: exported AGENTTEAMS_DATA_DIR must not be overwritten by the env file" >&2
    exit 1
fi

# Env files written before the DATA_DIR field existed must read back empty
# (the deep defense then derives the volume from the live container).
grep -v '^AGENTTEAMS_DATA_DIR=' "${env_file}" > "${env_file}.old"
unset AGENTTEAMS_DATA_DIR
AGENTTEAMS_ENV_FILE="${env_file}.old"
load_current_params_from_env
if [ -n "${AGENTTEAMS_DATA_DIR:-}" ]; then
    echo "FAIL: expected empty AGENTTEAMS_DATA_DIR when the env file predates the field" >&2
    exit 1
fi

echo "PASS: installer reads AGENTTEAMS_DATA_DIR back from the env file on upgrade"

# ── Section 2: detect_installed_data_volume + prepare_data_volume ─────────
# Regression for the named-volume bug: for a docker named volume the
# reusable identifier is .Name, not .Source (which is
# /var/lib/docker/volumes/<name>/_data). Bind paths must keep their spaces.

eval "$(sed -n '/^detect_installed_data_volume() {/,/^}/p' "${INSTALLER}")"
eval "$(sed -n '/^prepare_data_volume() {/,/^}/p' "${INSTALLER}")"

if ! type detect_installed_data_volume >/dev/null 2>&1; then
    echo "FAIL: could not extract detect_installed_data_volume from the installer" >&2
    exit 1
fi
if ! type prepare_data_volume >/dev/null 2>&1; then
    echo "FAIL: could not extract prepare_data_volume from the installer" >&2
    exit 1
fi

# The inspect --format template must select .Name for named volumes and
# fall back to .Source only for other mount types.
grep -q '{{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}' "${INSTALLER}" || {
    echo "FAIL: inspect template must use .Name for named volumes, .Source otherwise" >&2
    exit 1
}

# Fake docker: canned rendered inspect output per FAKE_MODE; volume state in
# ${FAKE_VOLS}; volume invocations logged in ${FAKE_CALLS}.
FAKE_MODE="absent"
FAKE_VOLS="${workdir}/fake-vols"
FAKE_CALLS="${workdir}/fake-calls"
: > "${FAKE_VOLS}"
: > "${FAKE_CALLS}"
fake_docker() {
    if [ "$1" = "inspect" ]; then
        case "${FAKE_MODE}" in
            named) printf '%s\n' "custom-data" ;;
            bind)  printf '%s\n' "/data/my data" ;;
        esac
        return 0
    fi
    if [ "$1" = "volume" ]; then
        if [ "$2" = "ls" ]; then
            cat "${FAKE_VOLS}"
        elif [ "$2" = "create" ]; then
            echo "volume-create:$3" >> "${FAKE_CALLS}"
            echo "$3" >> "${FAKE_VOLS}"
        fi
    fi
    return 0
}
DOCKER_CMD="fake_docker"

# (1) Named volume: the identifier must be the volume name, not the
# /var/lib/docker/volumes/... internal path.
FAKE_MODE="named"
detected="$(detect_installed_data_volume)"
if [ "${detected}" != "custom-data" ]; then
    echo "FAIL: named volume detection must return the volume name, got '${detected}'" >&2
    exit 1
fi
case "${detected}" in
    /var/lib/docker/*) echo "FAIL: named volume detection leaked the internal path" >&2; exit 1 ;;
esac

# (2) Unchanged named volume must not falsely trigger the mismatch check:
# env value == detected identifier.
AGENTTEAMS_DATA_DIR="custom-data"
if [ "${AGENTTEAMS_DATA_DIR}" != "${detected}" ]; then
    echo "FAIL: unchanged named volume would falsely trigger the mismatch warning" >&2
    exit 1
fi

# (3) Missing env value: the fallback must store the volume name (a valid
# `docker volume create` argument), not the internal path.
AGENTTEAMS_DATA_DIR=""
AGENTTEAMS_DATA_DIR="${AGENTTEAMS_DATA_DIR:-$(detect_installed_data_volume)}"
if [ "${AGENTTEAMS_DATA_DIR}" != "custom-data" ]; then
    echo "FAIL: missing-value fallback must store the named volume name, got '${AGENTTEAMS_DATA_DIR}'" >&2
    exit 1
fi

# (4) Bind mount: the host path must be preserved verbatim, spaces included.
FAKE_MODE="bind"
detected="$(detect_installed_data_volume)"
if [ "${detected}" != "/data/my data" ]; then
    echo "FAIL: bind path detection must preserve spaces, got '${detected}'" >&2
    exit 1
fi

# (5) No container: detection returns empty (fresh install path).
FAKE_MODE="absent"
detected="$(detect_installed_data_volume)"
if [ -n "${detected}" ]; then
    echo "FAIL: detection without a controller must be empty, got '${detected}'" >&2
    exit 1
fi

# (6) prepare_data_volume: missing named volume is created once.
FAKE_MODE="absent"
AGENTTEAMS_DATA_DIR="custom-data"
prepare_data_volume
if [ "$(grep -c '^volume-create:custom-data$' "${FAKE_CALLS}")" != 1 ]; then
    echo "FAIL: missing named volume must be created exactly once" >&2
    exit 1
fi
if [ "${DATA_MOUNT_ARGS[0]}" != "-v" ] || [ "${DATA_MOUNT_ARGS[1]}" != "custom-data:/data" ]; then
    echo "FAIL: named volume mount args wrong: ${DATA_MOUNT_ARGS[*]}" >&2
    exit 1
fi

# (7) prepare_data_volume: existing named volume is reused, not recreated.
AGENTTEAMS_DATA_DIR="custom-data"
prepare_data_volume
if [ "$(grep -c '^volume-create:custom-data$' "${FAKE_CALLS}")" != 1 ]; then
    echo "FAIL: existing named volume must not be re-created" >&2
    exit 1
fi

# (8) prepare_data_volume: a bind path is never passed to `docker volume
# create`; the directory is created instead (spaces must survive).
AGENTTEAMS_DATA_DIR="${workdir}/my data"
prepare_data_volume
if grep -q "volume-create:${workdir}/my data" "${FAKE_CALLS}"; then
    echo "FAIL: bind path must never be passed to docker volume create" >&2
    exit 1
fi
if [ "$(grep -c '^volume-create:' "${FAKE_CALLS}")" != 1 ]; then
    echo "FAIL: bind path prepare must not create any volume (total create calls changed)" >&2
    exit 1
fi
if [ ! -d "${workdir}/my data" ]; then
    echo "FAIL: bind path directory was not created" >&2
    exit 1
fi
if [ "${DATA_MOUNT_ARGS[0]}" != "-v" ] || [ "${DATA_MOUNT_ARGS[1]}" != "${workdir}/my data:/data" ]; then
    echo "FAIL: bind path mount args wrong (spaces must survive): ${DATA_MOUNT_ARGS[*]}" >&2
    exit 1
fi

echo "PASS: data volume detection distinguishes named volumes from bind mounts"
