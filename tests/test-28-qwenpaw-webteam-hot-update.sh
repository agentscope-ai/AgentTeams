#!/bin/bash
# test-28-qwenpaw-webteam-hot-update.sh - Case 28: QwenPaw Web Team package hot update
#
# Verifies the second split QwenPaw Web Team E2E slice:
#   1. Build three v1 AgentSpec packages and create a leader/dev/qa QwenPaw Team.
#   2. Patch each Worker CR to a v2 package ref through the Kubernetes API.
#   3. Verify runtime.yaml projection, workspace update, no container restart, and real model awareness.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "28-qwenpaw-webteam-hot-update"
minio_setup

TEST_TEAM="test28-webteam-$$"
TEST_RUN_ID="$$_$(date +%s)"
TEST_MODEL="${HICLAW_E2E_MODEL:-${HICLAW_DEFAULT_MODEL:-qwen3.7-max}}"
K8S_NAMESPACE="${HICLAW_E2E_NAMESPACE:-default}"

TEST_LEADER="${TEST_TEAM}-leader"
TEST_DEV="${TEST_TEAM}-dev"
TEST_QA="${TEST_TEAM}-qa"

LEADER_CONTAINER="hiclaw-worker-${TEST_LEADER}"
DEV_CONTAINER="hiclaw-worker-${TEST_DEV}"
QA_CONTAINER="hiclaw-worker-${TEST_QA}"

LEADER_V1_MARKER="TEST28_LEADER_PACKAGE_V1_${TEST_RUN_ID}"
DEV_V1_MARKER="TEST28_DEV_PACKAGE_V1_${TEST_RUN_ID}"
QA_V1_MARKER="TEST28_QA_PACKAGE_V1_${TEST_RUN_ID}"
LEADER_V2_MARKER="TEST28_LEADER_PACKAGE_V2_${TEST_RUN_ID}"
DEV_V2_MARKER="TEST28_DEV_PACKAGE_V2_${TEST_RUN_ID}"
QA_V2_MARKER="TEST28_QA_PACKAGE_V2_${TEST_RUN_ID}"

LEADER_V1_OBJECT="hiclaw-config/packages/${TEST_LEADER}-v1.tar.gz"
DEV_V1_OBJECT="hiclaw-config/packages/${TEST_DEV}-v1.tar.gz"
QA_V1_OBJECT="hiclaw-config/packages/${TEST_QA}-v1.tar.gz"
LEADER_V2_OBJECT="hiclaw-config/packages/${TEST_LEADER}-v2.tar.gz"
DEV_V2_OBJECT="hiclaw-config/packages/${TEST_DEV}-v2.tar.gz"
QA_V2_OBJECT="hiclaw-config/packages/${TEST_QA}-v2.tar.gz"

LEADER_V1_URI="oss://${LEADER_V1_OBJECT}"
DEV_V1_URI="oss://${DEV_V1_OBJECT}"
QA_V1_URI="oss://${QA_V1_OBJECT}"
LEADER_V2_URI="oss://${LEADER_V2_OBJECT}"
DEV_V2_URI="oss://${DEV_V2_OBJECT}"
QA_V2_URI="oss://${QA_V2_OBJECT}"

QWENPAW_WORKER_IMAGE=""
CONTROLLER_NAME=""
TEAM_ROOM=""
LEADER_DM=""
ADMIN_TOKEN=""
LEADER_MXID=""
DEV_MXID=""
QA_MXID=""

_member_name() {
    case "$1" in
        leader) printf '%s\n' "${TEST_LEADER}" ;;
        dev) printf '%s\n' "${TEST_DEV}" ;;
        qa) printf '%s\n' "${TEST_QA}" ;;
        *) return 1 ;;
    esac
}

_container_name() {
    printf 'hiclaw-worker-%s\n' "$(_member_name "$1")"
}

_team_role() {
    if [ "$1" = "leader" ]; then
        printf 'team_leader\n'
    else
        printf 'worker\n'
    fi
}

_business_role() {
    case "$1" in
        leader) printf 'web team leader\n' ;;
        dev) printf 'web developer\n' ;;
        qa) printf 'web QA tester\n' ;;
        *) return 1 ;;
    esac
}

_workspace_dir() {
    local member="$1"
    printf '/root/hiclaw-fs/agents/%s/.qwenpaw/workspaces/default\n' "${member}"
}

_package_marker() {
    local role="$1"
    local version="$2"
    case "${role}:${version}" in
        leader:v1) printf '%s\n' "${LEADER_V1_MARKER}" ;;
        dev:v1) printf '%s\n' "${DEV_V1_MARKER}" ;;
        qa:v1) printf '%s\n' "${QA_V1_MARKER}" ;;
        leader:v2) printf '%s\n' "${LEADER_V2_MARKER}" ;;
        dev:v2) printf '%s\n' "${DEV_V2_MARKER}" ;;
        qa:v2) printf '%s\n' "${QA_V2_MARKER}" ;;
        *) return 1 ;;
    esac
}

_package_object() {
    local role="$1"
    local version="$2"
    case "${role}:${version}" in
        leader:v1) printf '%s\n' "${LEADER_V1_OBJECT}" ;;
        dev:v1) printf '%s\n' "${DEV_V1_OBJECT}" ;;
        qa:v1) printf '%s\n' "${QA_V1_OBJECT}" ;;
        leader:v2) printf '%s\n' "${LEADER_V2_OBJECT}" ;;
        dev:v2) printf '%s\n' "${DEV_V2_OBJECT}" ;;
        qa:v2) printf '%s\n' "${QA_V2_OBJECT}" ;;
        *) return 1 ;;
    esac
}

_package_uri() {
    local role="$1"
    local version="$2"
    case "${role}:${version}" in
        leader:v1) printf '%s\n' "${LEADER_V1_URI}" ;;
        dev:v1) printf '%s\n' "${DEV_V1_URI}" ;;
        qa:v1) printf '%s\n' "${QA_V1_URI}" ;;
        leader:v2) printf '%s\n' "${LEADER_V2_URI}" ;;
        dev:v2) printf '%s\n' "${DEV_V2_URI}" ;;
        qa:v2) printf '%s\n' "${QA_V2_URI}" ;;
        *) return 1 ;;
    esac
}

_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed - preserving test28 resources for debugging"
        log_info "  Team: ${TEST_TEAM}"
        log_info "  Leader container: ${LEADER_CONTAINER}"
        log_info "  Dev container: ${DEV_CONTAINER}"
        log_info "  QA container: ${QA_CONTAINER}"
        [ -n "${TEAM_ROOM}" ] && log_info "  Team Room: ${TEAM_ROOM}"
        [ -n "${LEADER_DM}" ] && log_info "  Leader DM: ${LEADER_DM}"
        return
    fi

    log_info "Cleaning up test28 resources"
    _k8s_delete "teams" "${TEST_TEAM}" >/dev/null 2>&1 || true
    for role in leader dev qa; do
        local member
        member="$(_member_name "${role}")"
        _k8s_delete "workers" "${member}" >/dev/null 2>&1 || true
    done
    sleep 5
    docker rm -f "${LEADER_CONTAINER}" "${DEV_CONTAINER}" "${QA_CONTAINER}" 2>/dev/null || true
    exec_in_manager mc rm -r --force "hiclaw-test/hiclaw-storage/teams/${TEST_TEAM}/" 2>/dev/null || true
    for role in leader dev qa; do
        local member
        member="$(_member_name "${role}")"
        exec_in_manager mc rm -r --force "hiclaw-test/hiclaw-storage/agents/${member}/" 2>/dev/null || true
        exec_in_manager mc rm -r --force "hiclaw-test/hiclaw-storage/agents/${member}/runtime/" 2>/dev/null || true
        exec_in_manager mc rm --force "hiclaw-test/hiclaw-storage/$(_package_object "${role}" v1)" 2>/dev/null || true
        exec_in_manager mc rm --force "hiclaw-test/hiclaw-storage/$(_package_object "${role}" v2)" 2>/dev/null || true
    done
}
trap _cleanup EXIT

_controller_env() {
    local key="$1"
    docker exec "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" printenv "${key}" 2>/dev/null || true
}

_controller_labels_json() {
    if [ -n "${CONTROLLER_NAME}" ]; then
        jq -nc --arg controller "${CONTROLLER_NAME}" '{"hiclaw.io/controller":$controller}'
    else
        printf '{}\n'
    fi
}

_k8s_api() {
    local method="$1"
    local content_type="$2"
    local path="$3"

    if [ "${method}" = "GET" ] || [ "${method}" = "DELETE" ]; then
        docker exec "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" sh -c '
            token="$(cut -d, -f1 /data/hiclaw-controller/pki/token.csv)"
            curl -ksS -X "$1" \
                -H "Authorization: Bearer ${token}" \
                "https://127.0.0.1:6443$2"
        ' sh "${method}" "${path}"
        return $?
    fi

    docker exec -i "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" sh -c '
        token="$(cut -d, -f1 /data/hiclaw-controller/pki/token.csv)"
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

_yaml_to_json() {
    ruby -ryaml -rjson -e 'puts JSON.generate(YAML.load(STDIN.read) || {})'
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
        port="${HICLAW_CONSOLE_PORT:-8088}"
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

_wait_runtime_package_ref() {
    local member="$1"
    local expected_ref="$2"
    local timeout="${3:-240}"
    local elapsed=0
    local key="agents/${member}/runtime/runtime.yaml"
    while [ "${elapsed}" -lt "${timeout}" ]; do
        local yaml json
        yaml=$(minio_read_file "${key}" 2>/dev/null || true)
        if [ -n "${yaml}" ]; then
            json=$(printf '%s' "${yaml}" | _yaml_to_json 2>/dev/null || echo "{}")
            if echo "${json}" | jq -e --arg ref "${expected_ref}" '.desired.agentPackage.ref == $ref' >/dev/null 2>&1; then
                printf '%s\n' "${yaml}"
                return 0
            fi
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_wait_workspace_marker() {
    local container="$1"
    local path="$2"
    local marker="$3"
    local timeout="${4:-300}"
    local elapsed=0
    while [ "${elapsed}" -lt "${timeout}" ]; do
        if docker exec "${container}" sh -c "test -f '$path' && grep -Fq '$marker' '$path'" >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

_dump_debug_snapshot() {
    log_info "Debug snapshot for test28"
    for role in leader dev qa; do
        local container
        container="$(_container_name "${role}")"
        if docker ps -a --format '{{.Names}}' | grep -q "^${container}$"; then
            log_info "Logs tail: ${container}"
            docker logs --tail 80 "${container}" 2>&1 || true
            log_info "TeamHarness status: ${container}"
            _agent_api "${container}" GET /api/teamharness/status || true
        fi
    done
    if [ -n "${ADMIN_TOKEN}" ] && [ -n "${TEAM_ROOM}" ]; then
        log_info "Recent Team Room messages:"
        matrix_read_messages "${ADMIN_TOKEN}" "${TEAM_ROOM}" 60 2>/dev/null | \
            jq -r '.chunk[] | select(.type == "m.room.message") | "\(.sender): \(.content.body // "")"' || true
    fi
    if [ -n "${ADMIN_TOKEN}" ] && [ -n "${LEADER_DM}" ]; then
        log_info "Recent Leader DM messages:"
        matrix_read_messages "${ADMIN_TOKEN}" "${LEADER_DM}" 40 2>/dev/null | \
            jq -r '.chunk[] | select(.type == "m.room.message") | "\(.sender): \(.content.body // "")"' || true
    fi
}

_create_webteam_package() {
    local role="$1"
    local version="$2"
    local member="$(_member_name "${role}")"
    local team_role="$(_team_role "${role}")"
    local business_role="$(_business_role "${role}")"
    local marker="$(_package_marker "${role}" "${version}")"
    local object="$(_package_object "${role}" "${version}")"

    docker exec -i \
        -e TEST_MEMBER="${member}" \
        -e TEST_ROLE="${role}" \
        -e TEST_VERSION="${version}" \
        -e TEST_TEAM_ROLE="${team_role}" \
        -e TEST_BUSINESS_ROLE="${business_role}" \
        -e TEST_MARKER="${marker}" \
        -e TEST_OBJECT="${object}" \
        "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" bash -s <<'EOS'
set -eu

work="/tmp/test28-package-${TEST_MEMBER}-${TEST_VERSION}"
archive="/tmp/${TEST_MEMBER}-${TEST_VERSION}-agentspec.tar.gz"

rm -rf "${work}" "${archive}"
mkdir -p \
    "${work}/config/materials" \
    "${work}/skills/test28-hot-update-skill"

cat >"${work}/manifest.json" <<EOF
{"name":"test28-${TEST_ROLE}-webteam","version":"${TEST_VERSION}","role":"${TEST_ROLE}"}
EOF

cat >"${work}/config/AGENTS.md" <<EOF
# TEST28 Web Team Agent

Member: ${TEST_MEMBER}
Team role: ${TEST_TEAM_ROLE}
Business role: ${TEST_BUSINESS_ROLE}
Package version: ${TEST_VERSION}
Package marker: ${TEST_MARKER}

When asked for your current TEST28 package marker, reply exactly:

${TEST_MARKER}
EOF

cat >"${work}/config/SOUL.md" <<EOF
# TEST28 Web Team Soul

You are the ${TEST_BUSINESS_ROLE}.
Use concise English in test replies.
EOF

cat >"${work}/config/hot-update-marker.txt" <<EOF
${TEST_MARKER}
EOF

cat >"${work}/config/materials/webteam-hot-update.md" <<EOF
# TEST28 Hot Update Material

Role: ${TEST_ROLE}
Version: ${TEST_VERSION}
Marker: ${TEST_MARKER}
EOF

cat >"${work}/skills/test28-hot-update-skill/SKILL.md" <<EOF
---
name: test28-hot-update-skill
description: TEST28 hot update marker skill for QwenPaw package update verification.
---

# TEST28 Hot Update Skill

Role: ${TEST_ROLE}
Version: ${TEST_VERSION}
Package marker: ${TEST_MARKER}
EOF

tar -czf "${archive}" -C "${work}" .
mc cp "${archive}" "hiclaw-test/hiclaw-storage/${TEST_OBJECT}" >/dev/null
EOS
}

_create_qwenpaw_worker_cr() {
    local role="$1"
    local member="$(_member_name "${role}")"
    local package_uri="$(_package_uri "${role}" v1)"
    local labels
    local body
    labels="$(_controller_labels_json)"
    body=$(jq -nc \
        --arg name "${member}" \
        --arg namespace "${K8S_NAMESPACE}" \
        --arg model "${TEST_MODEL}" \
        --arg package "${package_uri}" \
        --argjson labels "${labels}" \
        '{
            apiVersion:"hiclaw.io/v1beta1",
            kind:"Worker",
            metadata:{name:$name, namespace:$namespace, labels:$labels},
            spec:{
                runtime:"qwenpaw",
                model:$model,
                package:$package
            }
        }')
    _k8s_create "workers" "${body}"
}

_create_webteam_cr() {
    local labels
    local body
    labels="$(_controller_labels_json)"
    body=$(jq -nc \
        --arg name "${TEST_TEAM}" \
        --arg namespace "${K8S_NAMESPACE}" \
        --arg leader "${TEST_LEADER}" \
        --arg dev "${TEST_DEV}" \
        --arg qa "${TEST_QA}" \
        --argjson labels "${labels}" \
        '{
            apiVersion:"hiclaw.io/v1beta1",
            kind:"Team",
            metadata:{name:$name, namespace:$namespace, labels:$labels},
            spec:{
                teamName:$name,
                workerMembers:[
                    {name:$leader, role:"team_leader"},
                    {name:$dev, role:"worker"},
                    {name:$qa, role:"worker"}
                ]
            }
        }')
    _k8s_create "teams" "${body}"
}

_patch_worker_package_ref() {
    local role="$1"
    local member="$(_member_name "${role}")"
    local package_uri="$(_package_uri "${role}" v2)"
    local body
    body=$(jq -nc --arg package "${package_uri}" '{spec:{package:$package}}')
    _k8s_patch_merge "workers" "${member}" "${body}"
}

_verify_workspace_version() {
    local role="$1"
    local version="$2"
    local member="$(_member_name "${role}")"
    local container="$(_container_name "${role}")"
    local marker="$(_package_marker "${role}" "${version}")"
    local workspace
    workspace="$(_workspace_dir "${member}")"

    if docker exec "${container}" sh -c "grep -Fq '${marker}' '${workspace}/AGENTS.md' && grep -Fq '${marker}' '${workspace}/hot-update-marker.txt' && grep -Fq '${marker}' '${workspace}/skills/test28-hot-update-skill/SKILL.md' && jq -e '.skills[\"test28-hot-update-skill\"].enabled == true' '${workspace}/skill.json'" >/dev/null 2>&1; then
        log_pass "${member} workspace contains ${version} AgentSpec prompt and skill"
    else
        log_fail "${member} workspace missing ${version} AgentSpec prompt or skill"
    fi
}

_query_member_v2_marker() {
    local role="$1"
    local mxid="$2"
    local marker="$(_package_marker "${role}" v2)"
    local prompt="Please tell me your current TEST28 package marker from your active AGENTS.md. Reply with only the marker."
    local reply=""

    if [ "${role}" = "leader" ]; then
        if matrix_send_message "${ADMIN_TOKEN}" "${LEADER_DM}" "${prompt}" >/dev/null 2>&1; then
            log_pass "Admin asked leader for current v2 marker"
        else
            log_fail "Admin failed to ask leader for current v2 marker"
        fi
        reply=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${LEADER_DM}" "${mxid}" "${marker}" 420 2>/dev/null || true)
    else
        if matrix_send_mention_message "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${mxid}" "${prompt}" >/dev/null 2>&1; then
            log_pass "Admin asked ${role} for current v2 marker"
        else
            log_fail "Admin failed to ask ${role} for current v2 marker"
        fi
        reply=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${mxid}" "${marker}" 420 2>/dev/null || true)
    fi

    if echo "${reply}" | grep -Fq "${marker}"; then
        log_pass "$(_member_name "${role}") replied with v2 package marker"
    else
        log_fail "$(_member_name "${role}") did not reply with v2 package marker"
    fi
}

log_section "QwenPaw Image Baseline"

QWENPAW_WORKER_IMAGE="$(_controller_env HICLAW_QWENPAW_WORKER_IMAGE)"
QWENPAW_WORKER_IMAGE="${HICLAW_E2E_QWENPAW_WORKER_IMAGE:-${QWENPAW_WORKER_IMAGE:-hiclaw/qwenpaw-worker:latest}}"
CONTROLLER_NAME="$(_controller_env HICLAW_CONTROLLER_NAME)"

if docker image inspect "${QWENPAW_WORKER_IMAGE}" >/dev/null 2>&1; then
    log_pass "QwenPaw worker image exists: ${QWENPAW_WORKER_IMAGE}"
else
    log_fail "QwenPaw worker image missing: ${QWENPAW_WORKER_IMAGE} (run make build-qwenpaw-worker)"
    test_teardown "28-qwenpaw-webteam-hot-update"; test_summary; exit $?
fi

if docker run --rm --entrypoint qwenpaw-worker "${QWENPAW_WORKER_IMAGE}" --help >/dev/null 2>&1; then
    log_pass "qwenpaw-worker --help works in image"
else
    log_fail "qwenpaw-worker --help failed in image"
fi

log_section "Create QwenPaw Web Team From V1 Packages"

if _create_webteam_package leader v1 && _create_webteam_package dev v1 && _create_webteam_package qa v1 && \
   _create_webteam_package leader v2 && _create_webteam_package dev v2 && _create_webteam_package qa v2; then
    log_pass "Leader, dev, and qa v1/v2 AgentSpec packages uploaded to MinIO"
else
    log_fail "Failed to upload Web Team AgentSpec packages to MinIO"
fi

for role in leader dev qa; do
    output=$(_create_qwenpaw_worker_cr "${role}" 2>&1 || true)
    member="$(_member_name "${role}")"
    if echo "${output}" | jq -e --arg name "${member}" '.metadata.name == $name and .spec.runtime == "qwenpaw"' >/dev/null 2>&1; then
        log_pass "${member} Worker CR created through Kubernetes API"
    else
        log_fail "${member} Worker CR create failed: ${output}"
    fi
done

CREATE_TEAM_OUTPUT=$(_create_webteam_cr 2>&1 || true)
if echo "${CREATE_TEAM_OUTPUT}" | jq -e --arg name "${TEST_TEAM}" '.metadata.name == $name and (.spec.workerMembers | length == 3)' >/dev/null 2>&1; then
    log_pass "Web Team CR created through Kubernetes API"
else
    log_fail "Web Team CR create failed: ${CREATE_TEAM_OUTPUT}"
fi

TEAM_JSON=$(_wait_k8s_jq "teams" "${TEST_TEAM}" '.status.phase == "Active"' 300 2>/dev/null || echo "{}")
if echo "${TEAM_JSON}" | jq -e '.status.phase == "Active"' >/dev/null 2>&1; then
    log_pass "Web Team is Active"
else
    log_fail "Web Team did not become Active"
fi

TEAM_ROOM=$(echo "${TEAM_JSON}" | jq -r '.status.teamRoomID // empty')
LEADER_DM=$(echo "${TEAM_JSON}" | jq -r '.status.leaderDMRoomID // empty')
assert_not_empty "${TEAM_ROOM}" "Team Room ID available"
assert_not_empty "${LEADER_DM}" "Leader DM Room ID available"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    member_json=$(_wait_k8s_jq "workers" "${member}" '.status.roomID and .status.matrixUserID' 240 2>/dev/null || echo "{}")
    if echo "${member_json}" | jq -e '.status.roomID and .status.matrixUserID' >/dev/null 2>&1; then
        log_pass "${member} provisioned"
    else
        log_fail "${member} not provisioned"
    fi
    member_json=$(_wait_k8s_jq "workers" "${member}" '.status.phase == "Running"' 240 2>/dev/null || echo "{}")
    if echo "${member_json}" | jq -e '.status.phase == "Running"' >/dev/null 2>&1; then
        log_pass "${member} is Running"
    else
        log_fail "${member} did not reach Running"
    fi
    if wait_for_worker_container "${member}" 240; then
        log_pass "Container for ${member} is running"
    else
        log_fail "Container for ${member} did not start"
    fi
done

LEADER_JSON=$(_k8s_get "workers" "${TEST_LEADER}" 2>/dev/null || echo "{}")
DEV_JSON=$(_k8s_get "workers" "${TEST_DEV}" 2>/dev/null || echo "{}")
QA_JSON=$(_k8s_get "workers" "${TEST_QA}" 2>/dev/null || echo "{}")
LEADER_MXID=$(echo "${LEADER_JSON}" | jq -r '.status.matrixUserID // empty')
DEV_MXID=$(echo "${DEV_JSON}" | jq -r '.status.matrixUserID // empty')
QA_MXID=$(echo "${QA_JSON}" | jq -r '.status.matrixUserID // empty')
assert_not_empty "${LEADER_MXID}" "Leader Matrix ID available"
assert_not_empty "${DEV_MXID}" "Dev Matrix ID available"
assert_not_empty "${QA_MXID}" "QA Matrix ID available"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    container="$(_container_name "${role}")"
    worker_json=$(_k8s_get "workers" "${member}" 2>/dev/null || echo "{}")
    assert_eq "qwenpaw" "$(echo "${worker_json}" | jq -r '.spec.runtime // empty')" "${member} runtime is qwenpaw"
    assert_eq "${QWENPAW_WORKER_IMAGE}" "$(docker inspect --format '{{.Config.Image}}' "${container}" 2>/dev/null || echo "")" "${member} uses QwenPaw image"
    if _container_has_cmdline "${container}" "qwenpaw app --host"; then
        log_pass "${member} qwenpaw app process is running"
    else
        log_fail "${member} qwenpaw app process is not running"
    fi
    if _wait_agent_api_ok "${container}" GET /api/teamharness/health '.ok == true and .plugin == "teamharness" and .adapter == "qwenpaw"' 240 >/dev/null; then
        log_pass "${member} TeamHarness health endpoint is healthy"
    else
        log_fail "${member} TeamHarness health endpoint did not become healthy"
    fi
done

log_section "V1 Runtime Projection And Workspace"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    container="$(_container_name "${role}")"
    uri="$(_package_uri "${role}" v1)"
    marker="$(_package_marker "${role}" v1)"
    workspace="$(_workspace_dir "${member}")"
    yaml="$(_wait_runtime_package_ref "${member}" "${uri}" 180 || true)"
    if printf '%s' "${yaml}" | _yaml_to_json 2>/dev/null | jq -e --arg package "${uri}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
        log_pass "${member} runtime.yaml projected v1 package ref"
    else
        log_fail "${member} runtime.yaml did not project v1 package ref"
    fi
    if _wait_workspace_marker "${container}" "${workspace}/hot-update-marker.txt" "${marker}" 240; then
        log_pass "${member} QwenPaw worker applied v1 AgentSpec package"
    else
        log_fail "${member} QwenPaw worker did not apply v1 AgentSpec package"
    fi
    _verify_workspace_version "${role}" v1
done

log_section "Patch Worker CRs To V2 Packages"

LEADER_CONTAINER_ID_BEFORE_UPDATE=$(docker inspect --format '{{.Id}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")
DEV_CONTAINER_ID_BEFORE_UPDATE=$(docker inspect --format '{{.Id}}' "${DEV_CONTAINER}" 2>/dev/null || echo "")
QA_CONTAINER_ID_BEFORE_UPDATE=$(docker inspect --format '{{.Id}}' "${QA_CONTAINER}" 2>/dev/null || echo "")
LEADER_CONTAINER_STARTED_BEFORE_UPDATE=$(docker inspect --format '{{.State.StartedAt}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")
DEV_CONTAINER_STARTED_BEFORE_UPDATE=$(docker inspect --format '{{.State.StartedAt}}' "${DEV_CONTAINER}" 2>/dev/null || echo "")
QA_CONTAINER_STARTED_BEFORE_UPDATE=$(docker inspect --format '{{.State.StartedAt}}' "${QA_CONTAINER}" 2>/dev/null || echo "")
assert_not_empty "${LEADER_CONTAINER_ID_BEFORE_UPDATE}" "Captured leader container identity before hot update"
assert_not_empty "${DEV_CONTAINER_ID_BEFORE_UPDATE}" "Captured dev container identity before hot update"
assert_not_empty "${QA_CONTAINER_ID_BEFORE_UPDATE}" "Captured qa container identity before hot update"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    uri="$(_package_uri "${role}" v2)"
    output=$(_patch_worker_package_ref "${role}" 2>&1 || true)
    if echo "${output}" | jq -e --arg package "${uri}" '.spec.package == $package' >/dev/null 2>&1; then
        log_pass "${member} Worker CR package patched to v2"
    else
        log_fail "${member} Worker CR package patch failed: ${output}"
    fi
done

log_section "V2 Runtime Projection And Workspace"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    uri="$(_package_uri "${role}" v2)"
    yaml="$(_wait_runtime_package_ref "${member}" "${uri}" 240 || true)"
    if printf '%s' "${yaml}" | _yaml_to_json 2>/dev/null | jq -e --arg package "${uri}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
        log_pass "${member} runtime.yaml projected v2 package ref"
    else
        log_fail "${member} runtime.yaml did not project v2 package ref"
    fi
done

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    container="$(_container_name "${role}")"
    marker="$(_package_marker "${role}" v2)"
    workspace="$(_workspace_dir "${member}")"
    if _wait_workspace_marker "${container}" "${workspace}/hot-update-marker.txt" "${marker}" 300; then
        log_pass "${member} QwenPaw worker applied v2 AgentSpec package"
    else
        log_fail "${member} QwenPaw worker did not apply v2 AgentSpec package"
    fi
    _verify_workspace_version "${role}" v2
done

assert_eq "${LEADER_CONTAINER_ID_BEFORE_UPDATE}" "$(docker inspect --format '{{.Id}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")" "Leader container was not recreated by package update"
assert_eq "${DEV_CONTAINER_ID_BEFORE_UPDATE}" "$(docker inspect --format '{{.Id}}' "${DEV_CONTAINER}" 2>/dev/null || echo "")" "Dev container was not recreated by package update"
assert_eq "${QA_CONTAINER_ID_BEFORE_UPDATE}" "$(docker inspect --format '{{.Id}}' "${QA_CONTAINER}" 2>/dev/null || echo "")" "QA container was not recreated by package update"
assert_eq "${LEADER_CONTAINER_STARTED_BEFORE_UPDATE}" "$(docker inspect --format '{{.State.StartedAt}}' "${LEADER_CONTAINER}" 2>/dev/null || echo "")" "Leader container start time unchanged after package update"
assert_eq "${DEV_CONTAINER_STARTED_BEFORE_UPDATE}" "$(docker inspect --format '{{.State.StartedAt}}' "${DEV_CONTAINER}" 2>/dev/null || echo "")" "Dev container start time unchanged after package update"
assert_eq "${QA_CONTAINER_STARTED_BEFORE_UPDATE}" "$(docker inspect --format '{{.State.StartedAt}}' "${QA_CONTAINER}" 2>/dev/null || echo "")" "QA container start time unchanged after package update"

log_section "Real Model Awareness Of V2 Package"

if ! require_llm_key; then
    log_fail "test28 requires a real LLM key because v2 package awareness uses real QwenPaw model calls"
    _dump_debug_snapshot
    test_teardown "28-qwenpaw-webteam-hot-update"
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

for role in leader dev qa; do
    case "${role}" in
        leader) mxid="${LEADER_MXID}" ;;
        dev) mxid="${DEV_MXID}" ;;
        qa) mxid="${QA_MXID}" ;;
    esac
    if matrix_wait_for_user_joined "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${mxid}" 240; then
        log_pass "$(_member_name "${role}") joined Team Room"
    else
        log_fail "$(_member_name "${role}") did not join Team Room"
    fi
done

_query_member_v2_marker leader "${LEADER_MXID}"
_query_member_v2_marker dev "${DEV_MXID}"
_query_member_v2_marker qa "${QA_MXID}"

if [ "${TESTS_FAILED}" -gt 0 ]; then
    _dump_debug_snapshot
fi

test_teardown "28-qwenpaw-webteam-hot-update"
test_summary
exit $?
