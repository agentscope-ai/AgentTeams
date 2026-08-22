#!/bin/bash
# lifecycle-worker.sh - Worker container lifecycle management
#
# Manages automatic stop/start of Worker containers based on idle time.
# State is persisted in ~/worker-lifecycle.json.
#
# Usage:
#   lifecycle-worker.sh --action sync-status
#   lifecycle-worker.sh --action check-idle
#   lifecycle-worker.sh --action stop --worker <name>
#   lifecycle-worker.sh --action start --worker <name>
#   lifecycle-worker.sh --action ensure-ready --worker <name>

set -euo pipefail

source /opt/hiclaw/scripts/lib/container-api.sh

LIFECYCLE_FILE="${HOME}/worker-lifecycle.json"
REGISTRY_FILE="${HOME}/workers-registry.json"
STATE_FILE="${HOME}/state.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_log() {
    echo "[lifecycle $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Read a field from the lifecycle JSON for a specific worker
# Usage: _get_worker_field <worker> <field>
_get_worker_field() {
    local worker="$1"
    local field="$2"
    jq -r --arg w "$worker" --arg f "$field" '.workers[$w][$f] // empty' "$LIFECYCLE_FILE" 2>/dev/null
}

# Update a field in the lifecycle JSON for a specific worker
# Usage: _set_worker_field <worker> <field> <value>
_set_worker_field() {
    local worker="$1"
    local field="$2"
    local value="$3"
    local tmp
    tmp=$(mktemp)
    jq --arg w "$worker" --arg f "$field" --arg v "$value" \
        '.workers[$w][$f] = $v | .updated_at = (now | strftime("%Y-%m-%dT%H:%M:%SZ"))' \
        "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
}

# Initialize lifecycle file if it doesn't exist
_init_lifecycle_file() {
    if [ ! -f "$LIFECYCLE_FILE" ]; then
        _log "Initializing $LIFECYCLE_FILE"
        cat > "$LIFECYCLE_FILE" << 'EOF'
{
  "version": 1,
  "idle_timeout_minutes": 720,
  "updated_at": "",
  "workers": {}
}
EOF
        _set_worker_field "__init__" "__discard__" "" 2>/dev/null || true
        # Re-initialize cleanly
        cat > "$LIFECYCLE_FILE" << EOF
{
  "version": 1,
  "idle_timeout_minutes": ${HICLAW_WORKER_IDLE_TIMEOUT:-720},
  "updated_at": "$(_ts)",
  "workers": {}
}
EOF
    else
        # File already exists — respect any manual edits.
        # HICLAW_WORKER_IDLE_TIMEOUT is only used for initial creation (above).
        true
    fi

    if [ ! -f "$STATE_FILE" ]; then
        _log "Initializing $STATE_FILE"
        cat > "$STATE_FILE" << EOF
{
  "active_tasks": [],
  "updated_at": "$(_ts)"
}
EOF
    fi
}

# Ensure a worker entry exists in lifecycle file
_ensure_worker_entry() {
    local worker="$1"
    local exists
    exists=$(jq -r --arg w "$worker" '.workers | has($w)' "$LIFECYCLE_FILE" 2>/dev/null)
    if [ "$exists" != "true" ]; then
        local tmp
        tmp=$(mktemp)
        jq --arg w "$worker" --arg ts "$(_ts)" \
            '.workers[$w] = {
                "container_status": "unknown",
                "idle_since": null,
                "auto_stopped_at": null,
                "last_started_at": null
            } | .updated_at = $ts' \
            "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
    fi
}

# Get list of all worker names from workers-registry.json
_get_all_workers() {
    if [ ! -f "$REGISTRY_FILE" ]; then
        _log "WARNING: $REGISTRY_FILE not found"
        return
    fi
    jq -r '.workers | keys[]' "$REGISTRY_FILE" 2>/dev/null
}

# Check if a worker has any active finite tasks in state.json
# Returns 0 if worker has active finite tasks, 1 otherwise
_worker_has_finite_tasks() {
    local worker="$1"
    if [ ! -f "$STATE_FILE" ]; then
        return 1
    fi
    local count
    count=$(jq -r --arg w "$worker" \
        '[.active_tasks[] | select(.assigned_to == $w and .type == "finite")] | length' \
        "$STATE_FILE" 2>/dev/null || echo "0")
    [ "$count" -gt 0 ]
}

# Check if a worker has any active tasks (finite or infinite) in state.json
# Returns 0 if worker has any active tasks, 1 otherwise
_worker_has_any_tasks() {
    local worker="$1"
    if [ ! -f "$STATE_FILE" ]; then
        return 1
    fi
    local count
    count=$(jq -r --arg w "$worker" \
        '[.active_tasks[] | select(.assigned_to == $w)] | length' \
        "$STATE_FILE" 2>/dev/null || echo "0")
    [ "$count" -gt 0 ]
}

# Check if a worker has enabled cron jobs in its .openclaw/cron/jobs.json
# Returns 0 if worker has enabled cron jobs, 1 otherwise
_worker_has_cron_jobs() {
    local worker="$1"
    local cron_file="/root/hiclaw-fs/agents/${worker}/.openclaw/cron/jobs.json"
    if [ ! -f "$cron_file" ]; then
        return 1
    fi
    local count
    # Handle both {"jobs":[...]} and bare array [...] formats
    count=$(jq '(if type == "object" then .jobs // [] else . end) | [.[] | select(.state.enabled == true)] | length' "$cron_file" 2>/dev/null || echo "0")
    [ "$count" -gt 0 ]
}

# ─── Actions ─────────────────────────────────────────────────────────────────

# Sync worker status into lifecycle file (Docker or cloud backend)
action_sync_status() {
    _init_lifecycle_file

    local backend
    backend=$(_detect_worker_backend)

    if [ "$backend" = "none" ]; then
        _log "No worker backend available — marking all workers as remote"
        local workers
        workers=$(_get_all_workers)
        for worker in $workers; do
            _ensure_worker_entry "$worker"
            _set_worker_field "$worker" "container_status" "remote"
        done
        return 0
    fi

    local workers
    workers=$(_get_all_workers)
    if [ -z "$workers" ]; then
        _log "No workers found in registry"
        return 0
    fi

    for worker in $workers; do
        _ensure_worker_entry "$worker"
        local status
        status=$(worker_backend_status "$worker")
        _log "Worker $worker: status=$status (backend=$backend)"
        local tmp
        tmp=$(mktemp)
        jq --arg w "$worker" --arg s "$status" --arg ts "$(_ts)" \
            '.workers[$w].container_status = $s | .updated_at = $ts' \
            "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
    done

    _log "Status sync complete"
}

# Check for idle workers and update idle_since timestamps
# Also auto-stops workers that have exceeded idle_timeout_minutes
action_check_idle() {
    _init_lifecycle_file

    local idle_timeout
    idle_timeout=$(jq -r '.idle_timeout_minutes // 30' "$LIFECYCLE_FILE")
    local now_epoch
    now_epoch=$(date -u +%s)

    local workers
    workers=$(_get_all_workers)
    if [ -z "$workers" ]; then
        return 0
    fi

    for worker in $workers; do
        _ensure_worker_entry "$worker"

        local container_status
        container_status=$(_get_worker_field "$worker" "container_status")

        # Skip remote workers and non-running containers
        if [ "$container_status" = "remote" ] || [ "$container_status" = "not_found" ]; then
            continue
        fi

        if _worker_has_any_tasks "$worker"; then
            # Worker is active (finite or infinite task) — clear idle_since
            local current_idle
            current_idle=$(_get_worker_field "$worker" "idle_since")
            if [ -n "$current_idle" ] && [ "$current_idle" != "null" ]; then
                _log "Worker $worker has active tasks — clearing idle_since"
                local tmp
                tmp=$(mktemp)
                jq --arg w "$worker" --arg ts "$(_ts)" \
                    '.workers[$w].idle_since = null | .updated_at = $ts' \
                    "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
            fi
        elif _worker_has_cron_jobs "$worker"; then
            # Worker has enabled cron jobs — never idle-stop
            local current_idle
            current_idle=$(_get_worker_field "$worker" "idle_since")
            if [ -n "$current_idle" ] && [ "$current_idle" != "null" ]; then
                _log "Worker $worker has cron jobs — clearing idle_since (idle-stop disabled)"
                local tmp
                tmp=$(mktemp)
                jq --arg w "$worker" --arg ts "$(_ts)" \
                    '.workers[$w].idle_since = null | .updated_at = $ts' \
                    "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
            fi
        else
            # Worker has no active tasks (neither finite nor infinite)
            if [ "$container_status" != "running" ]; then
                continue
            fi

            # Safety net: if worker was recently started, don't mark idle yet.
            # This protects against races where the Manager hasn't registered
            # the task in state.json yet.
            local last_started
            last_started=$(_get_worker_field "$worker" "last_started_at")
            if [ -n "$last_started" ] && [ "$last_started" != "null" ]; then
                local started_epoch
                started_epoch=$(date -u -d "$last_started" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$last_started" +%s 2>/dev/null)
                local since_start=$(( now_epoch - started_epoch ))
                local grace_seconds=$(( idle_timeout * 60 ))
                if [ "$since_start" -lt "$grace_seconds" ]; then
                    _log "Worker $worker has no tasks but was started ${since_start}s ago (grace: ${grace_seconds}s) — skipping idle check"
                    continue
                fi
            fi

            local idle_since
            idle_since=$(_get_worker_field "$worker" "idle_since")

            if [ -z "$idle_since" ] || [ "$idle_since" = "null" ]; then
                # Start counting idle time
                _log "Worker $worker is idle — setting idle_since"
                local tmp
                tmp=$(mktemp)
                jq --arg w "$worker" --arg ts "$(_ts)" \
                    '.workers[$w].idle_since = $ts | .updated_at = $ts' \
                    "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
            else
                # Check if idle timeout exceeded
                local idle_epoch
                idle_epoch=$(date -u -d "$idle_since" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$idle_since" +%s 2>/dev/null)
                local idle_seconds=$(( now_epoch - idle_epoch ))
                local timeout_seconds=$(( idle_timeout * 60 ))

                if [ "$idle_seconds" -ge "$timeout_seconds" ]; then
                    _log "Worker $worker idle for ${idle_seconds}s (timeout: ${timeout_seconds}s) — auto-stopping"
                    action_stop "$worker"
                fi
            fi
        fi
    done
}

# Stop a worker (Docker container or cloud instance)
action_stop() {
    local worker="$1"
    _init_lifecycle_file
    _ensure_worker_entry "$worker"

    local backend
    backend=$(_detect_worker_backend)
    if [ "$backend" = "none" ]; then
        _log "ERROR: No worker backend available"
        return 1
    fi

    _log "Stopping worker $worker (backend=$backend)"
    if worker_backend_stop "$worker"; then
        local tmp
        tmp=$(mktemp)
        jq --arg w "$worker" --arg ts "$(_ts)" \
            '.workers[$w].container_status = "stopped"
            | .workers[$w].auto_stopped_at = $ts
            | .updated_at = $ts' \
            "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
        _log "Worker $worker stopped and lifecycle file updated"
    else
        _log "ERROR: Failed to stop worker $worker"
        return 1
    fi
}

# Start (wake up) a stopped worker, or recreate if it no longer exists
# (e.g. after Manager upgrade where old containers were removed).
action_start() {
    local worker="$1"
    _init_lifecycle_file
    _ensure_worker_entry "$worker"

    # Skip remote workers — they are not Manager-managed containers
    local deployment
    deployment=$(jq -r --arg w "$worker" '.workers[$w].deployment // "local"' "$REGISTRY_FILE" 2>/dev/null)
    if [ "$deployment" = "remote" ]; then
        _log "Worker $worker is remote — cannot start via container API"
        _log "The admin should restart this worker on the target machine manually"
        return 1
    fi

    local backend
    backend=$(_detect_worker_backend)
    if [ "$backend" = "none" ]; then
        _log "ERROR: No worker backend available"
        return 1
    fi

    local status
    status=$(worker_backend_status "$worker")

    local ok=false
    if [ "$status" = "not_found" ]; then
        _log "Worker $worker not found — recreating (backend=$backend)"
        local creds_file="/data/worker-creds/${worker}.env"
        if [ -f "$creds_file" ]; then
            source "$creds_file"
        fi
        local runtime
        runtime=$(jq -r --arg w "$worker" '.workers[$w].runtime // "openclaw"' "$REGISTRY_FILE" 2>/dev/null)
        if [ "$backend" = "docker" ]; then
            if [ "$runtime" = "copaw" ]; then
                container_create_copaw_worker "$worker" "$worker" "${WORKER_MINIO_PASSWORD:-}" 2>&1 && ok=true
            else
                container_create_worker "$worker" "$worker" "${WORKER_MINIO_PASSWORD:-}" 2>&1 && ok=true
            fi
        else
            worker_backend_create "$worker" "" "" "[]" 2>&1 && ok=true
        fi
    else
        _log "Starting worker $worker (status: $status, backend=$backend)"
        worker_backend_start "$worker" && ok=true
    fi

    if [ "$ok" = true ]; then
        local tmp
        tmp=$(mktemp)
        jq --arg w "$worker" --arg ts "$(_ts)" \
            '.workers[$w].container_status = "running"
            | .workers[$w].idle_since = null
            | .workers[$w].last_started_at = $ts
            | .updated_at = $ts' \
            "$LIFECYCLE_FILE" > "$tmp" && mv "$tmp" "$LIFECYCLE_FILE"
        _log "Worker $worker running and lifecycle file updated"
    else
        _log "ERROR: Failed to start/recreate worker $worker"
        return 1
    fi
}

# Ensure a specific worker is ready to receive messages.
# If the container is stopped, start it; if not_found, recreate it.
# Outputs JSON: {"worker":"<name>","status":"ready|started|recreated|remote|failed","container_status":"..."}
# Usage: action_ensure_ready <worker_name>
action_ensure_ready() {
    local worker="$1"
    _init_lifecycle_file

    # Check deployment type — skip remote workers
    local deployment
    deployment=$(jq -r --arg w "$worker" '.workers[$w].deployment // "local"' "$REGISTRY_FILE" 2>/dev/null)
    if [ "$deployment" = "remote" ]; then
        _log "Worker $worker is remote — assumed ready"
        echo "{\"worker\":\"$worker\",\"status\":\"remote\",\"container_status\":\"remote\"}"
        return 0
    fi

    if ! container_api_available; then
        _log "Container API not available — cannot check worker $worker"
        echo "{\"worker\":\"$worker\",\"status\":\"failed\",\"container_status\":\"unknown\",\"error\":\"container_api_unavailable\"}"
        return 1
    fi

    local status
    status=$(container_status_worker "$worker")
    _log "Worker $worker container_status=$status"

    if [ "$status" = "running" ]; then
        echo "{\"worker\":\"$worker\",\"status\":\"ready\",\"container_status\":\"running\"}"
        return 0
    fi

    if [ "$status" = "not_found" ]; then
        _log "Worker $worker container not found — attempting recreate"
        _ensure_worker_entry "$worker"
        if action_start "$worker" 2>&1; then
            _log "Worker $worker recreated successfully"
            echo "{\"worker\":\"$worker\",\"status\":\"recreated\",\"container_status\":\"running\"}"
            return 0
        else
            _log "ERROR: Failed to recreate worker $worker"
            echo "{\"worker\":\"$worker\",\"status\":\"failed\",\"container_status\":\"not_found\",\"error\":\"recreate_failed\"}"
            return 1
        fi
    fi

    # stopped / exited / created — try to start
    _log "Worker $worker is $status — starting"
    _ensure_worker_entry "$worker"
    if action_start "$worker" 2>&1; then
        _log "Worker $worker started successfully"
        echo "{\"worker\":\"$worker\",\"status\":\"started\",\"container_status\":\"running\"}"
        return 0
    else
        _log "ERROR: Failed to start worker $worker"
        echo "{\"worker\":\"$worker\",\"status\":\"failed\",\"container_status\":\"$status\",\"error\":\"start_failed\"}"
        return 1
    fi
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
WORKER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)
            ACTION="$2"
            shift 2
            ;;
        --worker)
            WORKER="$2"
            shift 2
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <sync-status|check-idle|stop|start> [--worker <name>]" >&2
    exit 1
fi

case "$ACTION" in
    sync-status)
        action_sync_status
        ;;
    check-idle)
        action_check_idle
        ;;
    stop)
        if [ -z "$WORKER" ]; then
            echo "ERROR: --worker required for action 'stop'" >&2
            exit 1
        fi
        action_stop "$WORKER"
        ;;
    start)
        if [ -z "$WORKER" ]; then
            echo "ERROR: --worker required for action 'start'" >&2
            exit 1
        fi
        action_start "$WORKER"
        ;;
    ensure-ready)
        if [ -z "$WORKER" ]; then
            echo "ERROR: --worker required for action 'ensure-ready'" >&2
            exit 1
        fi
        action_ensure_ready "$WORKER"
        ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: sync-status, check-idle, stop, start, ensure-ready" >&2
        exit 1
        ;;
esac
