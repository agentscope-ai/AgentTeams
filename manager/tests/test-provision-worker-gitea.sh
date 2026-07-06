#!/bin/bash
# test-provision-worker-gitea.sh
# Static harness for provision-worker-gitea.sh + the setup-mcp-proxy.sh flag it relies on.
#
# PATH-shims curl and mc so nothing touches a real Gitea/Higress/MinIO — every
# invocation is appended to a call-log file that the assertions inspect.
#
# Usage: bash manager/tests/test-provision-worker-gitea.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
PROVISION_SCRIPT="${PROJECT_ROOT}/scripts/provision-worker-gitea.sh"
SETUP_MCP_PROXY_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found: ${haystack}"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "should NOT contain '${needle}'" "found it"
    fi
}

# ── CRLF strip (checkout is autocrlf=true) ────────────────────────────────────
strip_cr() {
    local f="$1"
    sed -i 's/\r$//' "${f}" 2>/dev/null || true
}
strip_cr "${PROVISION_SCRIPT}"
strip_cr "${SETUP_MCP_PROXY_SCRIPT}"

# ── Stub /opt/hiclaw/scripts/lib/hiclaw-env.sh ────────────────────────────────
# Both scripts `source /opt/hiclaw/scripts/lib/hiclaw-env.sh` unconditionally
# (the real path only exists inside a built container). Provide a harmless
# stub here so the harness can run standalone; real behavior is unaffected
# since production containers always have the real file.
HICLAW_ENV_STUB_DIR="/opt/hiclaw/scripts/lib"
if [ ! -f "${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh" ]; then
    if mkdir -p "${HICLAW_ENV_STUB_DIR}" 2>/dev/null; then
        cat > "${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh" << 'ENVSTUB'
# Test-harness stub — real containers ship the actual hiclaw-env.sh.
: "${HICLAW_STORAGE_PREFIX:=hiclaw/hiclaw-storage}"
ensure_mc_credentials() { :; }
log() { echo "[hiclaw-env-stub] $*" >&2; }
ENVSTUB
    else
        echo "SKIP: cannot write ${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh (no permission) — run this harness as root/in a container" >&2
        exit 1
    fi
fi

# ── Fake bin dir: PATH-shimmed curl / mc / jq(passthrough) ────────────────────
# Each test case gets its own sandbox (fake HOME, fake bin dir, fresh call log)
# so shim state never leaks between cases.
new_sandbox() {
    local sandbox
    sandbox=$(mktemp -d "${TMPDIR_ROOT}/sandbox-XXXXXX")
    mkdir -p "${sandbox}/bin" "${sandbox}/home" "${sandbox}/fs/shared/projects" "${sandbox}/fs/agents" "${sandbox}/hiclaw-fs/shared/projects"
    : > "${sandbox}/calls.log"
    echo "${sandbox}"
}

write_manifest() {
    # write_manifest <sandbox> <project-id> <json>
    local sandbox="$1" pid="$2" json="$3"
    mkdir -p "${sandbox}/hiclaw-fs/shared/projects/${pid}"
    printf '%s' "${json}" > "${sandbox}/hiclaw-fs/shared/projects/${pid}/manifest.json"
}

# The curl shim: logs every call ("METHOD URL BODY") to calls.log, and returns
# canned JSON bodies keyed on path so the scripts under test see success
# responses without ever reaching a network.
write_curl_shim() {
    local sandbox="$1"
    cat > "${sandbox}/bin/curl" << 'CURLSHIM'
#!/bin/bash
# Fake curl: records the call, fakes plausible JSON responses.
CALL_LOG="${FAKE_CALL_LOG:?FAKE_CALL_LOG not set}"

METHOD="GET"
URL=""
BODY=""
OUT_FILE=""
WANT_CODE="false"

args=("$@")
i=0
n=${#args[@]}
while [ $i -lt $n ]; do
    a="${args[$i]}"
    case "$a" in
        -X)
            i=$((i+1)); METHOD="${args[$i]}"
            ;;
        -d|--data)
            i=$((i+1)); BODY="${args[$i]}"
            ;;
        -o)
            i=$((i+1)); OUT_FILE="${args[$i]}"
            ;;
        -w)
            i=$((i+1))
            if [ "${args[$i]}" = "%{http_code}" ]; then WANT_CODE="true"; fi
            ;;
        -H|-b)
            i=$((i+1))
            ;;
        -s|-sf|-f|-sS)
            ;;
        http*://*)
            URL="$a"
            ;;
        *)
            ;;
    esac
    i=$((i+1))
done

echo "${METHOD} ${URL} ${BODY}" >> "${CALL_LOG}"

# ── Canned responses ──────────────────────────────────────────────────────
RESPONSE='{"success":true}'
HTTP_CODE=200

case "${URL}" in
    */api/v1/admin/users)
        RESPONSE='{"id":1,"login":"created"}'
        HTTP_CODE=201
        ;;
    */api/v1/admin/users/*)
        RESPONSE=''
        HTTP_CODE=204
        ;;
    */api/v1/users/*/tokens)
        RESPONSE='{"sha1":"faketoken1234567890"}'
        HTTP_CODE=201
        ;;
    */api/v1/repos/*/collaborators/*)
        RESPONSE=''
        HTTP_CODE=204
        ;;
    */api/v1/users/*)
        # Most-specific /users/<name>/... cases are matched above this one.
        if [ "${METHOD}" = "GET" ]; then
            # Simulate "user does not exist yet" so ensure_gitea_user creates it.
            RESPONSE='{"message":"not found"}'
            HTTP_CODE=404
        fi
        ;;
    */v1/mcpServer/consumers*)
        if [ "${METHOD}" = "GET" ]; then
            RESPONSE='{"total":0}'
        else
            RESPONSE='{"success":true}'
        fi
        HTTP_CODE=200
        ;;
    */v1/mcpServer*)
        RESPONSE='{"success":true}'
        HTTP_CODE=200
        ;;
    */v1/service-sources)
        RESPONSE='{"success":true}'
        HTTP_CODE=200
        ;;
    *)
        RESPONSE='{"success":true}'
        HTTP_CODE=200
        ;;
esac

if [ -n "${OUT_FILE}" ]; then
    printf '%s' "${RESPONSE}" > "${OUT_FILE}"
    if [ "${WANT_CODE}" = "true" ]; then
        printf '%s' "${HTTP_CODE}"
    fi
else
    printf '%s' "${RESPONSE}"
fi

if [ "${HTTP_CODE}" -ge 400 ]; then
    exit 22
fi
exit 0
CURLSHIM
    chmod +x "${sandbox}/bin/curl"
}

# The jq shim: wraps the REAL jq but forces a parse failure (non-zero exit,
# no stdout) whenever the target file contains the literal marker "not
# json" — reproducing exactly what a real jq does on invalid JSON, without
# needing an actually-corrupt-enough fixture. Every other invocation passes
# straight through to the real jq on PATH. Used only by TC13 (corrupt
# mcporter.json) so the rest of the suite keeps exercising real jq.
write_jq_corrupt_shim() {
    local sandbox="$1" real_jq
    real_jq=$(command -v jq) || { echo "SKIP: no real jq on PATH, cannot install corrupt-jq shim" >&2; return 1; }
    cat > "${sandbox}/bin/jq" << JQSHIM
#!/bin/bash
REAL_JQ="${real_jq}"
for a in "\$@"; do
    if [ -f "\$a" ] && grep -q "not json" "\$a" 2>/dev/null; then
        echo "jq: error: parse error (simulated)" >&2
        exit 5
    fi
done
exec "\${REAL_JQ}" "\$@"
JQSHIM
    chmod +x "${sandbox}/bin/jq"
}

# The mc shim: `cat` reads from the sandbox's fake hiclaw-fs manifest tree
# (falls back to empty), `cp`/`mirror`/`stat` are no-ops that succeed.
write_mc_shim() {
    local sandbox="$1"
    cat > "${sandbox}/bin/mc" << MCSHIM
#!/bin/bash
CALL_LOG="\${FAKE_CALL_LOG:?FAKE_CALL_LOG not set}"
echo "mc \$*" >> "\${CALL_LOG}"
SANDBOX="${sandbox}"

case "\$1" in
    cat)
        path="\$2"
        # path looks like hiclaw/hiclaw-storage/shared/projects/<id>/manifest.json
        rel="\${path#*/shared/}"
        f="\${SANDBOX}/hiclaw-fs/shared/\${rel}"
        if [ -f "\${f}" ]; then
            cat "\${f}"
            exit 0
        fi
        exit 1
        ;;
    cp|mirror|stat)
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
MCSHIM
    chmod +x "${sandbox}/bin/mc"
}

run_provision() {
    # run_provision <sandbox> <args...>
    # Preserves the wrapped script's real exit code (needed by TC11/TC12)
    # while still resetting the fake higress cookie afterward either way.
    local sandbox="$1"; shift
    local rc=0
    HOME="${sandbox}/home" \
    PATH="${sandbox}/bin:${PATH}" \
    FAKE_CALL_LOG="${sandbox}/calls.log" \
    GITEA_URL="https://git.fake.local" \
    GITEA_ADMIN_TOKEN="fake-admin-token" \
    HIGRESS_COOKIE_FILE="${sandbox}/higress-cookie" \
    HICLAW_AI_GATEWAY_DOMAIN="aigw-test.local" \
    HICLAW_STORAGE_PREFIX="hiclaw/hiclaw-storage" \
    SETUP_MCP_PROXY_SCRIPT="${SETUP_MCP_PROXY_SCRIPT}" \
    bash "${PROVISION_SCRIPT}" "$@" 2>&1 || rc=$?
    : > "${sandbox}/higress-cookie" 2>/dev/null || true
    return "${rc}"
}

run_setup_mcp_proxy() {
    # run_setup_mcp_proxy <sandbox> <args...>
    local sandbox="$1"; shift
    : > "${sandbox}/higress-cookie"
    HOME="${sandbox}/home" \
    PATH="${sandbox}/bin:${PATH}" \
    FAKE_CALL_LOG="${sandbox}/calls.log" \
    HIGRESS_COOKIE_FILE="${sandbox}/higress-cookie" \
    HICLAW_AI_GATEWAY_DOMAIN="aigw-test.local" \
    HICLAW_STORAGE_PREFIX="hiclaw/hiclaw-storage" \
    bash "${SETUP_MCP_PROXY_SCRIPT}" "$@" 2>&1
}

calls() {
    cat "$1/calls.log"
}

FIXTURE_MANIFEST='{
  "id": "proj1",
  "team": "teamA",
  "description": "test project",
  "repos": [
    {"url": "https://git.fake.local/teamA/repo-rw.git", "access": "rw", "name": "repo-rw"},
    {"url": "https://git.fake.local/teamA/repo-ro.git", "access": "ro", "name": "repo-ro"}
  ],
  "recordedWorkers": ["alice"],
  "updatedAt": "2026-07-03T00:00:00Z"
}'

# ── Tests ─────────────────────────────────────────────────────────────────────

echo ""
echo "=== TC1: bash -n on both scripts ==="
{
    if bash -n "${PROVISION_SCRIPT}"; then
        pass "provision-worker-gitea.sh parses (bash -n)"
    else
        fail "provision-worker-gitea.sh parses (bash -n)" "exit 0" "non-zero"
    fi
    if bash -n "${SETUP_MCP_PROXY_SCRIPT}"; then
        pass "setup-mcp-proxy.sh parses (bash -n)"
    else
        fail "setup-mcp-proxy.sh parses (bash -n)" "exit 0" "non-zero"
    fi
}

echo ""
echo "=== TC2: provision — no all-workers consumer PUT ever fires ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    # A workers-registry.json IS present here (with other workers besides
    # alice) precisely so that, if setup-mcp-proxy.sh's Step 5 broadcast path
    # were ever reached, it would show up as a multi-worker consumer list.
    mkdir -p "${s}/home"
    cat > "${s}/home/workers-registry.json" << 'EOF'
{"workers": {"alice": {}, "bob": {}, "carol": {}}}
EOF
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    out=$(run_provision "${s}" alice --project proj1)
    log=$(calls "${s}")

    # setup-mcp-proxy.sh's own Step 3 legitimately authorizes "manager" alone
    # for any server it sets up (unrelated to the #14 hazard) — that single-
    # consumer PUT is fine. What must NEVER appear is the Step-5 ALL-WORKERS
    # broadcast list built from workers-registry.json (manager + every
    # worker-<name>, here that would include "worker-bob"/"worker-carol").
    assert_not_contains "no broadcast PUT contains worker-bob" '"worker-bob"' "${log}"
    assert_not_contains "no broadcast PUT contains worker-carol" '"worker-carol"' "${log}"
    assert_not_contains "no multi-consumer (broadcast-shaped) PUT fires" '"consumers":["manager","worker-' "${log}"
}

echo ""
echo "=== TC3: provision — single-consumer PUT payload is exactly [\"worker-alice\"] ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    run_provision "${s}" alice --project proj1 > /dev/null
    log=$(calls "${s}")
    assert_contains "consumers PUT payload is exactly [\"worker-alice\"]" '"consumers":["worker-alice"]' "${log}"
}

echo ""
echo "=== TC4: provision — ro/rw manifest maps to read/write collaborator roles ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    run_provision "${s}" alice --project proj1 > /dev/null
    log=$(calls "${s}")
    assert_contains "rw repo -> collaborators PUT" "PUT https://git.fake.local/api/v1/repos/teamA/repo-rw/collaborators/worker-alice" "${log}"
    assert_contains "rw -> permission write" '"permission": "write"' "${log}"
    assert_contains "ro repo -> collaborators PUT" "PUT https://git.fake.local/api/v1/repos/teamA/repo-ro/collaborators/worker-alice" "${log}"
    assert_contains "ro -> permission read" '"permission": "read"' "${log}"
}

echo ""
echo "=== TC5: --deprovision reverses grants and removes the registration ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    run_provision "${s}" alice --project proj1 > /dev/null
    : > "${s}/calls.log"   # reset log to isolate the deprovision call sequence
    out=$(run_provision "${s}" alice --deprovision proj1)
    log=$(calls "${s}")
    assert_contains "reports deprovisioning complete" "Deprovisioning complete for worker=alice project=proj1" "${out}"
    assert_contains "removes rw collaborator grant" "DELETE https://git.fake.local/api/v1/repos/teamA/repo-rw/collaborators/worker-alice" "${log}"
    assert_contains "removes ro collaborator grant" "DELETE https://git.fake.local/api/v1/repos/teamA/repo-ro/collaborators/worker-alice" "${log}"
    assert_contains "removes the per-worker mcp server registration" "DELETE http://127.0.0.1:8001/v1/mcpServer?name=mcp-gitea-alice" "${log}"
}

echo ""
echo "=== TC6: --deprovision --delete-user also deletes the Gitea user ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    run_provision "${s}" alice --project proj1 > /dev/null
    : > "${s}/calls.log"
    run_provision "${s}" alice --deprovision proj1 --delete-user > /dev/null
    log=$(calls "${s}")
    assert_contains "deletes the Gitea user" "DELETE https://git.fake.local/api/v1/admin/users/worker-alice" "${log}"
}

echo ""
echo "=== TC7: PAT never appears in provision stdout/log ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    out=$(run_provision "${s}" alice --project proj1)
    assert_not_contains "PAT not echoed to stdout" "faketoken1234567890" "${out}"
}

echo ""
echo "=== TC8: setup-mcp-proxy.sh WITHOUT the new flags — default call sequence unchanged ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    # No workers-registry.json present -> Step 5's "no workers-registry.json found" path,
    # exactly like today (byte-identical: no --skip-worker-broadcast / --consumers given).
    out=$(run_setup_mcp_proxy "${s}" notion https://mcp.notion.com/mcp http --header "Authorization: Bearer ntn_xxx")
    assert_contains "Step 5 still runs its default authorization path" "Step 5: Authorizing existing Workers for mcp-notion" "${out}"
    assert_contains "falls back to the legacy no-registry message" "No workers-registry.json found, skipping Worker authorization" "${out}"
    assert_not_contains "does NOT print the skip-broadcast message" "Step 5: Skipped (--skip-worker-broadcast)" "${out}"
    assert_not_contains "does NOT print the explicit-consumers message" "Authorizing explicit consumer list" "${out}"
}

echo ""
echo "=== TC9: setup-mcp-proxy.sh WITHOUT flags, WITH a workers-registry.json — still broadcasts (unchanged) ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    mkdir -p "${s}/home"
    cat > "${s}/home/workers-registry.json" << 'EOF'
{"workers": {"bob": {}, "carol": {}}}
EOF
    run_setup_mcp_proxy "${s}" notion https://mcp.notion.com/mcp http > /dev/null
    log=$(calls "${s}")
    assert_contains "still authorizes manager+bob+carol (today's behavior)" '"consumers":["manager","worker-bob","worker-carol"]' "${log}"
}

echo ""
echo "=== TC10: --skip-worker-broadcast skips Step 5 entirely ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    mkdir -p "${s}/home"
    cat > "${s}/home/workers-registry.json" << 'EOF'
{"workers": {"bob": {}}}
EOF
    out=$(run_setup_mcp_proxy "${s}" gitea-alice https://gitea-mcp:8080 http --header "Authorization: Bearer PATXYZ" --skip-worker-broadcast)
    log=$(calls "${s}")
    assert_contains "reports skipped" "Step 5: Skipped (--skip-worker-broadcast)" "${out}"
    assert_not_contains "no broadcast PUT with worker-bob fires" '"consumers":["manager","worker-bob"]' "${log}"
}

echo ""
echo "=== TC11: provision — missing/unreadable manifest aborts BEFORE 'Provisioning complete' ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    # Deliberately do NOT write a manifest for proj-missing anywhere (mc cat
    # and the hiclaw-fs fallback both fail) so read_manifest_repos returns 1.
    rc=0
    out=$(run_provision "${s}" alice --project proj-missing) || rc=$?
    log=$(calls "${s}")
    assert_eq "do_provision exits non-zero on unreadable manifest" "1" "${rc}"
    assert_contains "logs the manifest-read error" "ERROR: could not read manifest for project proj-missing; aborting (no repo grants set)" "${out}"
    assert_not_contains "does NOT print Provisioning complete" "Provisioning complete" "${out}"
    assert_not_contains "no collaborator PUT ever fires" "/collaborators/worker-alice" "${log}"
}

echo ""
echo "=== TC12: --deprovision — missing/unreadable manifest aborts BEFORE deregistration ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"
    run_provision "${s}" alice --project proj1 > /dev/null
    : > "${s}/calls.log"
    # Now deprovision a DIFFERENT project id with no manifest on disk at all.
    rc=0
    out=$(run_provision "${s}" alice --deprovision proj-missing) || rc=$?
    log=$(calls "${s}")
    assert_eq "do_deprovision exits non-zero on unreadable manifest" "1" "${rc}"
    assert_contains "logs the manifest-read error" "ERROR: could not read manifest for project proj-missing; aborting (no repo grants reversed, registration untouched)" "${out}"
    assert_not_contains "does NOT print Deprovisioning complete" "Deprovisioning complete" "${out}"
    assert_not_contains "no collaborator DELETE ever fires" "/collaborators/worker-alice" "${log}"
    assert_not_contains "never reaches deregistration of the mcp server" "DELETE http://127.0.0.1:8001/v1/mcpServer?name=mcp-gitea-alice" "${log}"
}

echo ""
echo "=== TC13: provision — corrupt existing mcporter.json is kept intact, not truncated ==="
{
    # update_worker_mcporter() hardcodes /root/hiclaw-fs/agents/<worker> and
    # /data/worker-creds/<worker>.env (not sandboxable via HOME/PATH), so this
    # case seeds those real absolute paths for a throwaway worker name, runs
    # a full do_provision via run_provision (with a jq shim that simulates a
    # parse failure on the corrupt file, like a real jq would), and cleans up
    # afterward regardless of outcome.
    if ! mkdir -p /data/worker-creds 2>/dev/null; then
        echo "  SKIP: cannot write /data/worker-creds (no permission) — skipping TC13"
    else
        s=$(new_sandbox)
        write_curl_shim "${s}"
        write_mc_shim "${s}"
        write_manifest "${s}" "proj1" "${FIXTURE_MANIFEST}"

        tc13_worker="tc13probe$$"
        tc13_agent_dir="/root/hiclaw-fs/agents/${tc13_worker}"
        tc13_creds="/data/worker-creds/${tc13_worker}.env"
        cleanup_tc13() { rm -rf "${tc13_agent_dir}"; rm -f "${tc13_creds}"; }

        mkdir -p "${tc13_agent_dir}/config"
        CORRUPT_CONTENT='not json { this is deliberately invalid }'
        printf '%s' "${CORRUPT_CONTENT}" > "${tc13_agent_dir}/config/mcporter.json"
        printf 'WORKER_GATEWAY_KEY="tc13-fake-key"\n' > "${tc13_creds}"

        if ! write_jq_corrupt_shim "${s}"; then
            echo "  SKIP: no real jq on PATH to wrap — skipping TC13"
        else
            rc=0
            out=$(run_provision "${s}" "${tc13_worker}" --project proj1) || rc=$?

            assert_eq "provisioning does not abort on corrupt existing mcporter.json" "0" "${rc}"
            actual_content=$(cat "${tc13_agent_dir}/config/mcporter.json")
            assert_eq "corrupt mcporter.json left byte-for-byte intact (not truncated)" "${CORRUPT_CONTENT}" "${actual_content}"
            assert_contains "logs the mcporter-merge-failed WARNING" "WARNING: mcporter merge failed — keeping existing config/mcporter.json for ${tc13_worker}" "${out}"
            assert_contains "still completes provisioning after the merge failure" "Provisioning complete for worker=${tc13_worker} project=proj1" "${out}"
        fi

        cleanup_tc13
    fi
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
