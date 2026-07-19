#!/bin/bash
# manage-escalations.sh - Atomic escalations.json operations for structured escalation tracking
#
# Follows manage-state.sh pattern: all writes use tmp+mv for atomicity.
#
# Usage:
#   manage-escalations.sh --action init
#   manage-escalations.sh --action raise       --task-id T --severity S --category C --worker W --summary "..." [--question "..."] [--what-tried "..."]
#   manage-escalations.sh --action resolve     --id ESC_ID [--resolution "..."]
#   manage-escalations.sh --action acknowledge --id ESC_ID
#   manage-escalations.sh --action list        [--status open|acknowledged|resolved|all]
#   manage-escalations.sh --action check-stale
#   manage-escalations.sh --action summary

set -euo pipefail

ESCALATIONS_FILE="${HOME}/escalations.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_gen_id() {
    echo "esc-$(date -u '+%Y%m%d-%H%M%S')-$$"
}

_ensure_file() {
    if [ ! -f "$ESCALATIONS_FILE" ]; then
        cat > "$ESCALATIONS_FILE" << EOF
{
  "escalations": [],
  "updated_at": "$(_ts)"
}
EOF
    fi
}

# Re-escalation thresholds in hours by severity
_stale_threshold_hours() {
    case "$1" in
        CRITICAL) echo 1 ;;
        HIGH)     echo 4 ;;
        MEDIUM)   echo 24 ;;
        *)        echo 24 ;;
    esac
}

MAX_RE_ESCALATIONS=3

# ─── Actions ─────────────────────────────────────────────────────────────────

action_init() {
    _ensure_file
    echo "OK: escalations.json ready at $ESCALATIONS_FILE"
}

action_raise() {
    _ensure_file

    local severity="${SEVERITY:-MEDIUM}"
    local category="${CATEGORY:-technical}"
    local task_id="${TASK_ID:-}"
    local worker="${WORKER:-}"
    local summary="${SUMMARY:-}"
    local question="${QUESTION:-}"
    local what_tried="${WHAT_TRIED:-}"

    if [ -z "$summary" ]; then
        echo "ERROR: --summary is required" >&2
        exit 1
    fi

    # Validate severity
    case "$severity" in
        CRITICAL|HIGH|MEDIUM) ;;
        *) echo "ERROR: severity must be CRITICAL, HIGH, or MEDIUM" >&2; exit 1 ;;
    esac

    # Check for duplicate open escalation on same task+worker
    if [ -n "$task_id" ] && [ -n "$worker" ]; then
        local dup
        dup=$(jq -r --arg tid "$task_id" --arg w "$worker" \
            '[.escalations[] | select(.task_id == $tid and .worker == $w and .status == "open")] | length' \
            "$ESCALATIONS_FILE")
        if [ "$dup" -gt 0 ]; then
            echo "SKIP: open escalation already exists for task $task_id / worker $worker"
            return 0
        fi
    fi

    local esc_id
    esc_id=$(_gen_id)
    local now
    now=$(_ts)

    local tmp
    tmp=$(mktemp)
    jq --arg id "$esc_id" \
       --arg severity "$severity" \
       --arg category "$category" \
       --arg task_id "$task_id" \
       --arg worker "$worker" \
       --arg summary "$summary" \
       --arg question "$question" \
       --arg what_tried "$what_tried" \
       --arg now "$now" \
       '.escalations += [{
            id: $id,
            severity: $severity,
            category: $category,
            task_id: $task_id,
            worker: $worker,
            summary: $summary,
            question: $question,
            what_was_tried: $what_tried,
            created_at: $now,
            status: "open",
            acknowledged_at: null,
            resolved_at: null,
            resolution: null,
            last_re_escalated_at: null,
            re_escalation_count: 0
        }]
        | .updated_at = $now' \
       "$ESCALATIONS_FILE" > "$tmp" && mv "$tmp" "$ESCALATIONS_FILE"

    echo "OK: raised $severity escalation $esc_id for task ${task_id:-N/A} / worker ${worker:-N/A}"
    echo "{\"id\": \"$esc_id\", \"severity\": \"$severity\"}"
}

action_resolve() {
    _ensure_file

    local esc_id="${ID:-}"
    local resolution="${RESOLUTION:-}"

    if [ -z "$esc_id" ]; then
        echo "ERROR: --id is required" >&2
        exit 1
    fi

    local exists
    exists=$(jq -r --arg id "$esc_id" '[.escalations[] | select(.id == $id)] | length' "$ESCALATIONS_FILE")
    if [ "$exists" -eq 0 ]; then
        echo "ERROR: escalation $esc_id not found" >&2
        exit 1
    fi

    local now
    now=$(_ts)
    local tmp
    tmp=$(mktemp)
    jq --arg id "$esc_id" --arg now "$now" --arg res "$resolution" \
       '(.escalations[] | select(.id == $id)) |= (.status = "resolved" | .resolved_at = $now | .resolution = $res)
        | .updated_at = $now' \
       "$ESCALATIONS_FILE" > "$tmp" && mv "$tmp" "$ESCALATIONS_FILE"

    echo "OK: resolved escalation $esc_id"
}

action_acknowledge() {
    _ensure_file

    local esc_id="${ID:-}"

    if [ -z "$esc_id" ]; then
        echo "ERROR: --id is required" >&2
        exit 1
    fi

    local now
    now=$(_ts)
    local tmp
    tmp=$(mktemp)
    jq --arg id "$esc_id" --arg now "$now" \
       '(.escalations[] | select(.id == $id)) |= (.status = "acknowledged" | .acknowledged_at = $now)
        | .updated_at = $now' \
       "$ESCALATIONS_FILE" > "$tmp" && mv "$tmp" "$ESCALATIONS_FILE"

    echo "OK: acknowledged escalation $esc_id"
}

action_list() {
    _ensure_file

    local status_filter="${STATUS:-open}"

    if [ "$status_filter" = "all" ]; then
        jq '.escalations' "$ESCALATIONS_FILE"
    else
        jq --arg s "$status_filter" '[.escalations[] | select(.status == $s)]' "$ESCALATIONS_FILE"
    fi
}

action_check_stale() {
    _ensure_file

    local now_epoch
    now_epoch=$(date -u '+%s')
    local stale_items="[]"

    # Read all open or acknowledged (not resolved) escalations
    local escalations
    escalations=$(jq -c '[.escalations[] | select(.status != "resolved")]' "$ESCALATIONS_FILE")

    local count
    count=$(echo "$escalations" | jq 'length')

    for ((i=0; i<count; i++)); do
        local esc
        esc=$(echo "$escalations" | jq -c ".[$i]")

        local severity created_at last_re_esc re_count
        severity=$(echo "$esc" | jq -r '.severity')
        created_at=$(echo "$esc" | jq -r '.created_at')
        last_re_esc=$(echo "$esc" | jq -r '.last_re_escalated_at // .created_at')
        re_count=$(echo "$esc" | jq -r '.re_escalation_count')

        # Skip if max re-escalations reached
        if [ "$re_count" -ge "$MAX_RE_ESCALATIONS" ]; then
            # Add to stale with max_escalations flag
            stale_items=$(echo "$stale_items" | jq --argjson esc "$esc" '. + [$esc + {stale_reason: "max_re_escalations_reached"}]')
            continue
        fi

        # Calculate threshold
        local threshold_hours
        threshold_hours=$(_stale_threshold_hours "$severity")
        local threshold_secs=$((threshold_hours * 3600))

        # Parse last escalation time
        local last_epoch
        last_epoch=$(date -u -d "$last_re_esc" '+%s' 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$last_re_esc" '+%s' 2>/dev/null || echo "$now_epoch")

        local elapsed=$((now_epoch - last_epoch))

        if [ "$elapsed" -ge "$threshold_secs" ]; then
            stale_items=$(echo "$stale_items" | jq --argjson esc "$esc" --arg reason "threshold_exceeded" '. + [$esc + {stale_reason: $reason}]')

            # Update re-escalation tracking
            local esc_id
            esc_id=$(echo "$esc" | jq -r '.id')
            local now
            now=$(_ts)
            local tmp
            tmp=$(mktemp)
            jq --arg id "$esc_id" --arg now "$now" \
               '(.escalations[] | select(.id == $id)) |= (.last_re_escalated_at = $now | .re_escalation_count += 1)
                | .updated_at = $now' \
               "$ESCALATIONS_FILE" > "$tmp" && mv "$tmp" "$ESCALATIONS_FILE"
        fi
    done

    # Output stale items
    echo "$stale_items" | jq '{stale_count: length, items: .}'
}

action_summary() {
    _ensure_file

    jq '{
        open: [.escalations[] | select(.status == "open")] | length,
        acknowledged: [.escalations[] | select(.status == "acknowledged")] | length,
        resolved: [.escalations[] | select(.status == "resolved")] | length,
        highest_severity: (
            if ([.escalations[] | select(.status != "resolved" and .severity == "CRITICAL")] | length) > 0 then "CRITICAL"
            elif ([.escalations[] | select(.status != "resolved" and .severity == "HIGH")] | length) > 0 then "HIGH"
            elif ([.escalations[] | select(.status != "resolved" and .severity == "MEDIUM")] | length) > 0 then "MEDIUM"
            else "NONE"
            end
        )
    }' "$ESCALATIONS_FILE"
}

# ─── Argument parsing ────────────────────────────────────────────────────────

ACTION=""
SEVERITY=""
CATEGORY=""
TASK_ID=""
WORKER=""
SUMMARY=""
QUESTION=""
WHAT_TRIED=""
ID=""
RESOLUTION=""
STATUS=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)     ACTION="$2"; shift 2 ;;
        --severity)   SEVERITY="$2"; shift 2 ;;
        --category)   CATEGORY="$2"; shift 2 ;;
        --task-id)    TASK_ID="$2"; shift 2 ;;
        --worker)     WORKER="$2"; shift 2 ;;
        --summary)    SUMMARY="$2"; shift 2 ;;
        --question)   QUESTION="$2"; shift 2 ;;
        --what-tried) WHAT_TRIED="$2"; shift 2 ;;
        --id)         ID="$2"; shift 2 ;;
        --resolution) RESOLUTION="$2"; shift 2 ;;
        --status)     STATUS="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

case "$ACTION" in
    init)         action_init ;;
    raise)        action_raise ;;
    resolve)      action_resolve ;;
    acknowledge)  action_acknowledge ;;
    list)         action_list ;;
    check-stale)  action_check_stale ;;
    summary)      action_summary ;;
    *) echo "ERROR: unknown action '$ACTION'" >&2; exit 1 ;;
esac
