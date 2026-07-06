#!/bin/bash
# test-29-qwenpaw-webteam-heartbeat.sh - Case 29: QwenPaw native heartbeat
#
# Verifies the third split QwenPaw Web Team E2E slice:
#   1. Create a minimal leader/dev/qa QwenPaw Team.
#   2. Configure and manually trigger QwenPaw native heartbeat on the leader.
#   3. Verify heartbeat output is sent to Matrix through QwenPaw target=last.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "29-qwenpaw-webteam-heartbeat"
minio_setup

TEST_TEAM="test29-webteam-$$"
TEST_RUN_ID="$$_$(date +%s)"
TEST_MODEL="${HICLAW_E2E_MODEL:-${HICLAW_DEFAULT_MODEL:-qwen3.7-max}}"
K8S_NAMESPACE="${HICLAW_E2E_NAMESPACE:-default}"

TEST_LEADER="${TEST_TEAM}-leader"
TEST_DEV="${TEST_TEAM}-dev"
TEST_QA="${TEST_TEAM}-qa"

LEADER_CONTAINER="hiclaw-worker-${TEST_LEADER}"
DEV_CONTAINER="hiclaw-worker-${TEST_DEV}"
QA_CONTAINER="hiclaw-worker-${TEST_QA}"

LEADER_PACKAGE_OBJECT="hiclaw-config/packages/${TEST_LEADER}-v1.tar.gz"
DEV_PACKAGE_OBJECT="hiclaw-config/packages/${TEST_DEV}-v1.tar.gz"
QA_PACKAGE_OBJECT="hiclaw-config/packages/${TEST_QA}-v1.tar.gz"

LEADER_PACKAGE_URI="oss://${LEADER_PACKAGE_OBJECT}"
DEV_PACKAGE_URI="oss://${DEV_PACKAGE_OBJECT}"
QA_PACKAGE_URI="oss://${QA_PACKAGE_OBJECT}"

HEARTBEAT_MARKER="TEST29_HEARTBEAT_${TEST_RUN_ID}"
DISPATCH_MARKER="TEST29_DISPATCH_${TEST_RUN_ID}"

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

_package_object() {
    case "$1" in
        leader) printf '%s\n' "${LEADER_PACKAGE_OBJECT}" ;;
        dev) printf '%s\n' "${DEV_PACKAGE_OBJECT}" ;;
        qa) printf '%s\n' "${QA_PACKAGE_OBJECT}" ;;
        *) return 1 ;;
    esac
}

_package_uri() {
    case "$1" in
        leader) printf '%s\n' "${LEADER_PACKAGE_URI}" ;;
        dev) printf '%s\n' "${DEV_PACKAGE_URI}" ;;
        qa) printf '%s\n' "${QA_PACKAGE_URI}" ;;
        *) return 1 ;;
    esac
}

_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed - preserving test29 resources for debugging"
        log_info "  Team: ${TEST_TEAM}"
        log_info "  Leader container: ${LEADER_CONTAINER}"
        log_info "  Dev container: ${DEV_CONTAINER}"
        log_info "  QA container: ${QA_CONTAINER}"
        [ -n "${TEAM_ROOM}" ] && log_info "  Team Room: ${TEAM_ROOM}"
        [ -n "${LEADER_DM}" ] && log_info "  Leader DM: ${LEADER_DM}"
        return
    fi

    log_info "Cleaning up test29 resources"
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
        local member object
        member="$(_member_name "${role}")"
        object="$(_package_object "${role}")"
        exec_in_manager mc rm -r --force "hiclaw-test/hiclaw-storage/agents/${member}/" 2>/dev/null || true
        exec_in_manager mc rm -r --force "hiclaw-test/hiclaw-storage/agents/${member}/runtime/" 2>/dev/null || true
        exec_in_manager mc rm --force "hiclaw-test/hiclaw-storage/${object}" 2>/dev/null || true
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

_agent_api_json() {
    local container="$1"
    local method="$2"
    local path="$3"
    local body="$4"
    printf '%s' "${body}" | docker exec -i "${container}" sh -c '
        port="${HICLAW_CONSOLE_PORT:-8088}"
        curl -sf -X "$1" \
            -H "Content-Type: application/json" \
            --data-binary @- \
            "http://127.0.0.1:${port}$2"
    ' sh "${method}" "${path}" 2>/dev/null
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
    local timeout="${3:-180}"
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
    local timeout="${4:-240}"
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

_wait_matrix_message_marker() {
    local token="$1"
    local room_id="$2"
    local sender="$3"
    local marker="$4"
    local timeout="${5:-420}"
    local baseline_event="${6:-}"
    local elapsed=0
    if [ -z "${baseline_event}" ]; then
        baseline_event=$(_latest_matrix_message_event "${token}" "${room_id}" "${sender}")
    fi

    while [ "${elapsed}" -lt "${timeout}" ]; do
        local messages event
        messages=$(matrix_read_messages "${token}" "${room_id}" 80 2>/dev/null || echo "{}")
        event=$(echo "${messages}" | jq -c --arg user "${sender}" --arg baseline "${baseline_event}" --arg marker "${marker}" '
            [
                .chunk[]
                | select(.sender | startswith($user))
                | select(.type == "m.room.message")
            ] as $msgs
            | ($msgs | map(.event_id) | index($baseline)) as $idx
            | (if $idx == null then $msgs else $msgs[0:$idx] end)[]
            | select(
                ((.content.body // "") | contains($marker)) or
                ((.content.formatted_body // "") | contains($marker)) or
                ((.content["m.new_content"].body // "") | contains($marker)) or
                ((.content["m.new_content"].formatted_body // "") | contains($marker))
            )
        ' 2>/dev/null | head -1)
        if [ -n "${event}" ]; then
            printf '%s\n' "${event}"
            return 0
        fi
        sleep 10
        elapsed=$((elapsed + 10))
    done
    return 1
}

_latest_matrix_message_event() {
    local token="$1"
    local room_id="$2"
    local sender="$3"
    matrix_read_messages "${token}" "${room_id}" 30 2>/dev/null | \
        jq -r --arg user "${sender}" '
            [
                .chunk[]
                | select(.sender | startswith($user))
                | select(.type == "m.room.message")
                | .event_id
            ] | first // ""
        ' 2>/dev/null
}

_dump_debug_snapshot() {
    log_info "Debug snapshot for test29"
    for role in leader dev qa; do
        local container
        container="$(_container_name "${role}")"
        if docker ps -a --format '{{.Names}}' | grep -q "^${container}$"; then
            log_info "Logs tail: ${container}"
            docker logs --tail 100 "${container}" 2>&1 || true
            log_info "TeamHarness status: ${container}"
            _agent_api "${container}" GET /api/teamharness/status || true
            log_info "QwenPaw heartbeat config: ${container}"
            _agent_api "${container}" GET /api/config/heartbeat || true
        fi
    done
    if [ -n "${ADMIN_TOKEN}" ] && [ -n "${LEADER_DM}" ]; then
        log_info "Recent Leader DM messages:"
        matrix_read_messages "${ADMIN_TOKEN}" "${LEADER_DM}" 80 2>/dev/null | \
            jq -r '.chunk[] | select(.type == "m.room.message") | "\(.event_id) \(.sender): \(.content.body // .content["m.new_content"].body // "")"' || true
    fi
}

_create_webteam_package() {
    local role="$1"
    local member="$(_member_name "${role}")"
    local team_role="$(_team_role "${role}")"
    local business_role="$(_business_role "${role}")"
    local object="$(_package_object "${role}")"

    docker exec -i \
        -e TEST_MEMBER="${member}" \
        -e TEST_ROLE="${role}" \
        -e TEST_TEAM_ROLE="${team_role}" \
        -e TEST_BUSINESS_ROLE="${business_role}" \
        -e HEARTBEAT_MARKER="${HEARTBEAT_MARKER}" \
        -e TEST_OBJECT="${object}" \
        "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" bash -s <<'EOS'
set -eu

work="/tmp/test29-package-${TEST_MEMBER}"
archive="/tmp/${TEST_MEMBER}-agentspec.tar.gz"

rm -rf "${work}" "${archive}"
mkdir -p "${work}/config/materials" "${work}/skills/test29-heartbeat-skill"

cat >"${work}/manifest.json" <<EOF
{"name":"test29-${TEST_ROLE}-webteam","version":"v1","role":"${TEST_ROLE}"}
EOF

cat >"${work}/config/AGENTS.md" <<EOF
# TEST29 Web Team Agent

Member: ${TEST_MEMBER}
Team role: ${TEST_TEAM_ROLE}
Business role: ${TEST_BUSINESS_ROLE}

For heartbeat instructions, follow HEARTBEAT.md exactly.
For normal readiness checks, reply with the requested marker only.
EOF

cat >"${work}/config/SOUL.md" <<EOF
# TEST29 Web Team Soul

You are the ${TEST_BUSINESS_ROLE}.
Use concise English in test replies.
EOF

cat >"${work}/config/materials/heartbeat-note.md" <<EOF
# TEST29 Heartbeat Note

Heartbeat marker: ${HEARTBEAT_MARKER}
EOF

cat >"${work}/skills/test29-heartbeat-skill/SKILL.md" <<EOF
---
name: test29-heartbeat-skill
description: TEST29 heartbeat marker skill.
---

# TEST29 Heartbeat Skill

Heartbeat marker: ${HEARTBEAT_MARKER}
EOF

if [ "${TEST_ROLE}" = "leader" ]; then
    cat >"${work}/config/HEARTBEAT.md" <<EOF
# TEST29 Heartbeat

This heartbeat is non-interactive. Do not ask the user to confirm identity.
Reply with exactly this marker and nothing else:

${HEARTBEAT_MARKER}
EOF
fi

tar -czf "${archive}" -C "${work}" .
mc cp "${archive}" "hiclaw-test/hiclaw-storage/${TEST_OBJECT}" >/dev/null
EOS
}

_create_qwenpaw_worker_cr() {
    local role="$1"
    local member="$(_member_name "${role}")"
    local package_uri="$(_package_uri "${role}")"
    local labels body
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
    local labels body
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

log_section "QwenPaw Image Baseline"

QWENPAW_WORKER_IMAGE="$(_controller_env HICLAW_QWENPAW_WORKER_IMAGE)"
QWENPAW_WORKER_IMAGE="${HICLAW_E2E_QWENPAW_WORKER_IMAGE:-${QWENPAW_WORKER_IMAGE:-hiclaw/qwenpaw-worker:latest}}"
CONTROLLER_NAME="$(_controller_env HICLAW_CONTROLLER_NAME)"

if docker image inspect "${QWENPAW_WORKER_IMAGE}" >/dev/null 2>&1; then
    log_pass "QwenPaw worker image exists: ${QWENPAW_WORKER_IMAGE}"
else
    log_fail "QwenPaw worker image missing: ${QWENPAW_WORKER_IMAGE} (run make build-qwenpaw-worker)"
    test_teardown "29-qwenpaw-webteam-heartbeat"; test_summary; exit $?
fi

if docker run --rm --entrypoint qwenpaw-worker "${QWENPAW_WORKER_IMAGE}" --help >/dev/null 2>&1; then
    log_pass "qwenpaw-worker --help works in image"
else
    log_fail "qwenpaw-worker --help failed in image"
fi

log_section "Create QwenPaw Web Team With Leader Heartbeat Package"

if _create_webteam_package leader && _create_webteam_package dev && _create_webteam_package qa; then
    log_pass "Leader, dev, and qa AgentSpec packages uploaded to MinIO"
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

log_section "Runtime Projection And Heartbeat Materialization"

for role in leader dev qa; do
    member="$(_member_name "${role}")"
    uri="$(_package_uri "${role}")"
    yaml="$(_wait_runtime_package_ref "${member}" "${uri}" 180 || true)"
    if printf '%s' "${yaml}" | _yaml_to_json 2>/dev/null | jq -e --arg package "${uri}" '.desired.agentPackage.ref == $package' >/dev/null 2>&1; then
        log_pass "${member} runtime.yaml projected package ref"
    else
        log_fail "${member} runtime.yaml did not project package ref"
    fi
done

LEADER_WORKSPACE="$(_workspace_dir "${TEST_LEADER}")"
if _wait_workspace_marker "${LEADER_CONTAINER}" "${LEADER_WORKSPACE}/HEARTBEAT.md" "${HEARTBEAT_MARKER}" 240; then
    log_pass "Leader workspace contains package HEARTBEAT.md"
else
    log_fail "Leader workspace missing package HEARTBEAT.md"
fi

log_section "Configure And Trigger Native Heartbeat"

if ! require_llm_key; then
    log_fail "test29 requires a real LLM key because heartbeat uses real QwenPaw model calls"
    _dump_debug_snapshot
    test_teardown "29-qwenpaw-webteam-heartbeat"
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

dispatch_prompt="Please reply with exactly this marker and nothing else: ${DISPATCH_MARKER}"
if matrix_send_message "${ADMIN_TOKEN}" "${LEADER_DM}" "${dispatch_prompt}" >/dev/null 2>&1; then
    log_pass "Admin sent Leader DM prompt to establish heartbeat last_dispatch"
else
    log_fail "Admin failed to send Leader DM prompt for heartbeat last_dispatch"
fi

dispatch_reply=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}" "${DISPATCH_MARKER}" 420 2>/dev/null || true)
if echo "${dispatch_reply}" | grep -Fq "${DISPATCH_MARKER}"; then
    log_pass "Leader replied in Matrix DM before heartbeat"
else
    log_fail "Leader did not establish Matrix DM last_dispatch before heartbeat"
fi

heartbeat_body='{"enabled":true,"every":"6h","target":"last"}'
put_output=$(_agent_api_json "${LEADER_CONTAINER}" PUT /api/config/heartbeat "${heartbeat_body}" 2>&1 || true)
if echo "${put_output}" | jq -e '.enabled == true and .target == "last"' >/dev/null 2>&1; then
    log_pass "Leader native heartbeat config set to target last"
else
    log_fail "Leader native heartbeat config update failed: ${put_output}"
fi

heartbeat_baseline=$(_latest_matrix_message_event "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}")
run_output=$(_agent_api "${LEADER_CONTAINER}" POST /api/config/heartbeat/run 2>&1 || true)
if echo "${run_output}" | jq -e '.started == true' >/dev/null 2>&1; then
    log_pass "Leader native heartbeat run started"
else
    log_fail "Leader native heartbeat run did not start: ${run_output}"
fi

matrix_event=$(_wait_matrix_message_marker "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}" "${HEARTBEAT_MARKER}" 420 "${heartbeat_baseline}" 2>/dev/null || true)
if echo "${matrix_event}" | jq -e --arg marker "${HEARTBEAT_MARKER}" '((.content.body // "") | contains($marker)) or ((.content.formatted_body // "") | contains($marker)) or ((.content["m.new_content"].body // "") | contains($marker)) or ((.content["m.new_content"].formatted_body // "") | contains($marker))' >/dev/null 2>&1; then
    log_pass "Heartbeat final result was sent to Matrix DM with marker"
else
    log_fail "Heartbeat Matrix DM result with marker was not observed"
fi

if docker exec "${LEADER_CONTAINER}" sh -c "jq -e '.status == \"ready\"' '/root/hiclaw-fs/agents/${TEST_LEADER}/.qwenpaw/heartbeat.json'" >/dev/null 2>&1; then
    log_pass "Worker daemon heartbeat remains ready after native heartbeat run"
else
    log_fail "Worker daemon heartbeat is not ready after native heartbeat run"
fi

if docker ps --format '{{.Names}}' | grep -q "^${LEADER_CONTAINER}$"; then
    log_pass "Leader container is still running after native heartbeat run"
else
    log_fail "Leader container stopped after native heartbeat run"
fi

if [ "${TESTS_FAILED}" -gt 0 ]; then
    _dump_debug_snapshot
fi

test_teardown "29-qwenpaw-webteam-heartbeat"
test_summary
exit $?
