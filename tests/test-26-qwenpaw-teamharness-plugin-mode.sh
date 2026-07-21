#!/bin/bash
# test-26-qwenpaw-teamharness-plugin-mode.sh - Case 26: QwenPaw TeamHarness plugin-mode team work
#
# Verifies the real Team path:
#   controller -> qwenpaw leader/worker -> qwenpaw plugin install teamharness
#   -> TeamHarness MCP -> Matrix delegation -> shared task result.
#
# This is intentionally not just a plugin smoke test. The final pass condition
# is a real QwenPaw Team completing one delegated TeamHarness task.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "26-qwenpaw-teamharness-plugin-mode"
minio_setup

TEST_TEAM="test26-qwenpaw-team-$$"
TEST_LEADER="${TEST_TEAM}-leader"
TEST_WORKER="${TEST_TEAM}-worker"
TEST_MODEL="${AGENTTEAMS_E2E_MODEL:-${AGENTTEAMS_DEFAULT_MODEL:-${HICLAW_E2E_MODEL:-${HICLAW_DEFAULT_MODEL:-qwen3.7-max}}}}"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"
PROJECT_ID="test26-project-$$"
TASK_ID="test26-task-$$"
TEST_RUN_ID="$$_$(date +%s)"
MARKER="TEST26_TEAMHARNESS_PLUGIN_MODE_${TEST_RUN_ID}"
BOOTSTRAP_LEADER_MARKER="TEST26_QWENPAW_BOOTSTRAP_LEADER_${TEST_RUN_ID}"
BOOTSTRAP_WORKER_MARKER="TEST26_QWENPAW_BOOTSTRAP_WORKER_${TEST_RUN_ID}"
LEADER_PACKAGE_V1_MARKER="TEST26_LEADER_PACKAGE_V1_${TEST_RUN_ID}"
WORKER_PACKAGE_V1_MARKER="TEST26_WORKER_PACKAGE_V1_${TEST_RUN_ID}"
LEADER_PACKAGE_V2_MARKER="TEST26_LEADER_PACKAGE_V2_${TEST_RUN_ID}"
WORKER_PACKAGE_V2_MARKER="TEST26_WORKER_PACKAGE_V2_${TEST_RUN_ID}"
DONE_LINE="TEST26_TEAMHARNESS_DONE ${TASK_ID} ${MARKER}"
LEADER_CONTAINER="$(worker_container_name "${TEST_LEADER}")"
WORKER_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
K8S_NAMESPACE="${AGENTTEAMS_E2E_NAMESPACE:-${HICLAW_E2E_NAMESPACE:-default}}"
LEADER_PACKAGE_V1_OBJECT="agentteams-config/packages/${TEST_LEADER}-v1.tar.gz"
WORKER_PACKAGE_V1_OBJECT="agentteams-config/packages/${TEST_WORKER}-v1.tar.gz"
LEADER_PACKAGE_V2_OBJECT="agentteams-config/packages/${TEST_LEADER}-v2.tar.gz"
WORKER_PACKAGE_V2_OBJECT="agentteams-config/packages/${TEST_WORKER}-v2.tar.gz"
LEADER_PACKAGE_V1_URI="oss://${LEADER_PACKAGE_V1_OBJECT}"
WORKER_PACKAGE_V1_URI="oss://${WORKER_PACKAGE_V1_OBJECT}"
LEADER_PACKAGE_V2_URI="oss://${LEADER_PACKAGE_V2_OBJECT}"
WORKER_PACKAGE_V2_URI="oss://${WORKER_PACKAGE_V2_OBJECT}"
TEST_MCP_NAME="docs"
TEST_MCP_URL="https://gateway.example.com/mcp/docs"
TEST_MCP_TRANSPORT="http"
TEST_PACKAGE_MCP_NAME="package-docs"
TEST_PACKAGE_MCP_URL="https://package.example.com/mcp"
QWENPAW_WORKER_IMAGE=""
CONTROLLER_NAME=""
ADMIN_TOKEN=""
TEAM_ROOM=""
LEADER_DM=""
LEADER_MXID=""
WORKER_MXID=""
LEADER_CONTAINER_ID_BEFORE_UPDATE=""
WORKER_CONTAINER_ID_BEFORE_UPDATE=""
LEADER_CONTAINER_STARTED_BEFORE_UPDATE=""
WORKER_CONTAINER_STARTED_BEFORE_UPDATE=""

_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed - preserving test26 resources for debugging"
        log_info "  Team: ${TEST_TEAM}"
        log_info "  Leader container: ${LEADER_CONTAINER}"
        log_info "  Worker container: ${WORKER_CONTAINER}"
        [ -n "${TEAM_ROOM}" ] && log_info "  Team Room: ${TEAM_ROOM}"
        [ -n "${LEADER_DM}" ] && log_info "  Leader DM: ${LEADER_DM}"
        return
    fi

    log_info "Cleaning up test26 resources"
    _k8s_delete "teams" "${TEST_TEAM}" >/dev/null 2>&1 || true
    _k8s_delete "workers" "${TEST_LEADER}" >/dev/null 2>&1 || true
    _k8s_delete "workers" "${TEST_WORKER}" >/dev/null 2>&1 || true
    sleep 5
    remove_worker_container "${TEST_LEADER}"
    remove_worker_container "${TEST_WORKER}"
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/teams/${TEST_TEAM}/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_LEADER}/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_LEADER}/runtime/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/runtime/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/shared/tasks/${TASK_ID}/" 2>/dev/null || true
    for object in \
        "${LEADER_PACKAGE_V1_OBJECT}" \
        "${WORKER_PACKAGE_V1_OBJECT}" \
        "${LEADER_PACKAGE_V2_OBJECT}" \
        "${WORKER_PACKAGE_V2_OBJECT}"; do
        exec_in_manager mc rm --force "${STORAGE_PREFIX}/${object}" 2>/dev/null || true
    done
}
trap _cleanup EXIT

_controller_env() {
    local key="$1"
    docker exec "${TEST_CONTROLLER_CONTAINER:-agentteams-controller}" printenv "${key}" 2>/dev/null || true
}

_container_env() {
    local container="$1"
    docker exec "${container}" sh -c "tr '\\0' '\\n' < /proc/1/environ" 2>/dev/null || true
}

_env_value() {
    local env_text="$1"
    local key="$2"
    printf '%s\n' "${env_text}" | grep "^${key}=" | head -1 | cut -d= -f2-
}

_controller_labels_json() {
    if [ -n "${CONTROLLER_NAME}" ]; then
        jq -nc --arg controller "${CONTROLLER_NAME}" '{"agentteams.io/controller":$controller}'
    else
        printf '{}\n'
    fi
}

_k8s_api() {
    local method="$1"
    local content_type="$2"
    local path="$3"

    if [ "${method}" = "GET" ] || [ "${method}" = "DELETE" ]; then
        docker exec "${TEST_CONTROLLER_CONTAINER:-agentteams-controller}" sh -c '
            token="$(cut -d, -f1 /data/agentteams-controller/pki/token.csv)"
            curl -ksS -X "$1" \
                -H "Authorization: Bearer ${token}" \
                "https://127.0.0.1:6443$2"
        ' sh "${method}" "${path}"
        return $?
    fi

    docker exec -i "${TEST_CONTROLLER_CONTAINER:-agentteams-controller}" sh -c '
        token="$(cut -d, -f1 /data/agentteams-controller/pki/token.csv)"
        curl -ksS -X "$1" \
            -H "Authorization: Bearer ${token}" \
            -H "Content-Type: $2" \
            --data-binary @- \
            "https://127.0.0.1:6443$3"
    ' sh "${method}" "${content_type}" "${path}"
}

_k8s_resource_path() {
    local plural="$1"
    local name="${2:-}"
    local path="/apis/hiclaw.io/v1beta1/namespaces/${K8S_NAMESPACE}/${plural}"
    if [ -n "${name}" ]; then
        path="${path}/${name}"
    fi
    printf '%s\n' "${path}"
}

_k8s_get() {
    local plural="$1"
    local name="$2"
    _k8s_api GET "" "$(_k8s_resource_path "${plural}" "${name}")"
}

_k8s_delete() {
    local plural="$1"
    local name="$2"
    _k8s_api DELETE "" "$(_k8s_resource_path "${plural}" "${name}")"
}

_k8s_create() {
    local plural="$1"
    local body="$2"
    printf '%s' "${body}" | _k8s_api POST "application/json" "$(_k8s_resource_path "${plural}")"
}

_k8s_patch_merge() {
    local plural="$1"
    local name="$2"
    local body="$3"
    printf '%s' "${body}" | _k8s_api PATCH "application/merge-patch+json" "$(_k8s_resource_path "${plural}" "${name}")"
}

_wait_k8s_jq() {
    local plural="$1"
    local name="$2"
    local filter="$3"
    local timeout="${4:-180}"
    local elapsed=0
    local body=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        body="$(_k8s_get "${plural}" "${name}" 2>/dev/null || echo "{}")"
        if printf '%s\n' "${body}" | jq -e "${filter}" >/dev/null 2>&1; then
            printf '%s\n' "${body}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    printf '%s\n' "${body}"
    return 1
}

_container_has_cmdline() {
    local container="$1"
    local pattern="$2"
    docker exec "${container}" sh -c '
        for f in /proc/[0-9]*/cmdline; do
            tr "\0" " " < "$f"
            echo
        done | grep -q "$1"
    ' sh "${pattern}" >/dev/null 2>&1
}

_agent_api() {
    local container="$1"
    local method="$2"
    local path="$3"
    docker exec "${container}" sh -c '
        port="${AGENTTEAMS_CONSOLE_PORT:-${HICLAW_CONSOLE_PORT:-8088}}"
        curl -sf -X "'"${method}"'" "http://127.0.0.1:${port}'"${path}"'"
    ' 2>/dev/null
}

_wait_agent_api_ok() {
    local container="$1"
    local method="$2"
    local path="$3"
    local filter="$4"
    local timeout="${5:-180}"
    local elapsed=0
    while [ "${elapsed}" -lt "${timeout}" ]; do
        local body
        body=$(_agent_api "${container}" "${method}" "${path}" 2>/dev/null || true)
        if [ -n "${body}" ] && echo "${body}" | jq -e "${filter}" >/dev/null 2>&1; then
            printf '%s\n' "${body}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_wait_container_file() {
    local container="$1"
    local path="$2"
    local timeout="${3:-120}"
    local elapsed=0
    while [ "${elapsed}" -lt "${timeout}" ]; do
        if docker exec "${container}" test -f "${path}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_yaml_to_json() {
    ruby -ryaml -rjson -e 'puts JSON.generate(YAML.load(STDIN.read) || {})'
}

_qwenpaw_mcp_call() {
    local container="$1"
    local tool="$2"
    local args_json="$3"
    local output
    output=$(docker exec -i \
        -e TEST_MCP_TOOL="${tool}" \
        -e TEST_MCP_ARGS="${args_json}" \
        "${container}" \
        /opt/venv/qwenpaw/bin/python - <<'PY'
import json
import os
import subprocess
import sys
from pathlib import Path

for item in Path("/proc/1/environ").read_bytes().split(b"\0"):
    if item and b"=" in item:
        key, value = item.split(b"=", 1)
        os.environ.setdefault(key.decode(), value.decode())

workspace = Path(os.environ["QWENPAW_WORKING_DIR"]) / "workspaces" / "default"
agent_config = json.loads((workspace / "agent.json").read_text(encoding="utf-8"))
client = agent_config["mcp"]["clients"]["teamharness"]

env = dict(os.environ)
env.update({str(k): str(v) for k, v in (client.get("env") or {}).items()})
request = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
        "name": os.environ["TEST_MCP_TOOL"],
        "arguments": json.loads(os.environ["TEST_MCP_ARGS"]),
    },
}
proc = subprocess.run(
    [client["command"], *client.get("args", [])],
    input=json.dumps(request) + "\n",
    text=True,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    cwd=client.get("cwd") or None,
    env=env,
)
if proc.returncode != 0:
    print(proc.stderr, file=sys.stderr)
    print(proc.stdout, file=sys.stderr)
    raise SystemExit(proc.returncode)
lines = [line for line in proc.stdout.splitlines() if line.strip()]
if not lines:
    print("empty MCP response", file=sys.stderr)
    raise SystemExit(1)
response = json.loads(lines[-1])
if response.get("error"):
    print(json.dumps(response, ensure_ascii=False), file=sys.stderr)
    raise SystemExit(1)
print(response["result"]["content"][0]["text"])
PY
    ) || {
        printf '%s\n' "${output}" >&2
        return 1
    }
    printf '%s\n' "${output}" | python3 -c '
import json
import sys

for line in sys.stdin:
    text = line.strip()
    if not text.startswith("{"):
        continue
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        continue
    if isinstance(parsed, dict) and ("ok" in parsed or "tool" in parsed or "error" in parsed):
        print(json.dumps(parsed, ensure_ascii=False))
        raise SystemExit(0)
print("no JSON MCP payload found", file=sys.stderr)
raise SystemExit(1)
'
}

_leader_mcp_call() {
    _qwenpaw_mcp_call "${LEADER_CONTAINER}" "$@"
}

_worker_mcp_call() {
    _qwenpaw_mcp_call "${WORKER_CONTAINER}" "$@"
}

_wait_leader_shared_stat() {
    local path="$1"
    local timeout="${2:-120}"
    local elapsed=0
    local stat_args
    stat_args=$(jq -nc --arg path "${path}" '{action:"stat", path:$path}')
    while [ "${elapsed}" -lt "${timeout}" ]; do
        local result
        result=$(_leader_mcp_call filesync "${stat_args}" 2>/dev/null || echo "{}")
        if echo "${result}" | jq -e '.ok == true and .exists == true' >/dev/null 2>&1; then
            printf '%s\n' "${result}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_wait_runtime_package_ref() {
    local member="$1"
    local expected="$2"
    local timeout="${3:-180}"
    local elapsed=0
    local yaml=""
    local json="{}"
    while [ "${elapsed}" -lt "${timeout}" ]; do
        yaml="$(minio_read_file "agents/${member}/runtime/runtime.yaml" 2>/dev/null || true)"
        json="$(printf '%s' "${yaml}" | _yaml_to_json 2>/dev/null || echo "{}")"
        if printf '%s\n' "${json}" | jq -e --arg package "${expected}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
            printf '%s\n' "${yaml}"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    printf '%s\n' "${yaml}"
    return 1
}

_wait_workspace_marker() {
    local container="$1"
    local path="$2"
    local expected="$3"
    local timeout="${4:-180}"
    local elapsed=0
    while [ "${elapsed}" -lt "${timeout}" ]; do
        if docker exec "${container}" sh -c "grep -Fq '$expected' '$path'" >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_assert_runtime_yaml_has_no_secret_values() {
    local yaml="$1"
    local container="$2"
    local env_text
    env_text="$(_container_env "${container}")"
    for key in AGENTTEAMS_WORKER_MATRIX_TOKEN AGENTTEAMS_WORKER_GATEWAY_KEY AGENTTEAMS_FS_SECRET_KEY; do
        local value
        value="$(_env_value "${env_text}" "${key}")"
        if [ -n "${value}" ] && [ "${#value}" -ge 4 ] && printf '%s' "${yaml}" | grep -Fq "${value}"; then
            log_fail "runtime.yaml leaks secret value from ${key}"
        else
            log_pass "runtime.yaml does not leak ${key} value"
        fi
    done
    if printf '%s\n' "${yaml}" | grep -Eq '^[[:space:]]*(accessKey|fsAccessKey|storageAccessKey):[[:space:]]*'; then
        log_fail "runtime.yaml writes storage access key value instead of env reference"
    else
        log_pass "runtime.yaml references storage access key by env name only"
    fi
}

_dump_debug_snapshot() {
    log_info "Debug snapshot for test26"
    for container in "${LEADER_CONTAINER}" "${WORKER_CONTAINER}"; do
        if docker ps -a --format '{{.Names}}' | grep -q "^${container}$"; then
            log_info "Logs tail: ${container}"
            docker logs --tail 120 "${container}" 2>&1 || true
            log_info "TeamHarness status: ${container}"
            _agent_api "${container}" GET /api/teamharness/status || true
        fi
    done
    if [ -n "${ADMIN_TOKEN}" ] && [ -n "${TEAM_ROOM}" ]; then
        log_info "Recent Team Room messages:"
        matrix_read_messages "${ADMIN_TOKEN}" "${TEAM_ROOM}" 30 2>/dev/null | \
            jq -r '.chunk[] | select(.type == "m.room.message") | "\(.sender): \(.content.body // "")"' || true
    fi
    exec_in_manager mc ls --recursive "${STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/" 2>/dev/null || true
    exec_in_manager mc ls --recursive "${STORAGE_PREFIX}/shared/tasks/${TASK_ID}/" 2>/dev/null || true
}

_create_test_agentspec_package() {
    local role="$1"
    local version="$2"
    local marker="$3"
    local object="$4"
    local bootstrap_marker="${BOOTSTRAP_WORKER_MARKER}"
    if [ "${role}" = "leader" ]; then
        bootstrap_marker="${BOOTSTRAP_LEADER_MARKER}"
    fi

    exec_in_manager bash -c "set -eu
        work='/tmp/agentteams-test26-package-${TEST_TEAM}-${role}-${version}'
        archive='/tmp/${TEST_TEAM}-${role}-${version}-agentspec.tar.gz'
        rm -rf \"\${work}\" \"\${archive}\"
        mkdir -p \"\${work}/config/config\" \"\${work}/config/bootstrap\" \"\${work}/config/materials\" \"\${work}/skills/test26-package-skill\"
        cat >\"\${work}/manifest.json\" <<'EOF'
{\"name\":\"test26-${role}-agentspec\",\"version\":\"${version}\"}
EOF
        cat >\"\${work}/mcp.json\" <<'EOF'
{\"mcpServers\":{\"package-docs\":{\"url\":\"https://package.example.com/mcp\",\"transport\":\"http\",\"description\":\"test26 package MCP\"}}}
EOF
        cat >\"\${work}/config/AGENTS.md\" <<'EOF'
# TEST26 AgentSpec Package

This AGENTS.md came from the QwenPaw AgentSpec package.
Role: ${role}
Version: ${version}
Marker: ${marker}
EOF
        cat >\"\${work}/config/SOUL.md\" <<'EOF'
# TEST26 AgentSpec Package Soul

This SOUL.md came from the QwenPaw AgentSpec package.
Role: ${role}
Version: ${version}
Marker: ${marker}
EOF
        cat >\"\${work}/config/config/credagent.json\" <<'EOF'
{\"output_sanitize\":[]}
EOF
        cat >\"\${work}/config/config/settings.yaml\" <<'EOF'
test26: true
role: ${role}
version: ${version}
EOF
        cat >\"\${work}/config/hot-update-marker.txt\" <<'EOF'
${marker}
EOF
        cat >\"\${work}/config/BOOTSTRAP.md\" <<'EOF'
# TEST26 AgentSpec Package Bootstrap

This BOOTSTRAP.md came from the QwenPaw AgentSpec package root.

This bootstrap is non-interactive. Do not ask the user to confirm identity.
Complete these steps immediately before handling the user's request:

1. Run the packaged hello script:
   sh bootstrap/test26-hello.sh

2. Reply with exactly this marker:

${bootstrap_marker}
EOF
        cat >\"\${work}/config/bootstrap/test26-seed.md\" <<'EOF'
# TEST26 AgentSpec Config Bootstrap

This config/bootstrap file came from the QwenPaw AgentSpec package.
EOF
        cat >\"\${work}/config/bootstrap/test26-hello.sh\" <<'EOF'
#!/bin/sh
set -eu

marker='${bootstrap_marker}'
role=\"\${AGENTTEAMS_WORKER_ROLE:-\${AGENTTEAMS_AGENT_ROLE:-unknown}}\"

mkdir -p bootstrap
printf '%s\n' \"\${marker}\" > bootstrap/test26-hello-result.txt
printf '%s\n' \"\${role}\" > bootstrap/test26-hello-role.txt
EOF
        chmod +x \"\${work}/config/bootstrap/test26-hello.sh\"
        cat >\"\${work}/config/materials/test26-note.md\" <<'EOF'
# TEST26 AgentSpec Custom Material

This custom material came from the QwenPaw AgentSpec package root.
EOF
        cat >\"\${work}/skills/test26-package-skill/SKILL.md\" <<'EOF'
---
name: test26-package-skill
description: Marks that the AgentSpec package skill was installed into the QwenPaw workspace.
---

# TEST26 Package Skill

This skill came from the QwenPaw AgentSpec package.
EOF
        tar -czf \"\${archive}\" -C \"\${work}\" .
        mc cp \"\${archive}\" '${STORAGE_PREFIX}/${object}' >/dev/null
    "
}

_create_qwenpaw_worker_cr() {
    local name="$1"
    local package_uri="$2"
    local soul="$3"
    local labels
    local body
    labels="$(_controller_labels_json)"
    body=$(jq -nc \
        --arg name "${name}" \
        --arg namespace "${K8S_NAMESPACE}" \
        --arg model "${TEST_MODEL}" \
        --arg package "${package_uri}" \
        --arg mcp_name "${TEST_MCP_NAME}" \
        --arg mcp_url "${TEST_MCP_URL}" \
        --arg mcp_transport "${TEST_MCP_TRANSPORT}" \
        --arg soul "${soul}" \
        --argjson labels "${labels}" \
        '{
            apiVersion:"hiclaw.io/v1beta1",
            kind:"Worker",
            metadata:{name:$name, namespace:$namespace, labels:$labels},
            spec:{
                runtime:"qwenpaw",
                model:$model,
                package:$package,
                mcpServers:[{name:$mcp_name, url:$mcp_url, transport:$mcp_transport}],
                soul:$soul
            }
        }')
    _k8s_create "workers" "${body}"
}

_create_decoupled_team_cr() {
    local labels
    local body
    labels="$(_controller_labels_json)"
    body=$(jq -nc \
        --arg name "${TEST_TEAM}" \
        --arg namespace "${K8S_NAMESPACE}" \
        --arg leader "${TEST_LEADER}" \
        --arg worker "${TEST_WORKER}" \
        --argjson labels "${labels}" \
        '{
            apiVersion:"hiclaw.io/v1beta1",
            kind:"Team",
            metadata:{name:$name, namespace:$namespace, labels:$labels},
            spec:{
                teamName:$name,
                workerMembers:[
                    {name:$leader, role:"team_leader"},
                    {name:$worker, role:"worker"}
                ]
            }
        }')
    _k8s_create "teams" "${body}"
}

_patch_worker_package_ref() {
    local name="$1"
    local package_uri="$2"
    local body
    body=$(jq -nc --arg package "${package_uri}" '{spec:{package:$package}}')
    _k8s_patch_merge "workers" "${name}" "${body}"
}

# ============================================================
# Section 1: Image baseline
# ============================================================
log_section "QwenPaw Image Plugin Package Baseline"

QWENPAW_WORKER_IMAGE="$(_controller_env AGENTTEAMS_QWENPAW_WORKER_IMAGE)"
QWENPAW_WORKER_IMAGE="${AGENTTEAMS_E2E_QWENPAW_WORKER_IMAGE:-${HICLAW_E2E_QWENPAW_WORKER_IMAGE:-${QWENPAW_WORKER_IMAGE:-agentteams/qwenpaw-worker:latest}}}"
CONTROLLER_NAME="$(_controller_env AGENTTEAMS_CONTROLLER_NAME)"

if docker image inspect "${QWENPAW_WORKER_IMAGE}" >/dev/null 2>&1; then
    log_pass "QwenPaw worker image exists: ${QWENPAW_WORKER_IMAGE}"
else
    log_fail "QwenPaw worker image missing: ${QWENPAW_WORKER_IMAGE} (run make build-qwenpaw-worker)"
    test_teardown "26-qwenpaw-teamharness-plugin-mode"; test_summary; exit $?
fi

if docker run --rm --entrypoint qwenpaw-worker "${QWENPAW_WORKER_IMAGE}" --help >/dev/null 2>&1; then
    log_pass "qwenpaw-worker --help works in image"
else
    log_fail "qwenpaw-worker --help failed in image"
fi

if docker run --rm --entrypoint qwenpaw "${QWENPAW_WORKER_IMAGE}" --help >/dev/null 2>&1; then
    log_pass "qwenpaw --help works in image"
else
    log_fail "qwenpaw --help failed in image"
fi

IMAGE_PACKAGE_CHECK=$(docker run --rm -i --entrypoint /opt/venv/qwenpaw/bin/python "${QWENPAW_WORKER_IMAGE}" - <<'PY' 2>&1
from pathlib import Path
import zipfile

zip_path = Path("/opt/agentteams/plugins/teamharness-qwenpaw.zip")
assert zip_path.is_file(), zip_path
with zipfile.ZipFile(zip_path) as archive:
    names = set(archive.namelist())
    assert any(name.endswith("/plugin.json") for name in names)
    assert any(name.endswith("/plugin.py") for name in names)
    assert any(name.endswith("/teamharness/plugin.yaml") for name in names)
    assert any(name.endswith("/teamharness/prompts/team/TEAMS.md") for name in names)
    assert any(name.endswith("/teamharness/mcp/server.py") for name in names)
    assert not any("agentteam" in name for name in names)
print("ok")
PY
)
IMAGE_PACKAGE_CHECK=$(printf '%s\n' "${IMAGE_PACKAGE_CHECK}" | tail -n 1)
assert_eq "ok" "${IMAGE_PACKAGE_CHECK}" "Image carries opaque TeamHarness QwenPaw plugin zip"

# ============================================================
# Section 2: Create real QwenPaw Team
# ============================================================
log_section "Create QwenPaw Team"

if _create_test_agentspec_package "leader" "v1" "${LEADER_PACKAGE_V1_MARKER}" "${LEADER_PACKAGE_V1_OBJECT}" && \
   _create_test_agentspec_package "worker" "v1" "${WORKER_PACKAGE_V1_MARKER}" "${WORKER_PACKAGE_V1_OBJECT}" && \
   _create_test_agentspec_package "leader" "v2" "${LEADER_PACKAGE_V2_MARKER}" "${LEADER_PACKAGE_V2_OBJECT}" && \
   _create_test_agentspec_package "worker" "v2" "${WORKER_PACKAGE_V2_MARKER}" "${WORKER_PACKAGE_V2_OBJECT}"; then
    log_pass "Leader and worker AgentSpec package versions uploaded to MinIO"
else
    log_fail "Failed to upload test AgentSpec packages to MinIO"
fi

LEADER_SOUL=$(cat <<EOF
# ${TEST_LEADER}

You are the leader for ${TEST_TEAM}. Use TeamHarness tools for project
planning, task delegation, shared files, and team-room messages. Delegate
worker tasks instead of completing them yourself.
EOF
)
WORKER_SOUL=$(cat <<EOF
# ${TEST_WORKER}

You are a QwenPaw worker for ${TEST_TEAM}. When assigned a TeamHarness
task, acknowledge it, use the shared task spec, write requested
deliverables, submit the task, and reply in the Team Room.
EOF
)

CREATE_LEADER_OUTPUT=$(_create_qwenpaw_worker_cr "${TEST_LEADER}" "${LEADER_PACKAGE_V1_URI}" "${LEADER_SOUL}" 2>&1 || true)
if echo "${CREATE_LEADER_OUTPUT}" | jq -e --arg name "${TEST_LEADER}" '.metadata.name == $name' >/dev/null 2>&1; then
    log_pass "Leader Worker CR created through Kubernetes API"
else
    log_fail "Leader Worker CR create failed: ${CREATE_LEADER_OUTPUT}"
fi

CREATE_WORKER_OUTPUT=$(_create_qwenpaw_worker_cr "${TEST_WORKER}" "${WORKER_PACKAGE_V1_URI}" "${WORKER_SOUL}" 2>&1 || true)
if echo "${CREATE_WORKER_OUTPUT}" | jq -e --arg name "${TEST_WORKER}" '.metadata.name == $name' >/dev/null 2>&1; then
    log_pass "Worker Worker CR created through Kubernetes API"
else
    log_fail "Worker Worker CR create failed: ${CREATE_WORKER_OUTPUT}"
fi

CREATE_TEAM_OUTPUT=$(_create_decoupled_team_cr 2>&1 || true)
if echo "${CREATE_TEAM_OUTPUT}" | jq -e --arg name "${TEST_TEAM}" '.metadata.name == $name and (.spec.workerMembers | length == 2)' >/dev/null 2>&1; then
    log_pass "Decoupled Team CR created through Kubernetes API"
else
    log_fail "Decoupled Team CR create failed: ${CREATE_TEAM_OUTPUT}"
fi

TEAM_JSON=$(_wait_k8s_jq "teams" "${TEST_TEAM}" '.status.phase == "Active"' 300 2>/dev/null || echo "{}")
if echo "${TEAM_JSON}" | jq -e '.status.phase == "Active"' >/dev/null 2>&1; then
    log_pass "Team is Active"
else
    log_fail "Team did not become Active"
fi

TEAM_ROOM=$(echo "${TEAM_JSON}" | jq -r '.status.teamRoomID // empty')
LEADER_DM=$(echo "${TEAM_JSON}" | jq -r '.status.leaderDMRoomID // empty')
assert_not_empty "${TEAM_ROOM}" "Team Room ID available"
assert_not_empty "${LEADER_DM}" "Leader DM Room ID available"

for member in "${TEST_LEADER}" "${TEST_WORKER}"; do
    MEMBER_JSON=$(_wait_k8s_jq "workers" "${member}" '.status.roomID and .status.matrixUserID' 240 2>/dev/null || echo "{}")
    if echo "${MEMBER_JSON}" | jq -e '.status.roomID and .status.matrixUserID' >/dev/null 2>&1; then
        log_pass "Member ${member} provisioned"
    else
        log_fail "Member ${member} not provisioned"
    fi
    MEMBER_JSON=$(_wait_k8s_jq "workers" "${member}" '.status.phase == "Running"' 240 2>/dev/null || echo "{}")
    if echo "${MEMBER_JSON}" | jq -e '.status.phase == "Running"' >/dev/null 2>&1; then
        log_pass "Member ${member} is Running"
    else
        log_fail "Member ${member} did not reach Running"
    fi
    if wait_for_worker_container "${member}" 240; then
        log_pass "Container for ${member} is running"
    else
        log_fail "Container for ${member} did not start"
    fi
done

LEADER_JSON=$(_k8s_get "workers" "${TEST_LEADER}" 2>/dev/null || echo "{}")
WORKER_JSON=$(_k8s_get "workers" "${TEST_WORKER}" 2>/dev/null || echo "{}")
LEADER_MXID=$(echo "${LEADER_JSON}" | jq -r '.status.matrixUserID // empty')
WORKER_MXID=$(echo "${WORKER_JSON}" | jq -r '.status.matrixUserID // empty')
assert_eq "qwenpaw" "$(echo "${LEADER_JSON}" | jq -r '.spec.runtime // empty')" "Leader runtime is qwenpaw"
assert_eq "qwenpaw" "$(echo "${WORKER_JSON}" | jq -r '.spec.runtime // empty')" "Worker runtime is qwenpaw"
assert_not_empty "${LEADER_MXID}" "Leader Matrix ID available"
assert_not_empty "${WORKER_MXID}" "Worker Matrix ID available"

assert_eq "${QWENPAW_WORKER_IMAGE}" "$(docker inspect --format '{{.Config.Image}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")" "Leader uses QwenPaw image"
assert_eq "${QWENPAW_WORKER_IMAGE}" "$(docker inspect --format '{{.Config.Image}}' "${WORKER_CONTAINER}" 2>/dev/null || echo "")" "Worker uses QwenPaw image"

if _container_has_cmdline "${LEADER_CONTAINER}" "qwenpaw app --host"; then
    log_pass "Leader qwenpaw app process is running"
else
    log_fail "Leader qwenpaw app process is not running"
fi
if _container_has_cmdline "${WORKER_CONTAINER}" "qwenpaw app --host"; then
    log_pass "Worker qwenpaw app process is running"
else
    log_fail "Worker qwenpaw app process is not running"
fi

# ============================================================
# Section 3: Controller runtime.yaml projection
# ============================================================
log_section "Runtime Config Projection"

if minio_wait_for_file "agents/${TEST_LEADER}/runtime/runtime.yaml" 120; then
    log_pass "Leader runtime.yaml written"
else
    log_fail "Leader runtime.yaml missing"
fi
if minio_wait_for_file "agents/${TEST_WORKER}/runtime/runtime.yaml" 120; then
    log_pass "Worker runtime.yaml written"
else
    log_fail "Worker runtime.yaml missing"
fi

LEADER_RUNTIME_YAML=$(minio_read_file "agents/${TEST_LEADER}/runtime/runtime.yaml")
WORKER_RUNTIME_YAML=$(minio_read_file "agents/${TEST_WORKER}/runtime/runtime.yaml")
LEADER_RUNTIME_JSON=$(printf '%s' "${LEADER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null || echo "{}")
WORKER_RUNTIME_JSON=$(printf '%s' "${WORKER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null || echo "{}")

if echo "${LEADER_RUNTIME_JSON}" | jq -e --arg member "${TEST_LEADER}" \
    '.member.runtimeName == $member and .member.runtime == "qwenpaw"' >/dev/null 2>&1; then
    log_pass "Leader runtime.yaml contains QwenPaw worker facts"
else
    log_fail "Leader runtime.yaml missing QwenPaw worker facts"
fi

if echo "${WORKER_RUNTIME_JSON}" | jq -e --arg member "${TEST_WORKER}" \
    '.member.runtimeName == $member and .member.runtime == "qwenpaw"' >/dev/null 2>&1; then
    log_pass "Worker runtime.yaml contains QwenPaw worker facts"
else
    log_fail "Worker runtime.yaml missing QwenPaw worker facts"
fi

if echo "${LEADER_RUNTIME_JSON}" | jq -e '.desired.model.model and .credentials.matrixTokenEnv and .credentials.gatewayKeyEnv and .credentials.storageSecretKeyEnv' >/dev/null 2>&1 && \
   echo "${WORKER_RUNTIME_JSON}" | jq -e '.desired.model.model and .credentials.matrixTokenEnv and .credentials.gatewayKeyEnv and .credentials.storageSecretKeyEnv' >/dev/null 2>&1; then
    log_pass "runtime.yaml contains desired model and credential env names"
else
    log_fail "runtime.yaml missing desired model or credential env names"
fi

if echo "${LEADER_RUNTIME_JSON}" | jq -e --arg name "${TEST_MCP_NAME}" --arg url "${TEST_MCP_URL}" --arg transport "${TEST_MCP_TRANSPORT}" '.desired.mcpServers[] | select(.name == $name and .url == $url and .transport == $transport)' >/dev/null 2>&1 && \
   echo "${WORKER_RUNTIME_JSON}" | jq -e --arg name "${TEST_MCP_NAME}" --arg url "${TEST_MCP_URL}" --arg transport "${TEST_MCP_TRANSPORT}" '.desired.mcpServers[] | select(.name == $name and .url == $url and .transport == $transport)' >/dev/null 2>&1; then
    log_pass "runtime.yaml projects controller MCP servers"
else
    log_fail "runtime.yaml missing controller MCP servers"
fi

if echo "${LEADER_RUNTIME_JSON}" | jq -e --arg package "${LEADER_PACKAGE_V1_URI}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1 && \
   echo "${WORKER_RUNTIME_JSON}" | jq -e --arg package "${WORKER_PACKAGE_V1_URI}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
    log_pass "runtime.yaml projects role-specific AgentSpec package refs"
else
    log_fail "runtime.yaml missing role-specific AgentSpec package refs"
fi

if echo "${LEADER_RUNTIME_JSON}" | jq -e --arg leader "${TEST_LEADER}" --arg worker "${TEST_WORKER}" --arg workerMxid "${WORKER_MXID}" \
    '.team.members[] | select(.runtimeName == $leader and .role == "team_leader")' >/dev/null 2>&1 && \
   echo "${LEADER_RUNTIME_JSON}" | jq -e --arg worker "${TEST_WORKER}" --arg workerMxid "${WORKER_MXID}" \
    '.team.members[] | select(.runtimeName == $worker and .role == "worker" and .matrixUserId == $workerMxid)' >/dev/null 2>&1 && \
   echo "${WORKER_RUNTIME_JSON}" | jq -e --arg leader "${TEST_LEADER}" --arg worker "${TEST_WORKER}" \
    '([.team.members[].runtimeName] | index($leader) and index($worker))' >/dev/null 2>&1; then
    log_pass "runtime.yaml projects TeamHarness roster facts"
else
    log_fail "runtime.yaml missing TeamHarness roster facts"
fi

_assert_runtime_yaml_has_no_secret_values "${LEADER_RUNTIME_YAML}${WORKER_RUNTIME_YAML}" "${WORKER_CONTAINER}"

LEADER_ENV="$(_container_env "${LEADER_CONTAINER}")"
WORKER_ENV="$(_container_env "${WORKER_CONTAINER}")"
assert_eq "standalone" "$(_env_value "${LEADER_ENV}" AGENTTEAMS_WORKER_ROLE)" "Decoupled leader Worker daemon role remains standalone"
assert_eq "standalone" "$(_env_value "${WORKER_ENV}" AGENTTEAMS_WORKER_ROLE)" "Decoupled worker daemon role remains standalone"

# ============================================================
# Section 4: QwenPaw plugin-mode install and TeamHarness assets
# ============================================================
log_section "QwenPaw Plugin Mode"

for container in "${LEADER_CONTAINER}" "${WORKER_CONTAINER}"; do
    if _wait_agent_api_ok "${container}" GET /api/teamharness/health '.ok == true and .plugin == "teamharness" and .adapter == "qwenpaw"' 240 >/dev/null; then
        log_pass "${container} TeamHarness health endpoint is healthy"
    else
        log_fail "${container} TeamHarness health endpoint did not become healthy"
    fi
    if _wait_agent_api_ok "${container}" POST /api/teamharness/sync '.ok == true' 120 >/dev/null; then
        log_pass "${container} TeamHarness sync endpoint succeeded"
    else
        log_fail "${container} TeamHarness sync endpoint failed"
    fi
    if _agent_api "${container}" GET /api/agentteam/health >/dev/null 2>&1; then
        log_fail "${container} legacy /api/agentteam/health should not be required"
    else
        log_pass "${container} legacy /api/agentteam/health is absent"
    fi
done

WORKER_HOME="/root/agentteams-fs/agents/${TEST_WORKER}"
WORKER_QWENPAW_DIR="${WORKER_HOME}/.qwenpaw"
WORKER_DEFAULT_WS="${WORKER_QWENPAW_DIR}/workspaces/default"
LEADER_HOME="/root/agentteams-fs/agents/${TEST_LEADER}"
LEADER_QWENPAW_DIR="${LEADER_HOME}/.qwenpaw"
LEADER_DEFAULT_WS="${LEADER_QWENPAW_DIR}/workspaces/default"

for container in "${LEADER_CONTAINER}" "${WORKER_CONTAINER}"; do
    if [ "${container}" = "${LEADER_CONTAINER}" ]; then
        home="${LEADER_HOME}"
        qwenpaw_dir="${LEADER_QWENPAW_DIR}"
        workspace="${LEADER_DEFAULT_WS}"
        role="team_leader"
        package_v1_marker="${LEADER_PACKAGE_V1_MARKER}"
    else
        home="${WORKER_HOME}"
        qwenpaw_dir="${WORKER_QWENPAW_DIR}"
        workspace="${WORKER_DEFAULT_WS}"
        role="worker"
        package_v1_marker="${WORKER_PACKAGE_V1_MARKER}"
    fi

    for artifact in \
        "${qwenpaw_dir}/plugins/teamharness/plugin.json" \
        "${qwenpaw_dir}/plugins/teamharness/plugin.py" \
        "${qwenpaw_dir}/plugins/teamharness/teamharness/plugin.yaml" \
        "${qwenpaw_dir}/plugins/teamharness/teamharness/prompts/team/TEAMS.md" \
        "${qwenpaw_dir}/plugins/teamharness/teamharness/mcp/server.py"; do
        if docker exec "${container}" test -f "${artifact}" >/dev/null 2>&1; then
            log_pass "${container} has TeamHarness artifact: ${artifact}"
        else
            log_fail "${container} missing TeamHarness artifact: ${artifact}"
        fi
    done

    if docker exec "${container}" test ! -e "${qwenpaw_dir}/plugins/agentteam" >/dev/null 2>&1 && \
       docker exec "${container}" test ! -e "${home}/.agentteams" >/dev/null 2>&1; then
        log_pass "${container} does not use legacy agentteam plugin state"
    else
        log_fail "${container} contains legacy agentteam plugin state"
    fi

    if docker exec "${container}" test -f "${workspace}/TEAMS.md" >/dev/null 2>&1; then
        log_pass "${container} workspace TeamHarness prompt exists"
    else
        log_fail "${container} workspace TeamHarness prompt missing"
    fi

    if docker exec "${container}" sh -c "grep -q 'member.role: ${role}' '${workspace}/TEAMS.md' && grep -q '${TEST_TEAM}' '${workspace}/TEAMS.md'" >/dev/null 2>&1; then
        log_pass "${container} TEAMS.md contains ${role} team context"
    else
        log_fail "${container} TEAMS.md missing ${role} team context"
    fi

    if docker exec "${container}" sh -c "grep -Fq '<!-- BEGIN HICLAW RUNTIME TEAM CONTEXT -->' '${workspace}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_LEADER}' '${workspace}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_WORKER}' '${workspace}/TEAMS.md' && grep -Fq 'matrixUserId: ${WORKER_MXID}' '${workspace}/TEAMS.md' && test ! -e '${qwenpaw_dir}/teamharness/team-context.json'" >/dev/null 2>&1; then
        log_pass "${container} TEAMS.md contains runtime roster facts without intermediate cache"
    else
        log_fail "${container} TEAMS.md missing runtime roster facts or created intermediate cache"
    fi

    if docker exec "${container}" sh -c "jq -e '.system_prompt_files | index(\"TEAMS.md\")' '${workspace}/agent.json'" >/dev/null 2>&1; then
        log_pass "${container} agent prompt files include TEAMS.md"
    else
        log_fail "${container} agent prompt files missing TEAMS.md"
    fi

    if docker exec "${container}" sh -c "jq -e '.mcp.clients.teamharness.enabled == true and .mcp.clients.teamharness.command and (.mcp.clients.teamharness.args | length > 0)' '${workspace}/agent.json'" >/dev/null 2>&1; then
        log_pass "${container} agent config includes TeamHarness MCP client"
    else
        log_fail "${container} agent config missing TeamHarness MCP client"
    fi

    if docker exec "${container}" sh -c "jq -e --arg name '${TEST_PACKAGE_MCP_NAME}' --arg url '${TEST_PACKAGE_MCP_URL}' '.mcp.clients[\$name].url == \$url and (.mcp.clients[\$name].enabled != false)' '${workspace}/agent.json' && test ! -f '${workspace}/mcp.json'" >/dev/null 2>&1; then
        log_pass "${container} agent config embeds AgentSpec package MCP"
    else
        log_fail "${container} agent config missing AgentSpec package MCP"
    fi

    if docker exec "${container}" sh -c "grep -q 'TEST26 AgentSpec Package' '${workspace}/AGENTS.md' && grep -q 'TEST26 AgentSpec Package Soul' '${workspace}/SOUL.md'" >/dev/null 2>&1; then
        log_pass "${container} workspace includes AgentSpec package prompts"
    else
        log_fail "${container} workspace missing AgentSpec package prompts"
    fi

    if docker exec "${container}" sh -c "grep -q 'TEST26 AgentSpec Package Bootstrap' '${workspace}/BOOTSTRAP.md'" >/dev/null 2>&1; then
        log_pass "${container} workspace includes AgentSpec package bootstrap"
    else
        log_fail "${container} workspace missing AgentSpec package bootstrap"
    fi

    if docker exec "${container}" sh -c "grep -q 'TEST26 AgentSpec Config Bootstrap' '${workspace}/bootstrap/test26-seed.md' && grep -q 'test26-hello-result.txt' '${workspace}/bootstrap/test26-hello.sh' && grep -q 'TEST26 AgentSpec Custom Material' '${workspace}/materials/test26-note.md' && test -f '${workspace}/config/credagent.json' && test -f '${workspace}/config/settings.yaml'" >/dev/null 2>&1; then
        log_pass "${container} workspace includes AgentSpec package custom materials"
    else
        log_fail "${container} workspace missing AgentSpec package custom materials"
    fi

    if docker exec "${container}" sh -c "grep -Fq '${package_v1_marker}' '${workspace}/hot-update-marker.txt'" >/dev/null 2>&1; then
        log_pass "${container} workspace includes role-specific v1 package marker"
    else
        log_fail "${container} workspace missing role-specific v1 package marker"
    fi

    if docker exec "${container}" sh -c "test -f '${workspace}/skills/test26-package-skill/SKILL.md' && jq -e '.skills[\"test26-package-skill\"].enabled == true' '${workspace}/skill.json'" >/dev/null 2>&1; then
        log_pass "${container} workspace enables AgentSpec package skill"
    else
        log_fail "${container} workspace missing enabled AgentSpec package skill"
    fi
done

_workspace_skill_check() {
    local container="$1"
    local workspace="$2"
    local required="$3"
    local forbidden="$4"
    docker exec -i \
        -e TEST_WORKSPACE="${workspace}" \
        -e TEST_REQUIRED="${required}" \
        -e TEST_FORBIDDEN="${forbidden}" \
        "${container}" \
        /opt/venv/qwenpaw/bin/python - <<'PY' 2>/dev/null | tail -n 1
import json
import os
from pathlib import Path

workspace = Path(os.environ["TEST_WORKSPACE"])
required = [item for item in os.environ["TEST_REQUIRED"].split(",") if item]
forbidden = [item for item in os.environ["TEST_FORBIDDEN"].split(",") if item]
manifest = json.loads((workspace / "skill.json").read_text(encoding="utf-8"))
skills = manifest.get("skills") or {}
missing = [name for name in required if not (workspace / "skills" / name / "SKILL.md").is_file()]
disabled = [name for name in required if not skills.get(name, {}).get("enabled")]
unexpected = [name for name in forbidden if (workspace / "skills" / name).exists() or name in skills]
problems = []
if missing:
    problems.append("missing:" + ",".join(missing))
if disabled:
    problems.append("disabled:" + ",".join(disabled))
if unexpected:
    problems.append("unexpected:" + ",".join(unexpected))
print("ok" if not problems else ";".join(problems))
PY
}

_workspace_runtime_projection_check() {
    local container="$1"
    local workspace="$2"
    docker exec -i \
        -e TEST_WORKSPACE="${workspace}" \
        -e TEST_MODEL="${TEST_MODEL}" \
        -e TEST_MCP_NAME="${TEST_MCP_NAME}" \
        -e TEST_MCP_URL="${TEST_MCP_URL}" \
        -e TEST_MCP_TRANSPORT="${TEST_MCP_TRANSPORT}" \
        "${container}" \
        /opt/venv/qwenpaw/bin/python - <<'PY' 2>/dev/null | tail -n 1
import json
import os
from pathlib import Path

workspace = Path(os.environ["TEST_WORKSPACE"])
model = os.environ["TEST_MODEL"]
mcp_name = os.environ["TEST_MCP_NAME"]
mcp_url = os.environ["TEST_MCP_URL"]
mcp_transport = os.environ["TEST_MCP_TRANSPORT"]

problems = []
agent = json.loads((workspace / "agent.json").read_text(encoding="utf-8"))
active = agent.get("active_model") or {}
if active.get("provider_id") != "hiclaw-gateway" or active.get("model") != model:
    problems.append("active_model")

for rel in ["mcporter-servers.json", "config/mcporter.json"]:
    path = workspace / rel
    if not path.is_file():
        problems.append(f"missing:{rel}")
        continue
    data = json.loads(path.read_text(encoding="utf-8"))
    server = (data.get("mcpServers") or {}).get(mcp_name) or {}
    if server.get("url") != mcp_url or server.get("transport") != mcp_transport:
        problems.append(f"mcp:{rel}")
    authorization = (server.get("headers") or {}).get("Authorization", "")
    if not authorization.startswith("Bearer "):
        problems.append(f"auth:{rel}")

print("ok" if not problems else ";".join(problems))
PY
}

LEADER_RUNTIME_PROJECTION_CHECK=$(_workspace_runtime_projection_check "${LEADER_CONTAINER}" "${LEADER_DEFAULT_WS}")
assert_eq "ok" "${LEADER_RUNTIME_PROJECTION_CHECK}" "Leader applies controller model and MCP config"

WORKER_RUNTIME_PROJECTION_CHECK=$(_workspace_runtime_projection_check "${WORKER_CONTAINER}" "${WORKER_DEFAULT_WS}")
assert_eq "ok" "${WORKER_RUNTIME_PROJECTION_CHECK}" "Worker applies controller model and MCP config"

LEADER_SKILL_CHECK=$(_workspace_skill_check \
    "${LEADER_CONTAINER}" \
    "${LEADER_DEFAULT_WS}" \
    "teamharness-communication,teamharness-file-sharing,teamharness-team-coordination,teamharness-project-management,teamharness-task-delegation" \
    "teamharness-task-execution")
assert_eq "ok" "${LEADER_SKILL_CHECK}" "Leader workspace enables TeamHarness role skills"

WORKER_SKILL_CHECK=$(_workspace_skill_check \
    "${WORKER_CONTAINER}" \
    "${WORKER_DEFAULT_WS}" \
    "teamharness-communication,teamharness-file-sharing,teamharness-mcporter,teamharness-task-execution" \
    "teamharness-task-delegation")
assert_eq "ok" "${WORKER_SKILL_CHECK}" "Worker workspace enables TeamHarness role skills"

MCP_HEALTH=$(_worker_mcp_call health '{}' 2>/dev/null || echo "{}")
if echo "${MCP_HEALTH}" | jq -e '.ok == true' >/dev/null 2>&1; then
    log_pass "Worker TeamHarness MCP health tool ok"
else
    log_fail "Worker TeamHarness MCP health tool failed"
fi

MCP_LIST=$(_worker_mcp_call filesync "$(jq -nc '{action:"list", path:"shared/"}')" 2>/dev/null || echo "{}")
if echo "${MCP_LIST}" | jq -e '.ok == true' >/dev/null 2>&1; then
    log_pass "Worker TeamHarness filesync can list shared/"
else
    log_fail "Worker TeamHarness filesync failed to list shared/"
fi

# ============================================================
# Section 5: AgentSpec hot update through Worker CR package refs
# ============================================================
log_section "QwenPaw AgentSpec Hot Update"

LEADER_CONTAINER_ID_BEFORE_UPDATE=$(docker inspect --format '{{.Id}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")
WORKER_CONTAINER_ID_BEFORE_UPDATE=$(docker inspect --format '{{.Id}}' "${WORKER_CONTAINER}" 2>/dev/null || echo "")
LEADER_CONTAINER_STARTED_BEFORE_UPDATE=$(docker inspect --format '{{.State.StartedAt}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")
WORKER_CONTAINER_STARTED_BEFORE_UPDATE=$(docker inspect --format '{{.State.StartedAt}}' "${WORKER_CONTAINER}" 2>/dev/null || echo "")
assert_not_empty "${LEADER_CONTAINER_ID_BEFORE_UPDATE}" "Captured leader container identity before hot update"
assert_not_empty "${WORKER_CONTAINER_ID_BEFORE_UPDATE}" "Captured worker container identity before hot update"

PATCH_LEADER_OUTPUT=$(_patch_worker_package_ref "${TEST_LEADER}" "${LEADER_PACKAGE_V2_URI}" 2>&1 || true)
if echo "${PATCH_LEADER_OUTPUT}" | jq -e --arg package "${LEADER_PACKAGE_V2_URI}" '.spec.package == $package' >/dev/null 2>&1; then
    log_pass "Leader Worker CR package patched to v2"
else
    log_fail "Leader Worker CR package patch failed: ${PATCH_LEADER_OUTPUT}"
fi

PATCH_WORKER_OUTPUT=$(_patch_worker_package_ref "${TEST_WORKER}" "${WORKER_PACKAGE_V2_URI}" 2>&1 || true)
if echo "${PATCH_WORKER_OUTPUT}" | jq -e --arg package "${WORKER_PACKAGE_V2_URI}" '.spec.package == $package' >/dev/null 2>&1; then
    log_pass "Worker Worker CR package patched to v2"
else
    log_fail "Worker Worker CR package patch failed: ${PATCH_WORKER_OUTPUT}"
fi

LEADER_RUNTIME_YAML=$(_wait_runtime_package_ref "${TEST_LEADER}" "${LEADER_PACKAGE_V2_URI}" 240 || true)
if printf '%s' "${LEADER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null | jq -e --arg package "${LEADER_PACKAGE_V2_URI}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
    log_pass "Leader runtime.yaml projected v2 package ref"
else
    log_fail "Leader runtime.yaml did not project v2 package ref"
fi

WORKER_RUNTIME_YAML=$(_wait_runtime_package_ref "${TEST_WORKER}" "${WORKER_PACKAGE_V2_URI}" 240 || true)
if printf '%s' "${WORKER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null | jq -e --arg package "${WORKER_PACKAGE_V2_URI}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
    log_pass "Worker runtime.yaml projected v2 package ref"
else
    log_fail "Worker runtime.yaml did not project v2 package ref"
fi

LEADER_RUNTIME_JSON=$(printf '%s' "${LEADER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null || echo "{}")
WORKER_RUNTIME_JSON=$(printf '%s' "${WORKER_RUNTIME_YAML}" | _yaml_to_json 2>/dev/null || echo "{}")
if echo "${LEADER_RUNTIME_JSON}" | jq -e --arg leader "${TEST_LEADER}" --arg worker "${TEST_WORKER}" --arg workerMxid "${WORKER_MXID}" \
    '.member.role == "team_leader" and (.team.members[] | select(.runtimeName == $leader and .role == "team_leader"))' >/dev/null 2>&1 && \
   echo "${LEADER_RUNTIME_JSON}" | jq -e --arg worker "${TEST_WORKER}" --arg workerMxid "${WORKER_MXID}" \
    '.team.members[] | select(.runtimeName == $worker and .role == "worker" and .matrixUserId == $workerMxid)' >/dev/null 2>&1 && \
   echo "${WORKER_RUNTIME_JSON}" | jq -e --arg leader "${TEST_LEADER}" --arg worker "${TEST_WORKER}" \
    '.member.role == "worker" and ([.team.members[].runtimeName] | index($leader) and index($worker))' >/dev/null 2>&1; then
    log_pass "runtime.yaml preserves TeamHarness roster facts after hot update"
else
    log_fail "runtime.yaml lost TeamHarness roster facts after hot update"
fi

if _wait_workspace_marker "${LEADER_CONTAINER}" "${LEADER_DEFAULT_WS}/hot-update-marker.txt" "${LEADER_PACKAGE_V2_MARKER}" 300; then
    log_pass "Leader QwenPaw worker applied v2 AgentSpec package"
else
    log_fail "Leader QwenPaw worker did not apply v2 AgentSpec package"
fi

if _wait_workspace_marker "${WORKER_CONTAINER}" "${WORKER_DEFAULT_WS}/hot-update-marker.txt" "${WORKER_PACKAGE_V2_MARKER}" 300; then
    log_pass "Worker QwenPaw worker applied v2 AgentSpec package"
else
    log_fail "Worker QwenPaw worker did not apply v2 AgentSpec package"
fi

if docker exec "${LEADER_CONTAINER}" sh -c "grep -Fq 'member.role: team_leader' '${LEADER_DEFAULT_WS}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_LEADER}' '${LEADER_DEFAULT_WS}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_WORKER}' '${LEADER_DEFAULT_WS}/TEAMS.md' && grep -Fq 'matrixUserId: ${WORKER_MXID}' '${LEADER_DEFAULT_WS}/TEAMS.md'" >/dev/null 2>&1 && \
   docker exec "${WORKER_CONTAINER}" sh -c "grep -Fq 'member.role: worker' '${WORKER_DEFAULT_WS}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_LEADER}' '${WORKER_DEFAULT_WS}/TEAMS.md' && grep -Fq 'runtimeName: ${TEST_WORKER}' '${WORKER_DEFAULT_WS}/TEAMS.md'" >/dev/null 2>&1; then
    log_pass "TEAMS.md preserves runtime roster facts after hot update"
else
    log_fail "TEAMS.md lost runtime roster facts after hot update"
fi

assert_eq "${LEADER_CONTAINER_ID_BEFORE_UPDATE}" "$(docker inspect --format '{{.Id}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")" "Leader container was not recreated by package update"
assert_eq "${WORKER_CONTAINER_ID_BEFORE_UPDATE}" "$(docker inspect --format '{{.Id}}' "${WORKER_CONTAINER}" 2>/dev/null || echo "")" "Worker container was not recreated by package update"
assert_eq "${LEADER_CONTAINER_STARTED_BEFORE_UPDATE}" "$(docker inspect --format '{{.State.StartedAt}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")" "Leader container start time unchanged after package update"
assert_eq "${WORKER_CONTAINER_STARTED_BEFORE_UPDATE}" "$(docker inspect --format '{{.State.StartedAt}}' "${WORKER_CONTAINER}" 2>/dev/null || echo "")" "Worker container start time unchanged after package update"

# ============================================================
# Section 6: Real Team work through Matrix and TeamHarness
# ============================================================
log_section "Real Team Work Through QwenPaw + TeamHarness"

if ! require_llm_key; then
    log_fail "test26 requires a real LLM key because the pass condition is real Team work"
    _dump_debug_snapshot
    test_teardown "26-qwenpaw-teamharness-plugin-mode"
    test_summary
    exit $?
fi

ADMIN_TOKEN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null | jq -r '.access_token // empty')
assert_not_empty "${ADMIN_TOKEN}" "Admin Matrix login succeeded"

if matrix_wait_for_user_joined "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}" 240; then
    log_pass "QwenPaw leader joined Leader DM"
else
    log_fail "QwenPaw leader did not join Leader DM"
fi
if matrix_wait_for_user_joined "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${LEADER_MXID}" 240; then
    log_pass "QwenPaw leader joined Team Room"
else
    log_fail "QwenPaw leader did not join Team Room"
fi
if matrix_wait_for_user_joined "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${WORKER_MXID}" 240; then
    log_pass "QwenPaw worker joined Team Room"
else
    log_fail "QwenPaw worker did not join Team Room"
fi

log_section "QwenPaw Native Bootstrap"

LEADER_BOOTSTRAP_PROMPT="Please run your package bootstrap now. Follow BOOTSTRAP.md exactly: run the packaged hello script, then reply with the requested marker. Do not ask me to confirm identity."
if matrix_send_message "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_BOOTSTRAP_PROMPT}" >/dev/null 2>&1; then
    log_pass "Admin sent package bootstrap request to QwenPaw leader"
else
    log_fail "Admin failed to send package bootstrap request to QwenPaw leader"
fi

LEADER_BOOTSTRAP_REPLY=$(matrix_wait_for_message_containing \
    "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}" "${BOOTSTRAP_LEADER_MARKER}" 360 2>/dev/null || true)
if echo "${LEADER_BOOTSTRAP_REPLY}" | grep -Fq "${BOOTSTRAP_LEADER_MARKER}"; then
    log_pass "QwenPaw leader completed package bootstrap instructions"
else
    log_fail "QwenPaw leader did not complete package bootstrap instructions"
fi

if _wait_container_file "${LEADER_CONTAINER}" "${LEADER_DEFAULT_WS}/.bootstrap_completed" 120; then
    log_pass "QwenPaw leader wrote bootstrap completion flag"
else
    log_fail "QwenPaw leader did not write bootstrap completion flag"
fi

if docker exec "${LEADER_CONTAINER}" sh -c "grep -Fq '${BOOTSTRAP_LEADER_MARKER}' '${LEADER_DEFAULT_WS}/bootstrap/test26-hello-result.txt'" >/dev/null 2>&1; then
    log_pass "QwenPaw leader hello script wrote expected marker"
else
    log_fail "QwenPaw leader hello script did not write expected marker"
fi

WORKER_BOOTSTRAP_PROMPT="Please run your package bootstrap now. Follow BOOTSTRAP.md exactly: run the packaged hello script, then reply with the requested marker. Do not ask me to confirm identity."
if matrix_send_mention_message "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${WORKER_MXID}" "${WORKER_BOOTSTRAP_PROMPT}" >/dev/null 2>&1; then
    log_pass "Admin sent package bootstrap mention to QwenPaw worker"
else
    log_fail "Admin failed to send package bootstrap mention to QwenPaw worker"
fi

WORKER_BOOTSTRAP_REPLY=$(matrix_wait_for_message_containing \
    "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${WORKER_MXID}" "${BOOTSTRAP_WORKER_MARKER}" 360 2>/dev/null || true)
if echo "${WORKER_BOOTSTRAP_REPLY}" | grep -Fq "${BOOTSTRAP_WORKER_MARKER}"; then
    log_pass "QwenPaw worker completed package bootstrap instructions"
else
    log_fail "QwenPaw worker did not complete package bootstrap instructions"
fi

if _wait_container_file "${WORKER_CONTAINER}" "${WORKER_DEFAULT_WS}/.bootstrap_completed" 120; then
    log_pass "QwenPaw worker wrote bootstrap completion flag"
else
    log_fail "QwenPaw worker did not write bootstrap completion flag"
fi

if docker exec "${WORKER_CONTAINER}" sh -c "grep -Fq '${BOOTSTRAP_WORKER_MARKER}' '${WORKER_DEFAULT_WS}/bootstrap/test26-hello-result.txt'" >/dev/null 2>&1; then
    log_pass "QwenPaw worker hello script wrote expected marker"
else
    log_fail "QwenPaw worker hello script did not write expected marker"
fi

TASK_PROMPT=$(cat <<EOF
Please complete this TeamHarness plugin-mode E2E request by coordinating with
the worker. Do not complete the worker task yourself.

Project id: ${PROJECT_ID}
Task id: ${TASK_ID}
Completion marker: ${MARKER}

Required leader steps:
1. Use projectflow create_project for projectId ${PROJECT_ID}.
2. Use projectflow plan_dag with exactly one task whose taskId is ${TASK_ID}
   and whose assignedTo is the worker runtimeName from your TeamHarness
   roster facts. Do not invent a different task id.
3. Use taskflow delegate_task for projectId ${PROJECT_ID}, taskId ${TASK_ID},
   using the Team Room from your TeamHarness roster facts, and the task spec
   below. This must create
   shared/tasks/${TASK_ID}/spec.md.
4. Use the TeamHarness message tool to assign the task in the Team Room. The
   assignment must visibly mention the worker Matrix user from your TeamHarness
   roster facts, include ${TASK_ID}, include shared/tasks/${TASK_ID}/spec.md,
   and tell the assignee to call taskflow ack_task before reading the spec.

Task spec to delegate:
# Task: Write TeamHarness plugin-mode readiness note

First call taskflow ack_task for task ${TASK_ID}. Use the returned spec as the
execution contract.

Create shared/tasks/${TASK_ID}/workspace/readiness-note.txt with this exact
line:
${MARKER}

Then call taskflow submit_task with status SUCCESS, a summary containing
${MARKER}, and deliverable shared/tasks/${TASK_ID}/workspace/readiness-note.txt.

After submit_task succeeds, reply in the Team Room with exactly this completion
line and one short summary sentence:
${DONE_LINE}

Mention the leader Matrix user from your TeamHarness roster facts in the
completion message.
EOF
)

if matrix_send_message "${ADMIN_TOKEN}" "${LEADER_DM}" "${TASK_PROMPT}" >/dev/null 2>&1; then
    log_pass "Admin sent real TeamHarness task to QwenPaw leader"
else
    log_fail "Admin failed to send task to QwenPaw leader"
fi

LEADER_ASSIGNMENT=$(matrix_wait_for_message_containing \
    "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${LEADER_MXID}" "${TASK_ID}" 480 2>/dev/null || true)
if echo "${LEADER_ASSIGNMENT}" | grep -q "${TASK_ID}" && echo "${LEADER_ASSIGNMENT}" | grep -q "${WORKER_MXID}"; then
    log_pass "Leader assigned TeamHarness task to worker in Team Room"
else
    log_fail "Leader did not assign TeamHarness task to worker in Team Room"
fi

SPEC_STAT=$(_wait_leader_shared_stat "shared/tasks/${TASK_ID}/spec.md" 180 || echo "{}")
if echo "${SPEC_STAT}" | jq -e '.ok == true and .exists == true' >/dev/null 2>&1; then
    log_pass "Task spec exists in shared storage after leader delegation"
else
    log_fail "Task spec missing after leader delegation"
fi

WORKER_REPLY=$(matrix_wait_for_message_containing \
    "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${WORKER_MXID}" "${MARKER}" 720 2>/dev/null || true)
if echo "${WORKER_REPLY}" | grep -q "${DONE_LINE}"; then
    log_pass "Worker completed delegated TeamHarness task in Team Room"
else
    log_fail "Worker did not complete delegated TeamHarness task"
fi

RESULT_STAT=$(_wait_leader_shared_stat "shared/tasks/${TASK_ID}/result.md" 180 || echo "{}")
if echo "${RESULT_STAT}" | jq -e '.ok == true and .exists == true' >/dev/null 2>&1; then
    log_pass "Task result exists in shared storage"
else
    log_fail "Task result missing in shared storage"
fi

DELIVERABLE_STAT=$(_wait_leader_shared_stat "shared/tasks/${TASK_ID}/workspace/readiness-note.txt" 180 || echo "{}")
if echo "${DELIVERABLE_STAT}" | jq -e '.ok == true and .exists == true' >/dev/null 2>&1; then
    log_pass "Task deliverable exists in shared storage"
else
    log_fail "Task deliverable missing in shared storage"
fi

CHECK_ARGS=$(jq -nc --arg task "${TASK_ID}" '{action:"check_task", payload:{taskId:$task}}')
TASK_CHECK=$(_leader_mcp_call taskflow "${CHECK_ARGS}" 2>/dev/null || echo "{}")
if echo "${TASK_CHECK}" | jq -e --arg marker "${MARKER}" \
    '.ok == true and .effective == true and (.result.summary | contains($marker))' >/dev/null 2>&1; then
    log_pass "Leader verified submitted worker result through taskflow"
else
    log_fail "Leader could not verify submitted worker result through taskflow"
fi

_dump_debug_snapshot

test_teardown "26-qwenpaw-teamharness-plugin-mode"
test_summary
