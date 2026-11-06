#!/bin/bash
# worker-isolation.sh - Worker isolation bundle (shim + gitconfig), M7
#
# The worker's git surface is a small managed bundle in an isolation
# directory (default /root/.agentteams, override with
# AGENTTEAMS_ISOLATION_DIR), rewritten WHOLE on every start — never
# patched incrementally:
#
#   <dir>/bin/agentteams-sync   shim exec'ing the file-sync script
#   <dir>/gitconfig             the worker's entire git configuration
#
# Design (M7, 04-优化落地方案-20261101):
#   - whole-bundle replacement: each file is rendered to an mktemp file
#     in the same directory and atomically renamed into place;
#     incremental edits are lost by design (the marker comment says so);
#   - integrity self-check BEFORE writing: a non-empty bundle must be
#     exactly the expected files, each carrying the marker comment —
#     a foreign file (or a missing expected file) HALTS startup with an
#     explicit error. Nothing is ever silently repaired;
#   - environment isolation: worker_isolation_env scrubs inherited
#     GIT_CONFIG_* variables, points git at the bundle gitconfig via
#     GIT_CONFIG_GLOBAL, disables the system config, and clears askpass
#     and identity variables (the worker does not commit — git is
#     delegated to the Manager; a commit that does occur fails closed
#     instead of misattributing).
#
# Scope: this is a default-path guardrail, NOT an OS-level security
# boundary (research 02 §9).
#
# Usage (worker entrypoint):
#   source /opt/agentteams/scripts/lib/worker-isolation.sh
#   worker_isolation_setup "${WORKSPACE}" || exit 1
#   worker_isolation_env

# Marker identifying a file as written by the entrypoint. The shim keeps
# line 1 as the shebang and the marker on line 2; the gitconfig keeps
# the marker on line 1.
WORKER_ISOLATION_MARKER="# Written by agentteams worker entrypoint on every start; edits are lost."

# Expected bundle files, relative to the isolation directory.
WORKER_ISOLATION_FILES=("bin/agentteams-sync" "gitconfig")

worker_isolation_log() {
    echo "[worker-isolation $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

worker_isolation_dir() {
    printf '%s' "${AGENTTEAMS_ISOLATION_DIR:-/root/.agentteams}"
}

# Fail with the next-step instruction. Never repairs silently.
_worker_isolation_fail() {
    echo "[worker-isolation] ERROR: $1" >&2
    echo "[worker-isolation] Next step: stop the worker, remove the offending path (or the whole isolation dir $(worker_isolation_dir)), and restart — the entrypoint rewrites the bundle in full on every start. No silent repair is performed." >&2
}

# Check one bundle file: regular (not symlink) and marker-bearing.
# The marker may sit on line 1 (gitconfig) or line 2 behind the shebang
# (shim).
_worker_isolation_check_file() {
    local file="$1" rel="$2" first second
    if [ -L "$file" ] || [ ! -f "$file" ]; then
        _worker_isolation_fail "${rel} is not a regular file (symlink or directory — foreign entry)"
        return 1
    fi
    first="$(head -n 1 "$file" 2>/dev/null)"
    if [ "$first" = "${WORKER_ISOLATION_MARKER}" ]; then
        return 0
    fi
    if [[ "$first" == '#!'* ]]; then
        second="$(head -n 2 "$file" 2>/dev/null | tail -n 1)"
        if [ "$second" = "${WORKER_ISOLATION_MARKER}" ]; then
            return 0
        fi
    fi
    _worker_isolation_fail "${rel} does not carry the entrypoint marker (foreign content)"
    return 1
}

# Fail-closed integrity check. A fresh/empty directory (nothing found)
# passes — the caller then writes the bundle. Any non-empty bundle must
# be exactly the expected set of files, each intact.
worker_isolation_check() {
    local dir="$1"
    local entry base found=0 f

    for entry in "${dir}"/*; do
        [ -e "$entry" ] || [ -L "$entry" ] || continue
        base="$(basename "$entry")"
        case "$base" in
            bin)
                if [ -L "$entry" ] || [ ! -d "$entry" ]; then
                    _worker_isolation_fail "bin is not a plain directory (foreign entry)"
                    return 1
                fi
                # An empty bin counts as absent (a fresh dir where the
                # write bundle has not run yet).
                if [ -n "$(ls -A "$entry" 2>/dev/null)" ]; then
                    found=1
                fi
                ;;
            gitconfig)
                found=1
                _worker_isolation_check_file "$entry" "gitconfig" || return 1
                ;;
            *)
                _worker_isolation_fail "foreign file in isolation dir: ${base}"
                return 1
                ;;
        esac
    done

    for entry in "${dir}"/bin/*; do
        [ -e "$entry" ] || [ -L "$entry" ] || continue
        found=1
        base="$(basename "$entry")"
        if [ "$base" != "agentteams-sync" ]; then
            _worker_isolation_fail "foreign file in isolation dir: bin/${base}"
            return 1
        fi
        _worker_isolation_check_file "$entry" "bin/agentteams-sync" || return 1
    done

    if [ "$found" -eq 1 ]; then
        for f in "${WORKER_ISOLATION_FILES[@]}"; do
            if [ ! -f "${dir}/${f}" ]; then
                _worker_isolation_fail "incomplete bundle: ${f} is missing"
                return 1
            fi
        done
    fi
    return 0
}

# Whole-bundle rewrite: render each file to an mktemp file in the same
# directory, then atomically rename into place.
worker_isolation_write_bundle() {
    local dir="$1" workspace="$2" tmp

    mkdir -p "${dir}/bin"

    # Shim: exec via /bin/sh so it works even if the workspace copy
    # lost its exec bit (MinIO does not preserve Unix permission bits).
    tmp="$(mktemp "${dir}/bin/.agentteams-sync.XXXXXX")" || return 1
    if ! {
        printf '#!/bin/bash\n'
        printf '%s\n' "${WORKER_ISOLATION_MARKER}"
        printf 'exec /bin/sh "%s/skills/file-sync/scripts/agentteams-sync.sh" "$@"\n' "${workspace}"
    } > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod +x "$tmp"
    if ! mv "$tmp" "${dir}/bin/agentteams-sync"; then
        rm -f "$tmp"
        return 1
    fi
    worker_isolation_log "rewrote bin/agentteams-sync"

    # gitconfig: the worker's ENTIRE git config (whole replacement).
    # No identity on purpose: the worker does not commit (git is
    # delegated to the Manager per git-delegation-management); a commit
    # that does occur fails closed ("please tell me who you are")
    # instead of misattributing. The workspace is MinIO-synced, so the
    # workspace .gitconfig must not be the git surface either.
    tmp="$(mktemp "${dir}/.gitconfig.XXXXXX")" || return 1
    if ! {
        printf '%s\n' "${WORKER_ISOLATION_MARKER}"
        printf '#\n'
        printf '# Worker git surface (whole-bundle replacement, M7).\n'
        printf '# - The worker does not commit: git is delegated to the Manager.\n'
        printf '#   No identity is set on purpose — a commit that does occur\n'
        printf '#   will fail closed instead of misattributing.\n'
        printf '# - The workspace is MinIO-synced; the workspace .gitconfig is\n'
        printf '#   NOT the git surface (GIT_CONFIG_GLOBAL points here).\n'
        printf '[safe]\n'
        printf '\tdirectory = *\n'
    } > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    if ! mv "$tmp" "${dir}/gitconfig"; then
        rm -f "$tmp"
        return 1
    fi
    worker_isolation_log "rewrote gitconfig"
    return 0
}

# Pre-write self-check + whole-bundle rewrite. Returns 1 (halting the
# caller) on a foreign or incomplete bundle — never repairs silently.
worker_isolation_setup() {
    local workspace="${1:?usage: worker_isolation_setup <workspace>}"
    local dir
    dir="$(worker_isolation_dir)"
    # No mkdir here: the check must see the directory as found (an
    # empty bin counts as absent); the bundle write creates the layout.
    worker_isolation_check "$dir" || return 1
    worker_isolation_write_bundle "$dir" "$workspace"
}

# Point git at the bundle and scrub the inherited git environment so the
# worker's git surface is exactly the bundle:
#   - every GIT_CONFIG_* variable (COUNT/KEY/VALUE arrays from any parent)
#   - GIT_CONFIG_GLOBAL -> the bundle gitconfig (whole replacement)
#   - GIT_CONFIG_NOSYSTEM=1 (never read the system gitconfig)
#   - GIT_ASKPASS / SSH_ASKPASS (no interactive credential prompts)
#   - GIT_AUTHOR_*/GIT_COMMITTER_* identity (the worker does not commit)
worker_isolation_env() {
    local dir name
    dir="$(worker_isolation_dir)"
    for name in $(env | cut -d= -f1 | grep '^GIT_CONFIG_'); do
        unset "$name"
    done
    export GIT_CONFIG_GLOBAL="${dir}/gitconfig"
    export GIT_CONFIG_NOSYSTEM=1
    export GIT_ASKPASS=""
    unset SSH_ASKPASS
    unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL
}
