#!/bin/bash
# test-update-builtin-section.sh
# Unit tests for the update_builtin_section function in upgrade-builtins.sh
#
# Usage: bash manager/tests/test-update-builtin-section.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

# ── Resolve lib path relative to this script ─────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/../scripts/lib"

log() { :; }  # silence logs during tests
source "${LIB_DIR}/builtin-merge.sh"

# ── Test helpers ──────────────────────────────────────────────────────────────
pass() { echo "  PASS: $1"; PASS=$(( PASS + 1 )); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$(( FAIL + 1 )); }

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
    if echo "${haystack}" | grep -qF "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! echo "${haystack}" | grep -qF "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "should NOT contain '${needle}'" "found it"
    fi
}

count_occurrences() {
    grep -c "$1" "$2" 2>/dev/null || echo 0
}

new_workdir() {
    mktemp -d "${TMPDIR_ROOT}/test-XXXXXX"
}

# ── Tests ─────────────────────────────────────────────────────────────────────

echo ""
echo "=== TC1: First time — target does not exist ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Hello World" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains "has builtin-start marker"   "agentteams-builtin-start" "${content}"
    assert_contains "has builtin-end marker"     "agentteams-builtin-end"   "${content}"
    assert_contains "has source content"         "# Hello World"        "${content}"
    assert_eq       "exactly 1 start marker"     "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
}

echo ""
echo "=== TC2: Idempotent — same source, run twice, file must not change ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Stable Content" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    checksum_before=$(md5 -q "${tgt}" 2>/dev/null || md5sum "${tgt}" | cut -d' ' -f1)
    update_builtin_section "${tgt}" "${src}"
    checksum_after=$(md5 -q "${tgt}" 2>/dev/null || md5sum "${tgt}" | cut -d' ' -f1)
    assert_eq "file unchanged on second run" "${checksum_before}" "${checksum_after}"
}

echo ""
echo "=== TC3: Idempotent — run 5 times, still only 1 start marker ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Repeated" > "${src}"
    for i in 1 2 3 4 5; do
        update_builtin_section "${tgt}" "${src}"
    done
    assert_eq "exactly 1 start marker after 5 runs" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq "exactly 1 end marker after 5 runs"   "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
}

echo ""
echo "=== TC4: Content update — new source replaces builtin section ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Version 1" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    echo "# Version 2" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains     "has new content"     "# Version 2" "${content}"
    assert_not_contains "no old content"      "# Version 1" "${content}"
    assert_eq           "exactly 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
}

echo ""
echo "=== TC5: User content preserved below end marker ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Builtin" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    # Simulate user adding content after end marker
    printf '\n## My Custom Section\nsome user notes\n' >> "${tgt}"
    # Run upgrade with same source
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains "user content preserved" "## My Custom Section" "${content}"
    assert_contains "builtin still present"  "# Builtin"            "${content}"
    assert_eq       "exactly 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
}

echo ""
echo "=== TC6: User content preserved when builtin content changes ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Version 1" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    printf '\n## User Notes\nkeep this\n' >> "${tgt}"
    echo "# Version 2" > "${src}"
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains     "new builtin content"    "# Version 2"   "${content}"
    assert_not_contains "old builtin gone"       "# Version 1"   "${content}"
    assert_contains     "user content preserved" "## User Notes"  "${content}"
    assert_eq           "exactly 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
}

echo ""
echo "=== TC7: Corrupted file (2 start markers) — auto-repair ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Clean Content" > "${src}"
    # Simulate a corrupted file with duplicate builtin sections
    cat > "${tgt}" << 'EOF'
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT
> ...

# Old Content
<!-- agentteams-builtin-end -->
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT
> ...

# Old Content
<!-- agentteams-builtin-end -->
EOF
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_eq       "repaired to 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq       "repaired to 1 end marker"   "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
    assert_contains "has new source content"      "# Clean Content" "${content}"
}

echo ""
echo "=== TC8: Corrupted file (2 start markers) — force rewrite, no user content preserved ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# New Builtin" > "${src}"
    cat > "${tgt}" << 'EOF'
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT

# Builtin v1
<!-- agentteams-builtin-end -->
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT

# Builtin v1
<!-- agentteams-builtin-end -->
## Real User Content
user wrote this
EOF
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains "has new builtin content" "# New Builtin" "${content}"
    assert_eq       "exactly 1 start marker after repair" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq       "exactly 1 end marker after repair"   "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
}

echo ""
echo "=== TC9: Legacy file (no markers) — overwrite with markers ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# New Builtin" > "${src}"
    echo "# Old Legacy Content" > "${tgt}"
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains     "has start marker"       "agentteams-builtin-start" "${content}"
    assert_contains     "has new content"        "# New Builtin"        "${content}"
    assert_not_contains "legacy content gone"    "# Old Legacy Content" "${content}"
}

echo ""
echo "=== TC10: Source missing — target unchanged ==="
{
    d=$(new_workdir)
    src="${d}/nonexistent.md"; tgt="${d}/target.md"
    echo "# Existing" > "${tgt}"
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_eq "target unchanged when source missing" "# Existing" "${content}"
}

echo ""
echo "=== TC11: Corrupted — end marker missing (start=1, end=0) — force rewrite ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Fresh Content" > "${src}"
    # File has start marker but end marker was deleted
    cat > "${tgt}" << 'EOF'
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT

# Old Builtin
some more old content
EOF
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains     "has new content after repair"  "# Fresh Content" "${content}"
    assert_not_contains "old content gone"              "# Old Builtin"   "${content}"
    assert_eq           "exactly 1 start marker"        "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq           "exactly 1 end marker"          "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
}

echo ""
echo "=== TC12: Corrupted — start marker missing (start=0, end=1) — treated as legacy, overwrite ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    echo "# Fresh Content" > "${src}"
    # File has end marker but start marker was deleted (very broken)
    cat > "${tgt}" << 'EOF'
some content without start marker
<!-- agentteams-builtin-end -->
## User Notes
keep this
EOF
    update_builtin_section "${tgt}" "${src}"
    content=$(cat "${tgt}")
    assert_contains "has start marker after repair" "agentteams-builtin-start" "${content}"
    assert_contains "has new content"               "# Fresh Content"      "${content}"
    assert_eq       "exactly 1 start marker"        "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
}

echo ""
echo "=== TC13: Bloated after-end content (builtin leaked into user area) — force rewrite ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    printf '# Manager Agent Workspace\n\nsome content\nmore content\neven more\n' > "${src}"
    {
        printf '<!-- agentteams-builtin-start -->\n'
        printf '> ⚠️ DO NOT EDIT\n\n'
        printf '# Manager Agent Workspace\n\nsome content\n'
        printf '<!-- agentteams-builtin-end -->\n'
        for i in $(seq 1 200); do printf '# Manager Agent Workspace\nsome content\n'; done
    } > "${tgt}"
    update_builtin_section "${tgt}" "${src}"
    after_end=$(awk '$0 == "<!-- agentteams-builtin-end -->" {found=1; next} found{c++} END {print c+0}' "${tgt}")
    assert_eq "exactly 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq "exactly 1 end marker"   "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
    [ "${after_end}" -lt 20 ] && pass "after-end lines clean (${after_end})" || fail "after-end lines too many" "<20" "${after_end}"
}

echo ""
echo "=== TC14: Duplicate heading after end marker — force rewrite ==="
{
    d=$(new_workdir)
    src="${d}/source.md"; tgt="${d}/target.md"
    printf '# Manager Agent Workspace\nsome builtin content\n' > "${src}"
    cat > "${tgt}" << 'EOF'
<!-- agentteams-builtin-start -->
> ⚠️ DO NOT EDIT

# Manager Agent Workspace
some builtin content
<!-- agentteams-builtin-end -->
# Manager Agent Workspace
some builtin content
EOF
    update_builtin_section "${tgt}" "${src}"
    leaked=$(awk '$0 == "<!-- agentteams-builtin-end -->" {found=1; next} found && /^# Manager Agent Workspace/ {c++} END {print c+0}' "${tgt}")
    assert_eq "no leaked heading after end marker" "0" "${leaked}"
    assert_eq "exactly 1 start marker" "1" "$(count_occurrences 'agentteams-builtin-start' "${tgt}")"
    assert_eq "exactly 1 end marker"   "1" "$(awk '$0 == "<!-- agentteams-builtin-end -->" {c++} END {print c+0}' "${tgt}")"
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
