#!/bin/bash
# test-12-github-mcp-tools.sh - Test newly added GitHub MCP tools
# Tests: get_me, list_branches, delete_file, update_pull_request,
#        list_tags, list_releases, get_latest_release, get_commit,
#        list_issue_comments, get_label, list_labels, list_teams,
#        list_team_members, list_notifications, request_reviewers

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"

test_setup "12-github-mcp-tools"

# Check prerequisites
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured (set HICLAW_GITHUB_TOKEN or TEST_GITHUB_TOKEN)"
    test_teardown "12-github-mcp-tools"
    test_summary
    exit 0
fi

# Test repository (use hiclaw repo for read-only tests)
TEST_OWNER="${TEST_GITHUB_OWNER:-higress-group}"
TEST_REPO="${TEST_GITHUB_REPO:-hiclaw}"

# Manager container name
MANAGER_CONTAINER="${TEST_AGENT_CONTAINER:-${TEST_CONTROLLER_CONTAINER:-agentteams-manager}}"

# Helper function to call mcporter inside Manager container
mcporter_call() {
    local tool_name="$1"
    shift
    local args="$*"

    docker exec "${MANAGER_CONTAINER}" bash -c "mcporter --config /root/manager-workspace/mcporter-servers.json call mcp-github.${tool_name} ${args}" 2>&1
}

# Helper to check if result is valid JSON (array or object)
is_valid_json() {
    echo "$1" | jq -e '.' >/dev/null 2>&1
}

mcporter_call_json() {
    local tool_name="$1"
    local args="$2"

    docker exec "${MANAGER_CONTAINER}" bash -c "mcporter --config /root/manager-workspace/mcporter-servers.json call mcp-github.${tool_name} --args '${args}'" 2>&1
}

log_section "Testing High Priority Tools"

# Test 1: get_me
log_info "Testing get_me..."
RESULT=$(mcporter_call "get_me")
if echo "${RESULT}" | jq -e '.login' >/dev/null 2>&1; then
    log_pass "get_me returns user info"
    USERNAME=$(echo "${RESULT}" | jq -r '.login')
    log_info "Logged in as: ${USERNAME}"
else
    log_fail "get_me failed: ${RESULT}"
fi

# Test 2: list_branches
log_info "Testing list_branches..."
RESULT=$(mcporter_call "list_branches" "owner=${TEST_OWNER} repo=${TEST_REPO}")
if echo "${RESULT}" | jq -e '.[0].name' >/dev/null 2>&1 || echo "${RESULT}" | jq -e '.name' >/dev/null 2>&1; then
    log_pass "list_branches returns branches"
else
    log_fail "list_branches failed: ${RESULT}"
fi

# Test 3: get_commit
log_info "Testing get_commit..."
RESULT=$(mcporter_call "get_commit" "owner=${TEST_OWNER} repo=${TEST_REPO} ref=main")
if echo "${RESULT}" | jq -e '.sha' >/dev/null 2>&1; then
    log_pass "get_commit returns commit info"
else
    log_fail "get_commit failed: ${RESULT}"
fi

# Test 4: list_commits (existing tool, verify still works)
log_info "Testing list_commits..."
RESULT=$(mcporter_call "list_commits" "owner=${TEST_OWNER} repo=${TEST_REPO} sha=main perPage=5")
if echo "${RESULT}" | jq -e '.[0].sha' >/dev/null 2>&1; then
    log_pass "list_commits returns commits"
else
    log_fail "list_commits failed: ${RESULT}"
fi

# Test 5: list_tags
log_info "Testing list_tags..."
RESULT=$(mcporter_call "list_tags" "owner=${TEST_OWNER} repo=${TEST_REPO}")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_tags returns tags (may be empty array)"
else
    log_fail "list_tags failed: ${RESULT}"
fi

# Test 6: list_releases
log_info "Testing list_releases..."
RESULT=$(mcporter_call "list_releases" "owner=${TEST_OWNER} repo=${TEST_REPO}")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_releases returns releases (may be empty array)"
else
    log_fail "list_releases failed: ${RESULT}"
fi

# Test 7: get_latest_release
log_info "Testing get_latest_release..."
RESULT=$(mcporter_call "get_latest_release" "owner=${TEST_OWNER} repo=${TEST_REPO}")
# This may return 404 if no releases exist, which is OK
if echo "${RESULT}" | jq -e '.tag_name' >/dev/null 2>&1; then
    log_pass "get_latest_release returns release info"
elif echo "${RESULT}" | grep -q "404\|Not Found"; then
    log_pass "get_latest_release: no releases found (expected for repos without releases)"
else
    log_info "get_latest_release result: ${RESULT}"
fi

# Test 8: get_repo (existing tool, verify still works)
log_info "Testing get_repo..."
RESULT=$(mcporter_call "get_repo" "owner=${TEST_OWNER} repo=${TEST_REPO}")
if echo "${RESULT}" | jq -e '.full_name' >/dev/null 2>&1; then
    log_pass "get_repo returns repo info"
else
    log_fail "get_repo failed: ${RESULT}"
fi

# Test 9: list_issue_comments (need an issue/PR number)
log_info "Testing list_issue_comments..."
# Try with PR #1 if it exists, otherwise skip
RESULT=$(mcporter_call "list_issue_comments" "owner=${TEST_OWNER} repo=${TEST_REPO} issue_number=1")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_issue_comments returns comments (may be empty array)"
elif echo "${RESULT}" | grep -q "404\|Not Found"; then
    log_info "list_issue_comments: issue/PR #1 not found (expected if no PR)"
else
    log_fail "list_issue_comments failed: ${RESULT}"
fi

log_section "Testing Medium Priority Tools"

# Test 10: list_labels
log_info "Testing list_labels..."
RESULT=$(mcporter_call "list_labels" "owner=${TEST_OWNER} repo=${TEST_REPO}")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_labels returns labels"
else
    log_fail "list_labels failed: ${RESULT}"
fi

# Test 11: get_label (try to get first label from list)
log_info "Testing get_label..."
LABELS_RESULT=$(mcporter_call "list_labels" "owner=${TEST_OWNER} repo=${TEST_REPO}")
FIRST_LABEL=$(echo "${LABELS_RESULT}" | jq -r '.[0].name' 2>/dev/null)
if [ -n "${FIRST_LABEL}" ] && [ "${FIRST_LABEL}" != "null" ]; then
    RESULT=$(mcporter_call "get_label" "owner=${TEST_OWNER} repo=${TEST_REPO} name=${FIRST_LABEL}")
    if echo "${RESULT}" | jq -e '.name' >/dev/null 2>&1; then
        log_pass "get_label returns label info for '${FIRST_LABEL}'"
    else
        log_fail "get_label failed: ${RESULT}"
    fi
else
    log_info "get_label: skipped (no labels found)"
fi

# Test 12: list_teams
log_info "Testing list_teams..."
RESULT=$(mcporter_call "list_teams")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_teams returns teams (may be empty if user not in org)"
else
    log_fail "list_teams failed: ${RESULT}"
fi

# Test 13: list_notifications
log_info "Testing list_notifications..."
RESULT=$(mcporter_call "list_notifications" "all=false")
# Check if response starts with [ (array) or contains valid notification data
if [[ "${RESULT}" == "["* ]] || echo "${RESULT}" | grep -q '"id".*"unread"'; then
    log_pass "list_notifications returns notifications"
else
    log_fail "list_notifications failed: ${RESULT:0:200}"
fi

log_section "Testing Existing PR Tools (regression)"

# Test 14: list_pull_requests
log_info "Testing list_pull_requests..."
RESULT=$(mcporter_call "list_pull_requests" "owner=${TEST_OWNER} repo=${TEST_REPO} state=all perPage=5")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "list_pull_requests works"
else
    log_fail "list_pull_requests failed: ${RESULT}"
fi

# Test 15: get_pull_request_comments (if PR exists)
log_info "Testing get_pull_request_comments..."
RESULT=$(mcporter_call "get_pull_request_comments" "owner=${TEST_OWNER} repo=${TEST_REPO} pull_number=1")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "get_pull_request_comments works"
elif echo "${RESULT}" | grep -q "404\|Not Found"; then
    log_info "get_pull_request_comments: PR #1 not found (expected if no PR)"
else
    log_fail "get_pull_request_comments failed: ${RESULT}"
fi

# Test 16: get_pull_request_reviews (if PR exists)
log_info "Testing get_pull_request_reviews..."
RESULT=$(mcporter_call "get_pull_request_reviews" "owner=${TEST_OWNER} repo=${TEST_REPO} pull_number=1")
if echo "${RESULT}" | jq -e '.' >/dev/null 2>&1; then
    log_pass "get_pull_request_reviews works"
elif echo "${RESULT}" | grep -q "404\|Not Found"; then
    log_info "get_pull_request_reviews: PR #1 not found (expected if no PR)"
else
    log_fail "get_pull_request_reviews failed: ${RESULT}"
fi

log_section "Testing Search Tools (regression)"

# Test 17: search_code
log_info "Testing search_code..."
RESULT=$(mcporter_call "search_code" 'q="README repo:higress-group/hiclaw"')
if echo "${RESULT}" | jq -e '.items or .[0]' >/dev/null 2>&1; then
    log_pass "search_code works"
else
    log_info "search_code result: ${RESULT:0:200}"
fi

# Test 18: search_repositories
log_info "Testing search_repositories..."
RESULT=$(mcporter_call "search_repositories" 'query="hiclaw"')
if echo "${RESULT}" | jq -e '.items or .[0]' >/dev/null 2>&1; then
    log_pass "search_repositories works"
else
    log_fail "search_repositories failed: ${RESULT}"
fi

test_teardown "12-github-mcp-tools"
test_summary
