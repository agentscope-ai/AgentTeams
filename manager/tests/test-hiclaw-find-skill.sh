#!/bin/bash
# test-hiclaw-find-skill.sh
# Regression tests for the Worker hiclaw-find-skill wrapper.
#
# Usage: bash manager/tests/test-hiclaw-find-skill.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

WORKER_SCRIPT="${PROJECT_ROOT}/manager/agent/worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
COPAW_SCRIPT="${PROJECT_ROOT}/manager/agent/copaw-worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
HERMES_SCRIPT="${PROJECT_ROOT}/manager/agent/hermes-worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "should not contain '${needle}'" "found it"
    fi
}

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

strip_ansi() {
    perl -pe 's/\e\[[0-9;]*[[:alpha:]]//g'
}

skill_names_from_output() {
    strip_ansi | awk '
        /Install with/ { capture = 1; next }
        capture && NF == 0 { next }
        capture && $0 !~ /^└ / { print $0 }
    '
}

create_mock_npx() {
    local mockbin="$1"
    mkdir -p "${mockbin}"
    cat > "${mockbin}/npx" <<'EOF'
#!/bin/sh
set -eu

log_file="${TEST_NPX_LOG:?}"
printf '%s\n' "$*" >> "${log_file}"

if [ "${1:-}" = "-y" ]; then
    shift
fi

[ "${1:-}" = "@nacos-group/cli" ] || {
    echo "unexpected package: ${1:-}" >&2
    exit 1
}
shift

command_name="${1:-}"
shift || true

if [ "${command_name}" != "skill-list" ]; then
    echo "unexpected command: ${command_name}" >&2
    exit 1
fi

name=""
page="1"
size="20"

while [ $# -gt 0 ]; do
    case "$1" in
        --name) name="$2"; shift 2 ;;
        --page) page="$2"; shift 2 ;;
        --size) size="$2"; shift 2 ;;
        *) shift ;;
    esac
done

if [ "${page}" != "1" ]; then
    exit 0
fi

case "${name}" in
    "react performance")
        cat <<'OUT'
Search Results
1. react-render-performance - Diagnose React rendering bottlenecks
OUT
        ;;
    "react")
        cat <<'OUT'
Search Results
1. react-best-practices - Improve React app architecture and performance
2. react-render-performance - Diagnose React rendering bottlenecks
OUT
        ;;
    "performance")
        cat <<'OUT'
Search Results
1. web-performance-debugging - Investigate runtime performance issues
2. react-render-performance - Diagnose React rendering bottlenecks
OUT
        ;;
    "pr review")
        cat <<'OUT'
Search Results
1. requesting-code-review - Ask for an effective code review
2. receiving-code-review - Apply review feedback well
3. supabase-postgres-best-practices - Improve Postgres schema and query performance
OUT
        ;;
    "pr")
        cat <<'OUT'
Search Results
1. supabase-postgres-best-practices - Improve Postgres schema and query performance
2. pr-automation - Automate pull request metadata
OUT
        ;;
    "review")
        cat <<'OUT'
Search Results
1. supabase-postgres-best-practices - Improve Postgres schema and query performance
2. receiving-code-review - Apply review feedback well
3. requesting-code-review - Ask for an effective code review
OUT
        ;;
esac
EOF
    chmod +x "${mockbin}/npx"
}

create_mock_skills() {
    local mockbin="$1"
    cat > "${mockbin}/skills" <<'EOF'
#!/bin/sh
set -eu

log_file="${TEST_SKILLS_LOG:?}"
printf '%s\n' "$*" >> "${log_file}"

command_name="${1:-}"
shift || true

case "${command_name}" in
    find)
        cat <<'OUT'
skills.sh results
skill: react-performance-toolkit
OUT
        ;;
    add)
        echo "installed"
        ;;
    *)
        echo "unexpected skills command: ${command_name}" >&2
        exit 1
        ;;
esac
EOF
    chmod +x "${mockbin}/skills"
}

create_mock_curl() {
    local mockbin="$1"
    cat > "${mockbin}/curl" <<'EOF'
#!/bin/sh
set -eu

log_file="${TEST_CURL_LOG:?}"
printf '%s\n' "$*" >> "${log_file}"
cat <<'OUT'
{"access_key_id":"test-ak","access_key_secret":"test-sk","security_token":"test-sts"}
200
OUT
EOF
    chmod +x "${mockbin}/curl"
}

run_case() {
    local script_path="$1" query="$2" log_file="$3"
    local mockbin="${TMPDIR_ROOT}/mockbin"
    create_mock_npx "${mockbin}"
    create_mock_skills "${mockbin}"
    create_mock_curl "${mockbin}"

    PATH="${mockbin}:${PATH}" \
    TEST_NPX_LOG="${log_file}" \
    TEST_SKILLS_LOG="${TMPDIR_ROOT}/skills.log" \
    TEST_CURL_LOG="${TMPDIR_ROOT}/curl.log" \
    SKILLS_API_URL="nacos://registry.local:8848" \
    HICLAW_FIND_SKILL_MAX_RESULTS=3 \
    HICLAW_FIND_SKILL_NACOS_PAGE_SIZE=50 \
    /bin/sh "${script_path}" find ${query}
}

run_case_with_env() {
    local script_path="$1" query="$2" log_file="$3" skills_api_url="$4" skills_log_file="$5"
    local mockbin="${TMPDIR_ROOT}/mockbin"
    create_mock_npx "${mockbin}"
    create_mock_skills "${mockbin}"
    create_mock_curl "${mockbin}"

    PATH="${mockbin}:${PATH}" \
    TEST_NPX_LOG="${log_file}" \
    TEST_SKILLS_LOG="${skills_log_file}" \
    TEST_CURL_LOG="${TMPDIR_ROOT}/curl.log" \
    SKILLS_API_URL="${skills_api_url}" \
    HICLAW_FIND_SKILL_MAX_RESULTS=3 \
    HICLAW_FIND_SKILL_NACOS_PAGE_SIZE=50 \
    /bin/sh "${script_path}" find ${query}
}

echo ""
echo "=== TC1: react performance should still return React skills ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        log_file="${TMPDIR_ROOT}/${case_name}-react.log"
        output="$(run_case "${script_path}" "react performance" "${log_file}" | strip_ansi)"
        assert_not_contains "${case_name}: query should not be empty result" 'No skills found for "react performance"' "${output}"
        assert_contains "${case_name}: output should include a React skill" "react-render-performance" "${output}"
        assert_contains "${case_name}: output should identify nacos registry" "Registry: Nacos (nacos://registry.local:8848)" "${output}"
        assert_contains "${case_name}: backend call should use plain token filter" "--name react" "$(cat "${log_file}")"
        if grep -Eq 'skill-list( |$)' "${log_file}" && ! grep -q -- '--name' "${log_file}"; then
            fail "${case_name}: should not call unfiltered skill-list" "all skill-list calls include --name" "$(cat "${log_file}")"
        else
            pass "${case_name}: all skill-list calls include --name"
        fi
        assert_not_contains "${case_name}: should not use wildcard filters" "--name *react*" "$(cat "${log_file}")"
    }
done

echo ""
echo "=== TC2: pr review should rank code-review skills ahead of postgres matches ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        log_file="${TMPDIR_ROOT}/${case_name}-pr-review.log"
        output="$(run_case "${script_path}" "pr review" "${log_file}" | strip_ansi)"
        names="$(printf '%s\n' "${output}" | skill_names_from_output)"
        first="$(printf '%s\n' "${names}" | sed -n '1p')"
        second="$(printf '%s\n' "${names}" | sed -n '2p')"

        assert_eq "${case_name}: first result should be requesting-code-review" "requesting-code-review" "${first}"
        assert_eq "${case_name}: second result should be receiving-code-review" "receiving-code-review" "${second}"
    }
done

echo ""
echo "=== TC3: nacos backend should derive host/port from SKILLS_API_URL scheme ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        log_file="${TMPDIR_ROOT}/${case_name}-nacos-conn.log"
        skills_log="${TMPDIR_ROOT}/${case_name}-nacos-skills.log"
        output="$(run_case_with_env "${script_path}" "review" "${log_file}" "nacos://host.containers.internal:8848" "${skills_log}" | strip_ansi)"
        assert_contains "${case_name}: should still return results with nacos URL" "requesting-code-review" "${output}"
        assert_contains "${case_name}: output should identify derived nacos registry" "Registry: Nacos (nacos://host.containers.internal:8848)" "${output}"
        assert_contains "${case_name}: should derive host from SKILLS_API_URL" "--host host.containers.internal" "$(cat "${log_file}")"
        assert_contains "${case_name}: should derive port from SKILLS_API_URL" "--port 8848" "$(cat "${log_file}")"
    }
done

echo ""
echo "=== TC4: nacos backend should derive namespace from SKILLS_API_URL path ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        log_file="${TMPDIR_ROOT}/${case_name}-nacos-namespace.log"
        skills_log="${TMPDIR_ROOT}/${case_name}-nacos-namespace-skills.log"
        output="$(run_case_with_env "${script_path}" "review" "${log_file}" "nacos://host.containers.internal:8848/team-a" "${skills_log}" | strip_ansi)"
        assert_contains "${case_name}: should still return results with nacos namespace URL" "requesting-code-review" "${output}"
        assert_contains "${case_name}: should pass derived namespace" "--namespace team-a" "$(cat "${log_file}")"
    }
done

echo ""
echo "=== TC5: https skills api should use skills CLI backend ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        log_file="${TMPDIR_ROOT}/${case_name}-skills-sh.log"
        skills_log="${TMPDIR_ROOT}/${case_name}-skills-cli.log"
        output="$(run_case_with_env "${script_path}" "react performance" "${log_file}" "https://skills.sh" "${skills_log}" | strip_ansi)"
        assert_contains "${case_name}: output should identify skills.sh registry" "Registry: skills.sh (https://skills.sh)" "${output}"
        assert_contains "${case_name}: should use skills CLI output" "react-performance-toolkit" "${output}"
        assert_contains "${case_name}: should call skills find" "find react performance" "$(cat "${skills_log}")"
        assert_eq "${case_name}: nacos cli should not be used for https registry" "" "$(cat "${log_file}" 2>/dev/null || true)"
    }
done

echo ""
echo "=== TC6: sts-hiclaw nacos backend should request controller STS with cluster header ==="
for script_path in "${WORKER_SCRIPT}" "${COPAW_SCRIPT}" "${HERMES_SCRIPT}"; do
    {
        case_name="$(basename "$(dirname "$(dirname "${script_path}")")")"
        mockbin="${TMPDIR_ROOT}/${case_name}-sts-mockbin"
        log_file="${TMPDIR_ROOT}/${case_name}-sts-npx.log"
        curl_log="${TMPDIR_ROOT}/${case_name}-sts-curl.log"
        skills_log="${TMPDIR_ROOT}/${case_name}-sts-skills.log"
        create_mock_npx "${mockbin}"
        create_mock_skills "${mockbin}"
        create_mock_curl "${mockbin}"

        output="$(PATH="${mockbin}:${PATH}" \
            TEST_NPX_LOG="${log_file}" \
            TEST_SKILLS_LOG="${skills_log}" \
            TEST_CURL_LOG="${curl_log}" \
            SKILLS_API_URL="nacos://registry.local:8848/team-a" \
            NACOS_AUTH_TYPE="sts-hiclaw" \
            HICLAW_CONTROLLER_URL="http://controller:8090" \
            HICLAW_AUTH_TOKEN="controller-token" \
            HICLAW_CLUSTER_ID="remote-cluster-a" \
            HICLAW_FIND_SKILL_MAX_RESULTS=3 \
            HICLAW_FIND_SKILL_NACOS_PAGE_SIZE=50 \
            /bin/sh "${script_path}" find review | strip_ansi)"

        assert_contains "${case_name}: should still return sts-backed results" "requesting-code-review" "${output}"
        assert_contains "${case_name}: controller STS call should include cluster header" "X-HiClaw-Cluster-ID: remote-cluster-a" "$(cat "${curl_log}")"
        assert_contains "${case_name}: controller STS call should include bearer" "Authorization: Bearer controller-token" "$(cat "${curl_log}")"
        assert_contains "${case_name}: nacos cli should use sts auth type" "--auth-type sts-hiclaw" "$(cat "${log_file}")"
        assert_contains "${case_name}: nacos cli should pass sts access key" "--access-key test-ak" "$(cat "${log_file}")"
        assert_contains "${case_name}: nacos cli should pass sts token" "--security-token test-sts" "$(cat "${log_file}")"
    }
done

echo ""
if [ "${FAIL}" -eq 0 ]; then
    echo "All ${PASS} tests passed"
    exit 0
else
    echo "${FAIL} tests failed (${PASS} passed)"
    exit 1
fi
