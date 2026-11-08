#!/usr/bin/env bash
# check-test29-evidence-redaction.sh - credential safety of the test-29
# failure forensics (#1476 round 2).
#
# Stub-driven (fake `docker` on PATH + stub exec_in_agent /
# exec_in_manager / worker_container_name): feeds the REAL
# collect_test29_evidence() from tests/lib/test29-evidence.sh fixtures
# carrying SYNTHETIC credentials, and asserts:
#
#   A. Redaction rule matrix - every rule of evidence_redact (see the lib
#      header for the R1-R7 table) masks its target form, while
#      non-sensitive diagnostics (image ref, roomID) survive verbatim.
#   B. Structured scrub - evidence_scrub_json masks sensitive keys and
#      {name,value} pairs (both key orders) at any nesting.
#   C. Forced failure - the failure path (TESTS_FAILED=1, same gate as
#      _cleanup) collects all five artifacts; every synthetic value is
#      absent from every artifact (grep -F count 0); required diagnostic
#      fields are present; evidence is collected BEFORE cleanup deletes
#      (order file).
#   D. Normal upgrade (success path) - TESTS_FAILED=0: the collector is
#      not invoked, no evidence artifacts, no interference with cleanup.
#   E. Delayed start (readiness-timeout path) - the collector is invoked
#      directly, exactly as the readiness branch does, and still produces
#      redacted artifacts; the readiness branch in test-29 still calls the
#      collector before teardown (static wiring).
#   F. Predicates not weakened - static inventory: the assert_eq /
#      minio_presence_check predicates and the bounded deadlines of
#      test-29 are intact.
#
# No cluster, no real docker, no network. Exit 0 = all pass.

set -u

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${ROOT_DIR}/tests/lib/test29-evidence.sh"
TEST29="${ROOT_DIR}/tests/test-29-legacy-copaw-upgrade.sh"

[ -f "${LIB}" ] || { echo "FAIL: missing ${LIB}"; exit 1; }
[ -f "${TEST29}" ] || { echo "FAIL: missing ${TEST29}"; exit 1; }

bash -n "${LIB}" || { echo "FAIL: bash -n ${LIB}"; exit 1; }
bash -n "${TEST29}" || { echo "FAIL: bash -n ${TEST29}"; exit 1; }

# shellcheck source=tests/lib/test29-evidence.sh
source "${LIB}"

FAILURES=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES+1)); }

assert_present() { # needle file
    if [ -f "$2" ] && grep -qF -- "$1" "$2"; then
        pass "present: $1 in $(basename "$2")"
    else
        fail "missing field: $1 in ${2}"
    fi
}

assert_absent() { # value file
    local count
    if [ ! -f "$2" ]; then
        fail "artifact missing: $2"
        return
    fi
    count="$(grep -Fc -- "$1" "$2" 2>/dev/null || true)"
    if [ "${count:-0}" -eq 0 ]; then
        pass "absent: ${1:0:28} in $(basename "$2")"
    else
        fail "LEAK: ${1} found ${count}x in ${2}"
    fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
BIN="${TMP}/bin"
FIX="${TMP}/fix"
mkdir -p "${BIN}" "${FIX}"

# ------------------------------------------------------------
# Synthetic credentials (all unique; none of these strings exists in
# the real world - a leak of any of them into an artifact is a failure).
# ------------------------------------------------------------
SYN_ENV_KEY="sk-synthetic-envkey-9f8e7d6c5b4a39281706f5e4"
SYN_PASSWORD="synthetic-password-00001111222233334444"
SYN_BEARER="synthetic-bearer-abcdef01234567890123456789"
SYN_CONFIG_TOKEN="synthetic-config-token-2b4c6d8e0f1a3b5c7d9e"
SYN_CR_SECRET="synthetic-crsecret-88aabbcc1122334455667788"

# ============================================================
# A. Redaction rule matrix (evidence_redact)
# ============================================================
echo "=== A. redaction rule matrix ==="

check_rule() { # id input expected
    local out
    out="$(printf '%s\n' "$2" | evidence_redact)"
    if [ "${out}" = "$3" ]; then
        pass "rule ${1}"
    else
        fail "rule ${1}: input=${2} expected=${3} got=${out}"
    fi
}

# R1: Authorization header incl. Bearer scheme
check_rule R1-header \
    'GET /v1/chat/completions Authorization: Bearer sk-live-abcdef1234567890abcdef12' \
    'GET /v1/chat/completions Authorization: ***'
# R2: free-form Bearer
check_rule R2-bearer \
    'retry with Bearer sk-live-abcdef1234567890abcdef12' \
    'retry with Bearer ***'
# R3: JSON {"name": <sensitive>, "value": <secret>} pair (name kept)
check_rule R3-nvpair \
    '{"name":"AGENTTEAMS_LLM_API_KEY","value":"pair-secret-value-0123"}' \
    '{"name":"AGENTTEAMS_LLM_API_KEY","value":"***"}'
# R4: JSON sensitive key -> value
check_rule R4-jsonkey \
    '"access_token": "json-secret-value-0123", "other": "keep"' \
    '"access_token": "***", "other": "keep"'
# R5: NAME=value env form
check_rule R5-env \
    'AGENTTEAMS_LLM_API_KEY=sk-live-abcdef1234567890abcdef12' \
    'AGENTTEAMS_LLM_API_KEY=***'
# R5b: quoted env value
check_rule R5b-envquoted \
    'WORKER_TOKEN="quoted-secret-value-0123"' \
    'WORKER_TOKEN=***'
# R5c: CLI flag form (env-style identifier, no hyphens in the name)
check_rule R5c-flag \
    '--apiKey=sk-cli-secret-value-0123' \
    '--apiKey=***'
# R6: unquoted key: value
check_rule R6-colon \
    'db_password: plaintext-pw-0123' \
    'db_password: ***'
# R7: explicit blacklist names
check_rule R7-blacklist-token \
    'MY_WORKER_TOKEN=abcdef123456789012' \
    'MY_WORKER_TOKEN=***'
check_rule R7b-blacklist-secret \
    'GITHUB_SECRET=abcdef123456789012' \
    'GITHUB_SECRET=***'
# Preservation: non-sensitive diagnostics must survive verbatim
check_rule K1-image \
    'image=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4' \
    'image=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4'
check_rule K2-roomid \
    'roomID: !abcdef012345678901234567890123456789' \
    'roomID: !abcdef012345678901234567890123456789'

# ============================================================
# B. Structured JSON scrub (evidence_scrub_json)
# ============================================================
echo "=== B. structured JSON scrub ==="

cat > "${FIX}/scrub-in.json" <<JSON
{
  "kind": "Worker",
  "metadata": {
    "name": "test-legacy-42",
    "annotations": { "agentteams.io/worker-token": "${SYN_CR_SECRET}" }
  },
  "spec": {
    "runtime": "copaw",
    "env": [
      { "name": "AGENTTEAMS_LLM_API_KEY", "value": "${SYN_CR_SECRET}" },
      { "value": "${SYN_CR_SECRET}", "name": "REV_ORDER_TOKEN" }
    ]
  },
  "status": {
    "accessToken": "${SYN_CR_SECRET}",
    "roomID": "!synthetic-room-0abc1def2ghi3jkl4mno5pqr6stu7vwx"
  }
}
JSON

SCRUB_OUT="${TMP}/scrub-out.json"
evidence_scrub_json < "${FIX}/scrub-in.json" > "${SCRUB_OUT}"
assert_absent "${SYN_CR_SECRET}" "${SCRUB_OUT}"
assert_present '"kind":"Worker"' "${SCRUB_OUT}"
assert_present '"runtime":"copaw"' "${SCRUB_OUT}"
assert_present '"name":"test-legacy-42"' "${SCRUB_OUT}"
assert_present '"name":"AGENTTEAMS_LLM_API_KEY","value":"***"' "${SCRUB_OUT}"
assert_present '"value":"***","name":"REV_ORDER_TOKEN"' "${SCRUB_OUT}"
assert_present '"accessToken":"***"' "${SCRUB_OUT}"
assert_present '"agentteams.io/worker-token":"***"' "${SCRUB_OUT}"
assert_present '!synthetic-room-0abc1def2ghi3jkl4mno5pqr6stu7vwx' "${SCRUB_OUT}"

# ============================================================
# Fixtures with synthetic credentials (collector input)
# ============================================================

cat > "${FIX}/worker-logs.raw" <<EOF
2026-11-01T00:00:01Z INFO worker started name=test-legacy-42
2026-11-01T00:00:02Z INFO matrix send url=http://127.0.0.1:8008/_matrix/client Authorization: Bearer ${SYN_BEARER}
2026-11-01T00:00:03Z WARN retry request Bearer ${SYN_BEARER} failed
2026-11-01T00:00:04Z INFO configured AGENTTEAMS_LLM_API_KEY=${SYN_ENV_KEY} provider=qwen
2026-11-01T00:00:05Z INFO login password=${SYN_PASSWORD} ok
2026-11-01T00:00:06Z INFO probes ready room=!synthetic-room-0abc1def2ghi3jkl4mno5pqr6stu7vwx
EOF

cat > "${FIX}/worker-inspect.raw.json" <<JSON
[
  {
    "Id": "abcd1234efgh5678",
    "Created": "2026-11-01T00:00:00.000000000Z",
    "Name": "/agentteams-worker-test-legacy-42",
    "RestartCount": 2,
    "State": {
      "Status": "running",
      "Running": true,
      "Paused": false,
      "Restarting": false,
      "OOMKilled": false,
      "Dead": false,
      "Pid": 4242,
      "ExitCode": 0,
      "Error": "",
      "StartedAt": "2026-11-01T00:00:05.000000000Z",
      "FinishedAt": "0001-01-01T00:00:00Z"
    },
    "Config": {
      "Image": "agentteams/qwenpaw-worker:latest",
      "Env": [
        "PATH=/usr/local/sbin:/usr/local/bin",
        "AGENTTEAMS_WORKER_NAME=test-legacy-42",
        "AGENTTEAMS_LLM_API_KEY=${SYN_ENV_KEY}",
        "DB_PASSWORD=${SYN_PASSWORD}",
        "AGENTTEAMS_DATA_DIR=/data/agentteams-controller"
      ]
    },
    "HostConfig": { "NetworkMode": "agentteams-net" },
    "NetworkSettings": {
      "Ports": { "18789/tcp": [ { "HostIp": "0.0.0.0", "HostPort": "18789" } ] }
    },
    "Mounts": [
      {
        "Type": "bind",
        "Source": "/data/agentteams/agents/test-legacy-42",
        "Destination": "/root/agentteams-fs/agents/test-legacy-42",
        "Mode": "rw",
        "RW": true,
        "Propagation": "rprivate"
      }
    ]
  }
]
JSON

cat > "${FIX}/workspace-ls.raw" <<'EOF'
/root/agentteams-fs/agents/test-legacy-42:
total 16
drwxr-xr-x 3 root root 4096 Nov  1 00:00 .
drwxr-xr-x 1 root root 4096 Nov  1 00:00 ..
drwxr-xr-x 3 root root 4096 Nov  1 00:00 .qwenpaw

/root/agentteams-fs/agents/test-legacy-42/.qwenpaw:
total 12
-rw-r--r-- 1 root root   12 Nov  1 00:00 .copaw-migrated
-rw-r--r-- 1 root root  210 Nov  1 00:00 config.json
drwxr-xr-x 3 root root 4096 Nov  1 00:00 workspaces

/root/agentteams-fs/agents/test-legacy-42/.qwenpaw/workspaces/default:
total 20
-rw-r--r-- 1 root root   70 Nov  1 00:00 agent.json
-rw-r--r-- 1 root root    2 Nov  1 00:00 chats.json
-rw-r--r-- 1 root root   24 Nov  1 00:00 runtime-switch-state.txt
EOF

cat > "${FIX}/workspace-configs.raw" <<JSON
--- /root/agentteams-fs/agents/test-legacy-42/.qwenpaw/config.json ---
{
  "model": "qwen3.6-plus",
  "access_token": "${SYN_CONFIG_TOKEN}",
  "agents": { "profiles": { "default": { "workspace_dir": "/root/agentteams-fs/agents/test-legacy-42/.qwenpaw/workspaces/default" } } }
}
--- /root/agentteams-fs/agents/test-legacy-42/.qwenpaw/workspaces/default/agent.json ---
{
  "workspace_dir": "/root/agentteams-fs/agents/test-legacy-42/.qwenpaw/workspaces/default",
  "apiKey": "${SYN_CONFIG_TOKEN}"
}
JSON

cat > "${FIX}/worker-cr.raw.json" <<JSON
{
  "apiVersion": "agentteams.io/v1beta1",
  "kind": "Worker",
  "metadata": {
    "name": "test-legacy-42",
    "namespace": "default",
    "annotations": { "agentteams.io/worker-token": "${SYN_CR_SECRET}" }
  },
  "spec": {
    "runtime": "copaw",
    "image": "higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4",
    "model": "qwen3.6-plus",
    "workerName": "test-legacy-42",
    "env": [
      { "name": "AGENTTEAMS_LLM_API_KEY", "value": "${SYN_CR_SECRET}" }
    ]
  },
  "status": {
    "phase": "provisioned",
    "roomID": "!synthetic-room-0abc1def2ghi3jkl4mno5pqr6stu7vwx",
    "accessToken": "${SYN_CR_SECRET}"
  }
}
JSON

cat > "${FIX}/minio-listing.raw" <<'EOF'
[2026-11-01 00:00:10 +0000 UTC]     12 B agents/test-legacy-42/.qwenpaw/.copaw-migrated
[2026-11-01 00:00:10 +0000 UTC]     24 B agents/test-legacy-42/.qwenpaw/workspaces/default/runtime-switch-state.txt
[2026-11-01 00:00:10 +0000 UTC]      2 B agents/test-legacy-42/.qwenpaw/workspaces/default/chats.json
[2026-11-01 00:00:10 +0000 UTC]     24 B agents/test-legacy-42/.qwenpaw.secret/runtime-switch-secret.txt
EOF

# ------------------------------------------------------------
# Stub docker: serves the fixtures; records every call in ${ORDER}.
# ------------------------------------------------------------
ORDER="${TMP}/order.txt"
export ORDER
cat > "${BIN}/docker" <<STUBEOF
#!/usr/bin/env bash
printf '%s\n' "docker \$*" >> "\${ORDER}"
case "\$1" in
    logs)
        cat "${FIX}/worker-logs.raw"
        ;;
    inspect)
        cat "${FIX}/worker-inspect.raw.json"
        ;;
    exec)
        cmd="\${!#}"
        case "\${cmd}" in
            *"ls -laR"*) cat "${FIX}/workspace-ls.raw" ;;
            *)           cat "${FIX}/workspace-configs.raw" ;;
        esac
        ;;
esac
exit 0
STUBEOF
chmod +x "${BIN}/docker"
export PATH="${BIN}:${PATH}"

# ------------------------------------------------------------
# Stub the caller-provided functions the collector relies on.
# ------------------------------------------------------------
worker_container_name() { printf '%s\n' "agentteams-worker-${1}"; }
log_info() { printf 'INFO %s\n' "$*"; }
exec_in_agent() {
    printf '%s\n' "exec_in_agent $*" >> "${ORDER}"
    case "$1 $2" in
        "agt get")
            cat "${FIX}/worker-cr.raw.json"
            ;;
        "agt delete")
            printf '%s\n' "cleanup:agt-delete" >> "${ORDER}"
            ;;
    esac
    return 0
}
exec_in_manager() {
    printf '%s\n' "exec_in_manager $*" >> "${ORDER}"
    case "$1 $2" in
        "mc ls")
            cat "${FIX}/minio-listing.raw"
            ;;
        "mc rm")
            printf '%s\n' "cleanup:mc-rm" >> "${ORDER}"
            ;;
    esac
    return 0
}
remove_worker_container() {
    printf '%s\n' "cleanup:rm-container" >> "${ORDER}"
    return 0
}

count_in_files() { # value files...
    local value="$1" total=0 f c
    shift
    for f in "$@"; do
        [ -f "${f}" ] || continue
        c="$(grep -Fc -- "${value}" "${f}" 2>/dev/null || true)"
        total=$((total + ${c:-0}))
    done
    printf '%s' "${total}"
}

ARTIFACTS="worker-logs.txt worker-inspect.json workspace-listing.txt worker-cr.json minio-listing.txt"

assert_all_redacted() { # evidence-dir
    local evd="$1" v raw art
    for v in "${SYN_ENV_KEY}" "${SYN_PASSWORD}" "${SYN_BEARER}" "${SYN_CONFIG_TOKEN}" "${SYN_CR_SECRET}"; do
        for a in ${ARTIFACTS}; do
            assert_absent "${v}" "${evd}/${a}"
        done
    done
    echo "--- redaction comparison (raw fixture occurrences -> artifact occurrences) ---"
    for v in "${SYN_ENV_KEY}" "${SYN_PASSWORD}" "${SYN_BEARER}" "${SYN_CONFIG_TOKEN}" "${SYN_CR_SECRET}"; do
        raw="$(count_in_files "${v}" "${FIX}"/*.raw "${FIX}"/*.raw.json 2>/dev/null)"
        art="$(count_in_files "${v}" "${evd}"/* 2>/dev/null)"
        echo "redaction: ${v} raw=${raw} artifacts=${art}"
        [ "${art}" -eq 0 ] || fail "LEAK: ${v} present in ${evd} artifacts"
    done
}

assert_required_fields() { # evidence-dir
    local evd="$1"
    assert_present 'worker started name=test-legacy-42' "${evd}/worker-logs.txt"
    assert_present 'Bearer ***' "${evd}/worker-logs.txt"
    assert_present 'Authorization: ***' "${evd}/worker-logs.txt"
    assert_present '"image":"agentteams/qwenpaw-worker:latest"' "${evd}/worker-inspect.json"
    assert_present '"restartCount":2' "${evd}/worker-inspect.json"
    assert_present '"status":"running"' "${evd}/worker-inspect.json"
    assert_present '"exitCode":0' "${evd}/worker-inspect.json"
    assert_present '"networkMode":"agentteams-net"' "${evd}/worker-inspect.json"
    assert_present '"source":"/data/agentteams/agents/test-legacy-42"' "${evd}/worker-inspect.json"
    assert_present '18789/tcp' "${evd}/worker-inspect.json"
    assert_present '"name":"AGENTTEAMS_LLM_API_KEY","value":"***"' "${evd}/worker-inspect.json"
    assert_present '"name":"DB_PASSWORD","value":"***"' "${evd}/worker-inspect.json"
    assert_absent '/data/agentteams-controller' "${evd}/worker-inspect.json"
    assert_present '=== workspace listing: /root/agentteams-fs/agents/test-legacy-42 ===' "${evd}/workspace-listing.txt"
    assert_present 'runtime-switch-state.txt' "${evd}/workspace-listing.txt"
    assert_present '"access_token": "***"' "${evd}/workspace-listing.txt"
    assert_present '"apiKey": "***"' "${evd}/workspace-listing.txt"
    assert_present '"model": "qwen3.6-plus"' "${evd}/workspace-listing.txt"
    assert_present '"kind":"Worker"' "${evd}/worker-cr.json"
    assert_present '"runtime":"copaw"' "${evd}/worker-cr.json"
    assert_present '"name":"test-legacy-42"' "${evd}/worker-cr.json"
    assert_present '"accessToken":"***"' "${evd}/worker-cr.json"
    assert_present '"value":"***"' "${evd}/worker-cr.json"
    assert_present 'agents/test-legacy-42/.qwenpaw/.copaw-migrated' "${evd}/minio-listing.txt"
}

# ============================================================
# C. Forced failure: TESTS_FAILED=1 -> collect before cleanup
# ============================================================
echo "=== C. forced failure path ==="

C_EVDIR="${TMP}/evC"
: > "${ORDER}"
EVIDENCE_DIR="${C_EVDIR}"
EVIDENCE_COLLECTED=0
STORAGE_PREFIX="agentteams/agentteams-storage"
TESTS_FAILED=1
TEST_WORKER="test-legacy-42"

# The same gate _cleanup() uses before it deletes anything.
if [ "${TESTS_FAILED}" -gt 0 ]; then
    collect_test29_evidence "${TEST_WORKER}"
fi
# The deletes that follow in _cleanup (stubbed).
exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
remove_worker_container "${TEST_WORKER}"
exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true

for a in ${ARTIFACTS}; do
    [ -f "${C_EVDIR}/${a}" ] && pass "artifact exists: ${a}" || fail "artifact missing: ${a}"
done
assert_all_redacted "${C_EVDIR}"
assert_required_fields "${C_EVDIR}"

# Evidence-before-cleanup ordering: first evidence line < first delete line.
EV_LINE="$(grep -n 'docker logs' "${ORDER}" | head -1 | cut -d: -f1)"
CR_LINE="$(grep -n 'exec_in_agent agt get' "${ORDER}" | head -1 | cut -d: -f1)"
CL_LINE="$(grep -n 'cleanup:' "${ORDER}" | head -1 | cut -d: -f1)"
if [ -n "${EV_LINE:-}" ] && [ -n "${CL_LINE:-}" ] && [ "${EV_LINE}" -lt "${CL_LINE}" ]; then
    pass "evidence-before-cleanup: logs at line ${EV_LINE} < first delete at line ${CL_LINE}"
else
    fail "evidence-before-cleanup ordering broken (logs=${EV_LINE:-none} cr=${CR_LINE:-none} delete=${CL_LINE:-none})"
fi

# ============================================================
# D. Normal upgrade (success path): no evidence, no interference
# ============================================================
echo "=== D. success path ==="

D_EVDIR="${TMP}/evD"
: > "${ORDER}"
EVIDENCE_DIR="${D_EVDIR}"
EVIDENCE_COLLECTED=0
TESTS_FAILED=0
if [ "${TESTS_FAILED}" -gt 0 ]; then
    collect_test29_evidence "test-legacy-42"
fi
exec_in_agent agt delete worker "test-legacy-42" 2>/dev/null || true
remove_worker_container "test-legacy-42"
exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/test-legacy-42/" 2>/dev/null || true

if [ ! -e "${D_EVDIR}" ] && [ "${EVIDENCE_COLLECTED:-0}" != "1" ]; then
    pass "success path: collector not invoked, no evidence artifacts"
else
    fail "success path: evidence collected on the success path"
fi
if grep -q 'cleanup:agt-delete' "${ORDER}" && ! grep -q 'docker logs' "${ORDER}"; then
    pass "success path: cleanup ran without evidence collection"
else
    fail "success path: unexpected order file contents"
fi

# ============================================================
# E. Delayed start (readiness-timeout path): direct collector call
# ============================================================
echo "=== E. delayed-start / readiness-timeout path ==="

E_EVDIR="${TMP}/evE"
: > "${ORDER}"
EVIDENCE_DIR="${E_EVDIR}"
EVIDENCE_COLLECTED=0
# The readiness branch calls the collector directly (no TESTS_FAILED gate).
collect_test29_evidence "test-legacy-42"

for a in ${ARTIFACTS}; do
    [ -f "${E_EVDIR}/${a}" ] && pass "readiness artifact exists: ${a}" || fail "readiness artifact missing: ${a}"
done
assert_all_redacted "${E_EVDIR}"
assert_present '"image":"agentteams/qwenpaw-worker:latest"' "${E_EVDIR}/worker-inspect.json"
assert_present '"kind":"Worker"' "${E_EVDIR}/worker-cr.json"

# Static wiring: the readiness branch still collects before teardown.
READINESS_BLOCK="$(sed -n '/Worker not ready before seeding/,/exit 1/p' "${TEST29}")"
if printf '%s\n' "${READINESS_BLOCK}" | grep -q 'collect_test29_evidence'; then
    pass "readiness branch still calls collect_test29_evidence"
else
    fail "readiness branch no longer collects evidence"
fi
CB_COLLECT="$(printf '%s\n' "${READINESS_BLOCK}" | grep -n 'collect_test29_evidence' | head -1 | cut -d: -f1)"
CB_TEARDOWN="$(printf '%s\n' "${READINESS_BLOCK}" | grep -n 'test_teardown' | head -1 | cut -d: -f1)"
if [ -n "${CB_COLLECT:-}" ] && [ -n "${CB_TEARDOWN:-}" ] && [ "${CB_COLLECT}" -lt "${CB_TEARDOWN}" ]; then
    pass "readiness branch: collect (line ${CB_COLLECT}) precedes teardown (line ${CB_TEARDOWN})"
else
    fail "readiness branch: collect/teardown order broken"
fi

# Static wiring: _cleanup gate intact.
CLEANUP_BLOCK="$(sed -n '/^_cleanup()/,/^}/p' "${TEST29}")"
if printf '%s\n' "${CLEANUP_BLOCK}" | grep -q 'TESTS_FAILED' \
   && printf '%s\n' "${CLEANUP_BLOCK}" | grep -q 'collect_test29_evidence'; then
    pass "_cleanup still gates evidence on TESTS_FAILED"
else
    fail "_cleanup gate changed"
fi

# ============================================================
# F. Predicates not weakened (static inventory of test-29)
# ============================================================
echo "=== F. predicate inventory ==="

count_in_file() { # pattern file
    grep -cE -- "$1" "$2" 2>/dev/null || true
}

P_COUNT="$(count_in_file '^[[:space:]]*assert_eq ' "${TEST29}")"
if [ "${P_COUNT:-0}" = "6" ]; then
    pass "6 migrated-state assert_eq predicates intact"
else
    fail "assert_eq predicate count changed: want 6, got ${P_COUNT:-0}"
fi
M_COUNT="$(count_in_file '^minio_presence_check ' "${TEST29}")"
if [ "${M_COUNT:-0}" = "5" ]; then
    pass "5 minio presence predicates intact"
else
    fail "minio_presence_check count changed: want 5, got ${M_COUNT:-0}"
fi
assert_present 'READINESS_TIMEOUT=240' "${TEST29}"
assert_present 'MIGRATION_TIMEOUT=180' "${TEST29}"
assert_present 'MINIO_MIGRATION_TIMEOUT=120' "${TEST29}"
assert_present 'wait_worker_provisioned "${TEST_WORKER}" 180' "${TEST29}"

# ============================================================
# Summary
# ============================================================
echo
if [ "${FAILURES}" -eq 0 ]; then
    echo "ALL PASS: test-29 evidence redaction check"
    exit 0
else
    echo "${FAILURES} FAILURES in test-29 evidence redaction check"
    exit 1
fi
