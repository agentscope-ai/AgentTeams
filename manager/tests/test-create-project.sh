#!/bin/bash
# test-create-project.sh
# Static harness for create-project.sh's federation flags (--team / --repo,
# decision #16). PATH-shims curl, mc, AND hiclaw so nothing touches a real
# Matrix/MinIO/controller — create-project.sh curls Matrix and runs
# `mc mirror` unconditionally, and (when --team is given) shells out to
# `hiclaw apply -f`, so all three must be stubbed for the harness to run
# offline. Every shimmed call is appended to a call-log file the assertions
# inspect.
#
# Usage: bash manager/tests/test-create-project.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CREATE_PROJECT_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/project-management/scripts/create-project.sh"

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
strip_cr "${CREATE_PROJECT_SCRIPT}"

# ── Stub /opt/hiclaw/scripts/lib/hiclaw-env.sh ────────────────────────────────
# create-project.sh sources this unconditionally (the real path only exists
# inside a built Manager container). Provide a harmless stub so the harness
# can run standalone; real containers always have the actual file.
HICLAW_ENV_STUB_DIR="/opt/hiclaw/scripts/lib"
if [ ! -f "${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh" ]; then
    if mkdir -p "${HICLAW_ENV_STUB_DIR}" 2>/dev/null; then
        cat > "${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh" << 'ENVSTUB'
# Test-harness stub — real containers ship the actual hiclaw-env.sh.
: "${AGENTTEAMS_STORAGE_PREFIX:=agentteams/agentteams-storage}"
: "${AGENTTEAMS_MATRIX_URL:=http://matrix.fake.local}"
ensure_mc_credentials() { :; }
log() { echo "[hiclaw-env-stub] $*" >&2; }
ENVSTUB
    else
        echo "SKIP: cannot write ${HICLAW_ENV_STUB_DIR}/hiclaw-env.sh (no permission) — run this harness as root/in a container" >&2
        exit 1
    fi
fi

# ── Fake bin dir: PATH-shimmed curl / mc / hiclaw / jq(passthrough) ───────────
# Each test case gets its own sandbox so shim state never leaks between cases.
new_sandbox() {
    local sandbox
    sandbox=$(mktemp -d "${TMPDIR_ROOT}/sandbox-XXXXXX")
    mkdir -p "${sandbox}/bin" "${sandbox}/home" "${sandbox}/root/hiclaw-fs/shared/projects" \
        "${sandbox}/root/hiclaw-fs/agents/manager" "${sandbox}/data/worker-creds"
    : > "${sandbox}/calls.log"
    echo "${sandbox}"
}

# The curl shim: logs every call ("METHOD URL BODY") and returns plausible
# canned JSON so the script's Matrix flow completes without a network.
write_curl_shim() {
    local sandbox="$1"
    cat > "${sandbox}/bin/curl" << 'CURLSHIM'
#!/bin/bash
CALL_LOG="${FAKE_CALL_LOG:?FAKE_CALL_LOG not set}"

METHOD="GET"
URL=""
BODY=""

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

case "${URL}" in
    */_matrix/client/v3/login)
        echo '{"access_token":"faketoken-123"}'
        ;;
    */_matrix/client/v3/createRoom)
        echo '{"room_id":"!fakeroom:matrix.fake.local"}'
        ;;
    *)
        echo '{}'
        ;;
esac
exit 0
CURLSHIM
    chmod +x "${sandbox}/bin/curl"
}

# The mc shim: cp/mirror/stat/cat are all no-op successes (stat always finds
# the meta.json the script just wrote, since create-project.sh never checks
# mc's own storage — only that the exit code is 0).
write_mc_shim() {
    local sandbox="$1"
    cat > "${sandbox}/bin/mc" << 'MCSHIM'
#!/bin/bash
CALL_LOG="${FAKE_CALL_LOG:?FAKE_CALL_LOG not set}"
echo "mc $*" >> "${CALL_LOG}"
exit 0
MCSHIM
    chmod +x "${sandbox}/bin/mc"
}

# The hiclaw shim: logs the full invocation plus the applied YAML's content
# (so assertions can inspect what create-project.sh generated), and always
# succeeds unless FAKE_HICLAW_FAIL=1 is set for a test case.
write_hiclaw_shim() {
    local sandbox="$1"
    cat > "${sandbox}/bin/hiclaw" << 'HICLAWSHIM'
#!/bin/bash
CALL_LOG="${FAKE_CALL_LOG:?FAKE_CALL_LOG not set}"
echo "hiclaw $*" >> "${CALL_LOG}"

# Find the -f/--file argument and dump the applied YAML into the log too.
prev=""
for a in "$@"; do
    if [ "${prev}" = "-f" ]; then
        echo "---APPLIED-YAML-START---" >> "${CALL_LOG}"
        cat "${a}" >> "${CALL_LOG}"
        echo "---APPLIED-YAML-END---" >> "${CALL_LOG}"
    fi
    prev="${a}"
done

if [ "${FAKE_HICLAW_FAIL:-0}" = "1" ]; then
    echo "hiclaw: simulated failure" >&2
    exit 1
fi
echo "  project/fake created"
exit 0
HICLAWSHIM
    chmod +x "${sandbox}/bin/hiclaw"
}

run_create_project() {
    # run_create_project <sandbox> <args...>
    local sandbox="$1"; shift
    HOME="${sandbox}/home" \
    PATH="${sandbox}/bin:${PATH}" \
    FAKE_CALL_LOG="${sandbox}/calls.log" \
    FAKE_HICLAW_FAIL="${FAKE_HICLAW_FAIL:-0}" \
    AGENTTEAMS_MATRIX_DOMAIN="matrix.fake.local" \
    AGENTTEAMS_ADMIN_USER="admin" \
    AGENTTEAMS_MATRIX_URL="http://matrix.fake.local" \
    AGENTTEAMS_STORAGE_PREFIX="agentteams/agentteams-storage" \
    MANAGER_MATRIX_TOKEN="preset-token" \
    bash "${CREATE_PROJECT_SCRIPT}" "$@" 2>&1
}

calls() {
    cat "$1/calls.log"
}

# ── Direct unit test of the yaml_dq() escaping helper ─────────────────────────
# create-project.sh cannot be `source`d wholesale (it's a `set -e` script that
# runs its whole flow top-level and requires flags/env). Also, a --title
# containing a raw '"' or '\' currently crashes the script much earlier than
# Step 5 (federated Project CR) — the meta.json heredoc and the Matrix
# createRoom JSON body interpolate PROJECT_TITLE unescaped too (pre-existing,
# tracked separately, out of scope for the YAML-CR-only fix under test here).
# So we extract just the yaml_dq() function body via sed and source that
# snippet in a subshell to unit-test the escaping logic in isolation.
yaml_dq_direct() {
    local input="$1"
    local fn_src
    fn_src=$(sed -n '/^yaml_dq() {/,/^}/p' "${CREATE_PROJECT_SCRIPT}")
    bash -c "${fn_src}"$'\n''yaml_dq "$1"' -- "${input}"
}

project_dir() {
    # project_dir <sandbox> <project-id> — create-project.sh hardcodes
    # /root/hiclaw-fs/shared/projects/<id>; on this box that resolves under
    # the real filesystem root, not the sandbox, so tests read from there and
    # clean up afterward.
    echo "/root/hiclaw-fs/shared/projects/$2"
}

cleanup_real_project_dir() {
    rm -rf "/root/hiclaw-fs/shared/projects/$1" 2>/dev/null || true
}

# ── Tests ─────────────────────────────────────────────────────────────────────

echo ""
echo "=== TC1: bash -n on create-project.sh ==="
{
    if bash -n "${CREATE_PROJECT_SCRIPT}"; then
        pass "create-project.sh parses (bash -n)"
    else
        fail "create-project.sh parses (bash -n)" "exit 0" "non-zero"
    fi
}

echo ""
echo "=== TC2: no-flags invocation — byte-identical to today (no hiclaw call at all) ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc2-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "No Flags Project" --workers "alice,bob")
    log=$(calls "${s}")

    assert_contains "prints ---RESULT---" "---RESULT---" "${out}"
    assert_contains "result has project_id" "\"project_id\": \"${pid}\"" "${out}"
    assert_not_contains "hiclaw is NEVER invoked without --team" "hiclaw apply" "${log}"

    pd=$(project_dir "${s}" "${pid}")
    if [ -f "${pd}/meta.json" ]; then
        pass "meta.json written"
    else
        fail "meta.json written" "file exists" "missing"
    fi
    if [ -f "${pd}/project-cr.yaml" ]; then
        fail "no project-cr.yaml without --team" "absent" "present"
    else
        pass "no project-cr.yaml without --team"
    fi
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC3: --team without --repo is rejected before any side effects ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc3-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "Team No Repo" --workers "alice" --team "teamA")
    log=$(calls "${s}")

    assert_contains "error mentions --repo requirement" "--team requires at least one --repo" "${out}"
    assert_eq "no Matrix/MinIO/hiclaw calls happened" "" "${log}"
    if [ -f "$(project_dir "${s}" "${pid}")/meta.json" ]; then
        fail "no meta.json written on validation failure" "absent" "present"
    else
        pass "no meta.json written on validation failure"
    fi
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC4: --repo without --team is rejected ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc4-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "Repo No Team" --workers "alice" \
        --repo "https://git.fake.local/teamA/repo.git:rw")

    assert_contains "error mentions --team requirement" "--repo requires --team" "${out}"
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC5: --repo access enum is enforced (rejects anything but rw/ro) ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc5-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "Bad Access" --workers "alice" \
        --team "teamA" --repo "https://git.fake.local/teamA/repo.git:admin")

    assert_contains "error mentions rw or ro" "--repo access must be rw or ro" "${out}"
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC6: --team + --repo emits a Project CR and calls hiclaw apply -f ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc6-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "Federated Project" --workers "alice,bob" \
        --team "teamA" \
        --repo "https://git.fake.local/teamA/repo-rw.git:rw" \
        --repo "https://git.fake.local/teamA/repo-ro.git:ro")
    log=$(calls "${s}")

    assert_contains "hiclaw apply -f was invoked" "hiclaw apply -f" "${log}"
    assert_contains "applied YAML has apiVersion" "apiVersion: hiclaw.io/v1beta1" "${log}"
    assert_contains "applied YAML has kind: Project" "kind: Project" "${log}"
    # Scalars are emitted as quoted YAML strings (yaml_dq) so that chat-origin
    # values can never break out of the scalar — see the injection fix above.
    assert_contains "applied YAML names the project" "name: \"${pid}\"" "${log}"
    assert_contains "applied YAML sets team" 'team: "teamA"' "${log}"
    assert_contains "applied YAML has rw repo" 'url: "https://git.fake.local/teamA/repo-rw.git"' "${log}"
    assert_contains "applied YAML marks rw access" 'access: "rw"' "${log}"
    assert_contains "applied YAML has ro repo" 'url: "https://git.fake.local/teamA/repo-ro.git"' "${log}"
    assert_contains "applied YAML marks ro access" 'access: "ro"' "${log}"
    assert_contains "applied YAML lists workers" '- "alice"' "${log}"

    # Chat-flow layer still written, untouched, alongside the CR.
    pd=$(project_dir "${s}" "${pid}")
    if [ -f "${pd}/meta.json" ]; then
        pass "meta.json still written alongside the CR"
    else
        fail "meta.json still written alongside the CR" "file exists" "missing"
    fi
    if [ -f "${pd}/project-cr.yaml" ]; then
        pass "project-cr.yaml written to the project directory"
    else
        fail "project-cr.yaml written to the project directory" "file exists" "missing"
    fi
    assert_contains "result JSON still has project_id (unchanged shape)" "\"project_id\": \"${pid}\"" "${out}"
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC7: hiclaw apply failure does not fail the whole script (chat-flow project still created) ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc7-$$"
    cleanup_real_project_dir "${pid}"

    FAKE_HICLAW_FAIL=1 out=$(FAKE_HICLAW_FAIL=1 run_create_project "${s}" --id "${pid}" --title "Apply Fails" \
        --workers "alice" --team "teamA" --repo "https://git.fake.local/teamA/repo.git:rw")

    assert_contains "warns about the apply failure" "WARNING: hiclaw apply -f failed" "${out}"
    assert_contains "still prints the RESULT block" "---RESULT---" "${out}"
    pd=$(project_dir "${s}" "${pid}")
    if [ -f "${pd}/meta.json" ]; then
        pass "chat-flow meta.json unaffected by CR-apply failure"
    else
        fail "chat-flow meta.json unaffected by CR-apply failure" "file exists" "missing"
    fi
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC8: yaml_dq() escapes quotes/backslashes/newlines/tabs correctly (unit-level) ==="
# NOTE: a --title containing a raw '"' or '\' currently crashes create-project.sh
# well before Step 5 (the meta.json heredoc and the Matrix createRoom JSON body
# also interpolate PROJECT_TITLE unescaped — a separate, pre-existing JSON-
# injection issue outside this fix's scope, which only covers the YAML CR
# emitted in Step 5). So the adversarial quote+backslash case is exercised
# directly against yaml_dq() rather than through a full end-to-end run.
{
    evil_title='Say "hi" \ bye'
    got=$(yaml_dq_direct "${evil_title}")
    # backslash escaped first, then quotes: Say "hi" \ bye -> Say \"hi\" \\ bye
    assert_eq "quote+backslash title escapes correctly" '"Say \"hi\" \\ bye"' "${got}"

    # No unescaped '"' may appear anywhere except the wrapping quotes — i.e.
    # stripping the outer quotes and all valid \" / \\ escapes must leave zero
    # bare '"' characters that could terminate the scalar early.
    inner="${got#\"}"; inner="${inner%\"}"
    stripped="${inner//\\\\/}"; stripped="${stripped//\\\"/}"
    assert_not_contains "no unescaped quote remains inside the scalar" '"' "${stripped}"

    newline_tab_title=$'line one\nline two\ttabbed'
    got2=$(yaml_dq_direct "${newline_tab_title}")
    assert_eq "newline and tab map to YAML escapes" '"line one\nline two\ttabbed"' "${got2}"

    cr_title=$'has\r\ncarriage return'
    got3=$(yaml_dq_direct "${cr_title}")
    assert_eq "bare CR is stripped, newline still escaped" '"has\ncarriage return"' "${got3}"

    plain_title="Normal Project Title"
    got4=$(yaml_dq_direct "${plain_title}")
    assert_eq "plain slug-like title is unchanged apart from quoting" '"Normal Project Title"' "${got4}"
}

echo ""
echo "=== TC8b: end-to-end — a YAML-adversarial but JSON-safe title stays a single quoted scalar ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc8b-$$"
    cleanup_real_project_dir "${pid}"

    # No '"' or '\' (those crash the unrelated, out-of-scope JSON emission —
    # see note above), but a leading '- ' and an embedded ':' are both things
    # that would misparse as YAML structure if this scalar were left unquoted.
    yaml_title="- Title: with a colon"
    out=$(run_create_project "${s}" --id "${pid}" --title "${yaml_title}" --workers "alice" \
        --team "teamA" --repo "https://git.fake.local/teamA/repo.git:rw")
    log=$(calls "${s}")

    assert_contains "description scalar is quoted and intact" 'description: "- Title: with a colon"' "${log}"
    assert_contains "still applies the CR successfully" "hiclaw apply -f" "${log}"
    assert_contains "still prints the RESULT block" "---RESULT---" "${out}"
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC9: unquoted-looking repo/worker/team scalars stay quoted in the emitted CR ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc9-$$"
    cleanup_real_project_dir "${pid}"

    out=$(run_create_project "${s}" --id "${pid}" --title "Quoting Check" --workers "alice,bob" \
        --team "teamA" \
        --repo "https://git.fake.local/teamA/repo-rw.git:rw" \
        --repo "https://git.fake.local/teamA/repo-ro.git:ro")
    log=$(calls "${s}")

    assert_contains "metadata.name is double-quoted" "name: \"${pid}\"" "${log}"
    assert_contains "spec.team is double-quoted" 'team: "teamA"' "${log}"
    assert_contains "repo url is double-quoted" 'url: "https://git.fake.local/teamA/repo-rw.git"' "${log}"
    assert_contains "repo access is double-quoted" 'access: "rw"' "${log}"
    assert_contains "worker entry is double-quoted" '- "alice"' "${log}"

    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC10: --team containing invalid characters (space) hard-fails, no side effects ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc10-$$"
    cleanup_real_project_dir "${pid}"

    set +e
    out=$(run_create_project "${s}" --id "${pid}" --title "Bad Team" --workers "alice" \
        --team "team A" --repo "https://git.fake.local/teamA/repo.git:rw")
    rc=$?
    set -e
    log=$(calls "${s}")

    if [ "${rc}" -ne 0 ]; then
        pass "invalid --team (space) causes non-zero exit"
    else
        fail "invalid --team (space) causes non-zero exit" "non-zero" "0"
    fi
    assert_contains "error mentions --team validation" "--team must match" "${out}"
    assert_eq "no Matrix/MinIO/hiclaw calls happened" "" "${log}"
    if [ -f "$(project_dir "${s}" "${pid}")/meta.json" ]; then
        fail "no meta.json written on --team validation failure" "absent" "present"
    else
        pass "no meta.json written on --team validation failure"
    fi
    cleanup_real_project_dir "${pid}"
}

echo ""
echo "=== TC11: --team containing a colon hard-fails ==="
{
    s=$(new_sandbox)
    write_curl_shim "${s}"
    write_mc_shim "${s}"
    write_hiclaw_shim "${s}"
    pid="proj-tc11-$$"
    cleanup_real_project_dir "${pid}"

    set +e
    out=$(run_create_project "${s}" --id "${pid}" --title "Bad Team Colon" --workers "alice" \
        --team "team:A" --repo "https://git.fake.local/teamA/repo.git:rw")
    rc=$?
    set -e

    if [ "${rc}" -ne 0 ]; then
        pass "invalid --team (colon) causes non-zero exit"
    else
        fail "invalid --team (colon) causes non-zero exit" "non-zero" "0"
    fi
    assert_contains "error mentions --team validation" "--team must match" "${out}"
    cleanup_real_project_dir "${pid}"
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
