#!/bin/bash
# dispatch-gate.sh - Capacity-controlled dispatch governor
#
# Prevents overwhelming the AI gateway by limiting concurrent worker tasks.
# Implements per-worker circuit breaker for repeated failures.
#
# Usage:
#   dispatch-gate.sh --action check --worker W
#   dispatch-gate.sh --action record-failure --worker W
#   dispatch-gate.sh --action reset-failures --worker W
#   dispatch-gate.sh --action status
#   dispatch-gate.sh --action config --show
#   dispatch-gate.sh --action config --set-max-concurrent N
#   dispatch-gate.sh --action config --set-max-per-worker N

set -euo pipefail

CONFIG_FILE="${HOME}/dispatch-config.json"
STATE_FILE="${HOME}/state.json"
FAILURE_FILE="${HOME}/dispatch-failures.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_config() {
    if [ ! -f "$CONFIG_FILE" ]; then
        cat > "$CONFIG_FILE" << EOF
{
  "max_concurrent_workers": 0,
  "max_tasks_per_worker": 2,
  "circuit_breaker_threshold": 3,
  "circuit_breaker_cooldown_min": 30,
  "updated_at": "$(_ts)"
}
EOF
    fi
}

_ensure_failures() {
    if [ ! -f "$FAILURE_FILE" ]; then
        echo '{"workers": {}, "updated_at": "'$(_ts)'"}' > "$FAILURE_FILE"
    fi
}

_get_config() {
    _ensure_config
    jq -r --arg key "$1" '.[$key] // 0' "$CONFIG_FILE"
}

_get_active_task_count() {
    local worker="$1"
    if [ -f "$STATE_FILE" ]; then
        jq --arg w "$worker" '[.active_tasks[]? | select(.assigned_to == $w and .type == "finite")] | length' "$STATE_FILE" 2>/dev/null || echo "0"
    else
        echo "0"
    fi
}

_get_total_active_workers() {
    if [ -f "$STATE_FILE" ]; then
        jq '[.active_tasks[]? | select(.type == "finite") | .assigned_to] | unique | length' "$STATE_FILE" 2>/dev/null || echo "0"
    else
        echo "0"
    fi
}

# ─── Actions ─────────────────────────────────────────────────────────────────

action_check() {
    _ensure_config
    _ensure_failures

    local worker="${WORKER:-}"
    if [ -z "$worker" ]; then
        echo "ERROR: --worker is required" >&2
        exit 1
    fi

    local max_concurrent max_per_worker cb_threshold cb_cooldown
    max_concurrent=$(_get_config "max_concurrent_workers")
    max_per_worker=$(_get_config "max_tasks_per_worker")
    cb_threshold=$(_get_config "circuit_breaker_threshold")
    cb_cooldown=$(_get_config "circuit_breaker_cooldown_min")

    local active_count total_workers
    active_count=$(_get_active_task_count "$worker")
    total_workers=$(_get_total_active_workers)

    # Check circuit breaker
    local failure_count=0
    local last_failure_epoch=0
    local now_epoch
    now_epoch=$(date -u '+%s')

    if [ -f "$FAILURE_FILE" ]; then
        failure_count=$(jq -r --arg w "$worker" '.workers[$w].count // 0' "$FAILURE_FILE" 2>/dev/null || echo "0")
        local last_failure
        last_failure=$(jq -r --arg w "$worker" '.workers[$w].last_failure // ""' "$FAILURE_FILE" 2>/dev/null || echo "")
        if [ -n "$last_failure" ] && [ "$last_failure" != "null" ]; then
            last_failure_epoch=$(date -u -d "$last_failure" '+%s' 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$last_failure" '+%s' 2>/dev/null || echo "0")
        fi
    fi

    # Check if circuit breaker is open
    local circuit_open=false
    local cooldown_remaining=0
    if [ "$failure_count" -ge "$cb_threshold" ]; then
        local cooldown_secs=$((cb_cooldown * 60))
        local elapsed=$((now_epoch - last_failure_epoch))
        if [ "$elapsed" -lt "$cooldown_secs" ]; then
            circuit_open=true
            cooldown_remaining=$(( (cooldown_secs - elapsed) / 60 ))
        else
            # Cooldown expired, reset failures
            action_reset_failures_internal "$worker"
            failure_count=0
        fi
    fi

    # Determine if dispatch is allowed
    local allowed=true
    local reason="ok"

    if [ "$circuit_open" = true ]; then
        allowed=false
        reason="circuit_breaker_open (${cooldown_remaining}min cooldown remaining)"
    elif [ "$max_per_worker" -gt 0 ] && [ "$active_count" -ge "$max_per_worker" ]; then
        allowed=false
        reason="max_tasks_per_worker reached ($active_count/$max_per_worker)"
    elif [ "$max_concurrent" -gt 0 ] && [ "$total_workers" -ge "$max_concurrent" ] && [ "$active_count" -eq 0 ]; then
        # Only block NEW workers when at capacity; allow existing workers to get more tasks
        allowed=false
        reason="max_concurrent_workers reached ($total_workers/$max_concurrent)"
    fi

    jq -n \
        --argjson allowed "$allowed" \
        --arg reason "$reason" \
        --argjson active_count "$active_count" \
        --argjson max_per_worker "$max_per_worker" \
        --argjson total_workers "$total_workers" \
        --argjson max_concurrent "$max_concurrent" \
        --argjson circuit_open "$circuit_open" \
        --argjson failure_count "$failure_count" \
        '{
            allowed: $allowed,
            reason: $reason,
            worker_active_tasks: $active_count,
            max_tasks_per_worker: $max_per_worker,
            total_active_workers: $total_workers,
            max_concurrent_workers: $max_concurrent,
            circuit_breaker_open: $circuit_open,
            failure_count: $failure_count
        }'
}

action_record_failure() {
    _ensure_failures

    local worker="${WORKER:-}"
    if [ -z "$worker" ]; then
        echo "ERROR: --worker is required" >&2
        exit 1
    fi

    local now
    now=$(_ts)
    local tmp
    tmp=$(mktemp)
    jq --arg w "$worker" --arg now "$now" \
       '.workers[$w] = ((.workers[$w] // {count: 0}) | .count += 1 | .last_failure = $now)
        | .updated_at = $now' \
       "$FAILURE_FILE" > "$tmp" && mv "$tmp" "$FAILURE_FILE"

    local new_count
    new_count=$(jq -r --arg w "$worker" '.workers[$w].count' "$FAILURE_FILE")
    echo "OK: recorded failure for $worker (count: $new_count)"
}

action_reset_failures_internal() {
    local worker="$1"
    local now
    now=$(_ts)
    local tmp
    tmp=$(mktemp)
    jq --arg w "$worker" --arg now "$now" \
       'del(.workers[$w]) | .updated_at = $now' \
       "$FAILURE_FILE" > "$tmp" && mv "$tmp" "$FAILURE_FILE"
}

action_reset_failures() {
    _ensure_failures

    local worker="${WORKER:-}"
    if [ -z "$worker" ]; then
        echo "ERROR: --worker is required" >&2
        exit 1
    fi

    action_reset_failures_internal "$worker"
    echo "OK: reset failures for $worker"
}

action_status() {
    _ensure_config
    _ensure_failures

    local max_concurrent max_per_worker cb_threshold cb_cooldown
    max_concurrent=$(_get_config "max_concurrent_workers")
    max_per_worker=$(_get_config "max_tasks_per_worker")
    cb_threshold=$(_get_config "circuit_breaker_threshold")
    cb_cooldown=$(_get_config "circuit_breaker_cooldown_min")

    local total_workers
    total_workers=$(_get_total_active_workers)

    # Get workers with open circuit breakers
    local open_breakers="[]"
    if [ -f "$FAILURE_FILE" ]; then
        local now_epoch
        now_epoch=$(date -u '+%s')
        local cooldown_secs=$((cb_cooldown * 60))
        open_breakers=$(jq -c --argjson threshold "$cb_threshold" --argjson now "$now_epoch" --argjson cooldown "$cooldown_secs" \
            '[.workers | to_entries[] | select(.value.count >= $threshold) | {worker: .key, count: .value.count, last_failure: .value.last_failure}]' \
            "$FAILURE_FILE" 2>/dev/null || echo "[]")
    fi

    jq -n \
        --argjson max_concurrent "$max_concurrent" \
        --argjson max_per_worker "$max_per_worker" \
        --argjson cb_threshold "$cb_threshold" \
        --argjson cb_cooldown "$cb_cooldown" \
        --argjson total_workers "$total_workers" \
        --argjson open_breakers "$open_breakers" \
        '{
            config: {
                max_concurrent_workers: $max_concurrent,
                max_tasks_per_worker: $max_per_worker,
                circuit_breaker_threshold: $cb_threshold,
                circuit_breaker_cooldown_min: $cb_cooldown
            },
            current: {
                total_active_workers: $total_workers,
                capacity_available: (if $max_concurrent > 0 then ($max_concurrent - $total_workers) else -1 end)
            },
            circuit_breakers: $open_breakers
        }'
}

action_config() {
    _ensure_config

    if [ "${SHOW:-}" = "true" ]; then
        jq '.' "$CONFIG_FILE"
        return
    fi

    local tmp
    tmp=$(mktemp)

    if [ -n "${SET_MAX_CONCURRENT:-}" ]; then
        jq --argjson v "$SET_MAX_CONCURRENT" --arg now "$(_ts)" \
           '.max_concurrent_workers = $v | .updated_at = $now' \
           "$CONFIG_FILE" > "$tmp" && mv "$tmp" "$CONFIG_FILE"
        echo "OK: set max_concurrent_workers = $SET_MAX_CONCURRENT"
    fi

    if [ -n "${SET_MAX_PER_WORKER:-}" ]; then
        jq --argjson v "$SET_MAX_PER_WORKER" --arg now "$(_ts)" \
           '.max_tasks_per_worker = $v | .updated_at = $now' \
           "$CONFIG_FILE" > "$tmp" && mv "$tmp" "$CONFIG_FILE"
        echo "OK: set max_tasks_per_worker = $SET_MAX_PER_WORKER"
    fi
}

# ─── Argument parsing ────────────────────────────────────────────────────────

ACTION=""
WORKER=""
SHOW=""
SET_MAX_CONCURRENT=""
SET_MAX_PER_WORKER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)              ACTION="$2"; shift 2 ;;
        --worker)              WORKER="$2"; shift 2 ;;
        --show)                SHOW="true"; shift ;;
        --set-max-concurrent)  SET_MAX_CONCURRENT="$2"; shift 2 ;;
        --set-max-per-worker)  SET_MAX_PER_WORKER="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

case "$ACTION" in
    check)           action_check ;;
    record-failure)  action_record_failure ;;
    reset-failures)  action_reset_failures ;;
    status)          action_status ;;
    config)          action_config ;;
    *) echo "ERROR: unknown action '$ACTION'" >&2; exit 1 ;;
esac
