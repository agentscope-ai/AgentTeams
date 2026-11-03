#!/usr/bin/env bash
# ============================================================================
# upgrade_qwenpaw_runtime.sh — in-place QwenPaw runtime upgrade, v1
#
# Companion to check_qwenpaw_upgrade.py (pre-flight, checks C1-C4): the
# checker answers "can this deployment take version X?", this script answers
# "do it, with a rollback path" — the S3-S6 steps of the in-place upgrade
# lifecycle proposed in AgentTeams#1341 (end-to-end lifecycle comment,
# 2026-10-11).
#
# Phases (--phase, default full = the original single-process flow):
#   full         S0 -> S3 -> S3b -> S4 -> S5 -> S6 in one process
#   pre-restart  phase 1 of the two-phase model (the agentteams-upgrade
#                plugin spawns it DETACHED so it survives the app kill):
#                S0 full checks (C3 mandatory via --wheelhouse) -> S1 old
#                version -> S3 install -> S3b patch -> write pending.json
#                BEFORE the swap becomes visible -> S4 restart primitive ->
#                exit 0. Completion is owned by phase 2 (state file).
#   boot-check   phase 2 (entrypoint, before `exec qwenpaw-worker`; the app
#                is not up yet): no pending.json -> no-op exit 0; venv
#                version == target -> intermediate state boot-check-passed,
#                pending RETAINED (the app is not up yet, so this is only a
#                pre-check); otherwise reinstall old + re-apply patch, keep
#                pending with `expect=old` and park the job in the
#                NON-TERMINAL state `recovering` — the restored runtime
#                cannot be verified before boot, so the entrypoint's
#                post-start gate verifies the OLD version and only then
#                finalizes rolled_back. Crash-loop guard: if pending already
#                carries expect=old and the venv already holds the old
#                version, the reinstall is skipped (no pip in a crash
#                loop). Always exits 0 once handled (the container must
#                boot; the result lives in the state file).
#                --force-rollback skips the version check (host-side
#                runbook).
#   post-start   phase 3 (entrypoint, AFTER the app is up): the real
#                arbiter — health gate on the running app (version /
#                readiness / settle window). Pass -> pending removed, state
#                success, exit 0 (keep the new runtime). Fail -> rollback
#                (reinstall old + patch + restart): the job goes to the
#                NON-TERMINAL state `recovering` (the upgrade mutex stays
#                engaged) and only a PASSED verification of the restored
#                runtime finalizes it — supervisorctl mode re-verifies
#                in-process (rolled_back + pending removed, exit 3); kill
#                mode (worker images have no supervisor) keeps pending with
#                `expect=old` and exits 3 as a HANDOFF: the entrypoint
#                relaunches the old runtime and re-enters post-start, where
#                the `expect=old` branch health-gates the OLD version and
#                finalizes rolled_back (exit 0) or fails with
#                rollback_failed + pending kept (exit 4).
#                exit 0 = verified (keep the current runtime); 3 = restore
#                done, verification pending / in-process verified;
#                4 = FATAL restore/verification failure (pending kept,
#                manual intervention). The entrypoint passes only
#                --pending-file; the job context (job id + state file) is
#                recovered from pending.json.
#
# Steps (full phase):
#   S0  pre-flight   external checker (default: the co-located
#                    check_qwenpaw_upgrade.py); nonzero exit aborts before
#                    anything is installed (exit 2)
#   S3  package swap surgical reinstall into the runtime venv: uv
#                    `--reinstall-package qwenpaw` when uv is available, else
#                    venv pip `--force-reinstall --no-deps` (AgentTeams
#                    worker images ship no uv)
#   S4  restart      backend-specific primitive (see backends below); a
#                    failed restart is not rolled back immediately — the
#                    health gate below is the arbiter
#   S5  health gate  poll until the reported version equals the target
#                    (timeout Ns); on first match, hold a settle window
#                    (--settle, default 65s) verifying the runtime stays
#                    alive and the version stays put, to catch a delayed
#                    wrapper crash the raw version endpoint would not reveal;
#                    timeout/mismatch/settle-death triggers rollback
#   S6  rollback     install the recorded old version -> restart -> verify
#                    the old version is ready; success exit 3, failure exit 4
#                    with a loud FATAL banner (manual intervention)
#   S2' record       optional --record-file: JSON written ONLY after S5 is
#                    fully green ("verified, then recorded"); on rollback the
#                    record is written at the (restored) old version
#
# Backends — the actual restart primitive differs by deployment:
#   in-container (default): run this script inside the container. For images
#     whose supervisor respawns the app (QwenPaw runtime image: supervisord,
#     measured ~7-9s to ready): `supervisorctl restart app` (default) or
#     --restart-mode kill. Venv default: /app/venv.
#   docker (--container NAME): run on the host. The AgentTeams worker/manager
#     images exec the runtime as PID1 with no supervisor, so the restart
#     primitive is `docker restart NAME`: it keeps the writable layer (the
#     in-place swap survives) and re-runs the full init chain including the
#     MinIO config re-mirror (measured 30-90s to ready). Venv default:
#     /opt/venv/qwenpaw.
#
# Out of scope in v1 (tracked in the #1341 lifecycle design): S1 quiesce
# (stop dispatching before the upgrade — operator/Leader duty in v1), the
# MinIO-persisted applied-runtime record with a boot-time version guard
# (v1 keeps the record local via --record-file), and the
# k8s/sandbox OpenKruise spec.image in-place path (there the image update
# subsumes the package swap).
#
# Exit codes: 0=ok  2=pre-flight/usage failure (nothing installed)
#             3=rolled back  4=rollback failed (manual intervention needed)
#             (boot-check: always 0 once handled; the result is in the
#              state file; post-start: 0=verified, keep the current runtime
#              / 3=restore done, awaiting relaunch+verification (kill mode)
#              or already in-process verified (supervisorctl)
#              / 4=restore and/or its verification failed — the entrypoint
#              branches on this verdict)
#
# Usage:
#   upgrade_qwenpaw_runtime.sh --target X.Y.Z [--checker PATH] [--skip-check]
#       [--container NAME] [--venv PATH] [--uv PATH] [--restart-mode supervisorctl|kill]
#       [--timeout 90] [--settle 65] [--record-file PATH] [--dry-run] [--selftest]
#   # two-phase (agentteams-upgrade plugin / entrypoint):
#   upgrade_qwenpaw_runtime.sh --target X.Y.Z --phase pre-restart \
#       --job-id upg-... --state-file .../upgrade-<job-id>.json \
#       --pending-file .../pending.json
#   upgrade_qwenpaw_runtime.sh --phase boot-check --pending-file .../pending.json \
#       --venv /opt/venv/qwenpaw [--force-rollback]
#   upgrade_qwenpaw_runtime.sh --phase post-start --pending-file .../pending.json \
#       --venv /opt/venv/qwenpaw --restart-mode kill
#
# Environment hooks (all overridable; --selftest stubs every external action):
#   UG_VERSION_CMD   print the current version (raw "X.Y.Z" or the JSON
#                    {"version":"X"} form returned by /api/version; both are
#                    handled)
#   UG_INSTALL_CMD   install command with a {target} placeholder
#   UG_RESTART_CMD   restart command
#   UG_CHECKER_CMD   checker command with a {target} placeholder
#   UG_ALIVE_CMD     liveness probe (docker backend: prints the container
#                    Running state); used by the S5 settle window to catch a
#                    delayed wrapper crash that the raw version endpoint
#                    (served by the app subprocess) would not reveal
#   UG_PRE_INSTALL_CMD pre-install hook (docker backend: starts a stopped
#                    container, since the rollback's `docker exec ... pip
#                    install` needs a running container; no-op in-container)
#   UG_WHEELHOUSE    wheel cache dir for the pre-restart phase's mandatory
#                    C3 gate (default $QWENPAW_WORKING_DIR/upgrade/wheelhouse)
#   UG_TIMEOUT       health-gate timeout seconds (default 90); the entrypoint
#                    path passes no --timeout, so this env is the test/ops
#                    knob for that call
#   UG_SETTLE        settle window seconds (default 65; 0 disables); same
#                    test/ops role as UG_TIMEOUT
#
# Red line: --selftest runs a fully stubbed battery and never touches a real
# deployment; --dry-run prints the resolved commands without executing them.
# ============================================================================
set -u

VERSION_ENDPOINT="http://127.0.0.1:8088/api/version"

# ---------- environment hooks (caller values are kept; real defaults resolved in resolve_defaults) ----------
UG_VERSION_CMD="${UG_VERSION_CMD:-}"
UG_INSTALL_CMD="${UG_INSTALL_CMD:-}"
UG_PATCH_CMD="${UG_PATCH_CMD:-}"
UG_RESTART_CMD="${UG_RESTART_CMD:-}"
UG_CHECKER_CMD="${UG_CHECKER_CMD:-}"
UG_VENV="${UG_VENV:-}"
UG_UV="${UG_UV:-}"
UG_ALIVE_CMD="${UG_ALIVE_CMD:-}"
UG_PRE_INSTALL_CMD="${UG_PRE_INSTALL_CMD:-}"

# ---------- CLI state ----------
TARGET=""
CHECKER=""
SKIP_CHECK=0
CONTAINER=""
VENV=""
RESTART_MODE="supervisorctl"
# UG_TIMEOUT / UG_SETTLE are env-level defaults (same family as
# UG_SETTLE_POLL): explicit --timeout/--settle flags still win in parse_args.
TIMEOUT="${UG_TIMEOUT:-90}"
SETTLE="${UG_SETTLE:-65}"
SETTLE_POLL="${UG_SETTLE_POLL:-5}"
RECORD_FILE=""
DRY_RUN=0
SELFTEST=0
BACKEND="in-container"
PHASE="full"
JOB_ID=""
STATE_FILE=""
PENDING_FILE=""
FORCE_ROLLBACK=0
WHEELHOUSE_DIR=""

log() { printf '[upgrade] %s\n' "$*"; }
err() { printf '[upgrade] ERROR: %s\n' "$*" >&2; }

trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# Emit a version string from raw output:
#   bare version ("2.2.2b4") -> as-is
#   JSON form ('{"version":"2.2.2b4"}', the shape /api/version returns) -> extract the field
extract_version() {
  local raw="$1" v
  case "$raw" in
    \{*)
      if command -v python3 >/dev/null 2>&1; then
        v="$(printf '%s' "$raw" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
if isinstance(d, dict) and isinstance(d.get("version"), str):
    print(d["version"])
else:
    sys.exit(1)' 2>/dev/null)" || v=""
        if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
      fi
      ;;
  esac
  printf '%s' "$raw"
}

# Run UG_VERSION_CMD and return the version string; failure/empty output -> nonzero
get_version() {
  local out
  out="$(bash -c "$UG_VERSION_CMD" 2>/dev/null)" || return 1
  out="$(trim "$out")"
  [ -n "$out" ] || return 1
  extract_version "$out"
  return 0
}

# $1=command template  $2=version -> print with every {target} substituted
sub_target() {
  printf '%s' "${1//\{target\}/$2}"
}

now_ts() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

# --- state file: single source of truth across container restarts ---------
# write_state KEY=VALUE ... merges JSON values into $STATE_FILE (atomic
# replace). A value is stored as JSON when parseable, else as a string.
# No-op when STATE_FILE is empty (the full phase keeps its record.json
# behaviour and is untouched).
write_state() {
  [ -n "$STATE_FILE" ] || return 0
  [ $# -gt 0 ] || return 0
  python3 - "$STATE_FILE" "$@" <<'PYEOF'
import json, os, sys, tempfile
path = sys.argv[1]
data = {}
if os.path.isfile(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            loaded = json.load(fh)
        if isinstance(loaded, dict):
            data = loaded
    except Exception:
        data = {}
for arg in sys.argv[2:]:
    if "=" not in arg:
        continue
    key, value = arg.split("=", 1)
    try:
        data[key] = json.loads(value)
    except Exception:
        data[key] = value
directory = os.path.dirname(os.path.abspath(path))
os.makedirs(directory, exist_ok=True)
fd, tmp = tempfile.mkstemp(prefix=".upgrade-", dir=directory)
with os.fdopen(fd, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
os.replace(tmp, path)
PYEOF
}

# pending_get KEY -> print the value from $PENDING_FILE (empty when absent)
pending_get() {
  python3 - "$PENDING_FILE" "$1" <<'PYEOF'
import json, sys
try:
    with open(sys.argv[1], "r", encoding="utf-8") as fh:
        data = json.load(fh)
    value = data.get(sys.argv[2], "")
    print("" if value is None else value)
except Exception:
    print("")
PYEOF
}

# pending_set KEY VALUE -> merge one JSON value into $PENDING_FILE (atomic
# replace). The rollback paths use it to add expect=old without rewriting
# the whole pending record (job_id/old/target/venv/state_file stay intact).
pending_set() {
  python3 - "$PENDING_FILE" "$1" "$2" <<'PYEOF'
import json, os, sys, tempfile
path, key, value = sys.argv[1], sys.argv[2], sys.argv[3]
data = {}
if os.path.isfile(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            loaded = json.load(fh)
        if isinstance(loaded, dict):
            data = loaded
    except Exception:
        data = {}
try:
    data[key] = json.loads(value)
except Exception:
    data[key] = value
directory = os.path.dirname(os.path.abspath(path))
os.makedirs(directory, exist_ok=True)
fd, tmp = tempfile.mkstemp(prefix=".pending-", dir=directory)
with os.fdopen(fd, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
os.replace(tmp, path)
PYEOF
}

# Recover the job context from pending.json (JOB_ID + STATE_FILE). The
# entrypoint invokes the phase-2 phases with ONLY --venv and --pending-file,
# so without this their write_state calls no-op (STATE_FILE empty) and the
# job is stranded at its last written state (boot-check-passed) — the state
# file is the single source of truth and must close to a terminal state.
# STATE_FILE resolution: explicit pending field (written by pre-restart) >
# sibling convention (pending and the job state live in the same
# $WORKING/upgrade dir; also covers pending files written before the
# state_file field existed).
resolve_pending_context() {
  [ -n "$JOB_ID" ] || JOB_ID="$(pending_get job_id)"
  if [ -z "$STATE_FILE" ] && [ -n "$JOB_ID" ]; then
    STATE_FILE="$(pending_get state_file)"
  fi
  if [ -z "$STATE_FILE" ] && [ -n "$JOB_ID" ]; then
    STATE_FILE="$(dirname "$PENDING_FILE")/upgrade-$JOB_ID.json"
  fi
}

# write_pending JOB_ID OLD TARGET VENV [STATE_FILE] -> atomic pending.json.
# state_file travels in pending so the entrypoint-style phase-2 calls
# (--pending-file only, no --job-id/--state-file) can close the job state
# instead of no-op'ing write_state.
write_pending() {
  python3 - "$PENDING_FILE" "$1" "$2" "$3" "$4" "${5:-}" <<'PYEOF'
import datetime, json, os, sys, tempfile
job_id, old, target, venv, state_file = sys.argv[2:7]
data = {
    "job_id": job_id,
    "old": old,
    "target": target,
    "venv": venv,
    "state_file": state_file,
    "started_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}
path = sys.argv[1]
directory = os.path.dirname(os.path.abspath(path))
os.makedirs(directory, exist_ok=True)
fd, tmp = tempfile.mkstemp(prefix=".pending-", dir=directory)
with os.fdopen(fd, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2, ensure_ascii=False)
    fh.write("\n")
os.replace(tmp, path)
PYEOF
}

run_install() {
  local t="$1" cmd
  if [ -n "$UG_PRE_INSTALL_CMD" ]; then
    if ! bash -c "$UG_PRE_INSTALL_CMD"; then
      err "pre-install hook failed: $UG_PRE_INSTALL_CMD"
      return 1
    fi
  fi
  cmd="$(sub_target "$UG_INSTALL_CMD" "$t")"
  log "install: $cmd"
  bash -c "$cmd"
}

run_restart() {
  log "restart: $UG_RESTART_CMD"
  bash -c "$UG_RESTART_CMD"
}

# $1=expected version; 1 when the runtime is still alive (and version-stable)
# for the whole settle window. Polls every 5s. The wrapper's own version gate
# (qwenpaw_worker._wait_for_qwenpaw_api, 60s deadline) can kill the runtime
# AFTER the app already reports the right version, so the raw version endpoint
# alone is not a liveness proof - this window is.
settle_window() {
  local want="$1" settle="$2" now deadline v alive
  now="$(date +%s)"
  deadline=$(( now + settle ))
  log "health gate: settle window ${settle}s (detecting delayed wrapper crash)"
  while :; do
    sleep "$SETTLE_POLL"
    if [ -n "$UG_ALIVE_CMD" ]; then
      if ! alive="$(bash -c "$UG_ALIVE_CMD" 2>/dev/null)"; then
        log "health gate: settle: liveness probe failed (runtime gone)"
        return 1
      fi
      case "$alive" in
        true|True|TRUE|1|yes|Yes|running|Running) ;;
        *) log "health gate: settle: runtime not alive (state='$alive')"; return 1 ;;
      esac
    fi
    if ! v="$(get_version)"; then
      log "health gate: settle: version probe failed (runtime gone)"
      return 1
    fi
    if [ "$v" != "$want" ]; then
      log "health gate: settle: version drift '$v' != '$want'"
      return 1
    fi
    now="$(date +%s)"
    [ "$now" -ge "$deadline" ] && break
  done
  log "health gate: settle window ${settle}s held - runtime stable"
  return 0
}

# $1=expected version  $2=timeout seconds  $3=settle seconds (0 disables the window)
health_gate() {
  local want="$1" limit="$2" settle="${3:-0}" now deadline v last=""
  now="$(date +%s)"
  deadline=$(( now + limit ))
  log "health gate: waiting for version == '$want' (timeout ${limit}s)"
  while :; do
    if v="$(get_version)"; then
      last="$v"
      if [ "$v" = "$want" ]; then
        log "health gate: version='$v' matched"
        if [ "$settle" -gt 0 ] && ! settle_window "$want" "$settle"; then
          return 1
        fi
        log "health gate: version='$v' OK"
        return 0
      fi
    fi
    now="$(date +%s)"
    [ "$now" -ge "$deadline" ] && break
    sleep 1
  done
  log "health gate: timeout ${limit}s (want='$want', last='${last:-<unavailable>}')"
  return 1
}

fatal_rollback_failed() {
  local line="============================================================"
  {
    printf '\n%s\n' "$line"
    printf '!! FATAL: ROLLBACK FAILED - qwenpaw may be left in a broken state !!\n'
    printf '!! Manual intervention required: inspect the runtime venv, reinstall the old version by hand, restart, and re-check /api/version !!\n'
    printf '%s\n\n' "$line"
  } >&2
}

# $1=status (ok|rolledback)  $2=applied_version; written only when --record-file is set.
# Ordering discipline: called only after all-green (or after rollback re-verified all-green).
write_record() {
  [ -n "$RECORD_FILE" ] || return 0
  local ts
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if ! printf '{"status":"%s","applied_version":"%s","target_version":"%s","backend":"%s","recorded_at":"%s"}\n' \
    "$1" "$2" "$TARGET" "$BACKEND" "$ts" > "$RECORD_FILE"; then
    err "WARNING: record file write failed: $RECORD_FILE (does not affect the upgrade result; record it manually)"
  else
    log "record: $RECORD_FILE (verified, then recorded)"
  fi
}

# $1=previous version; does not return (exits 3/4 internally)
do_rollback() {
  local old="$1"
  log "ROLLBACK: restoring $old (target was $TARGET)"
  if ! run_install "$old"; then
    fatal_rollback_failed
    err "rollback failed: installing old version $old did not succeed"
    echo "SUMMARY: status=rollback-failed"
    exit 4
  fi
  if ! run_restart; then
    fatal_rollback_failed
    err "rollback failed: post-rollback restart did not succeed"
    echo "SUMMARY: status=rollback-failed"
    exit 4
  fi
  if ! health_gate "$old" "$TIMEOUT" "$SETTLE"; then
    fatal_rollback_failed
    err "rollback failed: post-rollback health gate not reached (want='$old')"
    echo "SUMMARY: status=rollback-failed"
    exit 4
  fi
  log "ROLLBACK: restored to $old"
  write_record "rolledback" "$old"
  echo "SUMMARY: status=rolledback old=$old target=$TARGET"
  exit 3
}

# Main upgrade flow (exits 0/2/3/4 internally)
do_upgrade() {
  local start_ts old elapsed ccmd
  start_ts="$(date +%s)"

  # 1) record the previous version
  if ! old="$(get_version)"; then
    err "could not read the current version (UG_VERSION_CMD failed or printed nothing)"
    echo "SUMMARY: status=aborted reason=old-version-unknown"
    exit 2
  fi
  log "previous version: $old"
  log "target version: $TARGET"

  # 2) compatibility checker (S0)
  if [ -n "$CHECKER" ] && [ "$SKIP_CHECK" -eq 0 ]; then
    ccmd="$(sub_target "$UG_CHECKER_CMD" "$TARGET")"
    log "checker: $ccmd"
    if ! bash -c "$ccmd"; then
      err "checker refused the upgrade (nonzero exit) - aborted, nothing was installed"
      echo "SUMMARY: status=aborted reason=checker-failed"
      exit 2
    fi
    log "checker passed"
  fi

  # 3) install the target
  if ! run_install "$TARGET"; then
    err "install of $TARGET failed -> rolling back"
    do_rollback "$old"
  fi

  # 3b) apply compatibility patches to the freshly-installed runtime.
  #      The in-place swap installs a pristine wheel, which does not carry the
  #      driver-reload compat fix the 2.2.1 image bakes at build time; re-apply
  #      it so the upgraded runtime keeps the fixed behavior. The patch is a
  #      no-op when the target already has the fix integrated upstream.
  if [ -n "$UG_PATCH_CMD" ]; then
    pcmd="$(sub_target "$UG_PATCH_CMD" "$TARGET")"
    log "compat patch: $pcmd"
    if ! bash -c "$pcmd"; then
      err "compat patch failed -> rolling back"
      do_rollback "$old"
    fi
    log "compat patch done"
  fi

  # 4) restart (failure does not roll back directly; the health gate below decides)
  if ! run_restart; then
    err "restart command failed (no immediate rollback; the health gate below decides)"
  fi

  # 5) health gate
  if ! health_gate "$TARGET" "$TIMEOUT" "$SETTLE"; then
    do_rollback "$old"
  fi

  # 7) success
  elapsed=$(( $(date +%s) - start_ts ))
  write_record "ok" "$TARGET"
  echo "SUMMARY: status=ok old=$old target=$TARGET elapsed=${elapsed}s"
  exit 0
}

# ============================================================================
# Two-phase execution (agentteams-upgrade plugin / entrypoint).
# The full phase above is untouched; these reuse run_install / write_record
# / do_rollback-style steps but NOT run_restart+health_gate (boot-check) or
# S5 (pre-restart defers to phase 2).
# ============================================================================

# Phase 1 (spawned DETACHED by the plugin so it survives the app kill in S4):
#   S0 full checks (C3 mandatory via --wheelhouse) -> S1 old version ->
#   S3 install -> S3b patch -> write pending.json -> S4 restart -> exit 0.
# Exits: 0 = restart issued (phase 2 owns completion); 2 = pre-checks failed;
# 3 = rolled back; 4 = rollback itself failed.
do_pre_restart() {
  log "phase=pre-restart job=$JOB_ID target=$TARGET venv=$UG_VENV"

  # 1) record the previous version (the app is still up in its old form)
  local old
  if ! old="$(get_version)"; then
    err "could not read the current version (UG_VERSION_CMD failed or printed nothing)"
    echo "SUMMARY: status=precheck_failed reason=old-version-unknown"
    write_state state=precheck_failed finished_at="$(now_ts)"
    exit 2
  fi
  log "previous version: $old"

  # 2) compatibility checker (S0) - C3 mandatory (wheelhouse)
  if [ -n "$CHECKER" ] && [ "$SKIP_CHECK" -eq 0 ]; then
    ccmd="$(sub_target "$UG_CHECKER_CMD" "$TARGET")"
    log "checker: $ccmd"
    if ! bash -c "$ccmd"; then
      err "checker refused the upgrade (nonzero exit) - aborted, nothing was installed"
      echo "SUMMARY: status=precheck_failed reason=checker-failed"
      write_state state=precheck_failed finished_at="$(now_ts)"
      exit 2
    fi
    log "checker passed"
  fi

  # 3) install the target
  if ! run_install "$TARGET"; then
    err "install of $TARGET failed -> rolling back (no restart; app still runs the old code)"
    do_pre_restart_rollback "$old" "install-failed"
  fi
  log "install of $TARGET done"

  # 3b) re-apply the compat patch (idempotent)
  if [ -n "$UG_PATCH_CMD" ]; then
    pcmd="$(sub_target "$UG_PATCH_CMD" "$TARGET")"
    log "compat patch: $pcmd"
    if ! bash -c "$pcmd"; then
      err "compat patch failed -> rolling back (no restart; app still runs the old code)"
      do_pre_restart_rollback "$old" "patch-failed"
    fi
    log "compat patch done"
  fi

  # 4) write pending BEFORE the swap becomes visible to a new process
  write_pending "$JOB_ID" "$old" "$TARGET" "$UG_VENV" "$STATE_FILE"
  log "pending written: $PENDING_FILE (old=$old target=$TARGET)"
  write_state state=running phase=boot-check job_id="$JOB_ID" target="$TARGET" current="$old" started_at="$(now_ts)"

  # 5) restart primitive. After this point the container runtime brings the
  #    container back up and the entrypoint runs phase 2 (boot-check).
  log "S4: restart (mode=$RESTART_MODE)"
  if ! run_restart; then
    err "restart command failed; the container may still be running the old version"
    echo "SUMMARY: status=precheck_failed reason=restart-failed"
    write_state state=precheck_failed finished_at="$(now_ts)"
    exit 2
  fi
  log "pre-restart complete: restart issued; boot-check owns completion (state: $STATE_FILE)"
  echo "SUMMARY: status=ok phase=pre-restart old=$old target=$TARGET"
  exit 0
}

# pre-restart rollback: reinstall old + re-apply patch, NO restart (the app
# never died), then finalize. Does not return (exits 3/4).
# State rule: the job is NON-terminal (recovering) while the restore actions
# run; rolled_back is written only AFTER they all succeed, together with the
# pending removal (the running app is the old code — it never died — so no
# further verification is needed).
do_pre_restart_rollback() {
  local old="$1"
  local reason="${2:-install-failed}"
  log "ROLLBACK (pre-restart): restoring $old (reason: $reason; no restart - app unchanged)"
  write_state state=recovering note="rollback:$reason"
  if ! run_install "$old"; then
    fatal_rollback_failed
    err "rollback failed: installing old version $old did not succeed"
    echo "SUMMARY: status=rollback_failed old=$old target=$TARGET"
    write_state state=rollback_failed
    exit 4
  fi
  if [ -n "$UG_PATCH_CMD" ]; then
    pcmd="$(sub_target "$UG_PATCH_CMD" "$old")"
    if ! bash -c "$pcmd"; then
      fatal_rollback_failed
      err "rollback failed: compat patch re-apply failed for $old"
      echo "SUMMARY: status=rollback_failed old=$old target=$TARGET"
      write_state state=rollback_failed
      exit 4
    fi
  fi
  # restore actions done: finalize here (the app is the old code, unaffected)
  rm -f "$PENDING_FILE"
  write_state state=rolled_back finished_at="$(now_ts)"
  log "ROLLBACK (pre-restart): restored to $old"
  echo "SUMMARY: status=rolled_back old=$old target=$TARGET reason=$reason"
  exit 3
}

# Phase 2 (entrypoint, before `exec qwenpaw-worker`; the app is not up yet).
# Always returns 0 once handled - the container MUST boot; the result lives
# in the state file (success / rolled_back / rollback_failed).
do_boot_check() {
  log "phase=boot-check venv=$UG_VENV pending=$PENDING_FILE"

  if [ ! -f "$PENDING_FILE" ]; then
    log "boot-check: no pending upgrade ($PENDING_FILE missing), nothing to do"
    return 0
  fi

  local pending_old pending_target
  pending_old="$(pending_get old)"
  pending_target="$(pending_get target)"
  [ -n "$TARGET" ] || TARGET="$pending_target"
  resolve_pending_context
  log "boot-check: pending job=$JOB_ID old=$pending_old target=$TARGET"

  if [ "$FORCE_ROLLBACK" = "1" ]; then
    log "boot-check: --force-rollback: rolling back regardless of the venv version"
    do_boot_check_rollback "$pending_old" "forced"
    return 0
  fi

  local boot_version
  boot_version="$(bash -c "$UG_VERSION_CMD" 2>/dev/null | tr -d '[:space:]')" || boot_version=""
  if [ -z "$boot_version" ]; then
    err "boot-check: could not read the venv version; assuming mismatch, rolling back"
    do_boot_check_rollback "$pending_old" "version-unreadable"
    return 0
  fi
  log "boot-check: venv reports qwenpaw==$boot_version (target $TARGET)"

  if [ "$boot_version" = "$TARGET" ]; then
    # venv version matches the target, but the app is not up yet: boot-check
    # runs before `exec qwenpaw-worker`. Do NOT remove pending or record
    # success here — a correctly installed package whose app then fails to
    # start would lose its recovery marker and any automatic health-triggered
    # rollback. Retain pending and mark the intermediate state; the
    # entrypoint's post-start health gate (version/readiness/settle) is the
    # arbiter and rolls back on startup failure.
    write_state state=boot-check-passed finished_at="" note="awaiting post-start health gate"
    log "boot-check: venv version matches target; pending RETAINED for post-start health gate"
    echo "SUMMARY: status=boot-check-passed phase=boot-check target=$TARGET"
    return 0
  fi

  # Crash-loop guard (I3): a previous boot-check rollback already restored
  # the old version and left expect=old. If the venv still holds it, do NOT
  # reinstall again (a crash loop would pip on every boot); keep pending and
  # let the entrypoint's post-start gate verify the restored runtime. Any
  # half-installed state (version unreadable or different) falls through to
  # the full rollback retry below.
  if [ "$(pending_get expect)" = "old" ] && [ "$boot_version" = "$pending_old" ]; then
    log "boot-check: already restored (expect=old, venv=$boot_version); retaining pending for post-start verification"
    echo "SUMMARY: status=recovering phase=boot-check old=$pending_old target=$TARGET reason=already-restored"
    return 0
  fi

  err "boot-check: version mismatch (expected $TARGET, venv has $boot_version); rolling back to $pending_old"
  do_boot_check_rollback "$pending_old" "version-mismatch"
  return 0
}

# boot-check rollback: reinstall old + re-apply patch, NO restart and NO
# health gate (the app is not up; it cannot be verified before boot).
# DEFERRED verification: the job goes to the non-terminal state `recovering`
# and pending is KEPT with expect=old — the entrypoint boots the app and its
# post-start gate verifies the OLD version (readiness + settle), finalizing
# rolled_back + pending removal only when that passes. pending is also kept
# on restore failure (manual retry via --force-rollback).
do_boot_check_rollback() {
  local old="$1"
  local reason="${2:-version-mismatch}"
  log "ROLLBACK (boot-check): restoring $old (reason: $reason; no restart - app not up yet)"
  write_state state=recovering note="rollback:$reason"
  if ! run_install "$old"; then
    fatal_rollback_failed
    err "boot-check: FATAL restore failed; pending kept for manual retry (--force-rollback)"
    echo "SUMMARY: status=rollback_failed old=$old target=$TARGET"
    write_state state=rollback_failed
    return 0
  fi
  if [ -n "$UG_PATCH_CMD" ]; then
    pcmd="$(sub_target "$UG_PATCH_CMD" "$old")"
    if ! bash -c "$pcmd"; then
      fatal_rollback_failed
      err "boot-check: FATAL compat patch re-apply failed; pending kept for manual retry"
      echo "SUMMARY: status=rollback_failed old=$old target=$TARGET"
      write_state state=rollback_failed
      return 0
    fi
  fi
  pending_set expect old
  log "boot-check: restored to $old; pending retained for post-start verification of the restored runtime"
  echo "SUMMARY: status=recovering phase=boot-check old=$old target=$TARGET reason=$reason"
  return 0
}

# Post-start health gate (run by the entrypoint AFTER `qwenpaw-worker` is up).
# boot-check verified the installed venv version before boot; this verifies the
# RUNNING app actually serves the target version and stays alive (settle
# window). On success it removes pending and records success; on failure it
# rolls back (reinstall old + patch + restart) so a correctly installed
# package whose app then fails to start is recovered automatically instead of
# being stranded without a recovery marker or an automatic rollback.
do_post_start() {
  log "phase=post-start venv=$UG_VENV pending=$PENDING_FILE"
  if [ ! -f "$PENDING_FILE" ]; then
    log "post-start: no pending upgrade ($PENDING_FILE missing), nothing to do"
    return 0
  fi
  local pending_target pending_old
  pending_target="$(pending_get target)"
  pending_old="$(pending_get old)"
  [ -n "$TARGET" ] || TARGET="$pending_target"
  # The entrypoint calls us with --pending-file only: recover the job context
  # so the verdict below actually lands in the job's state file (previously
  # STATE_FILE stayed empty -> write_state no-op -> job stuck at
  # boot-check-passed, which also left the 409 mutex disengaged).
  resolve_pending_context
  # RECOVERY verification (expect=old, set by the kill-mode post-start
  # rollback and the boot-check rollback): the running app is expected to be
  # the OLD version. The job is still `recovering` (non-terminal, mutex
  # engaged); only a passed readiness+settle gate finalizes rolled_back and
  # removes pending. A failed verification is rollback_failed with pending
  # KEPT — it must never read as a successful recovery.
  if [ "$(pending_get expect)" = "old" ]; then
    log "post-start: job=$JOB_ID recovery verification, health gate for the restored runtime, want=$pending_old"
    if health_gate "$pending_old" "$TIMEOUT" "$SETTLE"; then
      rm -f "$PENDING_FILE"
      write_state state=rolled_back phase=post-start finished_at="$(now_ts)"
      log "post-start: RECOVERY VERIFIED - restored runtime healthy; pending removed"
      echo "SUMMARY: status=recovered phase=post-start old=$pending_old target=$TARGET"
      return 0
    fi
    fatal_rollback_failed
    err "post-start: FATAL recovery verification failed (want='$pending_old'); pending kept for manual retry"
    echo "SUMMARY: status=rollback_failed phase=post-start old=$pending_old target=$TARGET reason=restore-verification-failed"
    write_state state=rollback_failed
    exit 4
  fi
  log "post-start: job=$JOB_ID health gate for the running app, target=$TARGET"
  if health_gate "$TARGET" "$TIMEOUT" "$SETTLE"; then
    rm -f "$PENDING_FILE"
    write_state state=success phase=post-start finished_at="$(now_ts)"
    log "post-start: SUCCESS - health gate passed; pending removed"
    echo "SUMMARY: status=ok phase=post-start target=$TARGET"
    return 0
  fi
  err "post-start: health gate failed (want=$TARGET); rolling back to $pending_old"
  # does not return: exit 3 (recovered) or 4 (rollback failed) — never 0,
  # which the entrypoint would read as "keep the new runtime"
  do_post_start_rollback "$pending_old" "post-start-health-failed"
}

# post-start rollback: the app is up (possibly serving the new version), so
# restoring the old version needs a restart to take effect. It does NOT
# return 0: the entrypoint branches on the exit code, and 0 means "keep the
# CURRENT runtime". State rule: the job is NON-terminal (recovering) while
# the restore actions run; rolled_back is written only by the path that
# actually verified the restored runtime. Outcomes:
#   3 = supervisorctl: old runtime restored AND in-process verified
#       (readiness + settle) -> pending removed, job rolled_back
#   3 = kill: restore done but UNVERIFIED (the app child is dead and the
#       entrypoint owns the relaunch) -> pending kept with expect=old, job
#       recovering; the entrypoint relaunches the old runtime and re-enters
#       post-start, whose expect=old branch performs the verification
#   4 = FATAL: the restore failed (install/patch/restart, or in
#       supervisorctl mode the restored runtime failed its own health gate)
#       -> pending KEPT for manual retry, job rollback_failed, loud banner
do_post_start_rollback() {
  local old="$1"
  local reason="${2:-post-start-health-failed}"
  log "ROLLBACK (post-start): restoring $old (reason: $reason, restart mode=$RESTART_MODE)"
  write_state state=recovering note="rollback:$reason"
  if ! run_install "$old"; then
    fatal_rollback_failed
    err "post-start: FATAL restore failed; pending kept for manual retry (--force-rollback)"
    echo "SUMMARY: status=rollback_failed phase=post-start old=$old target=$TARGET reason=$reason"
    write_state state=rollback_failed
    exit 4
  fi
  if [ -n "$UG_PATCH_CMD" ]; then
    pcmd="$(sub_target "$UG_PATCH_CMD" "$old")"
    if ! bash -c "$pcmd"; then
      fatal_rollback_failed
      err "post-start: FATAL compat patch re-apply failed; pending kept for manual retry"
      echo "SUMMARY: status=rollback_failed phase=post-start old=$old target=$TARGET reason=$reason"
      write_state state=rollback_failed
      exit 4
    fi
  fi
  # The restart must be the primitive that exists in THIS image: the worker
  # image has no supervisor (PID1 is the qwenpaw-worker wrapper), so its mode
  # is kill (the entrypoint passes --restart-mode kill); supervisorctl mode
  # is for the QwenPaw runtime image that ships supervisord.
  if ! run_restart; then
    fatal_rollback_failed
    err "post-start: FATAL restart after rollback failed (mode=$RESTART_MODE); pending kept for manual retry"
    echo "SUMMARY: status=rollback_failed phase=post-start old=$old target=$TARGET reason=$reason"
    write_state state=rollback_failed
    exit 4
  fi
  if [ "$RESTART_MODE" = "kill" ]; then
    # kill mode stops the app child and the wrapper exits with it: the
    # restored runtime CANNOT be verified in-process (the app is dead).
    # Hand off to the entrypoint: keep the recovery in flight (recovering,
    # mutex engaged) with pending(expect=old); the entrypoint relaunches the
    # old runtime and re-enters post-start, where the expect=old branch
    # verifies it (readiness + settle) and only then finalizes rolled_back.
    # This exit 3 is therefore a handoff, NOT a verdict.
    pending_set expect old
    log "post-start: kill-mode restore done; relaunch + verification by the entrypoint"
    echo "SUMMARY: status=recovering phase=post-start old=$old target=$TARGET reason=$reason"
    exit 3
  elif ! health_gate "$old" "$TIMEOUT" "$SETTLE"; then
    fatal_rollback_failed
    err "post-start: FATAL restored runtime failed its health gate (want='$old'); pending kept for manual retry"
    echo "SUMMARY: status=rollback_failed phase=post-start old=$old target=$TARGET reason=$reason"
    write_state state=rollback_failed
    exit 4
  fi
  # supervisorctl: the restored runtime passed its in-process health gate —
  # the recovery is verified, finalize here.
  rm -f "$PENDING_FILE"
  write_state state=rolled_back phase=post-start finished_at="$(now_ts)"
  log "post-start: RECOVERED to $old (restart mode=$RESTART_MODE); pending removed"
  echo "SUMMARY: status=recovered phase=post-start old=$old target=$TARGET reason=$reason"
  exit 3
}

# ============================================================================
# --selftest: built-in stubbed battery (19 scenarios; every external command mocked via tmp-dir state files)
# ============================================================================

SELFTEST_TOTAL_FAIL=0
SC_ID=""
SC_FAIL=0

st_begin() { # $1=id $2=description
  SC_FAIL=0
  log "selftest: --- $1 $2 ---"
}

st_check() { # $1=description $2=expected $3=actual
  if [ "$2" = "$3" ]; then
    printf '  [%s] %s: %s\n' "$SC_ID" "$1" "$3"
  else
    printf '  [%s] %s: expected=%s actual=%s\n' "$SC_ID" "$1" "$2" "$3"
    SC_FAIL=$((SC_FAIL+1))
  fi
}

st_grep() { # $1=description $2=file $3=pattern
  if grep -q "$3" "$2" 2>/dev/null; then
    printf '  [%s] %s: hit\n' "$SC_ID" "$1"
  else
    printf '  [%s] %s: miss (pattern=%s)\n' "$SC_ID" "$1" "$3"
    SC_FAIL=$((SC_FAIL+1))
  fi
}

st_end() { # $1=id $2=description
  if [ "$SC_FAIL" -eq 0 ]; then
    printf 'PASS: %s %s\n' "$1" "$2"
  else
    printf 'FAIL: %s %s (%d assertions unmet)\n' "$1" "$2" "$SC_FAIL"
    SELFTEST_TOTAL_FAIL=$((SELFTEST_TOTAL_FAIL+1))
  fi
}

run_selftest() {
  local tmp d rc
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/online_upgrade_v1.selftest.XXXXXX")" \
    || { err "cannot create temp dir"; return 1; }
  log "selftest: starting (19 scenarios, all external commands stubbed; tmp dir: $tmp)"
  # Keep the default scenarios fast; T8 exercises the settle window explicitly.
  SETTLE=0

  # ============ T1 all green ============
  d="$tmp/T1"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=30; CHECKER=""; SKIP_CHECK=1
  RECORD_FILE="$d/record.json"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T1"
  st_begin T1 "all green (2.2.1 -> install 2.2.2 -> restart -> health gate -> record)"
  st_check "exit" "0" "$rc"
  st_check "version file" "2.2.2" "$(cat "$d/version" 2>/dev/null)"
  st_check "restart count" "1" "$(wc -l < "$d/restarts" 2>/dev/null | tr -d '[:space:]')"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=ok old=2.2.1 target=2.2.2 elapsed="
  st_grep "record written" "$d/out.log" "verified, then recorded"
  if [ -f "$d/record.json" ] && grep -q '"applied_version":"2.2.2"' "$d/record.json" \
      && grep -q '"backend":"in-container"' "$d/record.json"; then
    printf 'rok' > "$d/rchk"
  else
    printf 'rbad' > "$d/rchk"
  fi
  st_check "record JSON" "rok" "$(cat "$d/rchk" 2>/dev/null)"
  st_end T1 "all green"

  # ============ T2 install fails -> rollback succeeds ============
  d="$tmp/T2"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=30; CHECKER=""; SKIP_CHECK=1
  RECORD_FILE="$d/record.json"
  UG_VERSION_CMD="cat $d/version"
  # install stub succeeds only for the old version 2.2.1; target 2.2.2 -> nonzero (simulates a failed install)
  UG_INSTALL_CMD='[ "{target}" = "2.2.1" ] && printf %s {target} > '"$d"'/version'
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T2"
  st_begin T2 "install fails -> rollback succeeds"
  st_check "exit" "3" "$rc"
  st_check "version file (restored)" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  # Arbitration note: task brief 168 T2 said "restart count x2", but per the flow definition
  # (install failure jumps straight to the rollback step, where the main-path restart is
  # never run) and per the S2 design (rollback = install old + restart + re-verify),
  # the whole run should be x1 (x2 is the T3 shape). Flow text wins; if a maintainer rules x2,
  st_check "restart count" "1" "$(wc -l < "$d/restarts" 2>/dev/null | tr -d '[:space:]')"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=rolledback old=2.2.1 target=2.2.2"
  st_grep "record written (rollback)" "$d/out.log" "verified, then recorded"
  if [ -f "$d/record.json" ] && grep -q '"status":"rolledback"' "$d/record.json" \
      && grep -q '"applied_version":"2.2.1"' "$d/record.json"; then
    printf 'rok' > "$d/rchk"
  else
    printf 'rbad' > "$d/rchk"
  fi
  st_check "record JSON (rollback = old version)" "rok" "$(cat "$d/rchk" 2>/dev/null)"
  st_end T2 "install fails -> rollback succeeds"

  # ============ T3 health gate mismatch -> rollback succeeds ============
  d="$tmp/T3"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=3; CHECKER=""; SKIP_CHECK=1
  RECORD_FILE=""
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='true'   # install "succeeds" but the version does not move (wrong/missing package)
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T3"
  st_begin T3 "health gate mismatch (version stuck at 2.2.1) -> rollback succeeds"
  st_check "exit" "3" "$rc"
  st_check "version file (still old)" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_check "restart count (main path 1 + rollback 1)" "2" "$(wc -l < "$d/restarts" 2>/dev/null | tr -d '[:space:]')"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=rolledback old=2.2.1 target=2.2.2"
  st_end T3 "health gate mismatch -> rollback succeeds"

  # ============ T4 rollback also fails ============
  d="$tmp/T4"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=3; CHECKER=""; SKIP_CHECK=1
  RECORD_FILE="$d/record.json"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='false'  # every install (including the rollback install) fails
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T4"
  st_begin T4 "rollback also fails"
  st_check "exit" "4" "$rc"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=rollback-failed"
  st_grep "loud error" "$d/out.log" "FATAL: ROLLBACK FAILED"
  # rollback failure = unknown state -> no record is written (record semantics stay clean)
  if [ -e "$d/record.json" ]; then printf 'wrote' > "$d/rchk"; else printf 'absent' > "$d/rchk"; fi
  st_check "no record written" "absent" "$(cat "$d/rchk" 2>/dev/null)"
  st_end T4 "rollback also fails"

  # ============ T5 checker refuses ============
  d="$tmp/T5"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=3; SKIP_CHECK=0
  RECORD_FILE=""
  CHECKER="/selftest/checker.py"   # checker given -> the check step runs
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo "install {target}" >> '"$d"'/installs'
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  UG_CHECKER_CMD='false'           # checker refuses
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T5"
  st_begin T5 "checker refuses (UG_CHECKER_CMD=false)"
  st_check "exit" "2" "$rc"
  st_check "install log empty (no install happened)" "" "$(cat "$d/installs" 2>/dev/null)"
  st_check "version file unchanged" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=aborted reason=checker-failed"
  st_end T5 "checker refuses and nothing installs"

  # ============ T6 docker backend default command assembly (resolve layer, not executed) ============
  d="$tmp/T6"; mkdir -p "$d"
  (
    TARGET="2.2.2"; CONTAINER="agentteams-worker-demo"; VENV=""; TIMEOUT=90
    BACKEND="docker"; RESTART_MODE="supervisorctl"; CHECKER=""; SKIP_CHECK=0
    RECORD_FILE=""; UG_VERSION_CMD=""; UG_INSTALL_CMD=""; UG_RESTART_CMD=""
    UG_CHECKER_CMD=""; UG_VENV=""; UG_UV=""
    resolve_defaults
    {
      case "$UG_VERSION_CMD" in "docker exec agentteams-worker-demo"*) printf 'vok' ;; *) printf 'vbad' ;; esac
      case "$UG_INSTALL_CMD" in "docker exec agentteams-worker-demo"*) printf 'iok' ;; *) printf 'ibad' ;; esac
      case "$UG_RESTART_CMD" in "docker restart agentteams-worker-demo") printf 'rok' ;; *) printf 'rbad' ;; esac
      case "$UG_VERSION_CMD" in *"/opt/venv/qwenpaw"*) printf 'pok' ;; *) printf 'pbad' ;; esac
      case "$UG_INSTALL_CMD" in *"--reinstall-package qwenpaw qwenpaw=={target}"*) printf 'uok' ;; *) printf 'ubad' ;; esac
      case "$UG_INSTALL_CMD" in *"--force-reinstall --no-deps qwenpaw=={target}"*) printf 'fok' ;; *) printf 'fbad' ;; esac
      case "$UG_CHECKER_CMD" in *"--target {target} --in-place") printf 'cok' ;; *) printf 'cbad' ;; esac
    } > "$d/asserts"
    exit 0
  ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T6"
  st_begin T6 "docker backend default command assembly"
  st_check "exit" "0" "$rc"
  st_check "seven assertions" "vokiokrokpokuokfokcok" "$(cat "$d/asserts" 2>/dev/null)"
  st_end T6 "docker backend default command assembly"

  # ============ T7 docker backend full stubbed flow + record ============
  d="$tmp/T7"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=30; CHECKER=""; SKIP_CHECK=1; BACKEND="docker"
  RECORD_FILE="$d/record.json"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_RESTART_CMD='echo restart >> '"$d"'/restarts'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T7"
  st_begin T7 "docker backend full stubbed flow + record written"
  st_check "exit" "0" "$rc"
  st_check "version file" "2.2.2" "$(cat "$d/version" 2>/dev/null)"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=ok old=2.2.1 target=2.2.2 elapsed="
  st_grep "record written" "$d/out.log" "verified, then recorded"
  if [ -f "$d/record.json" ] && grep -q '"status":"ok"' "$d/record.json" \
      && grep -q '"applied_version":"2.2.2"' "$d/record.json" \
      && grep -q '"backend":"docker"' "$d/record.json"; then
    printf 'rok' > "$d/rchk"
  else
    printf 'rbad' > "$d/rchk"
  fi
  st_check "record JSON (docker)" "rok" "$(cat "$d/rchk" 2>/dev/null)"
  st_end T7 "docker backend full stubbed flow + record written"

  # ============ T8 settle window catches a delayed wrapper crash -> rollback ============
  # The app reports the right version immediately, but the wrapper (whose own
  # version gate outlives the app) dies a few seconds later. The raw version
  # endpoint alone would pass; the settle window's liveness probe must not.
  d="$tmp/T8"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; TIMEOUT=30; CHECKER=""; SKIP_CHECK=1; BACKEND="docker"
  SETTLE=10; SETTLE_POLL=1; RECORD_FILE=""
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_RESTART_CMD='c=$(cat '"$d"'/rc 2>/dev/null || echo 0); c=$((c+1)); echo $c > '"$d"'/rc; echo r >> '"$d"'/restarts'
  # Alive: stay up during the first settle window until the 3rd poll, then die
  # (rc==1); stay up for the whole post-rollback settle window (rc>=2).
  UG_ALIVE_CMD='rc=$(cat '"$d"'/rc 2>/dev/null || echo 0); a=$(cat '"$d"'/ac 2>/dev/null || echo 0); a=$((a+1)); echo $a > '"$d"'/ac; if [ "$rc" -eq 1 ] && [ $a -ge 3 ]; then echo false; else echo true; fi'
  ( do_upgrade ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T8"
  st_begin T8 "settle window catches delayed wrapper crash -> rollback"
  st_check "exit" "3" "$rc"
  st_check "version restored" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_grep "settle detected" "$d/out.log" "health gate: settle"
  st_grep "rolled back" "$d/out.log" "SUMMARY: status=rolledback old=2.2.1 target=2.2.2"
  st_end T8 "settle window catches delayed wrapper crash -> rollback"

  # ============ T9 pre-restart happy path (two-phase, phase 1) ============
  # Order must be: install(target) -> patch -> pending write -> restart;
  # the state file ends with phase=boot-check.
  d="$tmp/T9"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"
  TARGET="2.2.2"; CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"
  RECORD_FILE=""
  PHASE="pre-restart"; JOB_ID="upg-selftest-t9"
  STATE_FILE="$d/state.json"; PENDING_FILE="$d/pending.json"
  UG_VENV="$d/venv"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  UG_RESTART_CMD='echo restart >> '"$d"'/calls'
  ( do_pre_restart ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T9"
  st_begin T9 "pre-restart happy path (install -> patch -> pending -> restart)"
  st_check "exit" "0" "$rc"
  st_check "target installed" "2.2.2" "$(cat "$d/version" 2>/dev/null)"
  if grep -q '"old": "2.2.1"' "$d/pending.json" 2>/dev/null \
      && grep -q '"target": "2.2.2"' "$d/pending.json" 2>/dev/null \
      && grep -q '"job_id": "upg-selftest-t9"' "$d/pending.json" 2>/dev/null; then
    printf 'pok' > "$d/pchk"
  else
    printf 'pbad' > "$d/pchk"
  fi
  st_check "pending fields" "pok" "$(cat "$d/pchk" 2>/dev/null)"
  if grep -q '"phase": "boot-check"' "$d/state.json" 2>/dev/null \
      && grep -q '"state": "running"' "$d/state.json" 2>/dev/null; then
    printf 'sok' > "$d/schk"
  else
    printf 'sbad' > "$d/schk"
  fi
  st_check "state phase=boot-check" "sok" "$(cat "$d/schk" 2>/dev/null)"
  st_check "first call = patch" "patch-2.2.2" "$(head -n1 "$d/calls" 2>/dev/null | tr -d '[:space:]')"
  st_check "last call = restart" "restart" "$(tail -n1 "$d/calls" 2>/dev/null | tr -d '[:space:]')"
  st_grep "SUMMARY" "$d/out.log" "SUMMARY: status=ok phase=pre-restart old=2.2.1 target=2.2.2"
  st_end T9 "pre-restart happy path"

  # ============ T10 boot-check mismatch -> rollback DEFERS to post-start ============
  # Round 3: the boot-check rollback restores the old version but CANNOT verify
  # it (the app is not up yet): the job goes to the non-terminal state
  # `recovering`, pending is KEPT with expect=old, and the exit is 0 so the
  # entrypoint proceeds to boot the app — its post-start gate (expect=old
  # branch) performs the verification and finalizes rolled_back.
  d="$tmp/T10"; mkdir -p "$d"
  printf '2.2.1' > "$d/version"   # venv reports the OLD version: mismatch
  PHASE="boot-check"; TARGET=""; CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"
  RECORD_FILE=""
  PENDING_FILE="$d/pending.json"; STATE_FILE="$d/state.json"
  UG_VENV="$d/venv"
  printf '{"job_id": "upg-selftest-t10", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv"}\n' > "$d/pending.json"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch >> '"$d"'/calls'
  UG_RESTART_CMD='echo RESTART-CALLED >> '"$d"'/calls; exit 1'
  ( do_boot_check ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T10"
  st_begin T10 "boot-check mismatch -> rollback (deferred, no restart)"
  st_check "exit (boot-check always 0 once handled)" "0" "$rc"
  st_check "version restored to old" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  if [ -e "$d/pending.json" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending RETAINED for post-start verification" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "pending marks expect=old" "$d/pending.json" '"expect": "old"'
  if grep -q '"state": "recovering"' "$d/state.json" 2>/dev/null; then printf 'sok' > "$d/schk"; else printf 'sbad' > "$d/schk"; fi
  st_check "state=recovering (non-terminal)" "sok" "$(cat "$d/schk" 2>/dev/null)"
  if grep -q 'RESTART-CALLED' "$d/calls" 2>/dev/null; then printf 'present' > "$d/rck"; else printf 'absent' > "$d/rck"; fi
  st_check "restart never called (app not up yet)" "absent" "$(cat "$d/rck" 2>/dev/null)"
  st_grep "SUMMARY recovering" "$d/out.log" "SUMMARY: status=recovering phase=boot-check old=2.2.1 target=2.2.2 reason=version-mismatch"
  st_end T10 "boot-check mismatch -> rollback defers (recovering, expect=old)"

  # ============ T11 boot-check no-op (no pending.json) ============
  d="$tmp/T11"; mkdir -p "$d"
  PHASE="boot-check"; TARGET=""; CHECKER=""; BACKEND="in-container"
  PENDING_FILE="$d/pending.json"; STATE_FILE="$d/state.json"
  UG_VENV="$d/venv"
  UG_VERSION_CMD="true"; UG_INSTALL_CMD="true"; UG_RESTART_CMD="true"
  ( do_boot_check ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T11"
  st_begin T11 "boot-check no-op (no pending.json)"
  st_check "exit" "0" "$rc"
  st_grep "nothing to do" "$d/out.log" "nothing to do"
  st_end T11 "boot-check no-op"

  # ============ T12 two-phase full chain, entrypoint-style phase 2 (P1-1) ============
  # The entrypoint invokes boot-check/post-start with ONLY --venv and
  # --pending-file, so the job context must travel in pending.json (state_file)
  # and be recovered: the job state must close to success (not be stranded at
  # boot-check-passed) and the 409 mutex stays engaged for the whole window.
  d="$tmp/T12"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"
  JOB="upg-selftest-t12"
  S12="$d/upgrade/upgrade-$JOB.json"; P12="$d/upgrade/pending.json"
  TARGET="2.2.2"; CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"
  RECORD_FILE=""
  PHASE="pre-restart"; JOB_ID="$JOB"
  STATE_FILE="$S12"; PENDING_FILE="$P12"
  UG_VENV="$d/venv"; RESTART_MODE="supervisorctl"
  UG_CHECKER_CMD=""
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  UG_RESTART_CMD='echo restart >> '"$d"'/calls'
  ( do_pre_restart ) > "$d/out1.log" 2>&1
  rc1=$?
  cp "$P12" "$d/pending-snap.json" 2>/dev/null
  # Phase 2 (boot-check): entrypoint-style — no --job-id/--state-file
  PHASE="boot-check"; TARGET=""; JOB_ID=""; STATE_FILE=""
  ( do_boot_check ) > "$d/out2.log" 2>&1
  rc2=$?
  cp "$S12" "$d/state-after-bootcheck.json" 2>/dev/null
  # Phase 3 (post-start): entrypoint-style — no --job-id/--state-file
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""
  TIMEOUT=5; SETTLE=0
  ( do_post_start ) > "$d/out3.log" 2>&1
  rc3=$?
  SC_ID="T12"
  st_begin T12 "two-phase chain: pre-restart -> boot-check -> post-start (entrypoint-style phase 2)"
  st_check "pre-restart exit" "0" "$rc1"
  st_check "boot-check exit" "0" "$rc2"
  st_check "post-start exit" "0" "$rc3"
  st_check "target installed" "2.2.2" "$(cat "$d/version" 2>/dev/null)"
  st_grep "pending carries job_id" "$d/pending-snap.json" '"job_id": "upg-selftest-t12"'
  st_grep "pending carries state_file" "$d/pending-snap.json" '"state_file": "'"$S12"'"'
  st_grep "boot-check parked intermediate state" "$d/state-after-bootcheck.json" '"state": "boot-check-passed"'
  if [ -e "$P12" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending removed after success" "absent" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job closed to success" "$S12" '"state": "success"'
  st_grep "job closed in post-start" "$S12" '"phase": "post-start"'
  st_end T12 "two-phase chain (state closes to success from pending alone)"

  # ============ T13 post-start health fail -> rollback -> recovered (P1-2) ============
  # supervisorctl mode re-verifies the restored old runtime IN-PROCESS after
  # the rollback restart: the recovery only counts when that gate passes.
  d="$tmp/T13"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"   # app dead/stale: the target gate cannot pass
  JOB="upg-selftest-t13"
  S13="$d/upgrade/upgrade-$JOB.json"; P13="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "boot-check-passed", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S13"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s"}\n' "$JOB" "$S13" > "$P13"
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P13"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="supervisorctl"
  TIMEOUT=5; SETTLE=0
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo install-{target} >> '"$d"'/calls; printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  UG_RESTART_CMD='echo restart >> '"$d"'/calls'
  ( do_post_start ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T13"
  st_begin T13 "post-start health fail -> rollback -> recovered (in-process re-verify)"
  st_check "exit (recovered = 3)" "3" "$rc"
  st_check "version restored to old" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_grep "rollback installed old" "$d/calls" "install-2.2.1"
  st_grep "rollback patched old" "$d/calls" "patch-2.2.1"
  st_grep "rollback restarted" "$d/calls" "restart"
  if [ -e "$P13" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending removed after recovery" "absent" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job rolled_back" "$S13" '"state": "rolled_back"'
  st_grep "job closed in post-start" "$S13" '"phase": "post-start"'
  st_grep "SUMMARY recovered" "$d/out.log" "SUMMARY: status=recovered phase=post-start old=2.2.1 target=2.2.2 reason=post-start-health-failed"
  st_end T13 "post-start rollback recovered (exit 3, verified)"

  # ============ T14 post-start rollback restart primitive absent (P1-2) ============
  # The worker image has no supervisor: supervisorctl is not there. A rollback
  # whose restart fails is a FAILED recovery -> loud FATAL + exit 4 (never 0,
  # which the entrypoint would read as "keep the new runtime"), job
  # rollback_failed, pending kept for manual retry.
  d="$tmp/T14"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"
  JOB="upg-selftest-t14"
  S14="$d/upgrade/upgrade-$JOB.json"; P14="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "boot-check-passed", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S14"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s"}\n' "$JOB" "$S14" > "$P14"
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P14"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="supervisorctl"   # the worker image does not have it
  TIMEOUT=3; SETTLE=0
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo install-{target} >> '"$d"'/calls; true'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  UG_RESTART_CMD='echo RESTART-ATTEMPTED >> '"$d"'/calls; exit 1'
  ( do_post_start ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T14"
  st_begin T14 "post-start rollback restart primitive absent (worker image) -> FATAL"
  st_check "exit (rollback failed = 4)" "4" "$rc"
  st_grep "FATAL banner" "$d/out.log" "FATAL: ROLLBACK FAILED"
  st_grep "restart was attempted" "$d/calls" "RESTART-ATTEMPTED"
  st_check "not silently recovered" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  if [ -e "$P14" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending kept for manual retry" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job rollback_failed" "$S14" '"state": "rollback_failed"'
  st_end T14 "post-start rollback failure is FATAL (exit 4, pending kept)"

  # ============ T15 post-start rollback in kill mode: UNVERIFIED HANDOFF (round 3) ============
  # Worker images restart by killing the app child: the wrapper exits with it,
  # so the restored runtime CANNOT be re-verified in-process. The recovery
  # therefore STAYS in flight: exit 3 is a handoff to the entrypoint (NOT a
  # verdict), pending is KEPT with expect=old, and the job is left in the
  # non-terminal state `recovering` (409 mutex engaged).
  d="$tmp/T15"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"
  JOB="upg-selftest-t15"
  S15="$d/upgrade/upgrade-$JOB.json"; P15="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "boot-check-passed", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S15"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s"}\n' "$JOB" "$S15" > "$P15"
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P15"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="kill"
  TIMEOUT=3; SETTLE=0
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo install-{target} >> '"$d"'/calls; printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  UG_RESTART_CMD='echo restart-kill >> '"$d"'/calls'
  ( do_post_start ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T15"
  st_begin T15 "post-start rollback in kill mode (worker image): unverified handoff"
  st_check "exit (handoff to entrypoint = 3)" "3" "$rc"
  st_check "version restored to old" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_grep "rollback installed old" "$d/calls" "install-2.2.1"
  st_grep "kill restart primitive used" "$d/calls" "restart-kill"
  st_grep "handoff logged" "$d/out.log" "relaunch + verification by the entrypoint"
  if [ -e "$P15" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending RETAINED for verification" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "pending marks expect=old" "$P15" '"expect": "old"'
  st_grep "job still recovering (non-terminal)" "$S15" '"state": "recovering"'
  st_grep "SUMMARY recovering handoff" "$d/out.log" "SUMMARY: status=recovering phase=post-start old=2.2.1 target=2.2.2 reason=post-start-health-failed"
  st_end T15 "kill-mode rollback handoff (exit 3, recovering, pending kept)"

  # ============ T16 entrypoint re-entry: expect=old verifies the restored runtime ============
  # Same dir as T15: the entrypoint relaunched the old runtime (the version
  # file already reads 2.2.1) and re-ran post-start entrypoint-style. The
  # expect=old branch must verify it (readiness + settle) and ONLY THEN
  # finalize: pending removed, job rolled_back with finished_at set.
  d="$tmp/T15"
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P15"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="kill"
  TIMEOUT=3; SETTLE=0
  UG_VERSION_CMD="cat $d/version"   # the relaunched old runtime serves 2.2.1
  ( do_post_start ) > "$d/out16.log" 2>&1
  rc=$?
  SC_ID="T16"
  st_begin T16 "post-start re-entry (expect=old) verifies the restored runtime"
  st_check "exit (recovery verified = 0)" "0" "$rc"
  if [ -e "$P15" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending removed after verification" "absent" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job rolled_back after verification" "$S15" '"state": "rolled_back"'
  st_grep "job closed in post-start" "$S15" '"phase": "post-start"'
  if grep -q '"finished_at": ""' "$S15" 2>/dev/null || grep -q '"finished_at": null' "$S15" 2>/dev/null; then
    printf 'empty' > "$d/eck"
  elif grep -q '"finished_at"' "$S15" 2>/dev/null; then
    printf 'set' > "$d/eck"
  else
    printf 'missing' > "$d/eck"
  fi
  st_check "finished_at set on finalization" "set" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "SUMMARY recovered" "$d/out16.log" "SUMMARY: status=recovered phase=post-start old=2.2.1 target=2.2.2"
  st_end T16 "recovery verification finalizes rolled_back (exit 0, pending removed)"

  # ============ T17 recovery verification failure -> FATAL (never rolled_back) ============
  # Re-entry with expect=old but the relaunched runtime is still broken (the
  # version probe keeps failing): the verification must FAIL LOUDLY — exit 4,
  # FATAL banner, pending KEPT, job rollback_failed. Never rolled_back.
  d="$tmp/T17"; mkdir -p "$d/upgrade"
  JOB="upg-selftest-t17"
  S17="$d/upgrade/upgrade-$JOB.json"; P17="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "recovering", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S17"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s", "expect": "old"}\n' "$JOB" "$S17" > "$P17"
  PHASE="post-start"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P17"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="kill"
  TIMEOUT=3; SETTLE=0
  UG_VERSION_CMD="exit 1"   # the relaunched runtime is still broken
  ( do_post_start ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T17"
  st_begin T17 "recovery verification failure (expect=old, probe dead) -> FATAL"
  st_check "exit (verification failed = 4)" "4" "$rc"
  st_grep "FATAL banner" "$d/out.log" "FATAL: ROLLBACK FAILED"
  if [ -e "$P17" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending kept for manual retry" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job rollback_failed" "$S17" '"state": "rollback_failed"'
  st_grep "SUMMARY rollback_failed" "$d/out.log" "SUMMARY: status=rollback_failed phase=post-start old=2.2.1 target=2.2.2 reason=restore-verification-failed"
  st_end T17 "recovery verification failure is FATAL (exit 4, pending kept)"

  # ============ T18 boot-check guard: expect=old with venv already restored (I3) ============
  # Reboot mid-recovery: pending(expect=old) is present and the venv already
  # reports the old version. The boot-check mismatch path must NOT reinstall
  # again (a crash loop would pip on every boot) — it defers to post-start and
  # returns 0 so the entrypoint proceeds to boot the app.
  d="$tmp/T18"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"   # the venv already reports the old version
  JOB="upg-selftest-t18"
  S18="$d/upgrade/upgrade-$JOB.json"; P18="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "recovering", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S18"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s", "expect": "old"}\n' "$JOB" "$S18" > "$P18"
  PHASE="boot-check"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P18"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="kill"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo install-{target} >> '"$d"'/calls; true'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls; true'
  ( do_boot_check ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T18"
  st_begin T18 "boot-check guard: expect=old + venv==old (no reinstall, defer)"
  st_check "exit (defer to post-start = 0)" "0" "$rc"
  if [ -e "$d/calls" ]; then printf 'called' > "$d/eck"; else printf 'uncalled' > "$d/eck"; fi
  st_check "install NOT called (crash-loop guard)" "uncalled" "$(cat "$d/eck" 2>/dev/null)"
  if [ -e "$P18" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending retained" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "job still recovering (non-terminal)" "$S18" '"state": "recovering"'
  st_grep "SUMMARY recovering" "$d/out.log" "SUMMARY: status=recovering phase=boot-check old=2.2.1 target=2.2.2 reason=already-restored"
  st_end T18 "boot-check guard defers (no reinstall, pending kept)"

  # ============ T19 boot-check mismatch -> rollback defers to post-start ============
  # Fresh reboot right after a crash: pending is present (no expect yet) and
  # the target install never completed, so the venv still reports the OLD
  # version (mismatch against the target). boot-check must (re)install/patch
  # the old version and then DEFER: state recovering, pending kept with
  # expect=old, exit 0 — verification happens in post-start once the app is up.
  d="$tmp/T19"; mkdir -p "$d/upgrade"
  printf '2.2.1' > "$d/version"   # the venv still reports the old version (install failed)
  JOB="upg-selftest-t19"
  S19="$d/upgrade/upgrade-$JOB.json"; P19="$d/upgrade/pending.json"
  printf '{"job_id": "%s", "state": "boot-check-passed", "target": "2.2.2", "started_at": "2026-01-01T00:00:00Z"}\n' "$JOB" > "$S19"
  printf '{"job_id": "%s", "old": "2.2.1", "target": "2.2.2", "venv": "/fake/venv", "state_file": "%s"}\n' "$JOB" "$S19" > "$P19"
  PHASE="boot-check"; TARGET=""; JOB_ID=""; STATE_FILE=""; PENDING_FILE="$P19"
  CHECKER=""; SKIP_CHECK=1; BACKEND="in-container"; RECORD_FILE=""
  UG_VENV="$d/venv"; RESTART_MODE="kill"
  UG_VERSION_CMD="cat $d/version"
  UG_INSTALL_CMD='echo install-{target} >> '"$d"'/calls; printf %s {target} > '"$d"'/version'
  UG_PATCH_CMD='echo patch-{target} >> '"$d"'/calls'
  ( do_boot_check ) > "$d/out.log" 2>&1
  rc=$?
  SC_ID="T19"
  st_begin T19 "boot-check mismatch rollback defers to post-start (expect=old)"
  st_check "exit (defer = 0)" "0" "$rc"
  st_check "version restored to old" "2.2.1" "$(cat "$d/version" 2>/dev/null)"
  st_grep "rollback installed old" "$d/calls" "install-2.2.1"
  st_grep "rollback patched old" "$d/calls" "patch-2.2.1"
  if [ -e "$P19" ]; then printf 'present' > "$d/eck"; else printf 'absent' > "$d/eck"; fi
  st_check "pending retained" "present" "$(cat "$d/eck" 2>/dev/null)"
  st_grep "pending marks expect=old" "$P19" '"expect": "old"'
  st_grep "job recovering (non-terminal)" "$S19" '"state": "recovering"'
  st_grep "SUMMARY recovering" "$d/out.log" "SUMMARY: status=recovering phase=boot-check old=2.2.1 target=2.2.2 reason=version-mismatch"
  st_end T19 "boot-check rollback defers (recovering, expect=old, pending kept)"

  log "selftest: tmp dir kept at $tmp (not removed automatically)"
  if [ "$SELFTEST_TOTAL_FAIL" -eq 0 ]; then
    echo "SELFTEST: 19/19 PASS"
    return 0
  fi
  echo "SELFTEST: $SELFTEST_TOTAL_FAIL/19 scenarios FAIL"
  return 1
}

# ============================================================================
# arguments / entrypoint
# ============================================================================

usage() {
  cat <<'EOF'
upgrade_qwenpaw_runtime.sh — in-place QwenPaw runtime upgrade, v1
(S0 pre-flight + S3 package swap + S4 restart + S5 health gate + S6 rollback + S2' record;
 lifecycle design = AgentTeams#1341 comment, 2026-10-11)

Usage:
  upgrade_qwenpaw_runtime.sh --target X.Y.Z [options]
  upgrade_qwenpaw_runtime.sh --selftest

Options:
  --target X.Y.Z        target version (required unless --selftest; PEP440 forms like 2.2.2b4 supported)
  --checker PATH        compatibility checker script (default: check_qwenpaw_upgrade.py
                        next to this script; S0 is skipped with a warning when neither
                        --checker nor the default is found)
  --skip-check          skip the checker (only meaningful when --checker is given)
  --container NAME      docker backend: run on the host and exec/restart container NAME
                        (venv defaults to /opt/venv/qwenpaw; restart primitive =
                        docker restart NAME)
  --venv PATH           runtime venv (overrides the backend default)
  --uv PATH             uv binary (forces the uv install form; when unset the
                        install command auto-detects uv and falls back to venv pip)
  --restart-mode MODE   supervisorctl (in-container default) | kill
  --timeout N           health-gate timeout in seconds (default 90)
  --settle N            post-match settle window in seconds (default 65; 0
                        disables). After the version first matches, the runtime
                        must stay alive and version-stable for this long; it
                        catches a delayed wrapper crash the raw version
                        endpoint (served by the app subprocess) would not reveal
  --phase PH            full (default, original flow) | pre-restart | boot-check
                        | post-start
                        (rollback deferral: boot-check/post-start leave the job in
                        the NON-terminal state `recovering` with pending.json kept —
                        expect=old in pending.json marks that the next post-start
                        must verify the RESTORED OLD runtime before finalizing
                        rolled_back)
  --job-id ID           job id (required for pre-restart; boot-check/post-start
                        take it from pending.json when omitted)
  --state-file PATH     state file for pre-restart (boot-check/post-start
                        recover it from the pending.json state_file field, else
                        by the sibling convention: pending.json and
                        upgrade-<job-id>.json live in the same dir)
  --pending-file PATH   pending.json (default $QWENPAW_WORKING_DIR/upgrade/pending.json)
  --force-rollback      boot-check only: roll back even if the version matches
  --record-file PATH    write a JSON version record after a successful upgrade
                        (verified, then recorded)
  --dry-run             print the resolved commands only; no install/restart/health gate
                        (full phase only)
  --selftest            run the built-in stubbed battery (19 scenarios, no real actions)
  -h, --help            this help

Environment hooks (all overridable; selftest relies on this mechanism):
  UG_VERSION_CMD / UG_INSTALL_CMD({target}) / UG_PATCH_CMD({target}) /
  UG_RESTART_CMD / UG_CHECKER_CMD({target}) / UG_ALIVE_CMD
  UG_VENV (default /app/venv, /opt/venv/qwenpaw with --container) / UG_UV (uv path;
  when unset the install command auto-detects uv and falls back to venv pip)
  UG_TIMEOUT (health-gate timeout seconds, default 90) / UG_SETTLE (settle window
  seconds, default 65; 0 disables) / UG_SETTLE_POLL (settle poll interval, default 5)
  — env-level defaults; explicit --timeout/--settle flags still win

Exit codes (full/pre-restart phases):
  0 upgraded   2 pre-flight/usage failure (nothing installed)
  3 rolled back   4 rollback failed (manual intervention needed)
Exit codes (post-start phase, consumed by the entrypoint as the verdict):
  0 the CURRENT runtime passed its verification gate — keep it
    (the new version on a success path; the restored OLD version when
    pending.json carries expect=old)
  3 restore done: kill mode = UNVERIFIED handoff (the entrypoint must relaunch
    the old runtime and re-run post-start, whose expect=old branch performs
    the verification); supervisorctl mode = restored runtime verified in-process
  4 restore and/or verification FAILED (manual intervention needed; the job is
    rollback_failed and pending.json is kept — never reads as a recovery)
EOF
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --target)
        [ $# -ge 2 ] || { err "--target missing value"; exit 2; }
        TARGET="$2"; shift 2 ;;
      --target=*)     TARGET="${1#*=}"; shift ;;
      --checker)
        [ $# -ge 2 ] || { err "--checker missing value"; exit 2; }
        CHECKER="$2"; shift 2 ;;
      --checker=*)    CHECKER="${1#*=}"; shift ;;
      --skip-check)   SKIP_CHECK=1; shift ;;
      --container)
        [ $# -ge 2 ] || { err "--container missing value"; exit 2; }
        CONTAINER="$2"; shift 2 ;;
      --container=*)  CONTAINER="${1#*=}"; shift ;;
      --venv)
        [ $# -ge 2 ] || { err "--venv missing value"; exit 2; }
        VENV="$2"; shift 2 ;;
      --venv=*)       VENV="${1#*=}"; shift ;;
      --uv)
        [ $# -ge 2 ] || { err "--uv missing value"; exit 2; }
        UG_UV="$2"; shift 2 ;;
      --uv=*)         UG_UV="${1#*=}"; shift ;;
      --record-file)
        [ $# -ge 2 ] || { err "--record-file missing value"; exit 2; }
        RECORD_FILE="$2"; shift 2 ;;
      --record-file=*) RECORD_FILE="${1#*=}"; shift ;;
      --restart-mode)
        [ $# -ge 2 ] || { err "--restart-mode missing value"; exit 2; }
        RESTART_MODE="$2"; shift 2 ;;
      --restart-mode=*) RESTART_MODE="${1#*=}"; shift ;;
      --phase)
        [ $# -ge 2 ] || { err "--phase missing value"; exit 2; }
        PHASE="$2"; shift 2 ;;
      --phase=*)    PHASE="${1#*=}"; shift ;;
      --job-id)
        [ $# -ge 2 ] || { err "--job-id missing value"; exit 2; }
        JOB_ID="$2"; shift 2 ;;
      --job-id=*)   JOB_ID="${1#*=}"; shift ;;
      --state-file)
        [ $# -ge 2 ] || { err "--state-file missing value"; exit 2; }
        STATE_FILE="$2"; shift 2 ;;
      --state-file=*) STATE_FILE="${1#*=}"; shift ;;
      --pending-file)
        [ $# -ge 2 ] || { err "--pending-file missing value"; exit 2; }
        PENDING_FILE="$2"; shift 2 ;;
      --pending-file=*) PENDING_FILE="${1#*=}"; shift ;;
      --force-rollback) FORCE_ROLLBACK=1; shift ;;
      --timeout)
        [ $# -ge 2 ] || { err "--timeout missing value"; exit 2; }
        TIMEOUT="$2"; shift 2 ;;
      --timeout=*)    TIMEOUT="${1#*=}"; shift ;;
      --settle)
        [ $# -ge 2 ] || { err "--settle missing value"; exit 2; }
        SETTLE="$2"; shift 2 ;;
      --settle=*)     SETTLE="${1#*=}"; shift ;;
      --dry-run)      DRY_RUN=1; shift ;;
      --selftest)     SELFTEST=1; shift ;;
      -h|--help)      usage; exit 0 ;;
      *)              err "unknown argument: $1"; usage; exit 2 ;;
    esac
  done
}

main() {
  parse_args "$@"

  # selftest first: fully stubbed, no real defaults, no --target required
  if [ "$SELFTEST" -eq 1 ]; then
    run_selftest
    exit $?
  fi

  # ---------- phase validation ----------
  case "$PHASE" in
    full|pre-restart|boot-check|post-start) ;;
    *) err "invalid --phase '$PHASE' (expected: full | pre-restart | boot-check | post-start)"; exit 2 ;;
  esac
  if [ -n "$CONTAINER" ] && [ "$PHASE" != "full" ]; then
    err "--container (docker backend) only supports --phase full; two-phase runs in-container"
    exit 2
  fi

  # ---------- backend mode ----------
  if [ -n "$CONTAINER" ]; then
    BACKEND="docker"
    command -v docker >/dev/null 2>&1 \
      || { err "docker backend requires the docker CLI on the host"; exit 2; }
  fi

  # boot-check and post-start take their target from pending.json; the other
  # phases need it
  if [ "$PHASE" != "boot-check" ] && [ "$PHASE" != "post-start" ]; then
    [ -n "$TARGET" ] || { err "--target is required"; usage; exit 2; }
  fi
  if [ "$PHASE" = "pre-restart" ] && [ -z "$JOB_ID" ]; then
    err "pre-restart phase requires --job-id (plus --state-file/--pending-file from the plugin)"
    exit 2
  fi
  case "$TIMEOUT" in
    ''|*[!0-9]*) err "--timeout must be a positive integer (got '$TIMEOUT')"; exit 2 ;;
  esac
  [ "$TIMEOUT" -ge 1 ] || { err "--timeout must be >= 1"; exit 2; }

  # two-phase file defaults (the full phase ignores both)
  [ -n "$PENDING_FILE" ] || PENDING_FILE="${QWENPAW_WORKING_DIR:-.}/upgrade/pending.json"
  if [ -z "$STATE_FILE" ] && [ -n "$JOB_ID" ] && [ "$PHASE" != "full" ]; then
    STATE_FILE="${QWENPAW_WORKING_DIR:-.}/upgrade/upgrade-$JOB_ID.json"
  fi

  resolve_defaults

  # ---------- dry-run: print only, do not execute ----------
  if [ "$DRY_RUN" -eq 1 ]; then
    if [ "$PHASE" != "full" ]; then
      err "--dry-run is only supported for the full phase"
      exit 2
    fi
    log "dry-run: printing the resolved commands only (no install/restart/health gate)"
    if [ "$BACKEND" = "docker" ]; then
      log "  backend : docker (container=$CONTAINER)"
    else
      log "  backend : in-container (restart-mode=$RESTART_MODE)"
    fi
    if [ -n "$CHECKER" ] && [ "$SKIP_CHECK" -eq 0 ]; then
      log "  0) check   : $(sub_target "$UG_CHECKER_CMD" "$TARGET")"
    fi
    if [ -n "$UG_PRE_INSTALL_CMD" ]; then
      log "  1a) ensure : $UG_PRE_INSTALL_CMD"
    fi
    log "  1) install : $(sub_target "$UG_INSTALL_CMD" "$TARGET")"
    log "  2) restart : $UG_RESTART_CMD"
    log "  3) health  : $UG_VERSION_CMD (poll until output == '$TARGET', timeout ${TIMEOUT}s; settle ${SETTLE}s)"
    if [ -n "$RECORD_FILE" ]; then
      log "  4) record  : $RECORD_FILE (written only after the health gate is all green)"
    fi
    exit 0
  fi

  case "$PHASE" in
    full)        do_upgrade ;;
    pre-restart) do_pre_restart ;;
    boot-check)  do_boot_check ;;
    post-start)  do_post_start ;;
  esac
}

# Default resolution (env hooks win); shared by main and selftest
resolve_defaults() {
  if [ -z "$WHEELHOUSE_DIR" ]; then
    WHEELHOUSE_DIR="${UG_WHEELHOUSE:-${QWENPAW_WORKING_DIR:-.}/upgrade/wheelhouse}"
  fi
  case "$RESTART_MODE" in
    supervisorctl) DEFAULT_RESTART="supervisorctl restart app" ;;
    # The worker is two processes: the qwenpaw-worker wrapper (PID 1) blocks on
    # the qwenpaw app child (the 8088 server). Killing the app makes the
    # wrapper's wait() return and the wrapper exits, so the container stops and
    # the runtime (restart policy / K8s) restarts it -> entrypoint runs the
    # boot-check (phase 2). The slim image has no ps/pkill, so scan /proc. The
    # cmdline is NUL-separated, so "qwenpaw" and "app" are separate fields —
    # convert NULs to spaces first, then match the server's distinctive
    # "qwenpaw app --host" (the wrapper's cmdline is "qwenpaw-worker", no
    # "app --host"). Exclude $$ so this shell (which carries the pattern in its
    # own cmdline) is not killed.
    kill)          DEFAULT_RESTART='for d in /proc/[0-9]*; do [ -r "$d/cmdline" ] || continue; p=$(basename "$d"); [ "$p" = "$$" ] && continue; c=$(tr "\0" " " < "$d/cmdline" 2>/dev/null); case "$c" in *"qwenpaw app --host"*) kill -9 "$p" 2>/dev/null; break;; esac; done' ;;
    *)             err "invalid --restart-mode: '$RESTART_MODE' (expected supervisorctl | kill)"; exit 2 ;;
  esac

  # venv resolution: UG_VENV hook > --venv > backend default
  if [ -z "$UG_VENV" ]; then
    if [ -n "$VENV" ]; then
      UG_VENV="$VENV"
    elif [ "$BACKEND" = "docker" ]; then
      UG_VENV="/opt/venv/qwenpaw"
    else
      UG_VENV="/app/venv"
    fi
  fi
  # Install tool: the runtime image may ship uv (fast path, surgical
  # --reinstall-package) or only pip (AgentTeams worker images, v1.2.x).
  # Explicit UG_UV/--uv forces the uv form; otherwise the install command
  # auto-detects at execution time and falls back to the venv pip with the
  # equivalent surgical flags (--force-reinstall --no-deps).

  if [ "$BACKEND" = "docker" ]; then
    # Host-side run: version probe and swap execute inside the container; restart = docker restart (writable layer kept)
    [ -n "$UG_VERSION_CMD" ] || UG_VERSION_CMD="docker exec $CONTAINER sh -c 'curl -fsS $VERSION_ENDPOINT || $UG_VENV/bin/qwenpaw --version'"
    if [ -n "$UG_UV" ]; then
      [ -n "$UG_INSTALL_CMD" ] || UG_INSTALL_CMD="docker exec $CONTAINER sh -c 'VIRTUAL_ENV=$UG_VENV $UG_UV pip install --reinstall-package qwenpaw qwenpaw=={target}'"
    else
      [ -n "$UG_INSTALL_CMD" ] || UG_INSTALL_CMD="docker exec $CONTAINER sh -c 'if command -v uv >/dev/null 2>&1; then VIRTUAL_ENV=$UG_VENV uv pip install --reinstall-package qwenpaw qwenpaw=={target}; else $UG_VENV/bin/pip install --force-reinstall --no-deps qwenpaw=={target}; fi'"
    fi
    [ -n "$UG_PATCH_CMD" ] || UG_PATCH_CMD="docker exec $CONTAINER sh -c '$UG_VENV/bin/python /opt/agentteams/scripts/patch-qwenpaw-driver-policy-reload.py && $UG_VENV/bin/python /opt/agentteams/scripts/patch-qwenpaw-matrix-cron-final.py'"
    [ -n "$UG_RESTART_CMD" ] || UG_RESTART_CMD="docker restart $CONTAINER"
    [ -n "$UG_ALIVE_CMD" ] || UG_ALIVE_CMD="docker inspect -f '{{.State.Running}}' $CONTAINER"
    # The rollback's `docker exec ... pip install` needs a running container;
    # after a wrapper crash the container is stopped, so start it first.
    [ -n "$UG_PRE_INSTALL_CMD" ] || UG_PRE_INSTALL_CMD='if [ "$(docker inspect -f "{{.State.Running}}" '"$CONTAINER"' 2>/dev/null)" != "true" ]; then echo "[upgrade] pre-install: container not running, starting it"; docker start '"$CONTAINER"'; fi'
  else
    if [ "$PHASE" = "boot-check" ]; then
      # The app is not up yet at boot-check time (this runs before
      # `exec qwenpaw-worker`): read the version from the venv metadata
      # directly instead of probing the live API.
      [ -n "$UG_VERSION_CMD" ] || UG_VERSION_CMD="$UG_VENV/bin/python -c 'import importlib.metadata as m; print(m.version(\"qwenpaw\"))'"
    else
      [ -n "$UG_VERSION_CMD" ] || UG_VERSION_CMD="curl -fsS $VERSION_ENDPOINT || $UG_VENV/bin/qwenpaw --version"
    fi
    if [ -n "$UG_UV" ]; then
      [ -n "$UG_INSTALL_CMD" ] || UG_INSTALL_CMD="VIRTUAL_ENV=$UG_VENV $UG_UV pip install --reinstall-package qwenpaw qwenpaw=={target}"
    else
      [ -n "$UG_INSTALL_CMD" ] || UG_INSTALL_CMD="sh -c 'if command -v uv >/dev/null 2>&1; then VIRTUAL_ENV=$UG_VENV uv pip install --reinstall-package qwenpaw qwenpaw=={target}; else $UG_VENV/bin/pip install --force-reinstall --no-deps qwenpaw=={target}; fi'"
    fi
    local _pd
    _pd="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    [ -n "$UG_PATCH_CMD" ] || UG_PATCH_CMD="$UG_VENV/bin/python $_pd/patch-qwenpaw-driver-policy-reload.py && $UG_VENV/bin/python $_pd/patch-qwenpaw-matrix-cron-final.py"
    [ -n "$UG_RESTART_CMD" ] || UG_RESTART_CMD="$DEFAULT_RESTART"
  fi

  # S0: without an explicit --checker, fall back to the co-located pre-flight checker (merged via AgentTeams#1343)
  if [ -z "$CHECKER" ]; then
    local dir
    dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    if [ -f "$dir/check_qwenpaw_upgrade.py" ]; then
      CHECKER="$dir/check_qwenpaw_upgrade.py"
    else
      log "WARNING: default checker check_qwenpaw_upgrade.py not found; S0 pre-flight skipped"
    fi
  fi
  if [ -n "$CHECKER" ] && [ "$SKIP_CHECK" -eq 0 ] && [ -z "$UG_CHECKER_CMD" ]; then
    if [ "$PHASE" = "pre-restart" ]; then
      # venv python (the system python has no qwenpaw_worker) and the
      # wheelhouse: C3 is MANDATORY in pre-restart (a missing wheel is a FAIL,
      # and the app cannot download after the restart).
      UG_CHECKER_CMD="$UG_VENV/bin/python $CHECKER --target {target} --in-place --wheelhouse $WHEELHOUSE_DIR"
    else
      UG_CHECKER_CMD="python3 $CHECKER --target {target} --in-place"
    fi
  fi
}

main "$@"
