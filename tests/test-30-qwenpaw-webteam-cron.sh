#!/bin/bash
# test-30-qwenpaw-webteam-cron.sh - Case 30: QwenPaw native cron skill
#
# Verifies the fourth split QwenPaw Web Team E2E slice:
#   1. Create a minimal leader/dev/qa QwenPaw Team.
#   2. Install the native QwenPaw cron skill from skill_pool into the leader workspace.
#   3. Ask the leader agent to create a disabled cron job with that skill.
#   4. Trigger the job and verify the Matrix Team Room receives the marker.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "30-qwenpaw-webteam-cron"
minio_setup

TEST_TEAM="test30-webteam-$$"
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

CRON_MARKER="TEST30_CRON_${TEST_RUN_ID}"
CRON_CREATE_MARKER="TEST30_CRON_CREATED_${TEST_RUN_ID}"
CRON_JOB_NAME="test30-cron-${TEST_RUN_ID}"

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
        log_info "Tests failed - preserving test30 resources for debugging"
        log_info "  Team: ${TEST_TEAM}"
        log_info "  Leader container: ${LEADER_CONTAINER}"
        log_info "  Dev container: ${DEV_CONTAINER}"
        log_info "  QA container: ${QA_CONTAINER}"
        [ -n "${TEAM_ROOM}" ] && log_info "  Team Room: ${TEAM_ROOM}"
        [ -n "${LEADER_DM}" ] && log_info "  Leader DM: ${LEADER_DM}"
        return
    fi

    log_info "Cleaning up test30 resources"
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

_install_leader_cron_skill() {
    local pool import_body import_output download_body download_output enable_output workspace

    pool=$(_agent_api "${LEADER_CONTAINER}" GET /api/agents/default/skills/pool 2>/dev/null || true)
    if ! echo "${pool}" | jq -e '.[] | select(.name == "cron")' >/dev/null 2>&1; then
        import_body='{"imports":[{"skill_name":"cron","language":"en"}],"overwrite_conflicts":true}'
        import_output=$(_agent_api_json "${LEADER_CONTAINER}" POST /api/agents/default/skills/pool/import-builtin "${import_body}" 2>&1 || true)
        if echo "${import_output}" | jq -e '((.imported // []) | index("cron")) or ((.updated // []) | index("cron")) or ((.unchanged // []) | index("cron"))' >/dev/null 2>&1; then
            log_pass "Leader QwenPaw builtin cron skill imported into skill_pool"
        else
            log_fail "Leader QwenPaw builtin cron skill import failed: ${import_output}"
            return 1
        fi
    else
        log_pass "Leader QwenPaw skill_pool already contains cron skill"
    fi

    download_body='{"skill_name":"cron","targets":[{"workspace_id":"default"}],"overwrite":true}'
    download_output=$(_agent_api_json "${LEADER_CONTAINER}" POST /api/agents/default/skills/pool/download "${download_body}" 2>&1 || true)
    if echo "${download_output}" | jq -e '.downloaded[] | select(.workspace_id == "default" and .name == "cron")' >/dev/null 2>&1; then
        log_pass "Leader downloaded cron skill from skill_pool into workspace"
    else
        log_fail "Leader cron skill pool download failed: ${download_output}"
        return 1
    fi

    enable_output=$(_agent_api "${LEADER_CONTAINER}" POST /api/agents/default/skills/cron/enable 2>&1 || true)
    if echo "${enable_output}" | jq -e '.enabled == true or .success == true' >/dev/null 2>&1; then
        log_pass "Leader enabled cron skill through QwenPaw skill API"
    else
        log_fail "Leader cron skill enable failed: ${enable_output}"
        return 1
    fi

    workspace="$(_workspace_dir "${TEST_LEADER}")"
    if docker exec "${LEADER_CONTAINER}" sh -c "test -f '${workspace}/skills/cron/SKILL.md' && jq -e '.skills.cron.enabled == true' '${workspace}/skill.json'" >/dev/null 2>&1; then
        log_pass "Leader workspace contains enabled native cron skill"
    else
        log_fail "Leader workspace does not contain enabled native cron skill"
        return 1
    fi
}

_wait_leader_cron_job() {
    local timeout="${1:-420}"
    local elapsed=0
    local body job
    while [ "${elapsed}" -lt "${timeout}" ]; do
        body=$(_agent_api "${LEADER_CONTAINER}" GET /api/agents/default/cron/jobs 2>/dev/null || true)
        job=$(echo "${body}" | jq -c --arg name "${CRON_JOB_NAME}" '[.[] | select(.name == $name)] | last // empty' 2>/dev/null || true)
        if [ -n "${job}" ]; then
            printf '%s\n' "${job}"
            return 0
        fi
        sleep 10
        elapsed=$((elapsed + 10))
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

_wait_matrix_room_marker() {
    local token="$1"
    local room_id="$2"
    local marker="$3"
    local timeout="${4:-240}"
    local elapsed=0

    while [ "${elapsed}" -lt "${timeout}" ]; do
        local messages event
        messages=$(matrix_read_messages "${token}" "${room_id}" 120 2>/dev/null || echo "{}")
        event=$(echo "${messages}" | jq -c --arg marker "${marker}" '
            .chunk[]
            | select(.type == "m.room.message")
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
    log_info "Debug snapshot for test30"
    for role in leader dev qa; do
        local container
        container="$(_container_name "${role}")"
        if docker ps -a --format '{{.Names}}' | grep -q "^${container}$"; then
            log_info "Logs tail: ${container}"
            docker logs --tail 100 "${container}" 2>&1 || true
            log_info "TeamHarness status: ${container}"
            _agent_api "${container}" GET /api/teamharness/status || true
            log_info "QwenPaw cron jobs: ${container}"
            _agent_api "${container}" GET /api/agents/default/cron/jobs || true
            log_info "QwenPaw leader skills: ${container}"
            _agent_api "${container}" GET /api/agents/default/skills || true
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
        -e CRON_MARKER="${CRON_MARKER}" \
        -e CRON_CREATE_MARKER="${CRON_CREATE_MARKER}" \
        -e CRON_JOB_NAME="${CRON_JOB_NAME}" \
        -e TEST_OBJECT="${object}" \
        "${TEST_CONTROLLER_CONTAINER:-hiclaw-controller}" bash -s <<'EOS'
set -eu

work="/tmp/test30-package-${TEST_MEMBER}"
archive="/tmp/${TEST_MEMBER}-agentspec.tar.gz"

rm -rf "${work}" "${archive}"
mkdir -p "${work}/config/materials"

cat >"${work}/manifest.json" <<EOF
{"name":"test30-${TEST_ROLE}-webteam","version":"v1","role":"${TEST_ROLE}"}
EOF

cat >"${work}/config/AGENTS.md" <<EOF
# TEST30 Web Team Agent

Member: ${TEST_MEMBER}
Team role: ${TEST_TEAM_ROLE}
Business role: ${TEST_BUSINESS_ROLE}

For cron instructions, follow CRON.md exactly.
For normal readiness checks, reply with the requested marker only.
EOF

cat >"${work}/config/SOUL.md" <<EOF
# TEST30 Web Team Soul

You are the ${TEST_BUSINESS_ROLE}.
Use concise English in test replies.
EOF

cat >"${work}/config/materials/cron-note.md" <<EOF
# TEST30 Cron Note

Cron marker: ${CRON_MARKER}
EOF

if [ "${TEST_ROLE}" = "leader" ]; then
    cat >"${work}/config/CRON.md" <<EOF
# TEST30 Cron

Create one QwenPaw native cron job. Do not ask the user to confirm identity.

Use the native cron skill and run qwenpaw cron through the shell tool.
The job must be disabled when created. After creating it, reply with exactly:

${CRON_CREATE_MARKER}

Job requirements:
- agent id: default
- job name: ${CRON_JOB_NAME}
- type: text
- schedule type: cron
- cron expression: 0 9 * * *
- enabled: false
- channel: matrix
- target user: cron
- target session: TEAM_ROOM_ID
- text: ${CRON_MARKER}

Resolve TEAM_ROOM_ID from TEAMS.md before creating the job.
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
    test_teardown "30-qwenpaw-webteam-cron"; test_summary; exit $?
fi

if docker run --rm --entrypoint qwenpaw-worker "${QWENPAW_WORKER_IMAGE}" --help >/dev/null 2>&1; then
    log_pass "qwenpaw-worker --help works in image"
else
    log_fail "qwenpaw-worker --help failed in image"
fi

log_section "Create QwenPaw Web Team With Leader Cron Package"

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

log_section "Runtime Projection And Cron Materialization"

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
if _wait_workspace_marker "${LEADER_CONTAINER}" "${LEADER_WORKSPACE}/CRON.md" "${CRON_MARKER}" 240; then
    log_pass "Leader workspace contains package CRON.md"
else
    log_fail "Leader workspace missing package CRON.md"
fi

log_section "Install Native Cron Skill And Let Agent Create Job"

if ! require_llm_key; then
    log_fail "test30 requires a real LLM key because cron setup uses real QwenPaw model calls"
    _dump_debug_snapshot
    test_teardown "30-qwenpaw-webteam-cron"
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

if _install_leader_cron_skill; then
    log_pass "Leader native cron skill is installed and enabled"
else
    log_fail "Leader native cron skill install failed"
fi

cron_prompt=$(cat <<EOF
Please set up the native QwenPaw cron job now. Follow CRON.md, use the cron skill, and run qwenpaw cron through the shell tool.

Concrete values for this test:
- TEAM_ROOM_ID: ${TEAM_ROOM}
- Job name: ${CRON_JOB_NAME}
- Job text: ${CRON_MARKER}
- After the disabled job is created, reply with exactly: ${CRON_CREATE_MARKER}

Do not run the job yet. Do not ask me to confirm identity.
EOF
)

if matrix_send_message "${ADMIN_TOKEN}" "${LEADER_DM}" "${cron_prompt}" >/dev/null 2>&1; then
    log_pass "Admin asked leader to create native disabled cron job"
else
    log_fail "Admin failed to ask leader to create native disabled cron job"
fi

cron_reply=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${LEADER_DM}" "${LEADER_MXID}" "${CRON_CREATE_MARKER}" 480 2>/dev/null || true)
if echo "${cron_reply}" | grep -Fq "${CRON_CREATE_MARKER}"; then
    log_pass "Leader reported native cron job creation"
else
    log_fail "Leader did not report native cron job creation"
fi

cron_job=$(_wait_leader_cron_job 240 2>/dev/null || true)
if echo "${cron_job}" | jq -e --arg name "${CRON_JOB_NAME}" --arg marker "${CRON_MARKER}" --arg session "${TEAM_ROOM}" '.name == $name and .enabled == false and .task_type == "text" and .text == $marker and .dispatch.channel == "matrix" and (.dispatch.target.session_id == $session or .dispatch.target.session_id == ("matrix:" + $session))' >/dev/null 2>&1; then
    log_pass "Leader created disabled native cron text job targeting Team Room"
else
    log_fail "Leader did not create expected disabled native cron job: ${cron_job}"
fi

cron_job_id=$(echo "${cron_job}" | jq -r '.id // empty')
assert_not_empty "${cron_job_id}" "Native cron job ID available"

log_section "Trigger Native Cron Job"

run_output=$(_agent_api "${LEADER_CONTAINER}" POST "/api/agents/default/cron/jobs/${cron_job_id}/run" 2>&1 || true)
if echo "${run_output}" | jq -e '.started == true' >/dev/null 2>&1; then
    log_pass "Native cron job manual run started"
else
    log_fail "Native cron job manual run did not start: ${run_output}"
fi

matrix_event=$(_wait_matrix_room_marker "${ADMIN_TOKEN}" "${TEAM_ROOM}" "${CRON_MARKER}" 240 2>/dev/null || true)
if echo "${matrix_event}" | jq -e --arg marker "${CRON_MARKER}" '((.content.body // "") | contains($marker)) or ((.content.formatted_body // "") | contains($marker)) or ((.content["m.new_content"].body // "") | contains($marker)) or ((.content["m.new_content"].formatted_body // "") | contains($marker))' >/dev/null 2>&1; then
    log_pass "Native cron text job sent marker to Matrix Team Room"
else
    log_fail "Native cron Matrix Team Room result with marker was not observed"
fi

cron_state=$(_agent_api "${LEADER_CONTAINER}" GET "/api/agents/default/cron/jobs/${cron_job_id}/state" 2>/dev/null || true)
if echo "${cron_state}" | jq -e '.running == false or .status == "idle" or .last_run_at != null' >/dev/null 2>&1; then
    log_pass "Native cron job state is readable after manual run"
else
    log_fail "Native cron job state was not readable after manual run: ${cron_state}"
fi

if docker exec "${LEADER_CONTAINER}" sh -c "jq -e '.status == \"ready\"' '/root/hiclaw-fs/agents/${TEST_LEADER}/.qwenpaw/heartbeat.json'" >/dev/null 2>&1; then
    log_pass "Worker daemon heartbeat remains ready after native cron run"
else
    log_fail "Worker daemon heartbeat is not ready after native cron run"
fi

if docker ps --format '{{.Names}}' | grep -q "^${LEADER_CONTAINER}$"; then
    log_pass "Leader container is still running after native cron run"
else
    log_fail "Leader container stopped after native cron run"
fi

if [ "${TESTS_FAILED}" -gt 0 ]; then
    _dump_debug_snapshot
fi

test_teardown "30-qwenpaw-webteam-cron"
test_summary
exit $?
