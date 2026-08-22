#!/bin/bash
# builtin-merge.sh - Shared logic for merging builtin sections in .md files
#
# Sourced by upgrade-builtins.sh, create-worker.sh, and tests.
# Provides: BUILTIN_START, BUILTIN_END, BUILTIN_HEADER,
#           update_builtin_section(), update_builtin_section_minio()

BUILTIN_START="<!-- hiclaw-builtin-start -->"
BUILTIN_END="<!-- hiclaw-builtin-end -->"
BUILTIN_HEADER='<!-- hiclaw-builtin-start -->
> ⚠️ **DO NOT EDIT** this section. It is managed by HiClaw and will be automatically
> replaced on upgrade. To customize, add your content **after** the
> `<!-- hiclaw-builtin-end -->` marker below.
'

# _extract_frontmatter <file>
# If the file starts with YAML frontmatter (---...---), prints it (including delimiters).
# Prints nothing if no frontmatter.
_extract_frontmatter() {
    awk 'NR==1 && /^---[[:space:]]*$/ { in_fm=1 }
         in_fm { print }
         in_fm && NR>1 && /^---[[:space:]]*$/ { exit }' "$1"
}

# _extract_body <file>
# Prints everything after YAML frontmatter. If no frontmatter, prints the whole file.
_extract_body() {
    awk 'NR==1 && /^---[[:space:]]*$/ { in_fm=1; next }
         in_fm && /^---[[:space:]]*$/ { in_fm=0; next }
         in_fm { next }
         { print }' "$1"
}

# _write_builtin_file <target> <source> [user_content]
# Writes a builtin-managed file, placing YAML frontmatter (if any) before the markers.
_write_builtin_file() {
    local target="$1" source="$2" user_content="${3:-}"
    local frontmatter
    frontmatter=$(_extract_frontmatter "${source}")

    {
        if [ -n "${frontmatter}" ]; then
            printf '%s\n\n' "${frontmatter}"
        fi
        printf '%s\n' "${BUILTIN_HEADER}"
        _extract_body "${source}"
        printf '\n%s\n' "${BUILTIN_END}"
        if [ -n "${user_content}" ]; then printf '\n%s\n' "${user_content}"; fi
    } > "${target}.tmp" || { log "  ERROR: failed to write ${target}.tmp"; return 1; }
    mv "${target}.tmp" "${target}" || { log "  ERROR: failed to move ${target}.tmp -> ${target}"; return 1; }
}

# update_builtin_section <target_file> <source_file>
#
# Merges the builtin section from source into target:
#   - If target doesn't exist: write marker-wrapped source content
#   - If target has markers: replace builtin section, preserve user content
#   - If target has no markers (old install): overwrite with new builtin + markers
update_builtin_section() {
    local target="$1"
    local source="$2"

    if [ ! -f "${source}" ]; then
        log "  WARNING: source not found: ${source}, skipping"
        return 0
    fi

    mkdir -p "$(dirname "${target}")" || { log "  ERROR: failed to create directory for ${target}"; return 1; }

    if [ ! -f "${target}" ]; then
        log "  Creating: ${target}"
        _write_builtin_file "${target}" "${source}"
        return 0
    fi

    if grep -q 'hiclaw-builtin-start' "${target}" 2>/dev/null; then
        # Detect corrupted file:
        # 1. markers must be exactly start=1, end=1
        # 2. the builtin heading must not appear after the end marker (leaked builtin content)
        local start_count end_count heading leaked_heading
        start_count=$(awk '$0 == "<!-- hiclaw-builtin-start -->" {c++} END {print c+0}' "${target}" 2>/dev/null || echo 0)
        end_count=$(awk '$0 == "<!-- hiclaw-builtin-end -->" {c++} END {print c+0}' "${target}" 2>/dev/null || echo 0)
        heading=$(grep -m1 '^#' "${source}" 2>/dev/null || true)
        leaked_heading=0
        if [ -n "${heading}" ]; then
            leaked_heading=$(awk -v h="${heading}" '$0 == "<!-- hiclaw-builtin-end -->" {found=1; next} found && $0 == h {c++} END {print c+0}' "${target}" 2>/dev/null || echo 0)
        fi
        if [ "${start_count}" -ne 1 ] || [ "${end_count}" -ne 1 ] || [ "${leaked_heading}" -gt 0 ]; then
            log "  Corrupted (start=${start_count}, end=${end_count}, leaked_heading=${leaked_heading}): ${target} — force rewriting"
            _write_builtin_file "${target}" "${source}"
            log "  Rewrote corrupted file: ${target}"
            return 0
        fi

        # Has markers: check if builtin content actually changed
        local current_builtin new_builtin
        current_builtin=$(awk '
            $0 == "<!-- hiclaw-builtin-start -->" { found=1; skip=1; next }
            $0 == "<!-- hiclaw-builtin-end -->"   { found=0; skip=0; next }
            !found { next }
            skip && /^[[:space:]]*$/ { next }
            skip && /^>/ { next }
            { skip=0; print }
        ' "${target}") || { log "  ERROR: awk failed reading builtin section from ${target}"; return 1; }
        new_builtin=$(_extract_body "${source}") || { log "  ERROR: failed to read source ${source}"; return 1; }
        if [ "${current_builtin}" = "${new_builtin}" ]; then
            log "  Up to date: ${target}"
            return 0
        fi

        # Extract user content after the end marker (|| true: empty user content is fine)
        local user_content
        user_content=$(awk '$0 == "<!-- hiclaw-builtin-end -->" {found=1; next} found{print}' "${target}" | grep -v 'hiclaw-builtin' || true)
        _write_builtin_file "${target}" "${source}" "${user_content}"
        log "  Updated builtin section: ${target}"
    else
        # Old install without markers: discard old content, write new builtin with markers
        log "  Adding markers to legacy file (discarding duplicate builtin content): ${target}"
        _write_builtin_file "${target}" "${source}"
    fi
}

# update_builtin_section_minio <minio_path> <source_file>
#
# Same merge logic as update_builtin_section, but operates on a file stored
# in MinIO instead of a local path. Pulls the current version to a temp file,
# runs update_builtin_section, then pushes the result back.
#
# If the remote file does not exist yet, creates a new marker-wrapped file
# and pushes it (same as update_builtin_section with a missing target).
update_builtin_section_minio() {
    local minio_path="$1"   # e.g. ${HICLAW_STORAGE_PREFIX}/agents/worker-1/AGENTS.md
    local source="$2"       # local source file (image builtin)

    if [ ! -f "${source}" ]; then
        log "  WARNING: source not found: ${source}, skipping"
        return 0
    fi

    local tmp_dir
    tmp_dir=$(mktemp -d /tmp/builtin-merge-minio-XXXXXX) || { log "  ERROR: mktemp failed"; return 1; }
    local tmp_target="${tmp_dir}/target.md"

    # Try to pull existing file from MinIO
    if mc cp "${minio_path}" "${tmp_target}" 2>/dev/null; then
        # File exists in MinIO — merge
        update_builtin_section "${tmp_target}" "${source}"
    else
        # File does not exist — create new with markers
        update_builtin_section "${tmp_target}" "${source}"
    fi

    # Push merged result back to MinIO
    if [ -f "${tmp_target}" ]; then
        mc cp "${tmp_target}" "${minio_path}" 2>/dev/null \
            || { log "  WARNING: Failed to push merged file to ${minio_path}"; rm -rf "${tmp_dir}"; return 1; }
    fi

    rm -rf "${tmp_dir}"
}
