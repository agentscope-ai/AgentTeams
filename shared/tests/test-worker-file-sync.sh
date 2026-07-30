#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

test_unknown_workspace_file_is_uploaded_once() {
    local tmpdir workspace state_dir remote_prefix
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${workspace}/.codex/tmp" "${state_dir}" "${tmpdir}/bin"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export PATH="${tmpdir}/bin:${PATH}"

    # shellcheck source=../lib/worker-file-sync.sh
    source "${REPO_ROOT}/shared/lib/worker-file-sync.sh"
    worker_sync_init "${state_dir}"

    sleep 1
    printf 'business state\n' > "${workspace}/.codex/tmp/result.txt"

    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"
    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "1" ] ||
        fail "the same file was uploaded more than once"
    grep -Fq \
        "cp ${workspace}/.codex/tmp/result.txt ${remote_prefix}/.codex/tmp/result.txt" \
        "${FAKE_MC_LOG}" ||
        fail "unknown workspace path was not preserved"

    rm -rf "${tmpdir}"
}

test_failed_upload_is_retried() {
    local tmpdir workspace state_dir remote_prefix
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${workspace}/results" "${state_dir}" "${tmpdir}/bin"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
if [ ! -f "${FAKE_MC_FAILED_ONCE}" ]; then
    touch "${FAKE_MC_FAILED_ONCE}"
    exit 1
fi
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export FAKE_MC_FAILED_ONCE="${tmpdir}/failed-once"
    export PATH="${tmpdir}/bin:${PATH}"

    worker_sync_init "${state_dir}"
    sleep 1
    printf 'task result\n' > "${workspace}/results/output.md"

    if worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"; then
        fail "the first upload should have failed"
    fi
    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "2" ] ||
        fail "failed upload was not retried"

    rm -rf "${tmpdir}"
}

test_manager_owned_and_local_runtime_files_stay_local() {
    local tmpdir workspace state_dir remote_prefix
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${workspace}/results" "${workspace}/credentials" \
        "${workspace}/.openclaw/matrix" "${state_dir}" "${tmpdir}/bin"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export PATH="${tmpdir}/bin:${PATH}"

    worker_sync_init "${state_dir}"
    sleep 1
    printf '{}\n' > "${workspace}/openclaw.json"
    printf 'secret\n' > "${workspace}/credentials/token"
    printf 'crypto\n' > "${workspace}/.openclaw/matrix/state"
    printf 'task result\n' > "${workspace}/results/output.md"

    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "1" ] ||
        fail "manager-owned or local runtime files were uploaded"
    grep -Fq "${workspace}/results/output.md" "${FAKE_MC_LOG}" ||
        fail "ordinary workspace file was not uploaded"

    rm -rf "${tmpdir}"
}

test_large_change_set_uses_one_mirror() {
    local tmpdir workspace state_dir remote_prefix
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${workspace}/results" "${state_dir}" "${tmpdir}/bin"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export PATH="${tmpdir}/bin:${PATH}"
    export AGENTTEAMS_WORKER_SYNC_MIRROR_THRESHOLD=2

    worker_sync_init "${state_dir}"
    sleep 1
    printf 'one\n' > "${workspace}/results/one"
    printf 'two\n' > "${workspace}/results/two"
    printf 'three\n' > "${workspace}/results/three"

    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "1" ] ||
        fail "large change set did not collapse to one operation"
    grep -Fq "mirror ${workspace}/ ${remote_prefix}/ --overwrite" "${FAKE_MC_LOG}" ||
        fail "large change set did not use mirror"

    unset AGENTTEAMS_WORKER_SYNC_MIRROR_THRESHOLD
    rm -rf "${tmpdir}"
}

test_concurrent_remote_pull_does_not_regress_push_watermark() {
    local tmpdir workspace state_dir remote_prefix expected_epoch actual_epoch
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${workspace}/results" "${state_dir}" "${tmpdir}/bin"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
sleep 2
touch "${FAKE_PUSH_MARKER}"
perl -e 'print((stat shift)[9])' "${FAKE_PUSH_MARKER}" > "${FAKE_EXPECTED_EPOCH}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_PUSH_MARKER="${state_dir}/last-successful-push"
    export FAKE_EXPECTED_EPOCH="${tmpdir}/expected-epoch"
    export PATH="${tmpdir}/bin:${PATH}"

    worker_sync_init "${state_dir}"
    sleep 1
    printf 'task result\n' > "${workspace}/results/output.md"

    worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}"

    expected_epoch="$(cat "${FAKE_EXPECTED_EPOCH}")"
    actual_epoch="$(perl -e 'print((stat shift)[9])' "${FAKE_PUSH_MARKER}")"
    [ "${actual_epoch}" = "${expected_epoch}" ] ||
        fail "successful push regressed a newer remote-pull watermark"

    rm -rf "${tmpdir}"
}

test_scan_failure_does_not_advance_watermark() {
    local tmpdir workspace state_dir remote_prefix before_epoch after_epoch
    tmpdir="$(mktemp -d)"
    workspace="${tmpdir}/missing-workspace"
    state_dir="${tmpdir}/state"
    remote_prefix="agentteams/agentteams-storage/agents/alice"
    mkdir -p "${state_dir}"

    worker_sync_init "${state_dir}"
    before_epoch="$(perl -e 'print((stat shift)[9])' "${state_dir}/last-successful-push")"
    sleep 1

    if worker_sync_push_once "${workspace}" "${remote_prefix}" "${state_dir}" 2>/dev/null; then
        fail "missing workspace scan should fail"
    fi

    after_epoch="$(perl -e 'print((stat shift)[9])' "${state_dir}/last-successful-push")"
    [ "${after_epoch}" = "${before_epoch}" ] ||
        fail "scan failure advanced the push watermark"

    rm -rf "${tmpdir}"
}

test_unknown_workspace_file_is_uploaded_once
test_failed_upload_is_retried
test_manager_owned_and_local_runtime_files_stay_local
test_large_change_set_uses_one_mirror
test_concurrent_remote_pull_does_not_regress_push_watermark
test_scan_failure_does_not_advance_watermark

echo "PASS: worker file sync"
