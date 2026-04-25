#!/bin/bash
# resolve-dag.sh - DAG dependency resolver for team project plan.md
#
# Parses plan.md task lines and resolves dependencies to determine
# which tasks are ready to execute, blocked, in-progress, or completed.
#
# Usage:
#   resolve-dag.sh --plan <PATH_TO_PLAN.md> --action ready|status|validate
#
# Actions:
#   ready    - Output tasks whose dependencies are all satisfied (pending + unblocked)
#   status   - Output full DAG state (all tasks grouped by status)
#   validate - Check for cycles in the dependency graph
#
# Task line format in plan.md:
#   - [ ] st-01 — Task title (assigned: @worker:domain)
#   - [ ] st-02 — Task title (assigned: @worker:domain, depends: st-01, st-03)
#
# Status markers: [ ] pending, [~] in-progress, [x] completed, [!] blocked, [→] revision

set -euo pipefail

PLAN_FILE=""
ACTION=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --plan)   PLAN_FILE="$2"; shift 2 ;;
        --action) ACTION="$2";    shift 2 ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

if [ -z "${PLAN_FILE}" ] || [ -z "${ACTION}" ]; then
    echo "Usage: resolve-dag.sh --plan <PATH> --action <ready|status|validate>" >&2
    exit 1
fi

if [ ! -f "${PLAN_FILE}" ]; then
    echo "ERROR: Plan file not found: ${PLAN_FILE}" >&2
    exit 1
fi

# ─── Parse task lines from plan.md ────────────────────────────────────────────
# Expected format:
#   - [ ] st-01 — Title text (assigned: @worker:domain)
#   - [x] st-02 — Title text (assigned: @worker:domain, depends: st-01)
# We extract: status_marker, task_id, title, assigned_worker, depends_list

parse_tasks() {
    # Match lines starting with "- [" followed by a status marker
    grep -E '^\s*- \[[ x~!→]\] ' "${PLAN_FILE}" | while IFS= read -r line; do
        # Extract status marker
        local marker
        marker=$(echo "$line" | sed -n 's/.*- \[\(.\)\].*/\1/p')

        # Map marker to status
        local status
        case "$marker" in
            ' ') status="pending" ;;
            '~') status="in_progress" ;;
            'x') status="completed" ;;
            '!') status="blocked" ;;
            '→') status="revision" ;;
            *)   status="unknown" ;;
        esac

        # Extract task ID (first word after the marker)
        local task_id
        task_id=$(echo "$line" | sed -n 's/.*- \[.\] \([a-zA-Z0-9_-]*\).*/\1/p')

        # Extract title (between task_id and the parenthetical)
        local title
        title=$(echo "$line" | sed -n 's/.*- \[.\] [a-zA-Z0-9_-]* — \(.*\) (assigned:.*/\1/p')
        [ -z "$title" ] && title=$(echo "$line" | sed -n 's/.*- \[.\] [a-zA-Z0-9_-]* — \(.*\)/\1/p')

        # Extract assigned worker (from "assigned: @worker:domain")
        local assigned
        assigned=$(echo "$line" | sed -n 's/.*assigned: @\([^:)]*\).*/\1/p')

        # Extract depends list (from "depends: st-01, st-02")
        local depends
        depends=$(echo "$line" | sed -n 's/.*depends: \([^)]*\).*/\1/p')

        # Output as JSON
        local depends_json="[]"
        if [ -n "$depends" ]; then
            depends_json=$(echo "$depends" | tr ',' '\n' | sed 's/^ *//;s/ *$//' | jq -R . | jq -s .)
        fi

        jq -n \
            --arg id "$task_id" \
            --arg title "$title" \
            --arg status "$status" \
            --arg assigned "$assigned" \
            --argjson depends "$depends_json" \
            '{id: $id, title: $title, status: $status, assigned: $assigned, depends: $depends}'
    done | jq -s '.'
}

# ─── Action: ready ────────────────────────────────────────────────────────────
# Find pending tasks whose dependencies are all completed

action_ready() {
    local tasks
    tasks=$(parse_tasks)

    # Get list of completed task IDs
    local completed_ids
    completed_ids=$(echo "$tasks" | jq -r '[.[] | select(.status == "completed") | .id]')

    # Find ready tasks: pending + all depends are in completed_ids
    local ready
    ready=$(echo "$tasks" | jq --argjson done "$completed_ids" '
        [.[] | select(.status == "pending") |
            select(.depends | length == 0 or (. as $deps | [$deps[] | select(. as $d | $done | index($d) | not)] | length == 0))]')

    # Find blocked tasks: pending + some depends not completed
    local blocked
    blocked=$(echo "$tasks" | jq --argjson done "$completed_ids" '
        [.[] | select(.status == "pending") |
            select(.depends | length > 0 and (. as $deps | [$deps[] | select(. as $d | $done | index($d) | not)] | length > 0)) |
            {id, blocked_by: [.depends[] | select(. as $d | $done | index($d) | not)]}]')

    # In-progress and completed
    local in_progress
    in_progress=$(echo "$tasks" | jq '[.[] | select(.status == "in_progress") | {id, title, assigned}]')
    local completed
    completed=$(echo "$tasks" | jq '[.[] | select(.status == "completed") | {id, title, assigned}]')

    jq -n \
        --argjson ready "$ready" \
        --argjson blocked "$blocked" \
        --argjson in_progress "$in_progress" \
        --argjson completed "$completed" \
        '{ready_tasks: $ready, blocked_tasks: $blocked, in_progress: $in_progress, completed: $completed}'
}

# ─── Action: status ───────────────────────────────────────────────────────────
# Full DAG status grouped by state

action_status() {
    local tasks
    tasks=$(parse_tasks)

    echo "$tasks" | jq '{
        pending:     [.[] | select(.status == "pending")],
        in_progress: [.[] | select(.status == "in_progress")],
        completed:   [.[] | select(.status == "completed")],
        blocked:     [.[] | select(.status == "blocked")],
        revision:    [.[] | select(.status == "revision")],
        total:       length
    }'
}

# ─── Action: validate ─────────────────────────────────────────────────────────
# Check for cycles using iterative topological sort (Kahn's algorithm)

action_validate() {
    local tasks
    tasks=$(parse_tasks)

    local total
    total=$(echo "$tasks" | jq 'length')

    if [ "$total" -eq 0 ]; then
        echo '{"valid": true, "message": "No tasks found in plan.md", "task_count": 0}'
        return 0
    fi

    # Check for unknown dependency references
    local missing
    missing=$(echo "$tasks" | jq -r '
        [.[] | .id] as $all_ids |
        [.[] | .depends[] | select(. as $d | $all_ids | index($d) | not)] | unique')
    if [ "$(echo "$missing" | jq 'length')" -gt 0 ]; then
        echo "$missing" | jq --argjson total "$total" \
            '{valid: false, message: "Unknown task IDs in depends: \(. | join(", "))", task_count: $total}'
        return 1
    fi

    # Kahn's algorithm via iterative shell loop
    # Build in-degree map and adjacency
    local in_degree_json
    in_degree_json=$(echo "$tasks" | jq '
        reduce .[] as $t ({};
            . as $acc |
            ($t.id) as $id |
            (if $acc[$id] then $acc else ($acc + {($id): 0}) end) as $acc |
            reduce ($t.depends[]) as $dep ($acc; . + {($id): (.[$id] + 1)})
        )')

    local visited=0
    local queue
    queue=$(echo "$in_degree_json" | jq -r '[to_entries[] | select(.value == 0) | .key] | .[]')

    while [ -n "$queue" ]; do
        local next_queue=""
        for node in $queue; do
            visited=$((visited + 1))
            # Find tasks that depend on this node, decrement their in-degree
            local dependents
            dependents=$(echo "$tasks" | jq -r --arg node "$node" \
                '[.[] | select(.depends | index($node)) | .id] | .[]')
            for dep in $dependents; do
                in_degree_json=$(echo "$in_degree_json" | jq --arg dep "$dep" \
                    '.[$dep] -= 1')
                local new_deg
                new_deg=$(echo "$in_degree_json" | jq -r --arg dep "$dep" '.[$dep]')
                if [ "$new_deg" -eq 0 ]; then
                    next_queue="$next_queue $dep"
                fi
            done
        done
        queue=$(echo "$next_queue" | xargs)
    done

    if [ "$visited" -eq "$total" ]; then
        jq -n --argjson total "$total" \
            '{valid: true, message: "DAG is valid — no cycles detected", task_count: $total}'
    else
        jq -n --argjson visited "$visited" --argjson total "$total" \
            '{valid: false, message: "Cycle detected in DAG! \($visited) of \($total) tasks are reachable", task_count: $total}'
        return 1
    fi
}

# ─── Dispatch ─────────────────────────────────────────────────────────────────

case "$ACTION" in
    ready)    action_ready ;;
    status)   action_status ;;
    validate) action_validate ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: ready, status, validate" >&2
        exit 1
        ;;
esac
