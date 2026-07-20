#!/bin/bash
# test-update-builtin-section-minio.sh
# Unit tests for update_builtin_section_minio() in builtin-merge.sh
#
# Uses a mock `mc` that reads/writes from a fake MinIO directory on disk.
#
# Usage: bash manager/tests/test-update-builtin-section-minio.sh

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

# ── Mock mc ──────────────────────────────────────────────────────────────────
# Simulates `mc cp <src> <dst>` using a local directory as fake MinIO.
# FAKE_MINIO_ROOT must be set before calling update_builtin_section_minio.
# MinIO paths like "agentteams/agentteams-storage/agents/w1/AGENTS.md" are mapped to
# ${FAKE_MINIO_ROOT}/agentteams/agentteams-storage/agents/w1/AGENTS.md
FAKE_MINIO_ROOT="${TMPDIR_ROOT}/fake-minio"
mkdir -p "${FAKE_MINIO_ROOT}"

mc() {
    local cmd="$1"
    if [ "${cmd}" != "cp" ]; then
        echo "mock mc: unsupported command '${cmd}'" >&2
        return 1
    fi
    local src="$2"
    local dst="$3"

    # Determine which arg is a MinIO path (canonical or legacy alias) vs local path.
    _resolve() {
        local p="$1"
        if [[ "${p}" == agentteams/* || "${p}" == hiclaw/* ]]; then
            echo "${FAKE_MINIO_ROOT}/${p}"
        else
            echo "${p}"
        fi
    }

    local real_src real_dst
    real_src=$(_resolve "${src}")
    real_dst=$(_resolve "${dst}")

    if [ ! -f "${real_src}" ]; then
        return 1  # simulate "object not found"
    fi
    mkdir -p "$(dirname "${real_dst}")"
    cp "${real_src}" "${real_dst}"
}
export -f mc

# ── Test helpers ──────────────────────────────────────────────────────────────
pass() { echo "  PASS: $1"; PASS=$(( PASS + 1 )); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$(( FAIL + 1 )); }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then pass "${desc}"; else fail "${desc}" "${expected}" "${actual}"; fi
}

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if echo "${haystack}" | grep -qF "${needle}"; then pass "${desc}"; else fail "${desc}" "contains '${needle}'" "not found"; fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! echo "${haystack}" | grep -qF "${needle}"; then pass "${desc}"; else fail "${desc}" "should NOT contain '${needle}'" "found it"; fi
}

count_occurrences() {
    grep -c "$1" "$2" 2>/dev/null || echo 0
}

# Helper: read a file from fake MinIO
minio_cat() {
    cat "${FAKE_MINIO_ROOT}/$1"
}

# Helper: write a file to fake MinIO
minio_put() {
    local path="${FAKE_MINIO_ROOT}/$1"
    mkdir -p "$(dirname "${path}")"
    cat > "${path}"
}

new_workdir() {
    mktemp -d "${TMPDIR_ROOT}/test-XXXXXX"
}

# ── Tests ─────────────────────────────────────────────────────────────────────

MINIO_PREFIX="agentteams/agentteams-storage/agents/test-worker"

echo ""
echo "=== MC1: Remote file does not exist — creates new with markers ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Worker Agent Workspace" > "${src}"
    minio_path="${MINIO_PREFIX}/AGENTS-mc1.md"
    # Ensure no file exists
    rm -f "${FAKE_MINIO_ROOT}/${minio_path}"

    update_builtin_section_minio "${minio_path}" "${src}"

    content=$(minio_cat "${minio_path}")
    assert_contains "has builtin-start marker"   "hiclaw-builtin-start" "${content}"
    assert_contains "has builtin-end marker"     "hiclaw-builtin-end"   "${content}"
    assert_contains "has source content"         "# Worker Agent Workspace" "${content}"
}

echo ""
echo "=== MC2: Remote file exists without markers (legacy) — overwrite with markers ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Worker Agent Workspace v2" > "${src}"
    minio_path="${MINIO_PREFIX}/AGENTS-mc2.md"
    # Simulate legacy file in MinIO (no markers)
    echo "# Worker Agent Workspace v1 (old)" | minio_put "${minio_path}"

    update_builtin_section_minio "${minio_path}" "${src}"

    content=$(minio_cat "${minio_path}")
    assert_contains     "has markers"        "hiclaw-builtin-start" "${content}"
    assert_contains     "has new content"    "# Worker Agent Workspace v2" "${content}"
    assert_not_contains "old content gone"   "v1 (old)" "${content}"
}

echo ""
echo "=== MC3: Remote file has markers + user content — preserves user content ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Worker Agent Workspace v2" > "${src}"
    minio_path="${MINIO_PREFIX}/AGENTS-mc3.md"
    # Simulate existing file with markers and user content
    {
        printf '<!-- hiclaw-builtin-start -->\n'
        printf '> ⚠️ **DO NOT EDIT** this section.\n\n'
        printf '# Worker Agent Workspace v1\n'
        printf '\n<!-- hiclaw-builtin-end -->\n'
        printf '\n## My Custom Notes\nWorker added this\n'
    } | minio_put "${minio_path}"

    update_builtin_section_minio "${minio_path}" "${src}"

    content=$(minio_cat "${minio_path}")
    assert_contains     "new builtin content"    "# Worker Agent Workspace v2" "${content}"
    assert_not_contains "old builtin gone"       "v1" "${content}"
    assert_contains     "user content preserved" "## My Custom Notes" "${content}"
    assert_contains     "user detail preserved"  "Worker added this" "${content}"
}

echo ""
echo "=== MC4: Idempotent — same source twice, file unchanged ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Stable Content" > "${src}"
    minio_path="${MINIO_PREFIX}/AGENTS-mc4.md"
    rm -f "${FAKE_MINIO_ROOT}/${minio_path}"

    update_builtin_section_minio "${minio_path}" "${src}"
    checksum_before=$(md5 -q "${FAKE_MINIO_ROOT}/${minio_path}" 2>/dev/null \
        || md5sum "${FAKE_MINIO_ROOT}/${minio_path}" | cut -d' ' -f1)

    update_builtin_section_minio "${minio_path}" "${src}"
    checksum_after=$(md5 -q "${FAKE_MINIO_ROOT}/${minio_path}" 2>/dev/null \
        || md5sum "${FAKE_MINIO_ROOT}/${minio_path}" | cut -d' ' -f1)

    assert_eq "file unchanged on second run" "${checksum_before}" "${checksum_after}"
}

echo ""
echo "=== MC5: Source missing — remote file unchanged ==="
{
    d=$(new_workdir)
    minio_path="${MINIO_PREFIX}/AGENTS-mc5.md"
    echo "# Existing Worker Content" | minio_put "${minio_path}"

    update_builtin_section_minio "${minio_path}" "${d}/nonexistent.md"

    content=$(minio_cat "${minio_path}")
    assert_eq "remote unchanged when source missing" "# Existing Worker Content" "${content}"
}

echo ""
echo "=== MC6: Corrupted remote (2 start markers) — auto-repair ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Clean Content" > "${src}"
    minio_path="${MINIO_PREFIX}/AGENTS-mc6.md"
    {
        printf '<!-- hiclaw-builtin-start -->\n'
        printf '> ⚠️ DO NOT EDIT\n\n'
        printf '# Old Content\n'
        printf '<!-- hiclaw-builtin-end -->\n'
        printf '<!-- hiclaw-builtin-start -->\n'
        printf '> ⚠️ DO NOT EDIT\n\n'
        printf '# Old Content\n'
        printf '<!-- hiclaw-builtin-end -->\n'
    } | minio_put "${minio_path}"

    update_builtin_section_minio "${minio_path}" "${src}"

    remote_file="${FAKE_MINIO_ROOT}/${minio_path}"
    content=$(cat "${remote_file}")
    assert_eq       "repaired to 1 start marker" "1" "$(awk '$0 == "<!-- hiclaw-builtin-start -->" {c++} END {print c+0}' "${remote_file}")"
    assert_eq       "repaired to 1 end marker"   "1" "$(awk '$0 == "<!-- hiclaw-builtin-end -->" {c++} END {print c+0}' "${remote_file}")"
    assert_contains "has new content"             "# Clean Content" "${content}"
}

echo ""
echo "=== MC7: Multiple workers — each gets independent merge ==="
{
    d=$(new_workdir)
    src="${d}/source.md"
    echo "# Worker AGENTS v2" > "${src}"

    # Worker A: has markers + user content
    minio_a="${MINIO_PREFIX}-a/AGENTS.md"
    {
        printf '<!-- hiclaw-builtin-start -->\n'
        printf '> ⚠️ **DO NOT EDIT**\n\n'
        printf '# Worker AGENTS v1\n'
        printf '\n<!-- hiclaw-builtin-end -->\n'
        printf '\n## Worker A Notes\nmy custom stuff\n'
    } | minio_put "${minio_a}"

    # Worker B: legacy (no markers)
    minio_b="${MINIO_PREFIX}-b/AGENTS.md"
    echo "# Worker AGENTS v1 (legacy)" | minio_put "${minio_b}"

    update_builtin_section_minio "${minio_a}" "${src}"
    update_builtin_section_minio "${minio_b}" "${src}"

    content_a=$(minio_cat "${minio_a}")
    content_b=$(minio_cat "${minio_b}")

    assert_contains     "A: new builtin"         "# Worker AGENTS v2" "${content_a}"
    assert_contains     "A: user content kept"   "## Worker A Notes"  "${content_a}"
    assert_contains     "B: new builtin"         "# Worker AGENTS v2" "${content_b}"
    assert_not_contains "B: legacy content gone" "legacy"             "${content_b}"
    assert_contains     "B: has markers now"     "hiclaw-builtin-start" "${content_b}"
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
