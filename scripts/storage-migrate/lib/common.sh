#!/usr/bin/env bash
# common.sh - shared library for the MinIO -> RustFS migration tool (stable function contract)
# Design: docs/design/storage-external-s3-backend.md (migration section)
# Dependencies: bash>=4, mc, curl, jq (checked by require_cmd); file locking via flock
set -euo pipefail

SCRIPT_VERSION="0.3.0-oneclick"

# ---------- globals (populated by parse_args) ----------
MODE="odm"                 # odm|mirror|inplace
PROFILE="generic"          # generic|docker-local|docker-nfs|k8s-rwo|k8s-rwx|agentteams
PHASE="all"                # all|quiesce|data|iam|verify|cutover|mark-cutover-done
BUCKETS=""                 # comma separated; default = every source bucket (filled after P0)
SHADOW_DAYS=14             # default shadow period (P0-13)
STATE_DIR=""               # default ./storage-migrate-state
DRY_RUN=false
STATUS_MODE=false           # --status: print the machine-readable status and exit
CUTOVER=false
# --- one-command stop-the-world orchestration (maintainer 3rd round, 2026-11-01) ---
DEPLOYMENT=""                 # embedded | helm | "" ("" = legacy data-plane flow P0->P4 only)
NS="default"                  # helm: k8s namespace
HELM_RELEASE="agentteams"
HELM_CHART="helm/agentteams"
S3_SECRET=""                  # helm: EXISTING k8s Secret holding the s3 keys (the tool never creates it)
CONTROLLER_CONTAINER="agentteams-controller"
CONTAINER_PREFIX="agentteams-"  # resource prefix (AGENTTEAMS_RESOURCE_PREFIX default)
QUIESCE_INTERVAL=10           # window-evidence sampling interval (s)
QUIESCE_SAMPLES=3             # window-evidence sample count (3 equal samples = freeze proven)
BACKUP_ROOT=""                # original-config snapshot root (default ./storage-migrate-backup)
RESUME=false                  # --resume: continue the chain from the current stage
ROLLBACK=false                # --rollback: restore original config + writer states
DST_DURABILITY="strict"    # declarative assertion (P0-15)
SRC_EP="" SRC_AK="" SRC_SK=""
DST_EP="" DST_AK="" DST_SK=""
DST_DATA_DIR=""            # host path of the target data directory (P0-4/5/14)
IAM_SECRETS_FILE=""        # JSON: {"<access-key>": "<secret>"} (IAM replay secret source; secrets are not exportable via the API)
BACKFILL_TIMEOUT=21600     # 6h
SRC_ALIAS="migrate_src"
DST_ALIAS="migrate_dst"
MC_CONFIG_DIR=""
LOG_FILE=""
STATE_FILE=""
REPORT_FILE=""
STATE_INT_LOCK=""

# ---------- logging (stderr + $LOG_FILE once parse_args has set it) ----------
# #1413: the run record is also appended to $LOG_FILE, so a caller whose
# consumer closes the pipe early (the terminal/CI tee losing the tail) cannot
# lose the output of the failing run. The file write is best-effort: a
# read-only state dir must not turn a log into a run killer.
_ts() { date +%Y-%m-%dT%H:%M:%S%z; }
_log() { # <tag e.g. "INFO "> <msg...> - stderr always; file when $LOG_FILE is set
  local line="[$(_ts)] [$1] ${*:2}"
  if [ -n "${LOG_FILE:-}" ]; then
    printf '%s\n' "$line" >>"$LOG_FILE" 2>/dev/null || true
  fi
  printf '%s\n' "$line" >&2
}
log_info()  { _log "INFO " "$@"; }
log_warn()  { _log "WARN " "$@"; }
log_fail()  { _log "FAIL " "$@"; }
die()       { local code="$1"; shift; log_fail "$*"; exit "$code"; }

# ---------- trap: leave a trace on interrupt + resume hint (exit code 130) ----------
on_interrupt() {
  local msg="interrupted (SIGINT/SIGTERM)"
  if [ -n "${STATE_FILE:-}" ] && [ -f "${STATE_FILE}" ]; then
    state_set interrupted true 2>/dev/null || true
    log_warn "$msg - state.json marked interrupted=true"
  else
    log_warn "$msg"
  fi
  log_info "resume: re-run the same command (the state machine continues from the current stage)"
  exit 130
}
trap on_interrupt INT TERM

# ---------- usage ----------
usage() {
  cat <<'EOF'
storage-migrate.sh - MinIO -> RustFS storage-plane migration (backfill + cutover)

One command (stop-the-world cutover, maintainer 3rd round):
  storage-migrate.sh --src-endpoint http://SRC:9000 --src-ak AK --src-sk SK \
                     --dst-endpoint http://DST:9000 --dst-ak AK --dst-sk SK \
                     --deployment embedded --phase all --cutover \
                     [--dst-data-dir /host/path] ...

  One command runs P0..P4 (preflight -> freeze writers -> data+IAM -> final
  delta+verify), then P5..P8 (switch controller config -> recreate consumers ->
  per-component read/write+permission verification -> resume + observation).
  Supported deployment mode: embedded (single-node, installer shape) = verified;
  helm = structurally supported, NOT end-to-end verified (see docs).

Usage:
  storage-migrate.sh --src-endpoint http://SRC:9000 --src-ak AK --src-sk SK \
                     --dst-endpoint http://DST:9000 --dst-ak AK --dst-sk SK \
                     [--buckets a,b,c] [--mode odm|mirror|inplace] \
                     [--profile generic|docker-local|docker-nfs|k8s-rwo|k8s-rwx|agentteams] \
                     [--phase all|quiesce|data|iam|verify|cutover|mark-cutover-done] \
                     [--dst-data-dir /host/path] [--shadow-days 14] \
                     [--backfill-timeout 21600] [--dst-durability strict|relaxed|none] \
                     [--iam-secrets-file /path/to/secrets.json] \
                     [--state-dir DIR] [--dry-run] [--status] [--cutover] \
                     [--resume] [--rollback] \
                     [--deployment embedded|helm] [--ns NS] [--helm-release R] \
                     [--helm-chart C] [--s3-secret S] [--controller-container N] \
                     [--container-prefix P] [--quiesce-interval 10] [--quiesce-samples 3] \
                     [--backup-root DIR]

Phases (stage machine in state.json):
  P0 preflight (P0-1..16, incl. writer enumeration + original-config snapshot)
  P1 quiesce  (stop writers with real mechanisms + quiesce-window evidence;
               needs --deployment; embedded controller stop is a documented
               manual step asserted by the tool - it hosts the source data)
  P2 data     (odm/mirror/inplace backfill)
  P3 IAM      (user/policy/service-credentials replay)
  P4 verify   (FINAL delta + V1-V9; runs only after P1 - the source is still
               readable at this point, so the final delta is possible)
  P5 switch   (original config fully snapshotted to the backup dir, then the
               controller env is switched to the new backend and re-asserted)
  P6 recreate (consumers recreated with their ORIGINAL states; per-component
               env assertion prints endpoint/bucket/prefix)
  P7 component (per-component read/write/list/delete probes with each component's
               OWN identity + cross-partition 403 negative + human-file-area check)
  P8 resume   (resume writers, observation window, rollback command printed)

  --phase all (default) = P0..P4; with --deployment P1 runs in between;
  --cutover (with --deployment) = one command P0..P8.
  --phase cutover = P5..P8 (requires state=VERIFIED, then --deployment).
  --resume continues from the last finished stage; --rollback restores the
  original config (if already switched) and the original writer states.
  --mark-cutover-done acknowledges a manually completed cutover (any post-verify
  stage; for the automated flow P8 does this itself).
  --status prints the machine-readable status (stable JSON) and exits 0;
  it is the integration contract for a future API/UI (see README)
Exit codes: 0 ok - 1 general failure - 2 preflight red - 3 verify/IAM red - 4 usage error - 130 interrupted
EOF
}

# ---------- argument parsing ----------
parse_args() {
  local script="$1"; shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --src-endpoint)      SRC_EP="$2"; shift 2 ;;
      --src-ak)            SRC_AK="$2"; shift 2 ;;
      --src-sk)            SRC_SK="$2"; shift 2 ;;
      --dst-endpoint)      DST_EP="$2"; shift 2 ;;
      # Container-side address: consumers do not share the host's loopback.
      --dst-endpoint-in-container) DST_EP_IN_CONTAINER="$2"; shift 2 ;;
      --dst-ak)            DST_AK="$2"; shift 2 ;;
      --dst-sk)            DST_SK="$2"; shift 2 ;;
      --buckets)           BUCKETS="$2"; shift 2 ;;
      --mode)              MODE="$2"; shift 2 ;;
      --profile)           PROFILE="$2"; shift 2 ;;
      --phase)             PHASE="$2"; shift 2 ;;
      --mark-cutover-done) PHASE="mark-cutover-done"; shift ;;
      --dst-data-dir)      DST_DATA_DIR="$2"; shift 2 ;;
      --iam-secrets-file)  IAM_SECRETS_FILE="$2"; shift 2 ;;
      --shadow-days)       SHADOW_DAYS="$2"; shift 2 ;;
      --backfill-timeout)  BACKFILL_TIMEOUT="$2"; shift 2 ;;
      --dst-durability)    DST_DURABILITY="$2"; shift 2 ;;
      --state-dir)         STATE_DIR="$2"; shift 2 ;;
      --dry-run)           DRY_RUN=true; shift ;;
      --status)            STATUS_MODE=true; shift ;;
      --cutover)           CUTOVER=true; shift ;;
      --resume)            RESUME=true; shift ;;
      --rollback)          ROLLBACK=true; shift ;;
      --deployment)        DEPLOYMENT="$2"; shift 2 ;;
      --ns)                NS="$2"; shift 2 ;;
      --helm-release)      HELM_RELEASE="$2"; shift 2 ;;
      --helm-chart)        HELM_CHART="$2"; shift 2 ;;
      --s3-secret)         S3_SECRET="$2"; shift 2 ;;
      --controller-container) CONTROLLER_CONTAINER="$2"; shift 2 ;;
      --container-prefix)  CONTAINER_PREFIX="$2"; shift 2 ;;
      --quiesce-interval)  QUIESCE_INTERVAL="$2"; shift 2 ;;
      --quiesce-samples)   QUIESCE_SAMPLES="$2"; shift 2 ;;
      --backup-root)       BACKUP_ROOT="$2"; shift 2 ;;
      -h|--help)           usage; exit 0 ;;
      *) die 4 "unknown argument: $1 (see --help)" ;;
    esac
  done

  # profile presets (known defaults for the AgentTeams shape)
  case "$PROFILE" in
    agentteams)
      # odm/inplace need the target data directory on the host; require it explicitly
      : # (no implicit default: pass --dst-data-dir when the profile needs it)
      ;;
  esac

  case "$MODE" in odm|mirror|inplace) : ;;
    *) die 4 "--mode must be odm|mirror|inplace (got: $MODE)" ;; esac
  case "$DST_DURABILITY" in strict|relaxed|none) : ;;
    *) die 4 "--dst-durability must be strict|relaxed|none (got: $DST_DURABILITY)" ;; esac
  case "$DEPLOYMENT" in ""|embedded|helm) : ;;
    *) die 4 "--deployment must be embedded|helm (got: $DEPLOYMENT)" ;; esac
  case "$QUIESCE_INTERVAL" in ''|*[!0-9]*) die 4 "--quiesce-interval must be a positive integer (got: $QUIESCE_INTERVAL)" ;; esac
  [ "$QUIESCE_INTERVAL" -ge 1 ] || die 4 "--quiesce-interval must be >= 1 (got: $QUIESCE_INTERVAL)"
  case "$QUIESCE_SAMPLES" in ''|*[!0-9]*) die 4 "--quiesce-samples must be an integer (got: $QUIESCE_SAMPLES)" ;; esac
  [ "$QUIESCE_SAMPLES" -ge 2 ] || die 4 "--quiesce-samples must be >= 2 (got: $QUIESCE_SAMPLES)"
  [ "$QUIESCE_SAMPLES" -le 10 ] || die 4 "--quiesce-samples must be <= 10 (got: $QUIESCE_SAMPLES)"
  if [ "$CUTOVER" = true ] && [ -z "$DEPLOYMENT" ]; then
    die 4 "--cutover requires --deployment embedded|helm (one-command flow; without --deployment run the legacy P0..P4 and cut over manually)"
  fi
  if [ "$RESUME" = true ] && [ "$ROLLBACK" = true ]; then
    die 4 "--resume and --rollback are mutually exclusive"
  fi

  [ -n "$STATE_DIR" ] || STATE_DIR="./storage-migrate-state"
  STATE_FILE="$STATE_DIR/state.json"
  REPORT_FILE="$STATE_DIR/report.json"
  MC_CONFIG_DIR="$STATE_DIR/mc"
  LOG_FILE="$STATE_DIR/migrate.log"
  mkdir -p "$STATE_DIR" "$MC_CONFIG_DIR"
}

# ---------- dependency checks ----------
require_cmd() {
  local missing=() c
  for c in "$@"; do command -v "$c" >/dev/null 2>&1 || missing+=("$c"); done
  if [ ${#missing[@]} -gt 0 ]; then
    die 4 "missing commands: ${missing[*]} (install them; note: dl.min.io only ships x86_64 - on arm64 take the mc.linux-arm64.* asset from the minio/mc GitHub releases and verify with file (must be ELF) + --version, because a 404 returns an HTML page)"
  fi
}

# dependency version gate (0.2.0: 0.1.0 only checked presence, so production dependencies were never verified)
# measured baseline: mc RELEASE.2025-08-13 / curl 8.14 / jq 1.7 / bash 5.2 on arm64
require_deps() {
  require_cmd bash mc curl jq flock
  # mc binary integrity (guards against an HTML 404 page saved as a binary)
  mc --version >/dev/null 2>&1 || die 4 "mc --version failed - the binary is corrupt (file $(which mc) must be ELF)"
  # #1413: capture each probe's output into a variable FIRST, then filter the
  # variable. The previous shape (`cmd | head -1 | sed`, `cmd | grep -q`) put
  # an early-exiting consumer on the pipe: when the producer's later write hit
  # the closed end it died with SIGPIPE (rc=141), and under set -e + pipefail
  # that 141 rode up through the bare assignment and killed the whole run -
  # before the state write, so --mark-cutover-done left state=CUTOVER_PENDING
  # (the CI signature). No pipes remain in this function.
  local mcv mcv_raw mcv_first curl_ver curl_help has_sigv4=0
  mcv_raw=$(mc --version 2>/dev/null) || mcv_raw=""
  mcv_first=${mcv_raw%%$'\n'*}
  mcv=$(sed -n 's/.*RELEASE\.\([0-9][0-9T:-]*\).*/\1/p' <<<"$mcv_first")
  curl_ver=$(curl --version 2>/dev/null) || curl_ver=""
  curl_ver=${curl_ver%%$'\n'*}
  read -r _ curl_ver _ <<<"$curl_ver" || curl_ver=""
  # curl aws-sigv4 (hard requirement for the odm admin plane; fallback channel for admin SigV4)
  if curl_help=$(curl --help all 2>/dev/null); then
    case "$curl_help" in
      *aws-sigv4*) has_sigv4=1 ;;
    esac
  fi
  if [ "$has_sigv4" -eq 0 ]; then
    if [ "$MODE" = "odm" ]; then
      die 4 "curl lacks --aws-sigv4 (needs curl >= 7.88, have ${curl_ver:-unknown}): the odm admin plane is unavailable. Upgrade curl, run inside an image with curl >= 7.88, or use --mode mirror"
    fi
    log_warn "curl lacks aws-sigv4 (${curl_ver:-unknown}) - odm unavailable; admin SigV4 fallback degrades (mc admin only)"
  fi
  log_info "dependency gate: bash=$BASH_VERSION mc=RELEASE.${mcv:-unknown} curl=${curl_ver:-unknown} jq=$(jq --version 2>/dev/null) aws-sigv4=$has_sigv4"
}

# ---------- robust mc admin list wrapper (0.2.0: 0.1.0 lost users/policies silently in two places) ----------
# facts (measured with mc RELEASE.2025-08-13):
#   mc admin user list --json = NDJSON (one {status,accessKey,userStatus,policyName} object per line), not an array
#   mc admin user list / policy list tables have no header (0.1.0 assumed one and tail -n +2 dropped a real row = one user/policy lost)
# mc_admin_user_records <alias> -> TSV: access_key \t status \t policy; 1 when both channels fail (not "zero users")
mc_admin_user_records() {
  local alias_="$1" out rc
  out=$(mc_admin user list "$alias_" --json 2>/dev/null); rc=$?
  if [ $rc -eq 0 ] && [ -n "$out" ] && jq -cs '(.[0] | has("accessKey"))' <<<"$out" >/dev/null 2>&1; then
    jq -cr 'select(.status == "success") | [.accessKey, (.userStatus // "enabled"), (.policyName // "")] | @tsv' <<<"$out"
    return 0
  fi
  out=$(mc_admin user list "$alias_" 2>/dev/null); rc=$?
  if [ $rc -ne 0 ]; then
    log_fail "mc admin user list: both --json and table channels failed (table rc=$rc)"
    return 1
  fi
  awk 'NF >= 2 && $1 != "Status" {print $2 "\t" $1 "\t" $3}' <<<"$out"
}
# mc_admin_policy_names <alias> -> one policy name per line (no-header assumption; skips header-looking words defensively)
mc_admin_policy_names() {
  local alias_="$1" out rc
  out=$(mc_admin policy list "$alias_" 2>/dev/null); rc=$?
  if [ $rc -ne 0 ]; then
    log_fail "mc admin policy list failed (rc=$rc)"
    return 1
  fi
  awk 'NF >= 1 && $1 != "Policy" && $1 != "Name" {print $1}' <<<"$out"
}

# ---------- mc wrapper (isolated config dir; never touches ~/.mc) ----------
mc_raw() { MC_CONFIG_DIR="$MC_CONFIG_DIR" mc "$@"; }
mc_alias() { # <name> <endpoint> <ak> <sk>
  # RustFS can briefly answer health probes before its storage quorum is
  # ready; mc then returns "Service not ready: waiting for storage_quorum".
  # Retry the alias setup instead of treating that startup race as a fatal
  # preflight failure. Secrets never appear in the retry log.
  local name="$1" endpoint="$2" ak="$3" sk="$4"
  local attempts="${STORAGE_MIGRATE_ALIAS_RETRIES:-30}" i
  case "$attempts" in ''|*[!0-9]*|0) attempts=30 ;; esac
  for ((i = 1; i <= attempts; i++)); do
    if mc_raw alias set "$name" "$endpoint" "$ak" "$sk" >/dev/null 2>&1; then
      return 0
    fi
    [ "$i" -lt "$attempts" ] && sleep 1
  done
  echo "mc alias set failed after ${attempts} attempts (endpoint ${endpoint})" >&2
  return 1
}
mc_admin() { mc_raw admin "$@"; }
mc_get()   { mc_raw cat "$@"; }
mc_put()   { # <content> <target-alias/path> (newer mc has no cp -; stream via stdin)
  printf '%s' "$1" | mc_raw pipe "$2" >/dev/null 2>&1
}

# ---------- admin API (SigV4 via curl 8.x native signing; hand-rolled signing is forbidden) ----------
# admin_sigv4 <src|dst> <method> <path> [json-body]
# prints the response body; callers judge body/HTTP code (the function always exits 0; HTTP-level failures yield an empty body)
admin_sigv4() {
  local side="$1" method="$2" path="$3" body="${4:-}"
  local ep ak sk
  if [ "$side" = "src" ]; then ep="$SRC_EP"; ak="$SRC_AK"; sk="$SRC_SK"
  else ep="$DST_EP"; ak="$DST_AK"; sk="$DST_SK"; fi
  local hostport="${ep#http://}"
  hostport="${hostport#https://}"
  local args=(-sS --max-time 60 --aws-sigv4 "aws:amz:us-east-1:s3" -u "${ak}:${sk}"
              -H "x-amz-content-sha256: UNSIGNED-PAYLOAD" -H "Content-Type: application/json"
              -X "$method" -w "\n__HTTP_CODE__:%{http_code}")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" "http://${hostport}${path}" 2>/dev/null || true
}
# split admin_sigv4 output: body + last line __HTTP_CODE__
split_http_code() { # <full-output> -> sets REPLY_BODY / REPLY_CODE
  REPLY_CODE=$(printf '%s' "$1" | grep -o '__HTTP_CODE__:[0-9]*' | tail -1 | cut -d: -f2)
  REPLY_BODY=$(printf '%s' "$1" | sed '$d')
  [ -n "$REPLY_CODE" ] || REPLY_CODE=000
}
# engine detection: admin route probe (mc admin info does not work against RustFS - service info is unimplemented)
# GET /rustfs/admin/v3/health: RustFS 1.0.x = 501 (route registered, handler missing); MinIO/other S3 = 404
detect_engine() { # <alias> -> sets REPLY_ENGINE = rustfs | s3-generic
  local out
  out=$(admin_sigv4 dst GET "/rustfs/admin/v3/health" 2>/dev/null || true)
  split_http_code "$out"
  case "$REPLY_CODE" in
    501|503) REPLY_ENGINE="rustfs" ;;
    *) REPLY_ENGINE="s3-generic" ;;
  esac
}

# ---------- state.json (jq + flock atomic updates; every critical section runs in a subshell so fds are released on subshell exit - structurally leak-free) ----------
_state_init_locked() { # runs inside the lock-holding subshell
  local buckets_json="[]"
  if [ -n "$BUCKETS" ]; then
    buckets_json=$(printf '%s\n' ${BUCKETS//,/ } | jq -R . | jq -s .)
  fi
  jq -n --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg ver "$SCRIPT_VERSION" \
        --arg mode "$MODE" --arg profile "$PROFILE" \
        --arg srcep "$SRC_EP" --arg dstep "$DST_EP" \
        --argjson buckets "$buckets_json" --argjson dry "$DRY_RUN" \
        --argjson shadow "$SHADOW_DAYS" \
    '{version:1, script_version:$ver, created:$now, updated:$now, mode:$mode, profile:$profile,
      src:{endpoint:$srcep, buckets:$buckets}, dst:{endpoint:$dstep},
      stage:"INIT", stage_detail:{}, interrupted:false, dry_run:$dry, shadow_days:$shadow}' \
    > "$STATE_FILE"
}
state_init() {
  [ -f "$STATE_FILE" ] && return 0   # resuming: keep the existing state
  mkdir -p "$STATE_DIR"
  ( exec 9>"$STATE_DIR/.state.lock"; flock -w 30 9 || { log_fail "state lock timeout (30s)"; exit 1; }; _state_init_locked ) \
    || return 1
  log_info "state initialised: $STATE_FILE"
}
state_set() { # <dotted-path e.g. stage_detail.P0-1> <json-fragment e.g. "pass">
  ( exec 9>"$STATE_DIR/.state.lock"; flock -w 30 9 || exit 1
    local tmp="$STATE_FILE.tmp.$$"
    jq --arg p "$1" --argjson frag "$2" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
       'setpath($p | split("."); $frag) | .updated = $now' \
       "$STATE_FILE" > "$tmp" 2>/dev/null && mv "$tmp" "$STATE_FILE" \
    || log_fail "state_set failed: path=$1 frag=$2 (jq error; state.json may be invalid JSON - check $STATE_FILE)" ) \
    || return 1
}
state_stage() { # <new-stage>
  state_set stage "\"$1\""
  log_info "stage -> $1"
}
state_get() { # <jq-expr>
  ( exec 9>"$STATE_DIR/.state.lock"; flock -w 30 9 || exit 1
    jq -r "$1" "$STATE_FILE" 2>/dev/null || echo "null" )
}

# ---------- report.json accumulation ----------
report_add() { # <section> <item> <status> <evidence>
  local tmp="${REPORT_FILE}.tmp.$$"
  [ -f "$REPORT_FILE" ] || echo '{}' > "$REPORT_FILE"
  jq --arg s "$1" --arg i "$2" --arg st "$3" --arg e "$4" \
     '.[$s][$i] = {status:$st, evidence:$e, ts:(now|todate)}' "$REPORT_FILE" > "$tmp"
  mv "$tmp" "$REPORT_FILE"
}
report_write() {
  [ -f "$REPORT_FILE" ] || echo '{}' > "$REPORT_FILE"
  jq -S . "$REPORT_FILE"
}

# ---------- helpers ----------
get_buckets() { # space-separated bucket list from state (set by P0)
  state_get '.src.buckets | join(" ")'
}
now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }
# extract host:port from an endpoint
ep_hostport() { local e="$1"; e="${e#http://}"; e="${e#https://}"; echo "${e%%/*}"; }
# probe HTTP liveness (any path; 2xx/4xx both mean "service is up", 000 = unreachable)
probe_http() { # <url> <timeout-sec>
  curl -s -m "${2:-5}" -o /dev/null -w "%{http_code}" "$1" 2>/dev/null || echo 000
}
