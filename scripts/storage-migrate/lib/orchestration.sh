#!/usr/bin/env bash
# orchestration.sh - one-command stop-the-world migration orchestration (P1, P5-P8,
# rollback). Sourced by storage-migrate.sh AFTER lib/common.sh.
#
# Design: docs/design/storage-external-s3-backend.md + 三轮-一键停机迁移 (2026-11-01).
#
# Invariants:
#   * fail-closed: every production action (stop/switch/recreate) requires a
#     reachable deployment context; if it is unreachable the phase refuses.
#   * DRY_RUN: mutating commands are printed, never executed.
#   * evidence-first: "writers stopped" is proven by state assertions
#     (CR state=Stopped + container/pod absent) PLUS a window in which
#     object count, total bytes and the top-level prefix digest do not
#     change (object mtimes alone are NOT acceptable proof).
#   * the quiesce window is held by $STATE_DIR/quiesce.lock from P1 until
#     P8 (or --rollback). P4/P5 refuse to run without it.

# ---------- naming ----------
# member container/pod name (docker backend: containerPrefix+name; k8s: same
# name for the pod). Manager "default" is special-cased to <prefix>manager
# (internal/auth/prefix.go ManagerPodName), other managers <prefix>manager-<name>.
member_container_name() { # <kind: worker|manager> <name>
  case "$1" in
    worker)  echo "${CONTAINER_PREFIX}worker-$2" ;;
    manager)
      if [ "$2" = "default" ]; then echo "${CONTAINER_PREFIX}manager"
      else echo "${CONTAINER_PREFIX}manager-$2"; fi ;;
  esac
}

# exec_or_print <cmd...> - dry-run: print; real: execute (returns the cmd's rc)
exec_or_print() {
  if [ "$DRY_RUN" = true ]; then
    log_info "[dry-run] $*"
    return 0
  fi
  "$@"
}
# exec_or_print_sh <shell-line> - same, for lines that need shell syntax
exec_or_print_sh() {
  if [ "$DRY_RUN" = true ]; then
    log_info "[dry-run] $1"
    return 0
  fi
  bash -c "$1"
}

first_bucket() { # the (first) bucket the migration targets
  local b; b=$(get_buckets); echo "${b%% *}"
}

backup_root_path() {
  # Absolute: the snapshot is created in P0 and read again by P5, which may run
  # from a different working directory (a relative path there pointed at a
  # directory that never existed).
  [ -n "$BACKUP_ROOT" ] || BACKUP_ROOT="$PWD/storage-migrate-backup"
  echo "$BACKUP_ROOT"
}
backup_dir_current() { # the original-config snapshot dir (P0 wrote it) or ""
  state_get '.original_config_snapshot // empty'
}

# ---------- deployment context (fail-closed) ----------
deployment_reachable() { # 0 when the control plane can be queried
  case "$DEPLOYMENT" in
    embedded)
      command -v docker >/dev/null 2>&1 || return 1
      docker inspect "$CONTROLLER_CONTAINER" >/dev/null 2>&1 || return 1
      docker exec "$CONTROLLER_CONTAINER" agt get workers -o json >/dev/null 2>&1 || return 1
      return 0 ;;
    helm)
      command -v kubectl >/dev/null 2>&1 || return 1
      command -v helm >/dev/null 2>&1 || return 1
      kubectl -n "$NS" get workers >/dev/null 2>&1 || return 1
      helm status "$HELM_RELEASE" -n "$NS" >/dev/null 2>&1 || return 1
      return 0 ;;
    *) return 1 ;;
  esac
}
deployment_unreachable_msg() {
  case "$DEPLOYMENT" in
    embedded) echo "deployment 'embedded' unreachable (need: docker + container '$CONTROLLER_CONTAINER' + 'agt' inside it)" ;;
    helm)     echo "deployment 'helm' unreachable (need: kubectl -n $NS + helm release '$HELM_RELEASE')" ;;
    *) echo "no --deployment given (embedded|helm)" ;;
  esac
}

# container_present <container|pod name> -> 0/1
container_present() {
  case "$DEPLOYMENT" in
    embedded) docker inspect "$1" >/dev/null 2>&1 ;;
    helm)     kubectl -n "$NS" get pod "$1" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

# container_env_json <container|pod name> -> JSON env (embedded: .Config.Env
# string list; helm: .spec.containers[0].env name/value objects, flattened)
container_env_json() {
  case "$DEPLOYMENT" in
    embedded)
      docker inspect "$1" --format '{{json .Config.Env}}' 2>/dev/null
      ;;
    helm)
      kubectl -n "$NS" get pod "$1" -o jsonpath='{.spec.containers[0].env}' 2>/dev/null \
        | jq -c '[ .[] | {name:.name, value:(.value // (if .valueFrom.secretKeyRef then ("secret:"+.valueFrom.secretKeyRef.name) else null end))} ]' 2>/dev/null
      ;;
  esac
}

# env_value <env-json> <KEY> -> value or ""
env_value() {
  case "$DEPLOYMENT" in
    embedded) jq -r --arg k "$2" 'map(select(startswith($k + "="))) | first // "" | split("=")[1:] | join("=")' <<<"$1" 2>/dev/null ;;
    helm)     jq -r --arg k "$2" 'map(select(.name == $k)) | first.value // ""' <<<"$1" 2>/dev/null ;;
  esac
}

# list_members <worker|manager> -> JSON array [{kind,name,state}]
# normalises both sources: embedded 'agt get <res> -o json' ({workers:[{name,state}]} /
# {managers:[{name,phase}]}) and helm 'kubectl get <res> -o json' ({items:[{metadata.name,spec.state,status.phase}]}).
list_members() {
  local res="$1" out
  case "$DEPLOYMENT" in
    embedded)
      # `agt` subcommands are plural (`agt get workers|managers`); the singular
      # form is not a subcommand and made the enumeration silently empty.
      local sub="$res"
      case "$res" in worker) sub=workers ;; manager) sub=managers ;; esac
      out=$(docker exec "$CONTROLLER_CONTAINER" agt get "$sub" -o json 2>/dev/null) || { log_fail "agt get $sub failed"; return 1; } ;;
    helm)     out=$(kubectl -n "$NS" get "$res" -o json 2>/dev/null) || { log_fail "kubectl get $res failed"; return 1; } ;;
    *) log_fail "list_members: no --deployment"; return 1 ;;
  esac
  jq -c --arg kind "$res" '
    ( .workers // .managers // .items // [] ) as $arr
    | [ $arr[] | {
        kind:  $kind,
        name:  ((.name // .metadata.name) | tostring),
        state: ((.state // .spec.state // .phase // .status.phase // "Running") | tostring) } ]' \
    <<<"$out"
}

# enum_writers -> REPLY_WRITERS_JSON [{kind,name,state,container,present}]
enum_writers() {
  local w m plist c
  if ! w=$(list_members worker); then REPLY_WRITERS_JSON="[]"; return 1; fi
  if ! m=$(list_members manager); then REPLY_WRITERS_JSON="[]"; return 1; fi
  REPLY_WRITERS_JSON=$(jq -n --argjson w "$w" --argjson m "$m" --arg p "$CONTAINER_PREFIX" '
    [ $w[] | . + {container: ($p + "worker-" + .name), present: false} ]
    + [ $m[] | . + {container: (if .name == "default" then $p + "manager" else $p + "manager-" + .name end), present: false} ]')
  plist="[]"
  while IFS= read -r c; do
    [ -z "$c" ] && continue
    if container_present "$c"; then
      plist=$(jq --arg c "$c" '. + [$c]' <<<"$plist")
    fi
  done < <(jq -r '.[].container' <<<"$REPLY_WRITERS_JSON")
  # `$e` pins the element: inside the `$pl | index(...)` pipe `.` is $pl, so
  # `.container` there would index the array and abort the whole enumeration.
  REPLY_WRITERS_JSON=$(jq --argjson pl "$plist" 'map(. as $e | . + {present: ($pl | index($e.container) != null)})' <<<"$REPLY_WRITERS_JSON")
}

# ---------- writer stop/restore (real mechanisms, verified in-repo) ----------
# worker:  agt update worker <name> --state <s>            (cmd/agt/update.go has --state)
# manager: agt apply -f <yaml with spec.state>             (agt update manager has NO --state;
#          the PUT handler merges only the spec fields present - cmd/agt/apply.go)
# helm:    kubectl patch <kind> <name> --type merge
set_member_state() { # <kind> <name> <state>
  local kind="$1" name="$2" state="$3" f
  case "$DEPLOYMENT" in
    embedded)
      case "$kind" in
        worker)
          # CLI drift: current builds require `--name`, older ones take a
          # positional name. Probe once from the help text (read-only) instead
          # of guessing, and cache the answer for the rest of the run.
          if [ -z "${AGTU_WORKER_FORM:-}" ]; then
            if docker exec "$CONTROLLER_CONTAINER" agt update worker -h 2>&1 | grep -q -- '--name string'; then
              AGTU_WORKER_FORM=flag
            else
              AGTU_WORKER_FORM=positional
            fi
          fi
          if [ "$AGTU_WORKER_FORM" = "flag" ]; then
            exec_or_print docker exec "$CONTROLLER_CONTAINER" agt update worker --name "$name" --state "$state"
          else
            exec_or_print docker exec "$CONTROLLER_CONTAINER" agt update worker "$name" --state "$state"
          fi ;;
        manager)
          f="$STATE_DIR/manager-$name-state-$state.yaml"
          printf 'apiVersion: agentteams.io/v1beta1\nkind: Manager\nmetadata:\n  name: %s\nspec:\n  state: %s\n' "$name" "$state" > "$f"
          exec_or_print docker cp "$f" "$CONTROLLER_CONTAINER:/tmp/agentteams-migrate-$name.yaml"
          exec_or_print docker exec "$CONTROLLER_CONTAINER" agt apply -f "/tmp/agentteams-migrate-$name.yaml"
          ;;
      esac ;;
    helm)
      exec_or_print kubectl -n "$NS" patch "$kind" "$name" --type merge -p "{\"spec\":{\"state\":\"$state\"}}" ;;
  esac
}

# member_state_now <kind> <name> -> current state (for assertions)
member_state_now() {
  list_members "$1" 2>/dev/null | jq -r --arg n "$2" 'map(select(.name == $n) | .state) | first // "unknown"'
}

# delete_member_container <name> - embedded: docker rm -f; helm: delete pod
delete_member_container() {
  case "$DEPLOYMENT" in
    embedded) exec_or_print docker rm -f "$1" ;;
    helm)     exec_or_print kubectl -n "$NS" delete pod "$1" --wait=false ;;
    *) return 1 ;;
  esac
}

# assert_consumer_env <env-json> <label> -> 0 when the consumer env points at the
# TARGET backend (endpoint contains the target host:port; bucket + keys present).
# Prints the per-component env line (maintainer req: endpoint/bucket/prefix printed).
assert_consumer_env() {
  local eenv="$1" label="$2" ep bucket ak sk
  ep=$(env_value "$eenv" AGENTTEAMS_FS_ENDPOINT)
  bucket=$(env_value "$eenv" AGENTTEAMS_FS_BUCKET)
  ak=$(env_value "$eenv" AGENTTEAMS_FS_ACCESS_KEY)
  sk=$(env_value "$eenv" AGENTTEAMS_FS_SECRET_KEY)
  log_info "$label env: AGENTTEAMS_FS_ENDPOINT=${ep:-<empty>} AGENTTEAMS_FS_BUCKET=${bucket:-<empty>} FS_ACCESS_KEY=${ak:+(set)} FS_SECRET_KEY=${sk:+(set)}"
  [ -n "$ep" ] || { log_fail "$label: AGENTTEAMS_FS_ENDPOINT is empty in the container env"; return 1; }
  case "$ep" in
    *"$DST_EP"*) : ;;
    *$(ep_hostport "$DST_EP")*) : ;;
    *) log_fail "$label: endpoint '$ep' does not point at the target '$DST_EP'"; return 1 ;;
  esac
  [ -n "$bucket" ] || { log_fail "$label: AGENTTEAMS_FS_BUCKET empty"; return 1; }
  [ -n "$ak" ] || { log_fail "$label: AGENTTEAMS_FS_ACCESS_KEY empty"; return 1; }
  [ -n "$sk" ] || { log_fail "$label: AGENTTEAMS_FS_SECRET_KEY empty"; return 1; }
  return 0
}

# wait_for_container <name> [timeout-s] -> 0 when present
wait_for_container() {
  local name="$1" t="${2:-120}" i=0
  while [ "$i" -lt "$t" ]; do
    container_present "$name" && return 0
    sleep 3; i=$((i + 3))
  done
  return 1
}

# wait_for_container_absent <name> [timeout-s] -> 0 when absent (Stopped deletes containers)
wait_for_container_absent() {
  local name="$1" t="${2:-60}" i=0
  while [ "$i" -lt "$t" ]; do
    container_present "$name" || return 0
    sleep 3; i=$((i + 3))
  done
  return 1
}

# ---------- quiesce window evidence (maintainer req #4) ----------
# One sample over all source buckets: "count<TAB>bytes<TAB>top-prefix-digest".
# count/bytes from 'mc ls --recursive --json'; digest over the top-level prefix
# (first path segment) with per-prefix object counts - small even for huge buckets.
window_sample() {
  # Prints "<total_count>\t<total_bytes>\t<digest>" AND sets REPLY_SAMPLE.
  # The digest is the per-bucket TOP-LEVEL PREFIX distribution (prefix:count,...),
  # not just totals: moving an object from prefix-a/ to prefix-b/ keeps the count
  # and the bytes identical but must still change the digest, otherwise a
  # reshuffling bucket would look frozen.
  local buckets b raw n bytes dist total_n=0 total_bytes=0 parts="" digest_all
  buckets=$(get_buckets)
  for b in $buckets; do
    raw=$(mc_raw ls --recursive --json "$SRC_ALIAS/$b" 2>/dev/null) || { REPLY_SAMPLE=""; return 1; }
    n=$(jq -s 'length' <<<"$raw" 2>/dev/null) || { REPLY_SAMPLE=""; return 1; }
    bytes=$(jq -s 'map(.size // 0) | add // 0' <<<"$raw" 2>/dev/null) || { REPLY_SAMPLE=""; return 1; }
    case "$n" in ''|*[!0-9]*) REPLY_SAMPLE=""; return 1 ;; esac
    case "$bytes" in ''|*[!0-9]*) REPLY_SAMPLE=""; return 1 ;; esac
    dist=$(jq -rs 'map(.key // "") | map(split("/")[0]) | sort | group_by(.) | map(.[0] + ":" + (length | tostring)) | join(",")' <<<"$raw" 2>/dev/null) || { REPLY_SAMPLE=""; return 1; }
    [ -n "$dist" ] || { REPLY_SAMPLE=""; return 1; }
    total_n=$((total_n + n))
    total_bytes=$((total_bytes + bytes))
    parts="$parts$b|$dist;"
  done
  digest_all=$(printf '%s' "$parts" | md5sum | awk '{print $1}')
  [ -n "$digest_all" ] || { REPLY_SAMPLE=""; return 1; }
  REPLY_SAMPLE="$total_n	$total_bytes	$digest_all"
  printf '%s\n' "$REPLY_SAMPLE"
}

# window_evidence -> REPLY_OK=0/1, REPLY_WINDOW_JSON (the samples, for state.json)
# FAILS when any of the QUIESCE_SAMPLES samples differs from the first one -
# a change in count/bytes/top-level-prefix-digest means a writer is still active.
window_evidence() {
  local i=0 s first="" count bytes digest
  local samples_json="[]"
  while [ "$i" -lt "$QUIESCE_SAMPLES" ]; do
    i=$((i + 1))
    if ! s=$(window_sample); then
      log_fail "quiesce window: sample $i/$QUIESCE_SAMPLES failed (source listing error) - the freeze cannot be proven"
      REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1
    fi
    if [ -z "${s:-}" ]; then
      log_fail "quiesce window: sample $i/$QUIESCE_SAMPLES produced no data (empty sample) - the freeze cannot be proven"
      REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1
    fi
    count=$(cut -f1 <<<"$s"); bytes=$(cut -f2 <<<"$s"); digest=$(cut -f3 <<<"$s")
    case "$count" in ''|*[!0-9]*) log_fail "quiesce window: invalid count '${count}' in sample $i - refusing to treat it as evidence"; REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1 ;; esac
    case "$bytes" in ''|*[!0-9]*) log_fail "quiesce window: invalid bytes '${bytes}' in sample $i - refusing to treat it as evidence"; REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1 ;; esac
    [ -n "$digest" ] || { log_fail "quiesce window: empty prefix digest in sample $i - refusing to treat it as evidence"; REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1; }
    samples_json=$(jq --argjson c "${count:--1}" --argjson b "${bytes:--1}" --arg d "$digest" --arg t "$(now_iso)" \
      '. + [{count:$c, bytes:$b, prefix_digest:$d, at:$t}]' <<<"$samples_json")
    if [ -z "$first" ]; then first="$count $bytes $digest"
    elif [ "$count $bytes $digest" != "$first" ]; then
      log_fail "quiesce window: sample $i differs from sample 1 (count/bytes/top-level-prefix digest changed) - a writer is still active"
      REPLY_OK=1; REPLY_WINDOW_JSON="$samples_json"; return 1
    fi
    [ "$i" -lt "$QUIESCE_SAMPLES" ] && sleep "$QUIESCE_INTERVAL"
  done
  REPLY_OK=0; REPLY_WINDOW_JSON="$samples_json"
}

# ---------- stage machine helpers ----------
# stage_completed_list <stage> -> "P0 P1 ..." (the phases already finished)
stage_completed_list() {
  case "$1" in
    INIT)                  echo "" ;;
    PREFLIGHT)             echo "P0" ;;
    QUIESCED)              echo "P0 P1" ;;
    DATA)                  echo "P0 P1 P2" ;;
    IAM)                   echo "P0 P1 P2 P3" ;;
    VERIFIED)              echo "P0 P1 P2 P3 P4" ;;
    CONFIG_SWITCHED)       echo "P0 P1 P2 P3 P4 P5" ;;
    CONSUMERS_RECREATED)   echo "P0 P1 P2 P3 P4 P5 P6" ;;
    COMPONENT_VERIFIED)    echo "P0 P1 P2 P3 P4 P5 P6 P7" ;;
    CUTOVER_PENDING|CUTOVER_DONE) echo "P0 P1 P2 P3 P4 P5 P6 P7 P8" ;;
    ROLLED_BACK)           echo "rollback applied" ;;
    *) echo "(unknown stage $1)" ;;
  esac
}

# resume_chain_for <stage> <deployment|none> -> the phases --resume would run
resume_chain_for() {
  local stage="$1" dep="${2:-none}"
  case "$stage" in
    INIT) die 1 "--resume: state is INIT (nothing finished) - run --phase all" ;;
    PREFLIGHT)
      if [ "$dep" != "none" ]; then echo "P1 P2 P3 P4 P5 P6 P7 P8"
      else echo "P2 P3 P4"; fi ;;
    QUIESCED)              echo "P2 P3 P4 P5 P6 P7 P8" ;;
    DATA)                  echo "P3 P4 P5 P6 P7 P8" ;;
    IAM)                   echo "P4 P5 P6 P7 P8" ;;
    VERIFIED)
      if [ "$dep" = "none" ]; then echo "(legacy flow complete; cutover needs --deployment)"
      else echo "P5 P6 P7 P8"; fi ;;
    CONFIG_SWITCHED)       echo "P6 P7 P8" ;;
    CONSUMERS_RECREATED)   echo "P7 P8" ;;
    COMPONENT_VERIFIED)    echo "P8" ;;
    CUTOVER_PENDING)       echo "P8" ;;
    CUTOVER_DONE)          echo "(complete - nothing to resume)" ;;
    ROLLED_BACK)           echo "(rolled back - start over with a fresh --state-dir)" ;;
    *) die 1 "--resume: unknown stage=$stage" ;;
  esac
}

# rollback_scope <stage> -> writers-only | config+writers | unsupported
rollback_scope() {
  case "$1" in
    QUIESCED|DATA|IAM|VERIFIED) echo "writers-only" ;;
    CONFIG_SWITCHED|CONSUMERS_RECREATED|COMPONENT_VERIFIED|CUTOVER_PENDING|CUTOVER_DONE) echo "config+writers" ;;
    *) echo "unsupported" ;;
  esac
}

# fail_notice <failed-phase> <msg> - prints completed phases + failure point +
# a copyable rollback command (maintainer req: failure behaviour must be explicit)
fail_notice() {
  local failed="$1" msg="$2" stage
  stage=$(state_get '.stage' 2>/dev/null || echo UNKNOWN)
  log_fail "FAILED at $failed (stage=$stage): $msg"
  log_fail "completed phases: $(stage_completed_list "$stage")"
  log_fail "rollback  (copy-paste): $0 <same-args> --state-dir $STATE_DIR --rollback"
  log_fail "resume    (after fix) : $0 <same-args> --state-dir $STATE_DIR --resume"
}

# ---------- P5: embedded controller reconstruction (fail-closed on shape drift) ----------
assert_embedded_shape() { # inspects the controller container; fail-closed unless it
                         # matches the installer shape (single-node embedded = make install-embedded)
  local insp a p ws
  insp=$(docker inspect "$CONTROLLER_CONTAINER") || die 1 "controller container '$CONTROLLER_CONTAINER' missing - fail-closed"
  jq -e '.[0].NetworkSettings.Networks["agentteams-net"]' <<<"$insp" >/dev/null \
    || die 1 "controller is not on network agentteams-net (not the installer shape) - fail-closed, use cutover-checklist.sh manually"
  for a in matrix-local.agentteams.io aigw-local.agentteams.io fs-local.agentteams.io; do
    jq -e --arg a "$a" '.[0].NetworkSettings.Networks["agentteams-net"].Aliases | index($a)' <<<"$insp" >/dev/null \
      || die 1 "network alias $a missing (not the installer shape) - fail-closed"
  done
  jq -e '.[0].Mounts[] | select(.Type == "bind" and .Source == "/var/run/docker.sock" and .Destination == "/var/run/docker.sock")' <<<"$insp" >/dev/null \
    || die 1 "docker.sock bind missing (not the installer shape) - fail-closed"
  jq -e '.[0].Mounts[] | select(.Type == "volume" and .Name == "agentteams-data" and .Destination == "/data")' <<<"$insp" >/dev/null \
    || die 1 "agentteams-data volume on /data missing (not the installer shape) - fail-closed"
  ws=$(jq -r '.[0].Mounts[] | select(.Type == "bind" and .Destination == "/root/agentteams-fs/agents/manager") | .Source' <<<"$insp")
  [ -n "$ws" ] && [ "$ws" != "null" ] || die 1 "workspace bind on /root/agentteams-fs/agents/manager missing (not the installer shape) - fail-closed"
  EMBED_WORKSPACE="$ws"
  # Container ports, not host ports: the installer publishes 8080->18080,
  # 8001->18001 and 8088->18088, so the third check must look for 8088.
  for p in 8080 8001 8088; do
    jq -e --arg p "$p" '.[0].HostConfig.PortBindings | has($p + "/tcp")' <<<"$insp" >/dev/null \
      || die 1 "port $p not published (not the installer shape) - fail-closed"
  done
  jq -e '.[0].HostConfig.RestartPolicy.Name == "unless-stopped" or .[0].HostConfig.RestartPolicy.Name == "always"' <<<"$insp" >/dev/null \
    || die 1 "restart policy is neither unless-stopped nor always (not the installer shape) - fail-closed"
}

# swap_storage_env <env-json-list> -> new env list pointing at the target backend.
# Swaps the storage-plane entries (and the MINIO_* admin fallbacks the controller
# reads via OSSConfig); every other env entry is preserved verbatim.
swap_storage_env() {  # <env-json> <endpoint> <bucket> <ak> <sk>
  # Values arrive as arguments: the caller runs this in a command substitution,
  # where relying on globals produced an empty bucket and empty credentials.
  local env_json="$1" ep="$2" bucket="$3" ak="$4" sk="$5"
  # jq strings do NOT interpolate $var - only \($var) does. Writing
  # "AGENTTEAMS_FS_ENDPOINT=$ep" produced a literal $ep in the env file, and an
  # env that never had a provider line kept none, so the rebuilt controller came
  # up without AGENTTEAMS_STORAGE_PROVIDER (the cutover assert caught it).
  jq -c --arg ep "$ep" --arg b "$bucket" --arg ak "$ak" --arg sk "$sk" '
    map(
      if   (. | startswith("AGENTTEAMS_STORAGE_PROVIDER")) or (. | startswith("HICLAW_STORAGE_PROVIDER")) then "AGENTTEAMS_STORAGE_PROVIDER=s3"
      elif (. | startswith("AGENTTEAMS_FS_ENDPOINT"))     or (. | startswith("HICLAW_FS_ENDPOINT"))     then "AGENTTEAMS_FS_ENDPOINT=\($ep)"
      elif (. | startswith("AGENTTEAMS_FS_BUCKET"))       or (. | startswith("HICLAW_FS_BUCKET"))       then "AGENTTEAMS_FS_BUCKET=\($b)"
      elif (. | startswith("AGENTTEAMS_FS_ACCESS_KEY"))   or (. | startswith("HICLAW_FS_ACCESS_KEY"))   then "AGENTTEAMS_FS_ACCESS_KEY=\($ak)"
      elif (. | startswith("AGENTTEAMS_FS_SECRET_KEY"))   or (. | startswith("HICLAW_FS_SECRET_KEY"))   then "AGENTTEAMS_FS_SECRET_KEY=\($sk)"
      elif (. | startswith("MINIO_USER"))     then "MINIO_USER=\($ak)"
      elif (. | startswith("MINIO_PASSWORD")) then "MINIO_PASSWORD=\($sk)"
      else . end )
    | if (map(select(startswith("AGENTTEAMS_STORAGE_PROVIDER"))) | length) > 0
      then . else . + ["AGENTTEAMS_STORAGE_PROVIDER=s3"] end
    | if (map(select(startswith("AGENTTEAMS_FS_ENDPOINT"))) | length) > 0
      then . else . + ["AGENTTEAMS_FS_ENDPOINT=\($ep)"] end
    | if (map(select(startswith("AGENTTEAMS_FS_BUCKET"))) | length) > 0
      then . else . + ["AGENTTEAMS_FS_BUCKET=\($b)"] end
    | if (map(select(startswith("AGENTTEAMS_FS_ACCESS_KEY"))) | length) > 0
      then . else . + ["AGENTTEAMS_FS_ACCESS_KEY=\($ak)"] end
    | if (map(select(startswith("AGENTTEAMS_FS_SECRET_KEY"))) | length) > 0
      then . else . + ["AGENTTEAMS_FS_SECRET_KEY=\($sk)"] end' <<<"$env_json"
}

# render_controller_run <out-script> <out-env-file> <image> <env-json>
# writes the reconstructed 'docker run' (installer shape, env via --env-file so
# values with special characters survive) - the SAME shape the P5 switch and the
# rollback use.
render_controller_run() {
  local out="$1" envfile="$2" image="$3" env_json="$4"
  jq -r '.[]' <<<"$env_json" > "$envfile"
  {
    echo '#!/usr/bin/env bash'
    echo "set -euo pipefail"
    echo "docker run -d --name $CONTROLLER_CONTAINER --network agentteams-net \\"
    # `--network-alias` is one flag with a value; `--network alias X` makes docker
    # treat X as the image name (that is how a cutover once tried to pull
    # matrix-local.agentteams.io). One flag per alias.
    echo "  --network-alias matrix-local.agentteams.io --network-alias aigw-local.agentteams.io --network-alias fs-local.agentteams.io \\"
    echo "  --env-file $envfile \\"
    echo "  -v /var/run/docker.sock:/var/run/docker.sock --security-opt label=disable \\"
    # The workspace is a bind mount onto the manager fs path only; a bare -v path
    # would create an anonymous volume instead of reusing the host directory.
    echo "  -v agentteams-data:/data -v '${EMBED_WORKSPACE:-<workspace>}:/root/agentteams-fs/agents/manager' \\"
    # Installer shape: host ports 18001/18080/18088 -> container 8001/8080/8088.
    echo "  -p 127.0.0.1:18001:8001 -p 127.0.0.1:18080:8080 -p 127.0.0.1:18088:8088 \\"
    echo "  --restart unless-stopped $image"
  } > "$out"
}

# wait_controller_up [timeout-s] -> 0 when the container is running AND the API port answers
wait_controller_up() {
  local t="${1:-90}" i=0 code
  while [ "$i" -lt "$t" ]; do
    if docker inspect -f '{{.State.Running}}' "$CONTROLLER_CONTAINER" 2>/dev/null | grep -q true; then
      code=$(probe_http "http://127.0.0.1:8080/" 3)
      [ "$code" != "000" ] && return 0
    fi
    sleep 2; i=$((i + 2))
  done
  return 1
}

# helm controller deployment name (label-based; "" when undeterminable)
helm_controller_deploy() {
  kubectl -n "$NS" get deploy -l "app.kubernetes.io/instance=$HELM_RELEASE" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}
