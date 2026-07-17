#!/usr/bin/env bash
# verify-output.sh — verify Worker task deliverables on the local task directory.
#
# Reads default claims (nonempty result.md + Deliverables from result.md) and optional
# meta.json verifiable_claims. Emits JSON on stdout; exit 0 when all required claims pass.
#
# Usage:
#   verify-output.sh --task-id T [--task-dir DIR] [--task-root ROOT]

set -euo pipefail

TASK_ID=""
TASK_DIR=""
TASK_ROOT="${HICLAW_TASK_ROOT:-/root/hiclaw-fs}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --task-id)  TASK_ID="$2";  shift 2 ;;
        --task-dir) TASK_DIR="$2"; shift 2 ;;
        --task-root) TASK_ROOT="$2"; shift 2 ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$TASK_ID" ]; then
    echo "Usage: $0 --task-id T [--task-dir DIR] [--task-root ROOT]" >&2
    exit 1
fi

if [ -z "$TASK_DIR" ]; then
    TASK_DIR="${TASK_ROOT%/}/shared/tasks/${TASK_ID}"
fi

_meta_file="${TASK_DIR}/meta.json"
_result_file="${TASK_DIR}/result.md"

_resolve_path() {
    local raw="$1"
    raw="${raw#"${raw%%[![:space:]]*}"}"
    raw="${raw%"${raw##*[![:space:]]}"}"
    if [ -z "$raw" ]; then
        return 1
    fi
    if [[ "$raw" == /* ]]; then
        printf '%s' "$raw"
        return 0
    fi
    if [[ "$raw" == shared/* ]]; then
        printf '%s' "${TASK_ROOT%/}/${raw}"
        return 0
    fi
    printf '%s' "${TASK_DIR%/}/${raw}"
}

_to_claim_path() {
    local abs="$1"
    local root="${TASK_ROOT%/}/"
    if [[ "$abs" == "$root"* ]]; then
        printf '%s' "${abs#"$root"}"
        return 0
    fi
    printf '%s' "$abs"
}

_check_claim() {
    local abs_path="$1"
    local check="$2"
    local detail=""

    case "$check" in
        exists)
            if [ -e "$abs_path" ]; then
                detail="path exists"
                printf 'true|%s' "$detail"
                return 0
            fi
            detail="path missing"
            printf 'false|%s' "$detail"
            return 1
            ;;
        nonempty)
            if [ ! -e "$abs_path" ]; then
                detail="path missing"
                printf 'false|%s' "$detail"
                return 1
            fi
            if [ ! -s "$abs_path" ]; then
                detail="file empty"
                printf 'false|%s' "$detail"
                return 1
            fi
            detail="file exists and is non-empty"
            printf 'true|%s' "$detail"
            return 0
            ;;
        *)
            detail="unknown check: ${check}"
            printf 'false|%s' "$detail"
            return 1
            ;;
    esac
}

_extract_deliverable_paths() {
    local result_file="$1"
    [ -f "$result_file" ] || return 0

    awk '
        function trim(s) {
            sub(/^[ \t\r\n]+/, "", s)
            sub(/[ \t\r\n]+$/, "", s)
            return s
        }
        /^DELIVERABLES:[[:space:]]*$/ { section = "deliverables"; next }
        /^##[[:space:]]+Deliverables[[:space:]]*$/ { section = "deliverables"; next }
        /^NOTES:[[:space:]]*$/ { if (section == "deliverables") section = ""; next }
        /^##[[:space:]]+/ {
            if (section == "deliverables") section = ""
            next
        }
        section == "deliverables" && /^-[[:space:]]+/ {
            line = $0
            sub(/^-[[:space:]]+/, "", line)
            line = trim(line)
            if (line != "") print line
        }
    ' "$result_file"
}

# Specs: JSON array of {path, check, required}. Later entries override same path.
_specs='[]'

_add_spec() {
    local claim_path="$1"
    local check="$2"
    local required="$3"
    _specs=$(jq -n \
        --argjson existing "$_specs" \
        --arg path "$claim_path" \
        --arg check "$check" \
        --argjson required "$required" \
        '$existing
         | map(select(.path != $path))
         + [{path: $path, check: $check, required: $required}]')
}

# Default claim: result.md must be non-empty.
_add_spec "shared/tasks/${TASK_ID}/result.md" "nonempty" "true"

# Default claims: deliverable paths listed in result.md (when present).
if [ -f "$_result_file" ]; then
    while IFS= read -r deliverable || [ -n "$deliverable" ]; do
        [ -n "$deliverable" ] || continue
        _add_spec "$deliverable" "nonempty" "true"
    done < <(_extract_deliverable_paths "$_result_file")
fi

# Optional meta.json verifiable_claims extend/override defaults.
if [ -f "$_meta_file" ]; then
    while IFS=$'\t' read -r claim_path claim_check claim_required; do
        [ -n "$claim_path" ] || continue
        if [ "$claim_required" = "false" ]; then
            req_json="false"
        else
            req_json="true"
        fi
        _add_spec "$claim_path" "$claim_check" "$req_json"
    done < <(jq -r '.verifiable_claims[]? | [.path, (.check // "nonempty"), (if .required == false then "false" else "true" end)] | @tsv' "$_meta_file")
fi

_claims_json='[]'
while IFS=$'\t' read -r claim_path claim_check claim_required; do
    abs_path=""
    if abs_path=$(_resolve_path "$claim_path"); then
        :
    else
        abs_path=""
    fi

    passed="false"
    detail=""
    if [ -n "$abs_path" ]; then
        set +e
        check_out=$(_check_claim "$abs_path" "$claim_check")
        set -e
        passed="${check_out%%|*}"
        detail="${check_out#*|}"
    else
        detail="invalid or empty path"
    fi

    if [ -n "$abs_path" ]; then
        display_path=$(_to_claim_path "$abs_path")
    else
        display_path="$claim_path"
    fi

    _claims_json=$(jq -n \
        --argjson existing "$_claims_json" \
        --arg path "$display_path" \
        --arg check "$claim_check" \
        --argjson required "$claim_required" \
        --argjson passed "$passed" \
        --arg detail "$detail" \
        '$existing + [{
            path: $path,
            check: $check,
            required: $required,
            passed: $passed,
            detail: $detail
        }]')
done < <(jq -r '.[] | [.path, .check, (if .required then "true" else "false" end)] | @tsv' <<< "$_specs")

verified=$(jq -r '
    if (. == []) then true
    else all(.[]; (.required | not) or .passed)
    end
' <<< "$_claims_json")

jq -n \
    --arg task_id "$TASK_ID" \
    --argjson verified "$verified" \
    --argjson claims "$_claims_json" \
    '{task_id: $task_id, verified: $verified, claims: $claims}'

if [ "$verified" = "true" ]; then
    exit 0
fi
exit 1
