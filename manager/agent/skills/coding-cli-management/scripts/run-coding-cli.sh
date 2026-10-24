#!/bin/bash
# Execute an AI coding CLI tool in a given workspace
# Usage: run-coding-cli.sh --cli <tool> --workspace <dir> --prompt-file <file> [--timeout <secs>]
#        [--model <name>] [--allowed-tools <csv>] [--allowed-mcp <csv>] [--safe-mode] [--bare] [--json-file <path>]
#        [--approval-mode <tier>] [--max-wall-time <val>]
#        [--task-dir <dir>] [--processor <name>] [--run-id <id>] [--worker <name>]
# (governance flags apply to the qwen runner; other runners warn and ignore them)

set -e

cli=""
workspace=""
prompt_file=""
# Default wall-clock bound for an unattended run. Field data (delegated
# coding runs) shows hour-plus runs are normal, not the tail — size for the
# long tail, not the common case.
timeout_secs=3600
g_model=""
g_allowed_tools=""
g_allowed_mcp=""
g_safe_mode=0
g_bare=0
g_json_file=""
# Execution boundary (qwen only): approval tier (plan/default/auto-edit/auto/
# yolo) and native wall budget (qwen formats: 90/30s/5m/1h/1.5h). Both resolve
# per-run flag > config (approval_mode / max_wall_time) > default (yolo / unset).
g_approval_mode=""
g_max_wall_time=""
# .processing guard: task root (where the marker lives — see task-coordination
# create/check/remove-processing-marker.sh) and this run's processor identity.
# Ownership requires both the processor name and the unique run ID used when
# creating your marker. A shared processor name alone cannot identify your run.
g_task_dir=""
g_processor=""
g_run_id=""
# The worker this coding task serves (passed by the delegating agent, who
# knows it from the coding-request). Recorded in runs.jsonl only; the run's
# own container identity is the AGENTTEAMS_WORKER_NAME env var (runner).
g_worker=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --cli)         cli="$2";         shift 2 ;;
        --workspace)   workspace="$2";   shift 2 ;;
        --prompt-file) prompt_file="$2"; shift 2 ;;
        --timeout)     timeout_secs="$2"; shift 2 ;;
        --model)       g_model="$2";       shift 2 ;;
        --allowed-tools) g_allowed_tools="$2"; shift 2 ;;
        --allowed-mcp) g_allowed_mcp="$2"; shift 2 ;;
        --safe-mode)   g_safe_mode=1;      shift ;;
        --bare)        g_bare=1;           shift ;;
        --json-file)   g_json_file="$2";   shift 2 ;;
        --approval-mode) g_approval_mode="$2"; shift 2 ;;
        --max-wall-time) g_max_wall_time="$2"; shift 2 ;;
        --task-dir)    g_task_dir="$2";    shift 2 ;;
        --processor)   g_processor="$2";   shift 2 ;;
        --run-id)      g_run_id="$2";      shift 2 ;;
        --worker)      g_worker="$2";      shift 2 ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

if [ -z "$cli" ] || [ -z "$workspace" ] || [ -z "$prompt_file" ]; then
    echo "Usage: $0 --cli <tool> --workspace <dir> --prompt-file <file> [--timeout <secs>]" >&2
    exit 1
fi

if [ ! -f "$prompt_file" ]; then
    echo "Prompt file not found: $prompt_file" >&2
    exit 1
fi

if [ ! -d "$workspace" ]; then
    echo "Workspace directory not found: $workspace" >&2
    exit 1
fi

# ── The run's artifacts: one transcript + one record ────────────────────────
# The run is identified from here on: it owns a transcript (the log the CLI
# tees into, or the diagnostic prose when a pre-flight guard refuses the run)
# and one appended runs.jsonl line naming that transcript. EVERY exit path
# below finalizes both. The read API enumerates transcripts, so an outcome
# recorded in the index without a transcript would be invisible — a blocked
# attempt must leave a readable artifact, not just metadata.
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
log_dir="$workspace/coding-cli-logs"
if ! mkdir -p "$log_dir"; then
    echo "[run-coding-cli] ERROR: cannot create the run-log directory '${log_dir}' — no run was started." >&2
    exit 1
fi
runs_jsonl="$log_dir/runs.jsonl"

# claim_transcript fixes this run's transcript path. The create is exclusive
# (noclobber) so a transcript from the same wall-clock second is never
# clobbered — the run that holds the task is exactly the one a blocked
# attempt collides with. ONLY an existing name counts as a collision: the
# candidate then steps FORWARD one second at a time, so a burst of runs within
# one second each gets its own transcript. Any other create failure (read-only
# or unwritable directory, full disk) refuses the run at once with a visible
# diagnostic instead of retrying: a retry loop here would sit before the
# pre-flight guards and outside the CLI timeout, so --timeout could not bound
# it. The attempt cap is a safety net only (a monotonic step terminates on its
# own; the cap guards against a pathological clock).
claim_transcript() {
    local epoch attempts=0
    epoch=$(date +%s)
    timestamp=$(date +%Y%m%d-%H%M%S)
    while true; do
        run_name="run-${timestamp}.log"
        log_file="$log_dir/${run_name}"
        # The read API only enumerates run-YYYYMMDD-HHMMSS.log, so never claim
        # a name outside that shape: a clock/date that cannot produce one would
        # otherwise leave a transcript no reader (and no index lookup) can see.
        if [[ ! "${run_name}" =~ ^run-[0-9]{8}-[0-9]{6}\.log$ ]]; then
            echo "[run-coding-cli] ERROR: unusable run transcript name '${run_name}' — the clock/date" >&2
            echo "[run-coding-cli] produced a stamp that run-YYYYMMDD-HHMMSS.log cannot match, so no" >&2
            echo "[run-coding-cli] reader could enumerate it. No run was started." >&2
            exit 1
        fi
        if ( set -C; : > "$log_file" ) 2>/dev/null; then
            return 0
        fi
        if [ ! -e "$log_file" ]; then
            echo "[run-coding-cli] ERROR: cannot create the run transcript '${log_file}'." >&2
            echo "[run-coding-cli] The coding-cli-logs directory must be writable — a read-only" >&2
            echo "[run-coding-cli] filesystem, directory permissions or a full disk blocks this run." >&2
            echo "[run-coding-cli] No run was started; fix the directory and retry." >&2
            exit 1
        fi
        attempts=$(( attempts + 1 ))
        if [ "$attempts" -ge 64 ]; then
            echo "[run-coding-cli] ERROR: no free run transcript name in '${log_dir}' after" >&2
            echo "[run-coding-cli] ${attempts} same-second collisions — refusing to run." >&2
            exit 1
        fi
        epoch=$(( epoch + 1 ))
        timestamp=$(date -d "@${epoch}" +%Y%m%d-%H%M%S)
    done
}
claim_transcript

# Open the transcript with the run metadata header so the log itself records
# which workspace/prompt/timeout this run used — for every exit path below,
# a blocked attempt included.
echo "[run-coding-cli] cli=$cli workspace=$workspace prompt_file=$prompt_file timeout=${timeout_secs}s" | tee -a "$log_file"
echo "[run-coding-cli] Log: $log_file"

# The script's exit code -> the recorded status (the two-guard 125s are
# "blocked, retry later"; 124 is the outer timeout; anything else failed).
status_of_exit() {
    case "$1" in
        0)   echo "ok" ;;
        124) echo "timeout" ;;
        125) echo "blocked" ;;
        *)   echo "failed" ;;
    esac
}

# fail_run <status> <reason> <exit_code> <message line>...
# Finalize a refusing/failing exit: the diagnostic lands in the transcript AND
# on stderr (so the run explains itself both in storage and to the caller),
# then the record is appended and the script exits with that code.
fail_run() {
    local rec_status="$1" rec_reason="$2" rec_exit="$3"
    shift 3
    local line
    for line in "$@"; do
        # Best-effort: the run's exit code must not depend on the log write.
        echo "$line" | tee -a "$log_file" >&2 || true
    done
    append_run_record "$rec_status" "$rec_reason" "$rec_exit"
    exit "$rec_exit"
}

# append_run_record <status> <reason> <exit_code>
# Empty values are omitted (aligned with the API's omitempty contract).
# Best-effort: a metadata failure must never change the run's outcome, so
# every step degrades and the function always returns 0.
append_run_record() {
    local rec_status="$1" rec_reason="$2" rec_exit="$3"
    command -v python3 >/dev/null 2>&1 || return 0
    local ended_at log_bytes="" prompt_sha="" prompt_summary=""
    ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    if [ -f "$log_file" ]; then
        log_bytes=$(wc -c < "$log_file" 2>/dev/null | tr -d '[:space:]') || log_bytes=""
    fi
    if [ -f "$prompt_file" ]; then
        prompt_sha=$(sha256sum "$prompt_file" 2>/dev/null | awk '{print $1}') || prompt_sha=""
        prompt_summary=$(head -c 200 "$prompt_file" 2>/dev/null | tr '\n\r' '  ' | sed -e 's/[[:space:]]*$//') || prompt_summary=""
    fi
    RUN_REC_RUN="$run_name" RUN_REC_CLI="$cli" RUN_REC_WORKER="$g_worker" \
    RUN_REC_RUNNER="${AGENTTEAMS_WORKER_NAME:-}" RUN_REC_STATUS="$rec_status" \
    RUN_REC_REASON="$rec_reason" RUN_REC_EXIT_CODE="$rec_exit" \
    RUN_REC_TIMEOUT_SECS="$timeout_secs" RUN_REC_STARTED_AT="$started_at" \
    RUN_REC_ENDED_AT="$ended_at" RUN_REC_LOG_BYTES="$log_bytes" \
    RUN_REC_PROMPT_FILE="$prompt_file" RUN_REC_PROMPT_SHA256="$prompt_sha" \
    RUN_REC_PROMPT_SUMMARY="$prompt_summary" \
    python3 -c '
import json, os
e = os.environ
def opt(k):
    v = e.get("RUN_REC_" + k, "")
    return v if v != "" else None
rec = {"v": 1, "run": e["RUN_REC_RUN"], "cli": e["RUN_REC_CLI"]}
for k in ("WORKER", "RUNNER", "STATUS", "REASON"):
    v = opt(k)
    if v is not None:
        rec[k.lower()] = v
rec["exit_code"] = int(e["RUN_REC_EXIT_CODE"])
rec["timeout_secs"] = int(e["RUN_REC_TIMEOUT_SECS"])
rec["started_at"] = e["RUN_REC_STARTED_AT"]
rec["ended_at"] = e["RUN_REC_ENDED_AT"]
v = opt("LOG_BYTES")
if v is not None:
    rec["log_bytes"] = int(v)
for k in ("PROMPT_FILE", "PROMPT_SHA256", "PROMPT_SUMMARY"):
    v = opt(k)
    if v is not None:
        rec[k.lower()] = v
print(json.dumps(rec, ensure_ascii=False))
' >> "$runs_jsonl" 2>/dev/null || true
    return 0
}

# Pre-flight guard: a .processing marker at the TASK ROOT means another
# operation holds the task. The marker lives at <task-root>/.processing (see
# task-coordination create/check/remove-processing-marker.sh) — NOT in the
# workspace (the workspace is <task-root>/workspace), so we must check the
# task root. You can pass through your own marker only when BOTH the processor
# and its non-empty run ID match. Active legacy markers without run IDs also
# block execution. An expired marker is ignored (matches check-processing-marker.sh
# TTL). Prompt discipline drops (small models skip the SKILL's check — the
# 10-10 QC git-write incident is the standing evidence), so this hard-fails in
# code before the CLI is invoked: exit 125 is the "blocked, retry later"
# contract (same code as the missing-CLI guard).
task_dir="${g_task_dir:-$(dirname "$workspace")}"
marker_file="${task_dir}/.processing"
if [ -f "$marker_file" ]; then
    marker_processor=$(jq -r '.processor // "unknown"' "$marker_file" 2>/dev/null || echo "unknown")
    marker_run_id=$(jq -r '.run_id // empty' "$marker_file" 2>/dev/null || true)
    marker_expires=$(jq -r '.expires_at // empty' "$marker_file" 2>/dev/null || true)
    marker_expired=false
    if [ -n "$marker_expires" ]; then
        if exp_epoch=$(date -d "$marker_expires" +%s 2>/dev/null); then
            [ "$(date -u +%s)" -ge "$exp_epoch" ] && marker_expired=true
        fi
    fi
    if ! $marker_expired && { [ -z "$g_processor" ] || [ "$marker_processor" != "$g_processor" ] || [ -z "$g_run_id" ] || [ "$marker_run_id" != "$g_run_id" ]; }; then
        fail_run blocked marker-conflict 125 \
            "[run-coding-cli] ERROR: ${marker_file} is held by '${marker_processor}'" \
            "[run-coding-cli] and does not match this run's ownership — a conflicting" \
            "[run-coding-cli] operation is in progress. Refusing to run; retry after the holder" \
            "[run-coding-cli] removes the marker or it expires."
    fi
    # Expired marker, or matching processor AND run ID -> proceed.
fi

# Pre-flight: the configured CLI binary must be on PATH. If it is missing here
# even though the config says delegation is enabled, the most common cause is a
# worker RECREATION: the CLI is installed into the container's ephemeral layer
# (npm install -g, i.e. the image layer at $npm prefix), which container
# recreation wipes, while the auth config in the persistent agent home
# (~/.qwen/settings.json) survives. Fail fast with an actionable message
# instead of a confusing "command not found" deep inside the run.
if ! command -v "$cli" >/dev/null 2>&1; then
    fail_run blocked cli-missing 125 \
        "[run-coding-cli] ERROR: coding CLI '$cli' is configured but not on PATH." \
        "[run-coding-cli] Usual cause: the worker was recreated and the CLI install" \
        "[run-coding-cli] (npm -g, container ephemeral layer) was wiped. Re-run the install" \
        "[run-coding-cli] endpoint (POST /api/v1/workers/{name}/coding-cli/$cli/install)," \
        "[run-coding-cli] then retry. Auth settings are written container-local by that" \
        "[run-coding-cli] surface and may not survive a recreation — probe settings and" \
        "[run-coding-cli] re-apply via the settings endpoint if the probe shows them missing."
fi

# Governance flags are defined by the qwen runner; other runners have no
# equivalent surface, so warn and ignore rather than fail the run.
if [ "$cli" != "qwen" ] && { [ -n "$g_model" ] || [ -n "$g_allowed_tools" ] || [ -n "$g_allowed_mcp" ] || [ "$g_safe_mode" = "1" ] || [ "$g_bare" = "1" ] || [ -n "$g_json_file" ] || [ -n "$g_approval_mode" ] || [ -n "$g_max_wall_time" ]; }; then
    echo "[run-coding-cli] note: governance flags (--model/--allowed-tools/--allowed-mcp/--safe-mode/--bare/--json-file/--approval-mode/--max-wall-time) are qwen-specific and were ignored for cli=$cli" >&2
fi

# Optional per-runner budget/boundary fields from the delegating agent's
# config (default ~/coding-cli-config.json, override with CODING_CLI_CONFIG
# for tests). Only the qwen case consumes them today; absent/0/false -> no
# extra flags.
config_file="${CODING_CLI_CONFIG:-${HOME}/coding-cli-config.json}"
max_session_turns=""
sandbox_setting=""
approval_mode_setting=""
max_wall_time_setting=""
if command -v jq >/dev/null 2>&1 && [ -f "${config_file}" ]; then
    max_session_turns=$(jq -r '.max_session_turns // empty' "${config_file}" 2>/dev/null || true)
    sandbox_setting=$(jq -r '.sandbox // empty' "${config_file}" 2>/dev/null || true)
    approval_mode_setting=$(jq -r '.approval_mode // empty' "${config_file}" 2>/dev/null || true)
    max_wall_time_setting=$(jq -r '.max_wall_time // empty' "${config_file}" 2>/dev/null || true)
fi

cd "$workspace"

case "$cli" in
    claude)
        timeout "$timeout_secs" claude -p "$(cat "$prompt_file")" \
            --dangerously-skip-permissions --output-format text 2>&1 | tee -a "$log_file"
        exit_code=${PIPESTATUS[0]}
        ;;
    gemini)
        timeout "$timeout_secs" gemini -p "$(cat "$prompt_file")" -y 2>&1 | tee -a "$log_file"
        exit_code=${PIPESTATUS[0]}
        ;;
    qodercli)
        timeout "$timeout_secs" qodercli -p "$(cat "$prompt_file")" \
            --yolo -w "$workspace" 2>&1 | tee -a "$log_file"
        exit_code=${PIPESTATUS[0]}
        ;;
    qwen)
        # qwen has no workspace flag (verified through qwen 0.25.0); its workspace is the
        # process cwd, so pin the run to $workspace explicitly.
        cd "$workspace"
        # Execution boundary (see SKILL.md "Execution boundary"): approval
        # tier and native wall budget, each resolving per-run flag > config
        # field > default. Defaults are byte-compatible with the pre-boundary
        # wrapper (yolo, no wall flag), so an unchanged caller gets an
        # identical qwen argv. Only yolo and plan are usable in a
        # non-interactive one-shot run — default/auto-edit/auto route
        # would-ask calls to an approver that is never present headless, so
        # the run would stall; they are deliberately not a default here.
        approval_mode="${g_approval_mode:-${approval_mode_setting}}"
        if [ -z "${approval_mode}" ]; then
            approval_mode="yolo"
        fi
        case "${approval_mode}" in
            plan|default|auto-edit|auto|yolo) ;;
            *)
                if [ -n "${g_approval_mode}" ]; then
                    approval_source="[run-coding-cli] source: --approval-mode"
                else
                    approval_source="[run-coding-cli] source: approval_mode in ${config_file}"
                fi
                fail_run failed invalid-approval-mode 1 \
                    "[run-coding-cli] ERROR: invalid approval mode '${approval_mode}'" \
                    "${approval_source}" \
                    "[run-coding-cli] Valid values: plan, default, auto-edit, auto, yolo"
                ;;
        esac
        max_wall_time="${g_max_wall_time:-${max_wall_time_setting}}"
        # Optional budget flags from the config (see "Config File" in
        # SKILL.md): max_session_turns -> --max-session-turns N (absent/0
        # = omit); sandbox: true -> --sandbox (Docker-backed, requires
        # docker at the execution site). No other runner consumes these.
        qwen_args=()
        if [ "${approval_mode}" = "yolo" ]; then
            qwen_args+=(--yolo)
        else
            qwen_args+=(--approval-mode "${approval_mode}")
        fi
        # Native wall budget (qwen value formats: 90/30s/5m/1h/1.5h); an
        # over-budget qwen run aborts with exit 55. The outer --timeout above
        # remains the hard backstop for every runner.
        if [ -n "${max_wall_time}" ]; then
            qwen_args+=(--max-wall-time "${max_wall_time}")
        fi
        if [ -n "${max_session_turns}" ] && [ "${max_session_turns}" != "0" ]; then
            qwen_args+=(--max-session-turns "${max_session_turns}")
        fi
        if [ "${sandbox_setting}" = "true" ]; then
            qwen_args+=(--sandbox)
        fi
        # Governance passthrough (delegating agent's per-run control surface):
        # --model <name>       per-run model override (two-tier model control)
        # --allowed-tools <csv> tool allowlist (code-level permission boundary)
        # --allowed-mcp <csv>   MCP server allowlist (scenario isolation)
        # --safe-mode / --bare  clean / minimal config loading
        # (--approval-mode / --max-wall-time are resolved above — they carry
        #  config-backed defaults, so they are not plain passthroughs.)
        if [ -n "${g_model}" ]; then
            qwen_args+=(-m "${g_model}")
        fi
        if [ -n "${g_allowed_tools}" ]; then
            qwen_args+=(--allowed-tools "${g_allowed_tools}")
        fi
        if [ -n "${g_allowed_mcp}" ]; then
            qwen_args+=(--allowed-mcp-server-names "${g_allowed_mcp}")
        fi
        if [ "${g_safe_mode}" = "1" ]; then
            qwen_args+=(--safe-mode)
        fi
        if [ "${g_bare}" = "1" ]; then
            qwen_args+=(--bare)
        fi
        # Optional structured-output passthrough (qwen --json-file <path>):
        # writes the final result JSON to <path> for machine audit. Off by
        # default, so the raw stream remains the audit source of truth.
        if [ -n "${g_json_file}" ]; then
            qwen_args+=(--json-file "${g_json_file}")
        fi
        # Governance visibility (code-level signal, NOT a hard gate): an
        # unattended qwen run that has none of a turn budget / sandbox / tool
        # allowlist is "ungoverned" — all tools, no container isolation, bounded
        # only by the wall-clock timeout above. Surface it in the run log (the
        # durable record) so the risk is visible even if the caller never set
        # the config. Tighten via ${config_file} (max_session_turns / sandbox)
        # or the per-run flags above. A bare run stays a deliberate option —
        # this only makes it visible, it never blocks it.
        ungoverned=1
        if [ -n "${max_session_turns}" ] && [ "${max_session_turns}" != "0" ]; then ungoverned=0; fi
        if [ "${sandbox_setting}" = "true" ]; then ungoverned=0; fi
        if [ -n "${g_allowed_tools}" ]; then ungoverned=0; fi
        # The read-only `plan` tier is governance in its own right: a plan run
        # neither mutates files nor executes tools, so the unattended run is
        # bounded even without a turn/sandbox/allowlist budget.
        if [ "${approval_mode}" = "plan" ]; then ungoverned=0; fi
        if [ "$ungoverned" = "1" ]; then
            echo "[run-coding-cli] WARNING: unattended qwen run is UNGOVERNED (no turn budget, no --sandbox, no tool allowlist) — bounded only by the ${timeout_secs}s wall-clock timeout. Tighten via ${config_file} (max_session_turns/sandbox) or --allowed-tools." | tee -a "$log_file"
        fi
        timeout "$timeout_secs" qwen "${qwen_args[@]}" "$(cat "$prompt_file")" 2>&1 | tee -a "$log_file"
        exit_code=${PIPESTATUS[0]}
        ;;
    opencode)
        # `opencode run --dir <path>` is the native working-directory flag
        # (opencode.ai/docs/cli); pin the run to $workspace explicitly.
        cd "$workspace"
        timeout "$timeout_secs" opencode run --auto --dir "$workspace" "$(cat "$prompt_file")" 2>&1 | tee -a "$log_file"
        exit_code=${PIPESTATUS[0]}
        ;;
    *)
        fail_run failed unknown-cli 1 \
            "Unknown CLI tool: $cli. Supported: claude, gemini, qodercli, qwen, opencode"
        ;;
esac

if [ "$exit_code" -eq 124 ]; then
    echo "[run-coding-cli] TIMEOUT after ${timeout_secs}s" | tee -a "$log_file"
    append_run_record timeout "" 124
    exit 124
fi

echo "[run-coding-cli] Finished with exit code $exit_code" | tee -a "$log_file"
append_run_record "$(status_of_exit "$exit_code")" "" "$exit_code"
exit "$exit_code"
