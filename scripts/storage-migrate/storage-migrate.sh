#!/usr/bin/env bash
# storage-migrate.sh - MinIO -> RustFS storage-plane migration (backfill + cutover)
# Design: docs/design/storage-external-s3-backend.md (migration section)
# Stage machine: P0 preflight, P1 quiesce (--deployment), P2 data, P3 IAM replay,
#                P4 final delta + verify, P5 switch config, P6 recreate consumers,
#                P7 per-component verification, P8 resume + observation
#                (legacy mode without --deployment: P0->P2->P3->P4, cutover manual)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/orchestration.sh"

PREFLIGHT_FAILS=()
PREFLIGHT_WARNS=()
pf_item() { # <id> <status> <evidence>
  report_add "preflight" "$1" "$2" "$3"
  state_set "stage_detail.preflight.$1" "\"$2\""
  case "$2" in
    fail) PREFLIGHT_FAILS+=("$1"); log_fail "P$1: $3" ;;
    warn) PREFLIGHT_WARNS+=("$1"); log_warn "P$1: $3" ;;
    *)    log_info  "P$1 [$2]: $3" ;;
  esac
}

# ============ Phase 0: preflight (P0-1 ~ P0-15) ============
phase_preflight() {
  log_info "phase: preflight start (mode=$MODE profile=$PROFILE dry_run=$DRY_RUN)"
  mc_alias "$SRC_ALIAS" "$SRC_EP" "$SRC_AK" "$SRC_SK"
  mc_alias "$DST_ALIAS" "$DST_EP" "$DST_AK" "$DST_SK"

  # Bucket list (--buckets unset = every bucket on the source)
  local buckets
  if [ -n "$BUCKETS" ]; then
    buckets="${BUCKETS//,/ }"
  else
    buckets=$(mc_raw ls "$SRC_ALIAS" 2>/dev/null | awk '{print $NF}' | sed 's|/.*||' | sort -u | tr '\n' ' ' || true)
    [ -n "$buckets" ] || { pf_item "0-1" fail "source unreachable or has no buckets (mc ls $SRC_ALIAS empty)"; preflight_summary; }
  fi
  log_info "buckets: $buckets"
  state_set "src.buckets" "$(printf '%s\n' $buckets | jq -R . | jq -s .)"

  # --- P0-1 source reachability (data plane first; admin reachability graded by mode) ---
  local src_ls_err
  src_ls_err=$(mc_raw ls "$SRC_ALIAS" 2>&1 >/dev/null | head -1 || true)
  if [ -z "$src_ls_err" ] || ! grep -qiE "connection refused|timeout|no such host|dial" <<<"$src_ls_err"; then
    if mc_admin info "$SRC_ALIAS" >/dev/null 2>&1; then
      pf_item "0-1" pass "source data plane + admin API reachable"
    else
      if [ "$MODE" = "odm" ]; then
        pf_item "0-1" fail "source admin API unreachable (odm probing needs it; check --src-endpoint/-ak/-sk)"; preflight_summary
      else
        pf_item "0-1" warn "source data plane reachable, admin API not (mirror: IAM replay degrades to no snapshot; odm unavailable)"
      fi
    fi
  else
    pf_item "0-1" fail "source data plane unreachable: $src_ls_err (check --src-endpoint/-ak/-sk)"; preflight_summary
  fi

  # --- P0-2 target reachability + engine type ---
  local code2
  code2=$(probe_http "$DST_EP/minio/health/live" 5)
  if [ "$code2" = "000" ]; then
    pf_item "0-2" fail "target unreachable ($DST_EP no response; start the target per the runbook first)"
    preflight_summary
  fi
  local engine="s3-generic"
  detect_engine; engine="$REPLY_ENGINE"
  pf_item "0-2" pass "target reachable (health=$code2, engine=$engine)"

  # --- P0-3 capacity + pass strategy (exact size via ls --json; du fallback above 50k objects) ---
  local total_bytes=0 b n_obj
  for b in $buckets; do
    n_obj=$({ mc_raw ls --recursive "$SRC_ALIAS/$b" 2>/dev/null || true; } | wc -l | tr -d ' ')
    if [ "${n_obj:-0}" -gt 50000 ]; then
      local du_out
      du_out=$(mc_raw du "$SRC_ALIAS/$b" 2>/dev/null | tail -1 | awk '{print $1}' || echo 0)
      total_bytes=$(( total_bytes + $(human_to_bytes "$du_out" 2>/dev/null || echo 0) ))
    else
      local sz
      sz=$({ mc_raw ls --recursive --json "$SRC_ALIAS/$b" 2>/dev/null || true; } | jq -s 'map(.size // 0) | add // 0' 2>/dev/null || echo 0)
      total_bytes=$(( total_bytes + sz ))
    fi
  done
  local total_gb=$(( total_bytes / 1073741824 ))
  local passes=1 pass_hint="single pass"
  if [ "$total_bytes" -gt $((50 * 1073741824)) ]; then
    passes=3; pass_hint="three incremental passes"
  elif [ "$total_bytes" -gt 1073741824 ]; then
    passes=2; pass_hint="two incremental passes"
  fi
  state_set "stage_detail.passes" "$passes"
  log_info "capacity: ${total_gb}GB -> $pass_hint (assuming 100MB/s, estimate only)"
  pf_item "0-3" pass "total capacity ~${total_gb}GB, mirror=$pass_hint, estimated ~$(( total_gb / 100 + 1 ))min at 100MB/s"

  # --- P0-4 target disk capacity ---
  if [ -n "$DST_DATA_DIR" ] && [ -d "$DST_DATA_DIR" ]; then
    local need=$(( total_bytes * 2 / 1073741824 + 1 ))
    local avail_gb
    avail_gb=$(df -P "$DST_DATA_DIR" | awk 'NR==2 {gsub("","",$4); print int($4/1048576)}')
    if [ "${avail_gb:-0}" -lt "$need" ]; then
      pf_item "0-4" fail "target free space ${avail_gb}GB < required ~${need}GB (2x capacity + 1)"
    else
      pf_item "0-4" pass "target free space ${avail_gb}GB >= required ~${need}GB"
    fi
  else
    pf_item "0-4" warn "--dst-data-dir not given; capacity check skipped (set it to avoid a cutover-day disk surprise)"
  fi

  # --- P0-5 storage substrate ---
  if [ -n "$DST_DATA_DIR" ] && [ -d "$DST_DATA_DIR" ]; then
    local fstype
    fstype=$(stat -f -c %T "$DST_DATA_DIR" 2>/dev/null || echo unknown)
    case "$fstype" in
      nfs|nfs4)
        pf_item "0-5" warn "NFS substrate (officially discouraged) = a risk-acceptance decision; verify the three target container env vars (see the migration guide); the combination test is a hard gate" ;;
      *) pf_item "0-5" pass "substrate=$fstype (supported)" ;;
    esac
  else
    pf_item "0-5" warn "substrate unknown (--dst-data-dir missing or unreachable)"
  fi

  # --- P0-6 IAM snapshot export ---
  export_iam_snapshot

  # --- P0-7 data-plane read/write (source GET on one object; target PUT/GET/DEL round trip) ---
  local probe_key="migrate-preflight-probe.txt"
  local first_obj
  first_obj=$({ mc_raw ls --recursive "$SRC_ALIAS" 2>/dev/null || true; } | head -1 | awk '{print $NF}')
  if [ -n "$first_obj" ] && mc_get "$SRC_ALIAS/$first_obj" >/dev/null 2>&1; then
    pf_item "0-7" pass "source GET sample OK ($first_obj)"
  else
    pf_item "0-7" warn "source has no objects to sample (GET sample skipped)"
  fi
  if [ "$DRY_RUN" = false ]; then
    local pbucket="migrate-probe-$(date +%s)"
    mc_raw mb "$DST_ALIAS/$pbucket" >/dev/null 2>&1 || true
    if mc_put probe "$DST_ALIAS/$pbucket/$probe_key" \
       && mc_get "$DST_ALIAS/$pbucket/$probe_key" 2>/dev/null | grep -q probe \
       && mc_raw rm --recursive --force "$DST_ALIAS/$pbucket" >/dev/null 2>&1 \
       && mc_raw rb "$DST_ALIAS/$pbucket" >/dev/null 2>&1; then
      pf_item "0-7" pass "target PUT/GET/DEL round trip OK (dedicated probe bucket created and removed)"
    else
      mc_raw rb "$DST_ALIAS/$pbucket" >/dev/null 2>&1 || true
      pf_item "0-7" fail "target write round trip failed ($DST_EP write path; probe bucket $pbucket cleanup attempted)"
    fi
  fi

  # --- P0-8 drive identity（inplace only）---
  if [ "$MODE" = "inplace" ]; then
    pf_item "0-8" warn "inplace mode: after the target first starts, confirm .rustfs.sys imported the MinIO data (manual check; not assertable remotely)"
  else
    pf_item "0-8" skipped "not inplace mode"
  fi

  # --- P0-9 incomplete multipart uploads ---
  local mp=""
  for b in $buckets; do
    mp=$(mc_raw ls --incomplete "$SRC_ALIAS/$b" 2>/dev/null | head -3 || true)
    [ -n "$mp" ] && break
  done
  if [ -n "$mp" ]; then
    pf_item "0-9" fail "source has incomplete multipart uploads (mc ls --incomplete not empty): clear them before migrating (leftover parts consume space and ODM ignores them)"
  else
    pf_item "0-9" pass "no incomplete multipart uploads"
  fi

  # --- P0-10 clock skew ---
  local skew="unknown"
  local lastmod
  lastmod=$(mc_raw ls --recursive --json "$SRC_ALIAS" 2>/dev/null | head -1 | jq -r '.lastModified // empty' 2>/dev/null || true)
  if [ -n "$lastmod" ]; then
    local src_epoch now_epoch
    src_epoch=$(date -d "$lastmod" +%s 2>/dev/null || echo 0)
    now_epoch=$(date +%s)
    skew=$(( now_epoch - src_epoch ))
    if [ "$skew" -gt 3600 ]; then
      pf_item "0-10" warn "newest source object mtime trails this host by $skew s (>1h: either no recent writes or a big clock skew - rule clock issues out before a production migration)"
    else
      pf_item "0-10" pass "source write activity acceptable (newest object mtime $skew s behind this host)"
    fi
  else
    pf_item "0-10" warn "cannot read source object mtime (mc ls --json unusable); clock skew unverified"
  fi

  # --- P0-11 STS/k8s probe ---
  case "$PROFILE" in
    k8s-rwo|k8s-rwx)
      pf_item "0-11" warn "k8s profile: the STS credential model needs the P0-11 probe (kubectl get secret + credProvider); this version prints runbook guidance instead" ;;
    *) pf_item "0-11" skipped "not the k8s profile" ;;
  esac

  # --- P0-12/13 ODM probe (odm mode) ---
  if [ "$MODE" = "odm" ]; then
    odm_probe
  else
    pf_item "0-12" skipped "not odm mode"
    pf_item "0-13" skipped "not odm mode"
  fi

  # --- P0-14 du breakdown ---
  if [ -n "$DST_DATA_DIR" ] && [ -d "$DST_DATA_DIR" ]; then
    pf_item "0-14" pass "capacity accounted for via --dst-data-dir (P0-3 namespace + P0-4 host disk)"
  else
    pf_item "0-14" warn "du breakdown incomplete (--dst-data-dir missing): hidden buckets (.minio.sys etc.) not counted"
  fi

  # --- P0-15 declarative durability assertion ---
  pf_item "0-15" "warn" "target durability claim = $DST_DURABILITY (G2). Declarative only: verify the target container env RUSTFS_DURABILITY_MODE=$DST_DURABILITY by hand (1.0.3 defaults to strict; relaxed/none reduce durability and are not recommended for migrations)"

  # --- P0-16 writer enumeration + original-config snapshot (one-command mode) ---
  # read-only: enumerates the members that write to the source and snapshots the
  # CURRENT controller config to the backup dir - the rollback source for P5.
  if [ -n "$DEPLOYMENT" ]; then
    local writers_json snapdir
    snapdir="$(backup_root_path)/$(date -u +%Y%m%d-%H%M%S)"
    if enum_writers; then
      writers_json="$REPLY_WRITERS_JSON"
      local wcount
      wcount=$(jq 'length' <<<"$writers_json" 2>/dev/null || echo 0)
      if [ "${wcount:-0}" = "0" ]; then
        # A real deployment that verifies per component needs writers; zero
        # members would make P6/P7 vacuous, so say it out loud.
        pf_item "0-16" warn "writer enumeration returned 0 members - P6/P7 would have nothing to verify; confirm the deployment really has no Workers/Managers"
      else
        pf_item "0-16" pass "writer enumeration: $wcount member(s) ($DEPLOYMENT)"
      fi
    else
      # Unknown writer set is not "no writers": refuse rather than verify nothing.
      writers_json="[]"
      pf_item "0-16" fail "writer enumeration failed (embedded: docker exec $CONTROLLER_CONTAINER agt get workers|managers; helm: kubectl get workers|managers) - refusing to migrate with an unknown writer set"
    fi
    if [ "$DRY_RUN" = false ]; then
      mkdir -p "$snapdir"
      case "$DEPLOYMENT" in
        embedded)
          docker inspect "$CONTROLLER_CONTAINER" > "$snapdir/controller-inspect-pre.json" 2>/dev/null || true
          jq -c '.[0].Config.Env' "$snapdir/controller-inspect-pre.json" > "$snapdir/controller-env-pre.json" 2>/dev/null || echo '[]' > "$snapdir/controller-env-pre.json"
          jq -r '.[0].Config.Image' "$snapdir/controller-inspect-pre.json" 2>/dev/null > "$snapdir/controller-image.txt" || true
          docker exec "$CONTROLLER_CONTAINER" agt get workers -o json > "$snapdir/workers-pre.json" 2>/dev/null || true
          docker exec "$CONTROLLER_CONTAINER" agt get managers -o json > "$snapdir/managers-pre.json" 2>/dev/null || true
          ;;
        helm)
          helm get values "$HELM_RELEASE" -n "$NS" > "$snapdir/helm-values-pre.yaml" 2>/dev/null || true
          kubectl -n "$NS" get workers -o yaml > "$snapdir/workers-pre.yaml" 2>/dev/null || true
          kubectl -n "$NS" get managers -o yaml > "$snapdir/managers-pre.yaml" 2>/dev/null || true
          ;;
      esac
    fi
    state_set "stage_detail.writers" "$writers_json"
    state_set "stage_detail.deployment" "\"$DEPLOYMENT\""
    state_set "original_config_snapshot" "\"$snapdir\""
  else
    state_set "stage_detail.deployment" '"none"'
    pf_item "0-16" skipped "no --deployment (legacy data-plane flow; writer enumeration/snapshot skipped)"
  fi

  preflight_summary
}

preflight_summary() {
  if [ ${#PREFLIGHT_FAILS[@]} -gt 0 ]; then
    report_write > "$STATE_DIR/preflight-report.json"
    die 2 "preflight red (${#PREFLIGHT_FAILS[@]} items): ${PREFLIGHT_FAILS[*]} - fix each item and re-run (the state machine resumes)"
  fi
  if [ ${#PREFLIGHT_WARNS[@]} -gt 0 ]; then
    log_warn "preflight has ${#PREFLIGHT_WARNS[@]} warning(s) (non-blocking, recorded in the report): ${PREFLIGHT_WARNS[*]}"
  fi
  log_info "preflight green"
  state_stage "PREFLIGHT"
}

human_to_bytes() { # 200MiB / 1.5GiB / 104857600 -> bytes (integer part, rounds down)
  local h="$1" v unit
  case "$h" in
    *KiB|*Ki)  v="${h%Ki*}"; echo "$(( ${v%%.*} * 1024 ))" ;;
    *MiB|*Mi)  v="${h%Mi*}"; echo "$(( ${v%%.*} * 1048576 ))" ;;
    *GiB|*Gi)  v="${h%Gi*}"; echo "$(( ${v%%.*} * 1073741824 ))" ;;
    *TiB|*Ti)  v="${h%Ti*}"; echo "$(( ${v%%.*} * 1099511627776 ))" ;;
    *) echo "$(( h + 0 ))" ;;
  esac
}

# --- P0-6: IAM snapshot export (admin API + mc --json dual channel, degrades without hanging) ---
export_iam_snapshot() {
  local snap="$STATE_DIR/iam-snapshot.json"
  log_info "exporting the IAM snapshot to $snap"
  # Source type: /minio/health/live 200 = MinIO family (admin plane expected; list failure is red), otherwise plain S3 (skip IAM)
  local src_is_minio=0
  [ "$(probe_http "$SRC_EP/minio/health/live" 5)" = "200" ] && src_is_minio=1
  # users (0.2.0 rewrite: NDJSON first, table fallback; the 0.1.0 array check never matched NDJSON and the table path dropped the first row)
  local users_json="[]" user_recs=""
  if ! user_recs=$(mc_admin_user_records "$SRC_ALIAS"); then
    if [ "$src_is_minio" -eq 1 ]; then
      pf_item "0-6-user" fail "source is MinIO family (health/live 200) but user list failed on both channels - refusing to continue rather than losing users silently"
    else
      pf_item "0-6-user" warn "source is not MinIO family (no health/live) - plain S3, skipping the IAM user plane"
    fi
  fi
  if [ -n "$user_recs" ]; then
    users_json=$(jq -Rn '[inputs | select(length > 0) | split("\t") | {access_key: .[0], status: .[1], policy: (.[2] // null)}]' <<<"$user_recs")
  fi
  # merge secrets (--iam-secrets-file; secrets are not exportable via the API)
  if [ -n "$IAM_SECRETS_FILE" ] && [ -f "$IAM_SECRETS_FILE" ]; then
    users_json=$(jq --slurpfile sec <(jq -M . "$IAM_SECRETS_FILE" 2>/dev/null) \
      'map(.secret = $sec[0][.access_key] // .secret // "NEEDS_SECRETS_FILE")' <<<"$users_json")
  else
    users_json=$(jq 'map(.secret = .secret // "NEEDS_SECRETS_FILE")' <<<"$users_json")
  fi
  # policies (name + JSON body; mc admin policy info first, SigV4 fallback)
  local policies_json="{}" pname pcontent
  local policy_names=""
  # 0.2.0: mc admin policy list has no header (0.1.0 dropped the first row and silently lost a readonly policy)
  if ! policy_names=$(mc_admin_policy_names "$SRC_ALIAS" 2>/dev/null | sort -u); then
    if [ "$src_is_minio" -eq 1 ]; then
      pf_item "0-6-policy" fail "source is MinIO family but policy list export failed - refusing to continue"
    else
      pf_item "0-6-policy" warn "plain S3 - skipping the IAM policy plane"
    fi
  fi
  for pname in $policy_names; do
    [ -z "$pname" ] && continue
    pcontent=$(mc_admin policy info "$SRC_ALIAS" "$pname" 2>/dev/null | jq -M '.' 2>/dev/null || true)
    if [ -z "$pcontent" ]; then
      local out
      out=$(admin_sigv4 src GET "/minio/admin/v3/policy?name=$pname")
      split_http_code "$out"
      pcontent=$(jq -M '.' <<<"$REPLY_BODY" 2>/dev/null || true)
    fi
    if [ -n "$pcontent" ]; then
      # mc admin policy info wraps the body as {PolicyName, Policy:{...}}; create expects the bare policy -> unwrap
      pcontent=$(jq -c 'if .Policy then .Policy else . end' <<<"$pcontent" 2>/dev/null || echo "$pcontent")
      policies_json=$(jq --arg n "$pname" --argjson c "$pcontent" '.[$n]=$c' <<<"$policies_json")
    else
      policies_json=$(jq --arg n "$pname" '.[$n]=null' <<<"$policies_json")
      log_warn "policy $pname body export failed (snapshot keeps the name only; replay creates a placeholder policy and marks it warn)"
    fi
  done
  jq -n --argjson u "$users_json" --argjson p "$policies_json" --arg now "$(now_iso)" \
    '{"exported":$now,"source":"'$SRC_EP'","users":$u,"policies":$p}' > "$snap"
  # Hard gate: snapshot counts vs a fresh listing of the source (0.2.0 - 0.1.0 only checked success, so three users could export as zero)
  # In production every Worker has one user; silently losing users = every cluster credential breaks after cutover)
  local nu np
  nu=$(jq '.users | length' "$snap"); np=$(jq '.policies | length' "$snap")
  if [ "$src_is_minio" -eq 1 ]; then
    local re_users re_pols n_re_u n_re_p
    if ! re_users=$(mc_admin_user_records "$SRC_ALIAS") || ! re_pols=$(mc_admin_policy_names "$SRC_ALIAS"); then
      pf_item "0-6" fail "IAM snapshot re-list failed (admin API unreachable) - the snapshot cannot be trusted, refusing to continue"
      return
    fi
    n_re_u=$(grep -c . <<<"$re_users" || true); n_re_p=$(grep -c . <<<"$re_pols" || true)
    if [ "$nu" -ne "$n_re_u" ] || [ "$np" -ne "$n_re_p" ]; then
      pf_item "0-6" fail "IAM snapshot count mismatch: users $nu/source $n_re_u, policies $np/source $n_re_p - export channel broken, refusing to continue"
      return
    fi
    if [ "$nu" -eq 0 ] && [ "$np" -eq 0 ]; then
      pf_item "0-6" warn "IAM snapshot is empty (no custom users/policies, admin reachable throughout - legitimately empty)"
    else
      pf_item "0-6" "pass" "IAM snapshot: $nu users / $np policies (re-list counts match) ($snap)"
    fi
  else
    pf_item "0-6" warn "plain S3 source: IAM not handled ($nu users / $np policies in the snapshot); recreate users/policies on the target manually after cutover"
  fi
}

# --- P0-12/13: ODM dry-run probe + window estimate ---
odm_probe() {
  # SSRF precondition: the source endpoint must not be loopback (the RustFS engine rejects it)
  local srchost
  srchost=$(ep_hostport "$SRC_EP")
  case "${srchost%%:*}" in
    localhost|127.*|::1)
      pf_item "0-12" fail "SSRF: --src-endpoint is loopback ($SRC_EP); the RustFS engine refuses loopback sources. Use a LAN IP or a container network name (RFC1918 is allowed)"
      return ;;
  esac
  local b="${buckets%% *}"
  # The admin handler checks the bucket first (404 when missing) - create the probe bucket before probing
  local pbucket="migrate-odm-probe-$(date +%s)"
  mc_raw mb "$DST_ALIAS/$pbucket" >/dev/null 2>&1 || true
  local body out
  # Semantics: path = target bucket (probe), source.bucket = the real source bucket (the probe validates source reachability)
  body=$(jq -n --arg ep "$SRC_EP" --arg sb "$b" --arg ak "$SRC_AK" --arg sk "$SRC_SK" \
    '{enabled:true, source:{provider:"minio", region:"us-east-1", endpoint:$ep, bucket:$sb, credentials:{access_key:$ak, secret_key:$sk}}}')
  # dry-run is a query parameter (?dry-run=true validates without writing; it is not a body field - sending it in the body returns 400)
  out=$(admin_sigv4 dst PUT "/rustfs/admin/v3/on-demand-migration/$pbucket?dry-run=true" "$body")
  split_http_code "$out"
  # remove the probe bucket
  mc_raw rb --force "$DST_ALIAS/$pbucket" >/dev/null 2>&1 || mc_raw rm --recursive --force "$DST_ALIAS/$pbucket" >/dev/null 2>&1; mc_raw rb "$DST_ALIAS/$pbucket" >/dev/null 2>&1 || true
  case "$REPLY_CODE" in
    200|202) pf_item "0-12" pass "ODM dry-run probe OK (probe bucket $pbucket, HTTP $REPLY_CODE)" ;;
    403|501) pf_item "0-12" fail "ODM rejected by the engine (HTTP $REPLY_CODE: strict build without a license, or unsupported version) -> use --mode mirror" ;;
    400)   pf_item "0-12" fail "ODM configuration 400 (body: ${REPLY_BODY:0:200})" ;;
    404)   pf_item "0-12" fail "ODM route 404 (target is not RustFS 1.0.3+ or the admin route is not registered - confirm the engine with mc admin info)" ;;
    *)     pf_item "0-12" fail "ODM dry-run probe failed (HTTP $REPLY_CODE)" ;;
  esac
  # P0-13 window estimate
  local est
  est=$(jq -r '.estimated_objects // .objects // "unknown"' <<<"$REPLY_BODY" 2>/dev/null || echo unknown)
  pf_item "0-13" pass "backfill window estimate: object count=$est (dry-run echo); shadow period suggested at $SHADOW_DAYS days (zero source writes before detach)"
}

# ============ Phase 1: quiesce (P1; --deployment required) ============
# Freezes the AgentTeams writers with the real in-repo mechanisms and proves
# the freeze with the quiesce window (N equal samples of object count / total
# bytes / top-level-prefix digest - object mtimes alone are NOT acceptable).
# On ANY failure the members stopped by this phase are restored to their
# original states (reverse order: managers, then workers) before exit.
restore_quiesced() { # <stopped-json>
  local sj="$1" k n o
  [ -n "$sj" ] && [ "$sj" != "[]" ] || return 0
  while IFS=$'\t' read -r k n o; do
    [ -z "${k:-}" ] && continue
    if set_member_state "$k" "$n" "$o"; then
      log_info "P1 restore: $k/$n back to $o"
    else
      log_warn "P1 restore: $k/$n -> $o FAILED (fix by hand: see the mechanism notes in the runbook)"
    fi
  done < <(jq -r 'reverse | .[] | [.kind, .name, .original_state] | @tsv' <<<"$sj")
}

phase_quiesce() {
  log_info "phase: quiesce start (deployment=$DEPLOYMENT)"
  local writers stopped_json
  if ! enum_writers; then
    fail_notice "P1 quiesce" "writer enumeration failed (${deployment_unreachable_msg})"
    exit 1
  fi
  writers="$REPLY_WRITERS_JSON"
  stopped_json="[]"

  log_warn "P1 MANUAL STEP (external clients): stop every client/CI/cron that writes to $SRC_EP now - the tool cannot enumerate them; the window evidence below will prove the freeze (or fail)."

  # --- stop members: workers first, then managers (the writers list is in that order) ---
  local k n st orig failed=""
  while IFS=$'\t' read -r k n st orig; do
    [ -z "${k:-}" ] && continue
    if [ "$st" = "Stopped" ]; then
      log_info "P1 $k/$n already Stopped (skip; original state preserved)"
      continue
    fi
    if set_member_state "$k" "$n" Stopped; then
      stopped_json=$(jq --argjson s "$stopped_json" --arg k "$k" --arg n "$n" --arg o "$orig" \
        '. + [{kind:$k, name:$n, original_state:$o}]' <<<"$stopped_json")
      if [ "$DRY_RUN" = false ]; then
        # Stopped deletes the member container (ensureMemberContainerAbsent); assert absence
        if ! wait_for_container_absent "$(member_container_name "$k" "$n")" 60; then
          log_warn "P1 $k/$n: container still present after state=Stopped (reconciler lag?) - asserting CR state only"
        fi
      fi
    else
      failed="$failed $k/$n"
    fi
  done < <(jq -r '.[] | [.kind, .name, .state, .state] | @tsv' <<<"$writers")
  if [ -n "$failed" ]; then
    restore_quiesced "$stopped_json"
    fail_notice "P1 quiesce" "stopping members failed:$failed (restored the already-stopped ones)"
    exit 1
  fi
  state_set "stage_detail.quiesce.stopped" "$stopped_json"

  # --- controller tier ---
  case "$DEPLOYMENT" in
    helm)
      local cdep
      cdep=$(helm_controller_deploy)
      if [ -n "$cdep" ]; then
        exec_or_print kubectl -n "$NS" scale deploy "$cdep" --replicas=0
        state_set "stage_detail.quiesce.controller" "{\"scaled_to_zero\":true,\"deployment\":\"$cdep\"}"
        log_info "P1 helm: controller deployment $cdep scaled to 0 (strict tier)"
      else
        state_set "stage_detail.quiesce.controller" "{\"scaled_to_zero\":false,\"reason\":\"controller deployment not found by label\"}"
        log_warn "P1 helm: controller deployment not found by label app.kubernetes.io/instance=$HELM_RELEASE - controller-level freeze relies on the member stops + window evidence"
      fi
      ;;
    embedded)
      # the embedded controller hosts the source MinIO -> it CANNOT be stopped before the
      # final delta. Degrade (maintainer rule: real mechanism if it exists, otherwise a
      # documented step + script assertion): the controller stays up and is frozen
      # operationally (no CR/config changes during the window); the tool-side assertion
      # is the quiesce lock + the window evidence below.
      state_set "stage_detail.quiesce.controller" "{\"embedded\":true,\"note\":\"source storage lives in the controller - it stays up until P5; frozen operationally, asserted by quiesce.lock + window evidence\"}"
      log_warn "P1 embedded: the controller container stays up (the source storage lives in it); its process is frozen and no CR/config changes happen until P5"
      ;;
  esac

  # --- assertions: every member stopped by P1 is Stopped in the CR ---
  if [ "$DRY_RUN" = false ]; then
    local st_now
    while IFS=$'\t' read -r k n; do
      [ -z "${k:-}" ] && continue
      st_now=$(member_state_now "$k" "$n")
      if [ "$st_now" != "Stopped" ]; then
        restore_quiesced "$stopped_json"
        fail_notice "P1 quiesce" "$k/$n CR state=$st_now (expected Stopped) - restored the stopped members"
        exit 1
      fi
    done < <(jq -r '.[] | [.kind, .name] | @tsv' <<<"$stopped_json")
  fi

  # --- window evidence: N equal samples => freeze proven; hold the lock until P8/rollback ---
  if [ "$DRY_RUN" = false ]; then
    if ! window_evidence; then
      restore_quiesced "$stopped_json"
      fail_notice "P1 quiesce" "quiesce window evidence failed (a writer is still active - e.g. an external client) - restored the stopped members"
      exit 1
    fi
    state_set "stage_detail.quiesce.window" "$REPLY_WINDOW_JSON"
    : > "$STATE_DIR/quiesce.lock"
    state_set "stage_detail.quiesce.lock" "\"$STATE_DIR/quiesce.lock\""
    log_info "P1 quiesce: freeze proven by $(jq length <<<"$REPLY_WINDOW_JSON") equal samples (object count / total bytes / top-level prefix digest, ${QUIESCE_INTERVAL}s apart); window locked"
  else
    state_set "stage_detail.quiesce.window" "[]"
    log_info "[dry-run] P1 would take $QUIESCE_SAMPLES window samples x ${QUIESCE_INTERVAL}s and create $STATE_DIR/quiesce.lock"
  fi
  # Freeze the controller process AFTER the window evidence: the evidence still
  # queries the controller, and freezing it earlier hangs those calls (a real
  # run stalled exactly there). MinIO is a separate process and keeps serving.
  if [ "$DRY_RUN" = false ] && [ "$DEPLOYMENT" = "embedded" ]; then
    local ctrl_pid
    ctrl_pid=$(docker exec "$CONTROLLER_CONTAINER" pgrep -f "/usr/local/bin/agentteams-controller" 2>/dev/null | head -1 || true)
    if [ -n "${ctrl_pid:-}" ]; then
      docker exec "$CONTROLLER_CONTAINER" kill -STOP "$ctrl_pid" 2>/dev/null || true
      state_set "stage_detail.frozen_controller_pid" "\"$ctrl_pid\""
      log_info "P1 embedded: controller process frozen (SIGSTOP pid $ctrl_pid); MinIO keeps serving reads"
    else
      log_warn "P1 embedded: controller pid not found; freeze relies on the window evidence alone"
    fi
  fi
  state_stage "QUIESCED"
}

# ============ Phase 1: data plane ============
phase_data() {
  state_stage "DATA"
  log_info "phase: data start (mode=$MODE)"
  case "$MODE" in
    mirror)  data_mirror ;;
    odm)     data_odm ;;
    inplace) data_inplace ;;
  esac
  state_stage "DATA"
}

data_inplace() {
  log_info "inplace mode: phase 1 is a no-op (rio-v2 imports MinIO-format data when the target first starts)"
  report_add "data" "inplace" "info" "import on first start; cutover = start the container and validate the whole chain"
}

data_mirror() {
  local buckets passes passes_hint
  buckets=$(get_buckets)
  passes=$(state_get '.stage_detail.passes // 1')
  [ -n "$passes" ] && [ "$passes" != "null" ] || passes=1
  local ts_prev="" i
  for i in $(seq 1 "$passes"); do
    local extra=""
    [ -n "$ts_prev" ] && extra="--newer-than $ts_prev"
    log_info "mirror pass $i/$passes ${extra:+($extra)}"
    for b in $buckets; do
      mc_raw mb "$DST_ALIAS/$b" 2>/dev/null || true
      # note: no --remove (target-side new objects must survive the migration)
      mc_raw mirror "$SRC_ALIAS/$b" "$DST_ALIAS/$b" $extra 2>&1 | tail -2 >&2 || die 1 "mirror failed: bucket=$b pass=$i"
    done
    ts_prev=$(now_iso)
    state_set "stage_detail.mirror_pass_$i" "\"$ts_prev\""
  done
  report_add "data" "mirror" "pass" "$passes pass(es) done (last at $ts_prev)"
}

data_odm() {
  local buckets
  buckets=$(get_buckets)
  for b in $buckets; do
    log_info "odm bucket=$b"
    # (1) create the target bucket (skip if it exists)
    mc_raw mb "$DST_ALIAS/$b" 2>/dev/null || log_info "bucket $b already exists, skipping"
    # (2) PUT the ODM configuration (for real)
    local body out
    body=$(jq -n --arg ep "$SRC_EP" --arg b "$b" --arg ak "$SRC_AK" --arg sk "$SRC_SK" \
      '{enabled:true, source:{provider:"minio", region:"us-east-1", endpoint:$ep, bucket:$b, credentials:{access_key:$ak, secret_key:$sk}}}')
    out=$(admin_sigv4 dst PUT "/rustfs/admin/v3/on-demand-migration/$b" "$body")
    split_http_code "$out"
    case "$REPLY_CODE" in
      200|202) : ;;
      *) die 1 "ODM configuration failed bucket=$b HTTP $REPLY_CODE body=${REPLY_BODY:0:300}" ;;
    esac
    # (3) status assertion (configured + enabled + breaker closed + no last_source_error)
    sleep 2
    out=$(admin_sigv4 dst GET "/rustfs/admin/v3/on-demand-migration/$b/status")
    split_http_code "$out"
    local st_ok
    st_ok=$(jq -r 'if (.configured == true) and (.enabled == true) and (.breaker.state == "closed") and (.last_source_error == null) then "ok" else "bad" end' <<<"$REPLY_BODY" 2>/dev/null || echo bad)
    [ "$st_ok" = "ok" ] || die 1 "ODM status not ok bucket=$b (configured/enabled/breaker/last_source_error criteria unmet) body=${REPLY_BODY:0:300} - check source network/SSRF"
    log_info "ODM status OK bucket=$b (breaker=closed, source reachable)"
    # (4) start the backfill (?op=start; empty body = defaults)
    out=$(admin_sigv4 dst POST "/rustfs/admin/v3/on-demand-migration/$b/backfill?op=start" "")
    split_http_code "$out"
    case "$REPLY_CODE" in
      200|202) log_info "backfill started bucket=$b" ;;
      *) die 1 "backfill start failed bucket=$b HTTP $REPLY_CODE body=${REPLY_BODY:0:300}" ;;
    esac
    # (5) poll until terminal (every 10s, bounded by BACKFILL_TIMEOUT; states: completed/completed_with_failures/failed/cancelled)
    local waited=0 st="unknown"
    while [ "$waited" -lt "$BACKFILL_TIMEOUT" ]; do
      sleep 10; waited=$((waited + 10))
      out=$(admin_sigv4 dst GET "/rustfs/admin/v3/on-demand-migration/$b/backfill")
      split_http_code "$out"
      st=$(jq -r '.job.state // .state // "unknown"' <<<"$REPLY_BODY" 2>/dev/null || echo unknown)
      case "$st" in
        completed)
          log_info "backfill completed bucket=$b (listed=$(jq -r '.job.listed // 0' <<<"$REPLY_BODY") pulled=$(jq -r '.job.pulled // 0' <<<"$REPLY_BODY") bytes=$(jq -r '.job.bytes // 0' <<<"$REPLY_BODY") failed=$(jq -r '.job.failed // 0' <<<"$REPLY_BODY"), $(( waited / 60 ))min)"
          break ;;
        completed_with_failures)
          die 1 "backfill partially failed bucket=$b (failed=$(jq -r '.job.failed // "?"' <<<"$REPLY_BODY")) - re-run the backfill to resume (the checkpoint lives on the RustFS side)" ;;
        failed|cancelled) die 1 "backfill failed bucket=$b state=$st body=${REPLY_BODY:0:300}" ;;
        *) [ $(( waited % 60 )) -eq 0 ] && log_info "backfill running bucket=$b state=$st pulled=$(jq -r '.job.pulled // 0' <<<"$REPLY_BODY" 2>/dev/null || echo 0)/listed=$(jq -r '.job.listed // 0' <<<"$REPLY_BODY" 2>/dev/null || echo 0) ($(( waited / 60 ))min)" ;;
      esac
    done
    [ "$st" = "completed" ] || die 1 "backfill timed out (${BACKFILL_TIMEOUT}s) bucket=$b last_state=${st} - re-running resumes (the checkpoint lives on the RustFS side)"
    state_set "stage_detail.odm.$b" "\"completed\""
  done
  report_add "data" "odm" "pass" "ODM configured and backfilled for every bucket"
}

# ============ Phase 2: IAM replay ============
phase_iam() {
  log_info "phase: iam start"
  local snap="$STATE_DIR/iam-snapshot.json"
  [ -f "$snap" ] || die 1 "IAM snapshot missing ($snap) - run preflight first"
  if [ "$MODE" = "inplace" ]; then
    log_info "inplace mode: IAM migrates with the first start; only diff verification hints remain (V4 covers it)"
    report_add "iam" "mode" "skipped" "inplace migrates automatically"
    state_stage "IAM"
    return
  fi
  # k8s + credProfile does not go through replay
  case "$PROFILE" in
    k8s-rwo|k8s-rwx)
      report_add "iam" "mode" "skipped" "the k8s STS credential model skips replay (passthrough runbook)"
      state_stage "IAM"
      return ;;
  esac

  local npolicy_created=0 nuser_added=0 nattach=0 nsecret_missing=0
  local existing_policies existing_users pname
  # 0.2.0: reuse the robust wrapper (0.1.0 dropped the first dst row, so skip-if-exists missed it and create hit an existing policy)
  existing_policies=$(mc_admin_policy_names "$DST_ALIAS" 2>/dev/null | sort -u || true)
  existing_users=$(mc_admin_user_records "$DST_ALIAS" 2>/dev/null | cut -f1 | sort -u || true)

  # policies (create takes positional args; skip if it exists)
  for pname in $(jq -r '.policies | keys[]' "$snap"); do
    if grep -qx "$pname" <<<"$existing_policies"; then
      log_info "policy $pname already exists, skipping"
      continue
    fi
    local pfile="$STATE_DIR/policy-$pname.json"
    if jq --arg n "$pname" '.policies[$n]' "$snap" | jq -e . >/dev/null 2>&1; then
      # tolerate old snapshots that stored the wrapped structure -> unwrap once more
      # The snapshot entry is {"policy_name": ..., "policy": {...}} (mc's
      # export shape); older exports used the capitalised .Policy. Unwrap
      # either, and keep a bare document as-is.
      jq --arg n "$pname" '.policies[$n] | if .policy then .policy elif .Policy then .Policy else . end' "$snap" > "$pfile"
    else
      # body export failed -> create a placeholder (read-only, conservative) + warn
      echo '{"Version":"2012-10-17","Statement":[]}' > "$pfile"
      report_add "iam" "policy:$pname" "warn" "snapshot has no body (export failed); an empty policy was created - fill it in before attaching"
    fi
    if mc_admin policy create "$DST_ALIAS" "$pname" "$pfile" >/dev/null 2>&1; then
      npolicy_created=$((npolicy_created + 1)); log_info "policy created: $pname"
    else
      die 3 "policy create failed: $pname"
    fi
  done

  # Users (add takes alias AK SK). Secrets are never stored in the snapshot
  # (the API cannot export them), so --iam-secrets-file is merged here - also
  # on a resumed `--phase iam`, where the snapshot was exported earlier
  # without it. Fields are joined with \x1f: a tab is IFS whitespace, so an
  # empty secret would otherwise collapse and shift the status into its slot.
  local users_tsv ak sk status pol
  if [ -n "$IAM_SECRETS_FILE" ] && [ -f "$IAM_SECRETS_FILE" ]; then
    users_tsv=$(jq -r --slurpfile sec <(jq -M . "$IAM_SECRETS_FILE" 2>/dev/null) \
      '.users[] | [.access_key, ($sec[0][.access_key] // .secret // ""), (.status // "enabled")] | join("\u001f")' "$snap")
  else
    users_tsv=$(jq -r '.users[] | [.access_key, (.secret // ""), (.status // "enabled")] | join("\u001f")' "$snap")
  fi

  while IFS=$'\x1f' read -r ak sk status; do
    [ -z "$ak" ] && continue
    if grep -qx "$ak" <<<"$existing_users"; then
      log_info "user $ak already exists, skipping add (attach is still verified)"
    else
      if [ "$sk" = "NEEDS_SECRETS_FILE" ] || [ -z "$sk" ]; then
        nsecret_missing=$((nsecret_missing + 1))
        die 3 "user $ak has no secret (secrets are not exportable from the source API); provide --iam-secrets-file with {\"$ak\":\"...\"} and resume with --phase iam (existing users are skipped)"
      fi
      if [ "${#sk}" -lt 8 ]; then
        die 3 "user $ak has a secret shorter than 8 characters (mc rejects it); fix --iam-secrets-file"
      fi
      if ! err_out=$(mc_admin user add "$DST_ALIAS" "$ak" "$sk" 2>&1); then
        die 3 "user add failed: $ak ($(printf '%s' "$err_out" | tail -1))"
      fi
      nuser_added=$((nuser_added + 1)); log_info "user added: $ak"
    fi
  done <<<"$users_tsv"

  # attach (compare by access key, never by display name)
  while IFS=$'\x1f' read -r ak pol; do
    [ -z "$ak" ] && continue
    [ -z "$pol" ] || [ "$pol" = "null" ] && continue
    if ! err_out=$(mc_admin policy attach "$DST_ALIAS" "$pol" --user "$ak" 2>&1); then
      die 3 "policy attach failed: $pol -> $ak ($(printf '%s' "$err_out" | tail -1))"
    fi
    nattach=$((nattach + 1))
  done < <(jq -r '.users[] | select(.policy != null and .policy != "") | [.access_key, .policy] | join("\u001f")' "$snap")

  # completion assertion: target user count >= snapshot count (0.2.0: headerless list; 0.1.0 undercounted by one and went red at exactly N users)
  local n_src n_dst
  n_src=$(jq '.users | length' "$snap")
  n_dst=$(mc_admin_user_records "$DST_ALIAS" 2>/dev/null | grep -c . || true)
  if [ "$n_dst" -lt "$n_src" ]; then
    die 3 "IAM completion assertion failed: target $n_dst users < snapshot $n_src"
  fi
  report_add "iam" "replay" "pass" "policies+$npolicy_created users+$nuser_added attaches+$nattach missing_secrets=$nsecret_missing"
  state_set "stage_detail.iam" "{\"policies_created\":$npolicy_created,\"users_added\":$nuser_added,\"attaches\":$nattach}"
  state_stage "IAM"
}

# ============ Phase 3: verify V1-V9 (read-only; red blocks cutover) ============
VERIFY_FAILS=()
ver_item() { # <id> <status> <evidence>
  report_add "verify" "$1" "$2" "$3"
  if [ "$2" = "fail" ]; then VERIFY_FAILS+=("$1"); log_fail "V$1: $3"; else log_info "V$1 [$2]: $3"; fi
}

# final_delta: source changes since the last mirror pass -> target.
# Legacy mode (no --deployment): the source write probe runs here (the source
# is expected to still be writable - the operator freezes it manually at cutover).
# Orchestration mode: the source is frozen - instead of writing to it, the
# tool asserts it is still READABLE (the final-delta precondition).
final_delta() {
  local buckets since b i n_pass
  buckets=$(get_buckets)
  n_pass=$(state_get '.stage_detail.passes // 1')
  for i in $(seq "${n_pass:-1}" -1 1); do
    since=$(state_get ".stage_detail.mirror_pass_$i // empty")
    [ -n "$since" ] && break
  done
  if [ "$MODE" = "inplace" ]; then
    log_info "inplace: final delta is a no-op (data imported on first start)"
    report_add "verify" "final-delta" "info" "inplace: import on first start"
  elif [ -n "$since" ]; then
    # Source is frozen here (window evidence), so the final delta must be a FULL
    # reconcile: --newer-than skips objects whose mtime equals the anchor and a
    # real run lost 39 bytes that way.
    log_info "final delta: mc mirror (full reconcile; source is frozen)"
    for b in $buckets; do
      mc_raw mirror --overwrite "$SRC_ALIAS/$b" "$DST_ALIAS/$b" 2>&1 | tail -1 >&2 || die 1 "final delta failed bucket=$b"
    done
    report_add "verify" "final-delta" "pass" "since=$since"
  else
    log_warn "no mirror timestamp to anchor on (odm mode) - rely on the V3 md5 sampling and the quiesce window evidence"
    report_add "verify" "final-delta" "warn" "odm mode has no delta anchor; relies on the V6 write-freeze assertion"
  fi
  local pb="${buckets%% *}" pk="finaldelta-probe-$(date +%s).txt"
  if [ "$DRY_RUN" = false ]; then
    local dep_now
    dep_now=$(state_get '.stage_detail.deployment // "none"')
    if [ "$dep_now" = "none" ]; then
      mc_put src-probe "$SRC_ALIAS/$pb/$pk" && mc_raw rm --force "$SRC_ALIAS/$pb/$pk" >/dev/null 2>&1 \
        && report_add "verify" "probe-src" "pass" "source write probe OK" \
        || report_add "verify" "probe-src" "warn" "source write probe failed (frozen? if expected, continue)"
    else
      mc_raw ls "$SRC_ALIAS/$pb" >/dev/null 2>&1 \
        && report_add "verify" "src-readable" "pass" "source still readable after the freeze (final delta possible)" \
        || report_add "verify" "src-readable" "fail" "source listing failed after the freeze"
    fi
    mc_put dst-probe "$DST_ALIAS/$pb/$pk" && mc_get "$DST_ALIAS/$pb/$pk" 2>/dev/null | grep -q dst-probe \
      && mc_raw rm --force "$DST_ALIAS/$pb/$pk" >/dev/null 2>&1 \
      && report_add "verify" "probe-dst" "pass" "target write probe OK" \
      || die 1 "target write probe failed - cutover aborted (target not writable)"
  fi
}

# ============ Phase 4: P4 = final delta + V1-V9 (runs AFTER the quiesce) ============
# The source is still accessible at this point (nothing has switched yet) -
# that is exactly what makes the FINAL delta possible: whatever the frozen
# writers' last in-flight operations wrote is mirrored one last time.
phase_verify() {
  log_info "phase: verify start (P4: final delta + V1-V9)"
  local dep_now
  dep_now=$(state_get '.stage_detail.deployment // "none"')
  if [ "$dep_now" != "none" ] && [ "$DRY_RUN" = false ]; then
    [ -f "$STATE_DIR/quiesce.lock" ] || { fail_notice "P4 verify" "quiesce lock missing (writers not frozen) - refusing the final delta"; exit 1; }
    mc_raw ls "$SRC_ALIAS" >/dev/null 2>&1 || { fail_notice "P4 verify" "source unreachable - the final delta is impossible"; exit 1; }
  fi
  final_delta
  local buckets src_blist dst_blist b
  buckets=$(get_buckets)

  # --- V1 bucket lists match ---
  src_blist=$(mc_raw ls "$SRC_ALIAS" 2>/dev/null | awk '{print $NF}' | sed 's|/.*||' | sort -u)
  dst_blist=$(mc_raw ls "$DST_ALIAS" 2>/dev/null | awk '{print $NF}' | sed 's|/.*||' | sort -u)
  local missing_on_dst
  missing_on_dst=$(comm -23 <<<"$src_blist" <<<"$dst_blist" 2>/dev/null || true)
  if [ -z "$missing_on_dst" ]; then
    ver_item "1" pass "bucket lists match (source subset of target, $(wc -l <<<"$src_blist") buckets)"
  else
    ver_item "1" fail "target is missing buckets: $missing_on_dst"
  fi

  # --- V2 object count + size reconciliation (target >= source: shadow writes land on the target) ---
  local b_fail=""
  for b in $buckets; do
    local s_n d_n s_sz d_sz
    s_n=$({ mc_raw ls --recursive "$SRC_ALIAS/$b" 2>/dev/null || true; } | wc -l | tr -d ' ')
    d_n=$({ mc_raw ls --recursive "$DST_ALIAS/$b" 2>/dev/null || true; } | wc -l | tr -d ' ')
    s_sz=$({ mc_raw ls --recursive --json "$SRC_ALIAS/$b" 2>/dev/null || true; } | jq -s 'map(.size // 0) | add // 0' 2>/dev/null || echo 0)
    d_sz=$({ mc_raw ls --recursive --json "$DST_ALIAS/$b" 2>/dev/null || true; } | jq -s 'map(.size // 0) | add // 0' 2>/dev/null || echo 0)
    if [ "${d_n:-0}" -lt "${s_n:-0}" ] || [ "${d_sz:-0}" -lt "${s_sz:-0}" ]; then
      b_fail="$b_fail $b(src=$s_n/$s_sz dst=$d_n/$d_sz)"
    else
      log_info "V2 $b: src=$s_n objects/${s_sz}B -> dst=$d_n objects/${d_sz}B ok"
    fi
  done
  if [ -n "$b_fail" ]; then ver_item "2" fail "reconciliation gap:$b_fail"; else ver_item "2" pass "every bucket reconciles (dst>=src)"; fi

  # --- V3 md5 sampling (min(10, N) per bucket) ---
  local bad=""
  for b in $buckets; do
    local keys k s_md5 d_md5
    # No `head` here: under `set -o pipefail` an early-exit reader SIGPIPEs the
    # producer (141) and `set -e` aborts the whole verify. awk reads it all.
    keys=$({ mc_raw ls --recursive "$SRC_ALIAS/$b" 2>/dev/null || true; } | awk '{print $NF}' | awk 'NR<=10')
    for k in $keys; do
      s_md5=$(mc_get "$SRC_ALIAS/$b/$k" 2>/dev/null | md5sum | awk '{print $1}')
      d_md5=$(mc_get "$DST_ALIAS/$b/$k" 2>/dev/null | md5sum | awk '{print $1}')
      if [ -n "$s_md5" ] && [ "$s_md5" != "$d_md5" ]; then bad="$bad $b/$k"; fi
    done
  done
  if [ -n "$bad" ]; then ver_item "3" fail "md5 mismatch:$bad"; else ver_item "3" pass "sampled md5s all match (<=10 objects per bucket)"; fi

  # --- V4 IAM diff (snapshot vs target) ---
  local snap="$STATE_DIR/iam-snapshot.json"
  if [ -f "$snap" ]; then
    local n_snap_u n_dst_u diff_u=""
    n_snap_u=$(jq '.users | length' "$snap")
    # 0.2.0: robust counting (0.1.0 undercounted by one) + list-based diff (mc admin user info gaps would go false-red)
    local dst_ak_list
    dst_ak_list=$(mc_admin_user_records "$DST_ALIAS" 2>/dev/null | cut -f1 | sort || true)
    n_dst_u=$(grep -c . <<<"$dst_ak_list" || true)
    local ak2
    for ak2 in $(jq -r '.users[].access_key' "$snap" 2>/dev/null | sort); do
      grep -qx "$ak2" <<<"$dst_ak_list" || diff_u="$diff_u $ak2"
    done
    if [ -n "$diff_u" ]; then ver_item "4" fail "target is missing users:$diff_u"; else ver_item "4" pass "IAM reconciles ($n_snap_u snapshot users all present, $n_dst_u users total)"; fi
  else
    ver_item "4" warn "no IAM snapshot to compare against (inplace skips it)"
  fi

  # --- V5 durability assertion (declarative: print the verification command, run it by hand) ---
  ver_item "5" warn "verify durability manually: target container env RUSTFS_DURABILITY_MODE=$DST_DURABILITY (docker exec <container> env | grep RUSTFS_DURABILITY)"

  # --- V6 write-freeze sampling (source max mtime < verify start; list failure = fail, never a fake pass) ---
  local v_start max_lmod lmod_raw lmod_rc=0
  v_start=$(date +%s)
  lmod_raw=$(mc_raw ls --recursive --json "$SRC_ALIAS" 2>/dev/null); lmod_rc=$?
  if [ $lmod_rc -ne 0 ]; then
    ver_item "6" fail "source listing failed (rc=$lmod_rc) - the write freeze cannot be asserted (V6 must not pass silently)"
  elif [ -z "$(jq -r '.lastModified // empty' <<<"$lmod_raw" | awk 'NR==1')" ]; then
    ver_item "6" warn "no source object mtimes available (write-freeze sampling skipped)"
  else
    max_lmod=$(jq -r '.lastModified // empty' <<<"$lmod_raw" | sort | tail -1)
    local ml_epoch
    ml_epoch=$(date -d "$max_lmod" +%s 2>/dev/null || echo 0)
    if [ "$ml_epoch" -ge "$v_start" ]; then
      ver_item "6" fail "the source is still being written (newest mtime=$max_lmod >= verify start) - shadow is not frozen, cutover blocked"
    else
      ver_item "6" pass "source write freeze confirmed (newest object mtime $(( v_start - ml_epoch ))s before verify)"
    fi
  fi

  # --- V7 incomplete multipart re-check ---
  local mp2=""
  for b in $buckets; do
    mp2=$(mc_raw ls --incomplete "$SRC_ALIAS/$b" 2>/dev/null | head -2 || true)
    [ -n "$mp2" ] && break
  done
  if [ -n "$mp2" ]; then ver_item "7" fail "the source still has incomplete multipart uploads"; else ver_item "7" pass "no incomplete multipart uploads"; fi

  # --- V8 target engine assertion (generic profile = test/plain S3, warn; production profiles assert) ---
  detect_engine
  if [ "$REPLY_ENGINE" = "rustfs" ]; then
    ver_item "8" pass "target is the RustFS engine (admin route probe 501)"
  else
    case "$PROFILE" in
      generic) ver_item "8" warn "target does not identify as RustFS (generic profile: test/plain S3, engine assertion skipped; production profiles require rustfs)" ;;
      *) ver_item "8" fail "mc admin info shows no rustfs marker (profile=$PROFILE requires RustFS; confirm --dst-endpoint points at the target)" ;;
    esac
  fi

  # --- V9 IAM consistency (target users/policies superset of the snapshot) ---
  local snapf="$STATE_DIR/iam-snapshot.json"
  if [ -f "$snapf" ] && [ "$MODE" != "inplace" ]; then
    local snap_users dst_users umiss snap_pols dst_pols pmiss
    snap_users=$(jq -r '.users[].access_key' "$snapf" 2>/dev/null | sort)
    dst_users=$(mc_admin_user_records "$DST_ALIAS" 2>/dev/null | cut -f1 | sort || true)
    umiss=$(comm -23 <<<"$snap_users" <<<"$dst_users" 2>/dev/null || true)
    if [ -n "$umiss" ]; then
      ver_item "9-user" fail "target is missing users (in snapshot, not on target): $umiss"
    else
      ver_item "9-user" pass "IAM users consistent (snapshot subset of target)"
    fi
    snap_pols=$(jq -r '.policies | keys[]' "$snapf" 2>/dev/null | sort)
    dst_pols=$(mc_admin_policy_names "$DST_ALIAS" 2>/dev/null | sort || true)
    pmiss=$(comm -23 <<<"$snap_pols" <<<"$dst_pols" 2>/dev/null || true)
    if [ -n "$pmiss" ]; then
      ver_item "9-policy" fail "target is missing policies (in snapshot, not on target): $pmiss"
    else
      ver_item "9-policy" pass "IAM policies consistent (snapshot subset of target)"
    fi
  else
    ver_item "9" warn "no IAM snapshot (expected for inplace/k8s), skipping the consistency check"
  fi

  # summary
  if [ ${#VERIFY_FAILS[@]} -gt 0 ]; then
    report_write > "$STATE_DIR/verify-report.json"
    die 2 "verify red (${#VERIFY_FAILS[@]} items): ${VERIFY_FAILS[*]} - fix and re-run verify; cutover stays blocked"
  fi
  log_info "verify green (final delta + V1-V9)"
  # the operator checklist (9 sections, [tool]/[manual] tagged) - written at P4
  # for BOTH flows: the automated P5-P8 follows it, the legacy flow executes it by hand
  [ -n "$DEPLOYMENT" ] && DEPLOY_MODE="$DEPLOYMENT"
  local list="$STATE_DIR/cutover-checklist.sh"
  render_cutover_checklist > "$list"
  chmod +x "$list"
  report_add "verify" "checklist" "pass" "cutover checklist -> $list (deploy-mode=$DEPLOY_MODE)"
  state_stage "VERIFIED"
}

# ============ cutover runbook (9-section operator checklist) ============
# render_cutover_checklist is the single source of truth for the cutover
# runbook: phase 4 writes exactly this text into cutover-checklist.sh, and
# --print-checklist prints it before the cutover (read-only: no state
# read/write, no mc, no dependency gate).
DEPLOY_MODE="both"      # embedded|helm|both: which command blocks the checklist prints
PRINT_CHECKLIST=false   # --print-checklist: print the runbook to stdout and exit 0

checklist_usage_extra() {
  cat <<'EOF'

Cutover runbook options (read-only; they never touch the state machine):
  --deploy-mode embedded|helm|both  which command blocks the cutover
                                     checklist prints (default: both)
  --print-checklist                 print the cutover checklist to stdout and
                                     exit 0 (no state read/write, no mc needed)
EOF
}

# Pre-parse the checklist-only flags before parse_args (which lives in
# lib/common.sh and is not extended for them):
#   pass 1: strip --deploy-mode / --print-checklist; the rest -> CHECKLIST_REST
#   pass 2: in print mode, consume only the endpoint values for the checklist
#           header and reject anything else, so the preview never reads or
#           writes state and never needs mc
checklist_preparse() {
  local rest=() a
  while [ $# -gt 0 ]; do
    a="$1"
    case "$a" in
      --deploy-mode)
        [ $# -ge 2 ] || die 4 "--deploy-mode requires a value (embedded|helm|both)"
        case "$2" in embedded|helm|both) : ;; *) die 4 "--deploy-mode must be embedded|helm|both (got: $2)" ;; esac
        DEPLOY_MODE="$2"; shift 2 ;;
      --print-checklist) PRINT_CHECKLIST=true; shift ;;
      -h|--help) usage; checklist_usage_extra; exit 0 ;;
      *) rest+=("$a"); shift ;;
    esac
  done
  CHECKLIST_REST=()
  [ ${#rest[@]} -gt 0 ] && CHECKLIST_REST=("${rest[@]}")
  [ "$PRINT_CHECKLIST" = true ] || return 0
  local i=0 n=${#CHECKLIST_REST[@]}
  while [ $i -lt $n ]; do
    a="${CHECKLIST_REST[$i]}"
    case "$a" in
      --src-endpoint)
        [ $((i + 1)) -lt $n ] || die 4 "--src-endpoint requires a value"
        SRC_EP="${CHECKLIST_REST[$((i + 1))]}"; i=$((i + 2)) ;;
      --dst-endpoint)
        [ $((i + 1)) -lt $n ] || die 4 "--dst-endpoint requires a value"
        DST_EP="${CHECKLIST_REST[$((i + 1))]}"; i=$((i + 2)) ;;
      # The consumers run INSIDE the controller/worker network namespaces, where
      # the host's 127.0.0.1 is their own loopback. Give the address they can
      # actually reach (e.g. http://mig-target:9000); defaults to --dst-endpoint.
      --dst-endpoint-in-container)
        [ $((i + 1)) -lt $n ] || die 4 "--dst-endpoint-in-container requires a value"
        DST_EP_IN_CONTAINER="${CHECKLIST_REST[$((i + 1))]}"; i=$((i + 2)) ;;
      *) die 4 "unexpected argument with --print-checklist: $a (print mode accepts only --deploy-mode, --src-endpoint, --dst-endpoint)" ;;
    esac
  done
}

render_cutover_checklist() {
  cat <<EOF
#!/usr/bin/env bash
# cutover-checklist.sh - generated at $(now_iso)
# deploy-mode=$DEPLOY_MODE | source=${SRC_EP:-<source endpoint>} | target=${DST_EP:-<target endpoint>} | shadow=$SHADOW_DAYS days
# Step tags: [tool] = done by the tool (one-command flow with --deployment, or the
#              legacy data-plane run), [manual] = an operator runs it. The [tool]
#              steps state exactly what the tool did and where the evidence is.
# [1] [tool]  P0 preflight green (P0-1..P16) + writer enumeration + original-config
#             snapshot to the backup dir (see state.json stage_detail; report-main.json)
#     on a RustFS target the ODM source fallback stays off after cutover (RUSTFS_ON_DEMAND_MIGRATION_ENABLED=false; RUSTFS_DURABILITY_MODE=$DST_DURABILITY unchanged)
# [2] [manual] freeze the writers you control OUTSIDE AgentTeams, then let the tool
#             freeze the members: stop the external clients that write to the source
#             FIRST (CI, cron, sync jobs), then run the tool (P1 stops the members).
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     member stop mechanism the tool uses (embedded):
#       agt update worker <name> --state Stopped   (per worker)
#       manager: agt apply -f <manager yaml with spec.state Stopped> (agt update manager has no --state)
#       # do NOT delete pods/containers to freeze: they are recreated immediately and keep writing
#     controller (embedded): it CANNOT be stopped - the source storage lives in it.
#       It is frozen operationally (no CR/config changes during the window) and the
#       tool proves the freeze with the quiesce window (3 equal samples of object
#       count / total bytes / top-level prefix digest) + the quiesce lock.
EOF
      ;;
  esac
  case "$DEPLOY_MODE" in
    helm|both)
      cat <<'EOF'
#     member stop mechanism the tool uses (helm):
#       kubectl -n <ns> patch worker <name> --type merge -p '{"spec":{"state":"Stopped"}}'
#       kubectl -n <ns> patch manager <name> --type merge -p '{"spec":{"state":"Stopped"}}'
#     controller (helm): the tool scales the controller deployment to 0 (strict tier)
EOF
      ;;
  esac
  cat <<'EOF'
# [3] [tool]  P2 data backfill (odm/mirror/inplace) + P3 IAM replay (report-data/
#             report-iam; every worker/manager user + policy re-created on the target)
# [4] [tool]  P4 FINAL delta (source still readable after the freeze - that is why
#             the delta runs after P1 and before the switch) + V1-V9 verify green
# [5] [tool]  P5 switch the controller config in ONE change (provider + endpoint +
#             bucket + credentials); the original env/values were snapshotted to the
#             backup dir BEFORE the switch (rollback source)
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     embedded (the tool rebuilds the container in the installer shape):
#       docker inspect agentteams-controller --format '{{json .Config.Env}}'   # captured into the backup dir
#       docker rm -f agentteams-controller
#       docker run -d --name agentteams-controller --network agentteams-net \
#         --network-alias matrix-local.agentteams.io --network-alias aigw-local.agentteams.io --network-alias fs-local.agentteams.io \
#         --env-file <backup dir>/controller-env-post.env \
#         -e AGENTTEAMS_STORAGE_PROVIDER=s3 \
#         -e AGENTTEAMS_FS_ENDPOINT=<dst endpoint> -e AGENTTEAMS_FS_BUCKET=<bucket> \
#         -e AGENTTEAMS_FS_ACCESS_KEY=<key> -e AGENTTEAMS_FS_SECRET_KEY=<secret> \
#         -v /var/run/docker.sock:/var/run/docker.sock --security-opt label=disable \
#         -v agentteams-data:/data -v <workspace>:/root/agentteams-fs/agents/manager \
#         -p 8080:8080 -p 8001:8001 -p 18088:18088 --restart unless-stopped <embedded image>
#       # WARNING: never use the installer reinstall path to change storage config (it deletes agentteams-data, the workspace and the env file)
#       # the tool re-asserts the new env inside the rebuilt container before continuing
EOF
      ;;
  esac
  case "$DEPLOY_MODE" in
    helm|both)
      cat <<'EOF'
#     helm:
#       helm upgrade <release> <chart> -n <ns> \
#         --set storage.provider=s3 --set storage.mode=existing --set storage.bucket=<bucket> \
#         --set storage.s3.endpoint=<dst endpoint> --set storage.s3.existingSecret=<secret> --wait
#       # the MinIO StatefulSet stops rendering after the switch; do NOT delete its PVC (it is the rollback data source)
#       # the tool re-asserts the new env in the controller deployment template before continuing
EOF
      ;;
  esac
  cat <<'EOF'
# [6] [tool]  P6 recreate every consumer with its ORIGINAL state (env is baked at
#             creation; "restart" does NOT re-read env)
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     embedded: docker rm -f <prefix><manager> <prefix><worker-1> ... where needed; the controller recreates them from the current config
EOF
      ;;
  esac
  case "$DEPLOY_MODE" in
    helm|both)
      cat <<'EOF'
#     helm:     kubectl -n <ns> delete pod <prefix><manager>; kubectl -n <ns> delete pod <prefix><worker>
EOF
      ;;
  esac
  cat <<EOF
#     per-component env assertion (the tool prints and checks each one):
#       inside a recreated consumer: env | grep AGENTTEAMS_FS_  -> new endpoint, same bucket as controller
# [7] [tool]  P7 per-component read/write + permission checks on the NEW backend,
#             each component with ITS OWN identity (not a storage-engine test):
#     controller: admin-identity probe object lands on the target only
#     each worker: write/read/list/delete in its own partition (agents/<name>/);
#       negative: worker A must get 403 writing worker B's partition (IAM isolation)
#     manager: write/read in manager/ + the shared/ human file area
#     quota: source-vs-target bucket quota compared (degrades to a warning when the
#       engine does not report quotas - there is no in-repo quota mechanism yet)
#     criterion: every probe object appears ONLY on the new backend; PASS table in the report
# [8] [manual] P8 resume + observation window: the tool resumes the members and
#             releases the quiesce lock; you watch the ${SHADOW_DAYS}-day window
#     keep the source read-only for the shadow window; do not delete it (it is the rollback data source)
#     rollback (copy-paste, any time before the source is retired):
#       <tool> <same args> --state-dir <state dir> --rollback
#     target-side new writes disposition:
#       (a) DEFAULT: discard - writes that landed on the target after the cutover are not preserved
#       (b) optional manual reverse delta (only while the source took no writes - Gate A empty), per bucket:
#         mc mirror --overwrite --remove <dst>/<bucket> <src>/<bucket>
#       --remove is safe ONLY while the source took no writes (Gate A empty); otherwise reconcile key by key and do NOT use --remove
#     rollback restores the original controller config from the backup dir AND the
#     original per-member states, then recreates consumers so their env points back
#     at the source
# [9] [manual] record completion: --mark-cutover-done records an operator ACKNOWLEDGEMENT
#             only; it does not verify restarts or configuration (the automated flow
#             marks CUTOVER_DONE itself at P8)
EOF
}

# ============ Phase cutover: P5-P8 chain (one-command; --deployment required) ============
# Gate order (stable for the test contract): stage first (exit 1), then
# --deployment (exit 4), then deployment reachability (fail-closed, exit 1).
phase_cutover() {
  log_info "phase: cutover start (P5-P8)"
  local stage
  stage=$(state_get '.stage')
  case "$stage" in
    CONFIG_SWITCHED)    p6_recreate_consumers; p7_verify_components; p8_resume; return 0 ;;
    CONSUMERS_RECREATED) p7_verify_components; p8_resume; return 0 ;;
    COMPONENT_VERIFIED) p8_resume; return 0 ;;
    VERIFIED) : ;;
    *) die 1 "state.stage=$stage (cutover requires state=VERIFIED - verify must be green first)" ;;
  esac
  if [ -z "$DEPLOYMENT" ]; then
    die 4 "cutover requires --deployment embedded|helm (one-command flow; without it the cutover stays manual via cutover-checklist.sh)"
  fi
  if ! deployment_reachable; then
    fail_notice "P5 switch" "${deployment_unreachable_msg} - refusing to touch production"
    exit 1
  fi
  [ -f "$STATE_DIR/quiesce.lock" ] || { fail_notice "P5 switch" "quiesce lock missing (writers not frozen - run --phase quiesce or --resume first)"; exit 1; }
  p5_switch_config
  p6_recreate_consumers
  p7_verify_components
  p8_resume
}

# ---- P5: switch the controller config (original config fully snapshotted first) ----
p5_switch_config() {
  log_info "P5: switch the controller config to the target backend"
  local bdir
  bdir=$(backup_dir_current)
  # A snapshot path recorded by a dry-run (or from another working directory)
  # can point at a directory that was never written. Create the rollback source
  # now rather than switching without one - the controller is still on the old
  # config at this point, so this is exactly when it must be captured.
  if [ "$DRY_RUN" = false ] && { [ -z "$bdir" ] || [ ! -d "$bdir" ]; }; then
    log_info "P5: original-config snapshot missing (${bdir:-unset}); capturing it now before the switch"
    bdir="$(backup_root_path)/$(date -u +%Y%m%d-%H%M%S)"
    mkdir -p "$bdir"
    case "$DEPLOYMENT" in
      embedded)
        docker inspect "$CONTROLLER_CONTAINER" > "$bdir/controller-inspect-pre.json" 2>/dev/null || true
        jq -c '.[0].Config.Env' "$bdir/controller-inspect-pre.json" > "$bdir/controller-env-pre.json" 2>/dev/null || echo '[]' > "$bdir/controller-env-pre.json"
        jq -r '.[0].Config.Image' "$bdir/controller-inspect-pre.json" 2>/dev/null > "$bdir/controller-image.txt" || true
        docker exec "$CONTROLLER_CONTAINER" agt get workers -o json > "$bdir/workers-pre.json" 2>/dev/null || true
        docker exec "$CONTROLLER_CONTAINER" agt get managers -o json > "$bdir/managers-pre.json" 2>/dev/null || true
        ;;
      helm)
        helm get values "$HELM_RELEASE" -n "$NS" > "$bdir/helm-values-pre.yaml" 2>/dev/null || true
        kubectl -n "$NS" get workers -o yaml > "$bdir/workers-pre.yaml" 2>/dev/null || true
        kubectl -n "$NS" get managers -o yaml > "$bdir/managers-pre.yaml" 2>/dev/null || true
        ;;
    esac
    state_set "original_config_snapshot" "\"$bdir\""
  fi
  [ -n "$bdir" ] && [ "$DRY_RUN" = true ] || { [ -d "$bdir" ] || { fail_notice "P5 switch" "original-config snapshot missing ($bdir) - refusing to switch without a rollback source"; exit 1; }; }
  state_set "stage_detail.p5.backup_dir" "\"$bdir\""
  case "$DEPLOYMENT" in
    embedded) p5_embedded "$bdir" ;;
    helm)     p5_helm "$bdir" ;;
  esac
  state_stage "CONFIG_SWITCHED"
}

p5_embedded() {
  local bdir="$1" env_pre image out_sh envfile
  assert_embedded_shape   # fail-closed on installer-shape drift BEFORE touching anything
  env_pre=$(cat "$bdir/controller-env-pre.json" 2>/dev/null || true)
  image=$(cat "$bdir/controller-image.txt" 2>/dev/null || true)
  if [ -z "$env_pre" ] || [ "$env_pre" = "[]" ] || [ -z "$image" ]; then
    fail_notice "P5 switch" "P0 snapshot incomplete ($bdir/controller-env-pre.json / controller-image.txt missing) - cannot reconstruct the controller"
    exit 1
  fi
  EMBED_WORKSPACE=$(jq -r '.[0].Mounts[] | select(.Type == "bind" and .Destination == "/root/agentteams-fs/agents/manager") | .Source' "$bdir/controller-inspect-pre.json" 2>/dev/null || true)
  local env_new
  env_new=$(swap_storage_env "$env_pre" "${DST_EP_IN_CONTAINER:-$DST_EP}" "$(first_bucket)" "$DST_AK" "$DST_SK")
  out_sh="$bdir/controller-run-post.sh"; envfile="$bdir/controller-env-post.env"
  render_controller_run "$out_sh" "$envfile" "$image" "$env_new"
  log_info "P5 embedded: switching the controller (docker rm -f + installer-shape docker run, env via --env-file; backup: $bdir)"
  exec_or_print_sh "docker rm -f $CONTROLLER_CONTAINER"
  exec_or_print_sh "bash $out_sh"
  if [ "$DRY_RUN" = false ]; then
    wait_controller_up 120 || { fail_notice "P5 switch" "controller did not come up on the new backend (check $out_sh)"; exit 1; }
    local env_now
    env_now=$(container_env_json "$CONTROLLER_CONTAINER")
    if [ "$(env_value "$env_now" AGENTTEAMS_STORAGE_PROVIDER)" != "s3" ]; then
      fail_notice "P5 switch" "env assert failed: AGENTTEAMS_STORAGE_PROVIDER != s3 in the rebuilt controller"
      exit 1
    fi
    # The container talks to the in-cluster address, not the host's loopback.
    local _ep_saved="$DST_EP"
    DST_EP="${DST_EP_IN_CONTAINER:-$DST_EP}"
    assert_consumer_env "$env_now" "P5 controller" || { DST_EP="$_ep_saved"; fail_notice "P5 switch" "controller env does not point at the target"; exit 1; }
    DST_EP="$_ep_saved"
  fi
  report_add "cutover" "p5-switch" "pass" "embedded controller switched to s3 (original env snapshotted: $bdir)"
}

p5_helm() {
  local bdir="$1" cdep tenv
  [ -n "$S3_SECRET" ] || { fail_notice "P5 switch" "helm mode requires --s3-secret <name> (an EXISTING k8s Secret with the s3 keys; the tool does not create secrets)"; exit 1; }
  if [ "$DRY_RUN" = false ] && ! kubectl -n "$NS" get secret "$S3_SECRET" >/dev/null 2>&1; then
    fail_notice "P5 switch" "secret '$S3_SECRET' not found in ns '$NS' - create it first (kubectl create secret generic $S3_SECRET --from-literal=accessKey=... --from-literal=secretKey=...)"
    exit 1
  fi
  cdep=$(helm_controller_deploy)
  log_info "P5 helm: helm upgrade (values backup: $bdir/helm-values-pre.yaml)"
  exec_or_print helm upgrade "$HELM_RELEASE" "$HELM_CHART" -n "$NS" \
    --set storage.provider=s3 --set storage.mode=existing --set storage.bucket="$(first_bucket)" \
    --set storage.s3.endpoint="$DST_EP" --set storage.s3.existingSecret="$S3_SECRET" --wait
  if [ "$DRY_RUN" = false ]; then
    if [ -n "$cdep" ]; then
      kubectl -n "$NS" rollout status "deploy/$cdep" --timeout=180s \
        || { fail_notice "P5 switch" "controller rollout failed after helm upgrade"; exit 1; }
      tenv=$(kubectl -n "$NS" get deploy "$cdep" -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -c '. // []')
      if ! jq -e 'map(select(.name == "AGENTTEAMS_STORAGE_PROVIDER" and .value == "s3")) | length > 0' <<<"$tenv" >/dev/null 2>&1; then
        fail_notice "P5 switch" "deployment template env assert failed (AGENTTEAMS_STORAGE_PROVIDER=s3 not found)"
        exit 1
      fi
    else
      log_warn "P5 helm: controller deployment not found by label - template env assert skipped (rollout status was the gate)"
    fi
  fi
  report_add "cutover" "p5-switch" "pass" "helm release switched to storage.provider=s3 existingSecret=$S3_SECRET"
}

# ---- P6: recreate consumers with their ORIGINAL states ----
p6_recreate_consumers() {
  log_info "P6: recreate consumers (original per-member states; env is baked at creation)"
  local writers k n orig cname
  writers=$(state_get '.stage_detail.writers // []')
  [ -n "$writers" ] && [ "$writers" != "[]" ] || { fail_notice "P6 recreate" "no writer list in state (P0-16 did not run with --deployment?)" ; exit 1; }
  while IFS=$'\t' read -r k n orig; do
    [ -z "${k:-}" ] && continue
    if [ "$orig" = "Stopped" ]; then
      log_info "P6 $k/$n was Stopped before the migration - stays Stopped (no container)"
      continue
    fi
    cname=$(member_container_name "$k" "$n")
    if [ "$DRY_RUN" = false ] && container_present "$cname"; then
      # a stale container may carry the old env - force a fresh one from the switched config
      log_info "P6 $k/$n: removing the stale container so the controller recreates it from the current config"
      delete_member_container "$cname"
    fi
    if ! set_member_state "$k" "$n" "$orig"; then
      fail_notice "P6 recreate" "restoring $k/$n state to $orig failed"
      exit 1
    fi
    if [ "$DRY_RUN" = false ]; then
      wait_for_container "$cname" 180 || { fail_notice "P6 recreate" "$k/$n container '$cname' did not come back (check controller logs)"; exit 1; }
      assert_consumer_env "$(container_env_json "$cname")" "P6 $k/$n" \
        || { fail_notice "P6 recreate" "$k/$n env does not point at the target backend"; exit 1; }
    fi
  done < <(jq -r '.[] | select(.state != "Stopped") | [.kind, .name, .state] | @tsv' <<<"$writers")
  report_add "cutover" "p6-recreate" "pass" "consumers recreated from the P0 writer list (original states; per-component env asserted)"
  state_stage "CONSUMERS_RECREATED"
}

# ---- P7: per-component read/write + permission verification on the NEW backend ----
# Each component probes with ITS OWN identity (the credentials the controller
# handed it, read from the container env) - not a storage-engine test.
p7_verify_components() {
  log_info "P7: per-component read/write + permission verification on the NEW backend"
  local ts="p7-$(date +%s)" bucket writers fail7=""
  bucket=$(first_bucket)
  local pass_json="[]"
  add_pass() {
    pass_json=$(jq --argjson p "$pass_json" --argjson e "$1" '. + [$e]' <<<"$pass_json")
  }
  print_table() {
    log_info "P7 PASS table:"
    jq -r '.[] | "  \(.component): \(.verdict) (\(.detail))"' <<<"$pass_json"
  }

  if [ "$DRY_RUN" = true ]; then
    log_info "[dry-run] P7 would run per-component W/R/L/D probes + cross-partition 403 negative + shared/ probe + source-absence + quota compare"
    report_add "cutover" "p7-components" "info" "dry-run: probes not executed"
    state_stage "COMPONENT_VERIFIED"
    return 0
  fi

  # --- controller (admin identity from its own env) ---
  local cenv cep cbucket cak csk
  cenv=$(container_env_json "$CONTROLLER_CONTAINER")
  # The probe runs mc on THIS host, so it needs the host-facing endpoint; the
  # container's own endpoint (e.g. http://mig-target:9000) is not resolvable
  # here. The credentials still come from the container env: they are what the
  # controller itself uses against the new backend.
  cep="$DST_EP"
  cbucket=$(env_value "$cenv" AGENTTEAMS_FS_BUCKET); [ -z "$cbucket" ] && cbucket="$bucket"
  cak=$(env_value "$cenv" AGENTTEAMS_FS_ACCESS_KEY); [ -z "$cak" ] && cak=$(env_value "$cenv" MINIO_USER)
  csk=$(env_value "$cenv" AGENTTEAMS_FS_SECRET_KEY); [ -z "$csk" ] && csk=$(env_value "$cenv" MINIO_PASSWORD)
  local calias="p7_controller"
  if mc_alias "$calias" "$cep" "$cak" "$csk"; then
    local ckey="agentteams-config/migrate-probe-controller-$ts.txt"
    if mc_put "probe-controller-$ts" "$calias/$cbucket/$ckey" && mc_get "$calias/$cbucket/$ckey" 2>/dev/null | grep -q probe-controller && mc_raw rm --force "$calias/$cbucket/$ckey" >/dev/null 2>&1 \
       && ! mc_raw ls "$SRC_ALIAS/$cbucket/$ckey" 2>/dev/null | grep -q "migrate-probe-controller"; then
      add_pass '{"component":"controller","verdict":"PASS","detail":"W/R/D with admin identity on the new backend only"}'
      log_info "P7 controller: PASS"
    else
      fail7="$fail7 controller"
      add_pass '{"component":"controller","verdict":"FAIL","detail":"probe did not complete on the new backend or leaked to the source"}'
    fi
  else
    fail7="$fail7 controller"
    add_pass '{"component":"controller","verdict":"FAIL","detail":"admin identity alias to the new backend refused"}'
  fi

  # --- members: each with its OWN identity from the recreated container env ---
  writers=$(state_get '.stage_detail.writers // []')
  local wnames
  # Every worker gets a row: a member whose container did not come up after the
  # recreate is reported as SKIP (not silently omitted), so the table always
  # accounts for each component the deployment has.
  wnames=$(jq -r '.[] | select(.kind == "worker") | .name' <<<"$writers")
  local k n cname menv mep mbucket mak msk malias mkey cross_key other
  while IFS= read -r n; do
    [ -z "$n" ] && continue
    k="worker"; cname=$(member_container_name "$k" "$n")
    # P6 recreates consumers asynchronously; wait for the container before
    # probing it, otherwise the row would be skipped or fail on empty env.
    local _w=0
    while [ "$_w" -lt 120 ]; do
      docker inspect -f '{{.State.Running}}' "$cname" 2>/dev/null | grep -q true && break
      sleep 5; _w=$((_w + 5))
    done
    if ! docker inspect -f '{{.State.Running}}' "$cname" 2>/dev/null | grep -q true; then
      fail7="$fail7 worker/$n"; add_pass "{\"component\":\"worker/$n\",\"verdict\":\"SKIP\",\"detail\":\"container did not reach Running within 120s after P6 recreate (state=$(_S=); agt state not re-read)\"}"; continue
    fi
    menv=$(container_env_json "$cname")
    mep=$(env_value "$menv" AGENTTEAMS_FS_ENDPOINT); mbucket=$(env_value "$menv" AGENTTEAMS_FS_BUCKET)
    mak=$(env_value "$menv" AGENTTEAMS_FS_ACCESS_KEY); msk=$(env_value "$menv" AGENTTEAMS_FS_SECRET_KEY)
    malias="p7_worker_$n"
    if ! mc_alias "$malias" "${mep:-$DST_EP}" "$mak" "$msk"; then
      fail7="$fail7 worker/$n"; add_pass "{\"component\":\"worker/$n\",\"verdict\":\"FAIL\",\"detail\":\"own-identity alias to the new backend refused\"}"; continue
    fi
    mkey="agents/$n/migrate-probe-$ts.txt"
    # W/R/L/D in its own partition
    local own_ok=1
    mc_put "probe-worker-$ts" "$malias/${mbucket:-$bucket}/$mkey" || own_ok=0
    mc_get "$malias/${mbucket:-$bucket}/$mkey" 2>/dev/null | grep -q probe-worker || own_ok=0
    mc_raw ls "$malias/${mbucket:-$bucket}/agents/$n" >/dev/null 2>&1 || own_ok=0
    mc_raw rm --force "$malias/${mbucket:-$bucket}/$mkey" >/dev/null 2>&1 || own_ok=0
    # source-absence: the probe object must NOT exist on the source
    mc_raw ls "$SRC_ALIAS/${mbucket:-$bucket}/$mkey" 2>/dev/null | grep -q "migrate-probe-$ts" && own_ok=0
    # cross-partition negative: worker n must NOT write another worker's partition (403)
    other=$(grep -vx "$n" <<<"$wnames" | head -1)
    local cross_prefix="agents/${other:-manager}"
    if [ -n "$other" ]; then cross_prefix="agents/$other"; fi
    if mc_put "probe-cross-$ts" "$malias/${mbucket:-$bucket}/$cross_prefix/migrate-probe-cross-$ts.txt" 2>/dev/null; then
      mc_raw rm --force "$malias/${mbucket:-$bucket}/$cross_prefix/migrate-probe-cross-$ts.txt" >/dev/null 2>&1 || true
      own_ok=0
    fi
    # shared/ human file area
    mc_put "probe-shared-$ts" "$malias/${mbucket:-$bucket}/shared/migrate-probe-shared-$ts-$n.txt" 2>/dev/null \
      && mc_raw rm --force "$malias/${mbucket:-$bucket}/shared/migrate-probe-shared-$ts-$n.txt" >/dev/null 2>&1 || log_warn "P7 worker/$n: shared/ (human file area) probe not writable - check the policy"
    if [ "$own_ok" = 1 ]; then
      add_pass "{\"component\":\"worker/$n\",\"verdict\":\"PASS\",\"detail\":\"W/R/L/D own partition + cross 403 + source-absence\"}"
      log_info "P7 worker/$n: PASS"
    else
      fail7="$fail7 worker/$n"
      add_pass "{\"component\":\"worker/$n\",\"verdict\":\"FAIL\",\"detail\":\"own-identity probe or the 403 negative failed\"}"
    fi
  done <<<"$wnames"

  # --- manager(s) ---
  local mnames mn
  mnames=$(jq -r '.[] | select(.kind == "manager" and .state != "Stopped") | .name' <<<"$writers")
  while IFS= read -r mn; do
    [ -z "$mn" ] && continue
    k="manager"; cname=$(member_container_name "$k" "$mn")
    menv=$(container_env_json "$cname")
    mep=$(env_value "$menv" AGENTTEAMS_FS_ENDPOINT); mbucket=$(env_value "$menv" AGENTTEAMS_FS_BUCKET)
    mak=$(env_value "$menv" AGENTTEAMS_FS_ACCESS_KEY); msk=$(env_value "$menv" AGENTTEAMS_FS_SECRET_KEY)
    malias="p7_manager_$mn"
    if ! mc_alias "$malias" "${mep:-$DST_EP}" "$mak" "$msk"; then
      fail7="$fail7 manager/$mn"; add_pass "{\"component\":\"manager/$mn\",\"verdict\":\"FAIL\",\"detail\":\"own-identity alias to the new backend refused\"}"; continue
    fi
    local mg_ok=1
    mc_put "probe-manager-$ts" "$malias/${mbucket:-$bucket}/manager/migrate-probe-$ts.txt" || mg_ok=0
    mc_get "$malias/${mbucket:-$bucket}/manager/migrate-probe-$ts.txt" 2>/dev/null | grep -q probe-manager || mg_ok=0
    mc_raw rm --force "$malias/${mbucket:-$bucket}/manager/migrate-probe-$ts.txt" >/dev/null 2>&1 || mg_ok=0
    mc_put "probe-mshared-$ts" "$malias/${mbucket:-$bucket}/shared/migrate-probe-shared-$ts.txt" 2>/dev/null \
      && mc_raw rm --force "$malias/${mbucket:-$bucket}/shared/migrate-probe-shared-$ts.txt" >/dev/null 2>&1 || mg_ok=0
    if [ "$mg_ok" = 1 ]; then
      add_pass "{\"component\":\"manager/$mn\",\"verdict\":\"PASS\",\"detail\":\"W/R in manager/ + shared/ human file area\"}"
      log_info "P7 manager/$mn: PASS"
    else
      fail7="$fail7 manager/$mn"
      add_pass "{\"component\":\"manager/$mn\",\"verdict\":\"FAIL\",\"detail\":\"manager/ or shared/ probe failed\"}"
    fi
  done <<<"$mnames"

  # --- bucket quota: source vs target (degrades to a warning - no in-repo quota mechanism) ---
  local qsrc qdst
  qsrc=$(mc_raw admin bucket quota "$SRC_ALIAS/$bucket" 2>/dev/null | head -1 || true)
  qdst=$(mc_raw admin bucket quota "$DST_ALIAS/$bucket" 2>/dev/null | head -1 || true)
  if [ -n "$qsrc" ] && [ -n "$qdst" ]; then
    log_info "P7 quota: source='$qsrc' target='$qdst'"
    report_add "cutover" "p7-quota" "pass" "source='$qsrc' target='$qdst'"
  else
    log_warn "P7 quota: engine does not report bucket quotas (indeterminate - there is no in-repo quota mechanism; set the human file area quota by hand after cutover if needed)"
    report_add "cutover" "p7-quota" "warn" "quota indeterminate (engine does not report it)"
  fi

  print_table
  state_set "stage_detail.p7" "$pass_json"
  report_add "cutover" "p7-components" "info" "$(jq -c . <<<"$pass_json")"
  if [ -n "$fail7" ]; then
    fail_notice "P7 components" "component verification failed:$fail7 (the PASS table above shows which)"
    exit 1
  fi
  state_stage "COMPONENT_VERIFIED"
}

# ---- P8: resume + observation window + rollback command ----
p8_resume() {
  log_info "P8: resume complete + observation window"
  if [ "$DRY_RUN" = false ]; then
    local _fp
    _fp=$(state_get '.stage_detail.frozen_controller_pid // empty')
    if [ -n "${_fp:-}" ]; then
      docker exec "$CONTROLLER_CONTAINER" kill -CONT "$_fp" 2>/dev/null || true
      log_info "P8: controller process resumed (SIGCONT pid $_fp)"
      state_unset "stage_detail.frozen_controller_pid" 2>/dev/null || true
    fi
  fi
  local writers k n orig cname missing=""
  writers=$(state_get '.stage_detail.writers // []')
  if [ "$DRY_RUN" = false ]; then
    while IFS=$'\t' read -r k n orig; do
      [ -z "${k:-}" ] && continue
      [ "$orig" = "Stopped" ] && continue
      cname=$(member_container_name "$k" "$n")
      container_present "$cname" || missing="$missing $cname"
    done < <(jq -r '.[] | [.kind, .name, .state] | @tsv' <<<"$writers")
    if [ -n "$missing" ]; then
      fail_notice "P8 resume" "expected consumer containers missing:$missing"
      exit 1
    fi
  fi
  # the source goes read-only from here - OPERATIONAL DISCIPLINE, deliberately not
  # enforced via the source IAM (the rollback path must be able to use the source again)
  log_warn "P8: from now on the source $SRC_EP is READ-ONLY by discipline (point no client at it). The tool does NOT change the source IAM - the rollback path needs it."
  cat <<EOF
==================== OBSERVATION WINDOW ($SHADOW_DAYS days) ====================
  * daily: compare object counts source vs target; the source count must NOT grow
  * watch the controller + member logs for storage errors
  * keep the source and its data disk/PVC alive - it is the rollback data source
rollback (copy-paste): $0 <same-args> --state-dir $STATE_DIR --rollback
===============================================================================
EOF
  rm -f "$STATE_DIR/quiesce.lock"
  report_add "cutover" "p8-resume" "pass" "consumers running on the new backend; quiesce lock released; observation window started"
  state_stage "CUTOVER_DONE"
  report_write > "$STATE_DIR/report-final.json"
  log_info "ONE-COMMAND MIGRATION COMPLETE (stage=CUTOVER_DONE)"
}

# ============ rollback (restores original config + writer states) ============
do_rollback() {
  local stage scope
  stage=$(state_get '.stage')
  [ "$stage" != "INIT" ] || die 1 "state.stage=INIT - nothing to roll back"
  scope=$(rollback_scope "$stage")
  [ "$scope" = "unsupported" ] && die 1 "state.stage=$stage is not a rollback-able state"
  log_info "rollback: stage=$stage scope=$scope"
  if [ "$scope" = "config+writers" ]; then
    rollback_config
  fi
  rollback_writers "$scope"
  rm -f "$STATE_DIR/quiesce.lock" 2>/dev/null || true
  state_stage "ROLLED_BACK"
  report_write > "$STATE_DIR/report-rollback.json"
  cat <<EOF
rollback complete (stage=ROLLED_BACK).
Target-side new writes disposition:
  (a) DEFAULT: discard - writes that landed on the target after the cutover are NOT preserved.
  (b) optional manual reverse delta - only while the source took no writes (Gate A empty), per bucket:
        mc mirror --overwrite --remove <dst>/<bucket> <src>/<bucket>
      --remove is safe ONLY while Gate A is empty; otherwise reconcile key by key.
Start over with a FRESH --state-dir (this state file is terminal).
EOF
}

rollback_config() {
  local bdir
  bdir=$(backup_dir_current)
  [ -n "$bdir" ] && [ -d "$bdir" ] || { log_fail "rollback: original-config snapshot missing ($bdir) - restore the controller config by hand from your own records"; return 0; }
  case "$DEPLOYMENT" in
    embedded)
      local env_pre image out_sh envfile ws
      env_pre=$(cat "$bdir/controller-env-pre.json" 2>/dev/null || true)
      image=$(cat "$bdir/controller-image.txt" 2>/dev/null || true)
      if [ -z "$env_pre" ] || [ "$env_pre" = "[]" ]; then
        log_fail "rollback: $bdir/controller-env-pre.json missing - restore the controller env by hand"
        return 1
      fi
      ws=$(jq -r '.[0].Mounts[] | select(.Type == "bind" and .Destination == "/root/agentteams-fs/agents/manager") | .Source' "$bdir/controller-inspect-pre.json" 2>/dev/null || true)
      EMBED_WORKSPACE="$ws"
      out_sh="$bdir/controller-run-rollback.sh"; envfile="$bdir/controller-env-rollback.env"
      render_controller_run "$out_sh" "$envfile" "${image:-<embedded image>}" "$env_pre"
      log_info "rollback: rebuilding the embedded controller with its ORIGINAL env ($bdir)"
      exec_or_print_sh "docker rm -f $CONTROLLER_CONTAINER"
      exec_or_print_sh "bash $out_sh"
      [ "$DRY_RUN" = false ] && wait_controller_up 120 || true
      if [ "$DRY_RUN" = false ]; then
        local env_now
        env_now=$(container_env_json "$CONTROLLER_CONTAINER")
        log_info "rollback: controller env restored (endpoint=$(env_value "$env_now" AGENTTEAMS_FS_ENDPOINT), provider=$(env_value "$env_now" AGENTTEAMS_STORAGE_PROVIDER))"
      fi
      ;;
    helm)
      if [ -f "$bdir/helm-values-pre.yaml" ]; then
        log_info "rollback: helm upgrade -f $bdir/helm-values-pre.yaml"
        exec_or_print helm upgrade "$HELM_RELEASE" "$HELM_CHART" -n "$NS" -f "$bdir/helm-values-pre.yaml" --wait
      else
        log_fail "rollback: $bdir/helm-values-pre.yaml missing - restore the helm values by hand"
        return 1
      fi
      ;;
    *)
      log_info "rollback: no --deployment (writers-only scope anyway); nothing to restore on the config side"
      ;;
  esac
  return 0
}

rollback_writers() { # <scope>
  local scope="$1" writers k n orig cname ep
  writers=$(state_get '.stage_detail.writers // []')
  if [ -z "$writers" ] || [ "$writers" = "[]" ]; then
    log_info "rollback: no writer list in state (pre-0.3.0 state?) - restore the member states by hand"
    return 0
  fi
  # reverse of the stop order: managers first, then workers
  while IFS=$'\t' read -r k n orig; do
    [ -z "${k:-}" ] && continue
    if ! set_member_state "$k" "$n" "$orig"; then
      log_warn "rollback: $k/$n -> $orig FAILED (fix by hand)"
    fi
    [ "$orig" = "Stopped" ] && continue
    cname=$(member_container_name "$k" "$n")
    if [ "$scope" = "config+writers" ] && [ "$DRY_RUN" = false ] && container_present "$cname"; then
      # the consumer may carry the SWITCHED env (recreated in P6) - force a fresh one
      # from the RESTORED config
      log_info "rollback: $k/$n - recreating the container from the restored config"
      delete_member_container "$cname"
    fi
    if [ "$DRY_RUN" = false ]; then
      wait_for_container "$cname" 180 || { log_warn "rollback: $cname not back yet (the controller recreates it - check its logs)"; continue; }
      ep=$(env_value "$(container_env_json "$cname")" AGENTTEAMS_FS_ENDPOINT)
      case "$ep" in
        *$(ep_hostport "$SRC_EP")*) log_info "rollback: $k/$n env back on the source ($ep)" ;;
        *) log_warn "rollback: $k/$n env endpoint='$ep' (expected the source $(ep_hostport "$SRC_EP")) - if it still points at the target, delete the container once so the controller recreates it" ;;
      esac
    fi
  done < <(jq -r 'reverse | .[] | [.kind, .name, .state] | @tsv' <<<"$writers")
}

# ============ --resume: continue the chain from the current stage ============
run_resume_chain() {
  local stage dep chain ph
  stage=$(state_get '.stage')
  dep=$(state_get '.stage_detail.deployment // "none"')
  [ -n "$DEPLOYMENT" ] && dep="$DEPLOYMENT"
  chain=$(resume_chain_for "$stage" "$dep")
  log_info "resume: stage=$stage deployment=$dep -> chain: $chain"
  if printf '%s' "$chain" | grep -q '^('; then
    log_info "$chain"
    return 0
  fi
  for ph in $chain; do
    case "$ph" in
      P1) phase_quiesce ;;
      P2) phase_data ;;
      P3) phase_iam ;;
      P4) phase_verify ;;
      P5) p5_switch_config ;;
      P6) p6_recreate_consumers ;;
      P7) p7_verify_components ;;
      P8) p8_resume ;;
      *) die 1 "resume: unknown phase $ph in chain '$chain'" ;;
    esac
  done
  report_write > "$STATE_DIR/report-resume.json"
}

# ============ --dry-run: print the actual command lines for P1-P8 ============
print_full_plan() {
  local dep writers passes
  dep=$(state_get '.stage_detail.deployment // "none"')
  if [ "$dep" = "none" ]; then
    log_info "dry-run plan (legacy mode, no --deployment): P0 preflight (done) -> P2 data (mc mirror, mode=$MODE) -> P3 IAM replay -> P4 final delta + V1-V9; the cutover itself stays manual (cutover-checklist.sh)"
    return 0
  fi
  writers=$(state_get '.stage_detail.writers // []')
  passes=$(state_get '.stage_detail.passes // 1')
  log_info "dry-run plan (deployment=$dep): the actual commands P1-P8 would run:"
  log_info "P1 quiesce:"
  log_info "  [manual first] stop the external clients that write to $SRC_EP"
  local k n st cn
  while IFS=$'\t' read -r k n st; do
    [ -z "${k:-}" ] && continue
    if [ "$st" = "Stopped" ]; then
      log_info "  $k/$n already Stopped (skip)"
      continue
    fi
    case "$dep" in
      embedded)
        if [ "$k" = "worker" ]; then
          log_info "  docker exec $CONTROLLER_CONTAINER agt update worker $n --state Stopped"
        else
          log_info "  docker cp <manager-$n-state-Stopped.yaml> $CONTROLLER_CONTAINER:/tmp/agentteams-migrate-$n.yaml && docker exec $CONTROLLER_CONTAINER agt apply -f /tmp/agentteams-migrate-$n.yaml"
        fi ;;
      helm)
        log_info "  kubectl -n $NS patch $k $n --type merge -p {\"spec\":{\"state\":\"Stopped\"}}" ;;
    esac
  done < <(jq -r '.[] | [.kind, .name, .state] | @tsv' <<<"$writers")
  case "$dep" in
    helm)
      local cd2
      cd2=$(helm_controller_deploy)
      [ -n "$cd2" ] && log_info "  kubectl -n $NS scale deploy $cd2 --replicas=0 (controller, strict tier)"
      ;;
    embedded)
      log_info "  controller: stays up (hosts the source storage) - freeze asserted by quiesce.lock + window evidence" ;;
  esac
  log_info "  window evidence: $QUIESCE_SAMPLES equal samples x ${QUIESCE_INTERVAL}s (object count / total bytes / top-level prefix digest)"
  log_info "P2 data: mc mirror $SRC_ALIAS/<bucket> $DST_ALIAS/<bucket> (mode=$MODE, $passes pass(es))"
  log_info "P3 IAM: replay users/policies/service-credentials from the P0 snapshot"
  log_info "P4: final delta (mc mirror --newer-than <last pass anchor>) + V1-V9 + cutover-checklist.sh"
  case "$dep" in
    embedded)
      log_info "P5: (original env+inspect snapshotted to $(state_get '.original_config_snapshot // empty')) docker rm -f $CONTROLLER_CONTAINER && docker run (installer shape, AGENTTEAMS_STORAGE_PROVIDER=s3, AGENTTEAMS_FS_ENDPOINT=$DST_EP, AGENTTEAMS_FS_BUCKET=$(first_bucket), env via --env-file)" ;;
    helm)
      log_info "P5: helm upgrade $HELM_RELEASE $HELM_CHART -n $NS --set storage.provider=s3 --set storage.mode=existing --set storage.bucket=$(first_bucket) --set storage.s3.endpoint=$DST_EP --set storage.s3.existingSecret=${S3_SECRET:-<--s3-secret>} --wait" ;;
  esac
  log_info "P6: recreate consumers with their original states:"
  while IFS=$'\t' read -r k n st; do
    [ -z "${k:-}" ] && continue
    cn=$(member_container_name "$k" "$n")
    case "$dep" in
      embedded) log_info "  (if present) docker rm -f $cn ; docker exec $CONTROLLER_CONTAINER agt ... state=$st ; assert env AGENTTEAMS_FS_ENDPOINT=$DST_EP" ;;
      helm)     log_info "  (if present) kubectl -n $NS delete pod $cn ; kubectl -n $NS patch $k $n --type merge -p {\"spec\":{\"state\":\"$st\"}} ; assert pod env" ;;
    esac
  done < <(jq -r '.[] | select(.state != "Stopped") | [.kind, .name, .state] | @tsv' <<<"$writers")
  log_info "P7: per-component W/R/L/D probes (own identity) + cross-partition 403 negative + shared/ human file area probe + source-absence assert + quota compare + PASS table"
  log_info "P8: re-assert consumers, source read-only by discipline, $SHADOW_DAYS-day observation window, rollback command printed, quiesce lock released"
}

# ============ main ============
# status_json - machine-readable status for a future API/UI.
# Stable contract: schema id + stage + per-item preflight/verify verdicts +
# artifact paths. Reads only the state directory; needs no S3 access.
status_json() {
  local sf="$STATE_DIR/state.json" rf="$STATE_DIR/report.json"
  local schema="storage-migrate.status/v1"
  if [ ! -f "$sf" ]; then
    jq -n --arg s "$schema" --arg d "$STATE_DIR" \
      '{schema:$s, stage:"NONE", state_dir:$d, preflight:{}, verify:{}, artifacts:{}}'
    return 0
  fi
  local pre ver stage updated
  stage=$(jq -r '.stage // "UNKNOWN"' "$sf")
  updated=$(jq -r '.updated // ""' "$sf")
  if [ -f "$rf" ]; then
    pre=$(jq -c '[.[]? | select(.phase=="preflight")] | map({key:.id, value:.status}) | from_entries' "$rf" 2>/dev/null || echo '{}')
    ver=$(jq -c '[.[]? | select(.phase=="verify")] | map({key:.id, value:.status}) | from_entries' "$rf" 2>/dev/null || echo '{}')
  else
    pre='{}'; ver='{}'
  fi
  local bdir
  bdir=$(jq -r '.original_config_snapshot // empty' "$sf" 2>/dev/null || true)
  jq -n --arg s "$schema" --arg st "$stage" --arg up "$updated" \
    --argjson pre "$pre" --argjson ver "$ver" \
    --arg sd "$STATE_DIR" --arg sf "$sf" --arg rf "$rf" \
    --arg ck "$STATE_DIR/cutover-checklist.sh" --arg pl "$STATE_DIR/dryrun-plan.json" \
    --arg bd "$bdir" \
    '{schema:$s, stage:$st, updated_at:$up, preflight:$pre, verify:$ver,
      artifacts:{state:$sf, report:$rf, checklist:$ck, plan:$pl,
                 backup_dir:(if $bd == "" then null else $bd end)}}'
}

main() {
  checklist_preparse "$@"
  # --print-checklist is a read-only preview: pre-parse consumed its remaining
  # arguments (the endpoint values for the header), so it must not read or
  # write state and needs no mc - it bypasses parse_args entirely (which
  # would create the state directory and gate on dependencies).
  if [ "$PRINT_CHECKLIST" = true ]; then
    render_cutover_checklist
    exit 0
  fi
  parse_args "$0" ${CHECKLIST_REST[@]+"${CHECKLIST_REST[@]}"}
  # --status is a read-only introspection path: no dependency gate, no state
  # mutation. A future API/UI polls it to render progress.
  if [ "$STATUS_MODE" = true ]; then
    status_json
    exit 0
  fi
  require_deps
  trap 'on_interrupt' INT TERM
  [ -n "$SRC_EP" ] && [ -n "$SRC_AK" ] && [ -n "$SRC_SK" ] || die 4 "missing --src-endpoint/--src-ak/--src-sk (see --help)"
  [ -n "$DST_EP" ] && [ -n "$DST_AK" ] && [ -n "$DST_SK" ] || die 4 "missing --dst-endpoint/--dst-ak/--dst-sk"
  state_init
  report_add "run" "start" "info" "mode=$MODE profile=$PROFILE dry_run=$DRY_RUN script=$SCRIPT_VERSION"

  # --rollback / --resume operate on an existing state and bypass the phase gates
  if [ "$ROLLBACK" = true ]; then
    do_rollback
    exit 0
  fi
  if [ "$RESUME" = true ]; then
    run_resume_chain
    exit 0
  fi

  local stage
  stage=$(state_get '.stage')
  case "$PHASE" in
    all)
      [ "$stage" = "INIT" ] || die 1 "state.stage=$stage (not INIT): resume with --resume, or delete $STATE_FILE to start over"
      state_stage "PREFLIGHT"
      phase_preflight
      if [ "$DRY_RUN" = true ]; then
        print_full_plan
        report_write > "$STATE_DIR/dryrun-plan.json"
        log_info "dry-run complete: plan and preflight report -> $STATE_DIR/dryrun-plan.json (drop --dry-run to execute for real)"
        exit 0
      fi
      if [ -n "$DEPLOYMENT" ]; then
        phase_quiesce
      else
        log_warn "--deployment not given: legacy data-plane flow (P0->P2->P3->P4); writers are NOT stopped, the cutover stays manual (cutover-checklist.sh)"
      fi
      phase_data
      phase_iam
      phase_verify
      if [ "$CUTOVER" = true ] && [ -n "$DEPLOYMENT" ]; then
        phase_cutover
        report_write > "$STATE_DIR/report-main.json"
      else
        local dep_hint="${DEPLOYMENT:-embedded|helm}"
        log_info "main flow complete (P0..P4 green; writers frozen: $([ -n "$DEPLOYMENT" ] && echo yes || echo 'no - legacy mode')). cutover is a human gate: run --phase cutover --deployment $dep_hint with this same --state-dir"
        report_write > "$STATE_DIR/report-main.json"
      fi
      ;;
    quiesce)
      [ "$stage" = "PREFLIGHT" ] || die 1 "state.stage=$stage (quiesce requires state=PREFLIGHT - run --phase all preflight first, or delete the state)"
      [ -n "$DEPLOYMENT" ] || die 4 "quiesce requires --deployment embedded|helm"
      deployment_reachable || die 1 "${deployment_unreachable_msg} - refusing to stop writers"
      phase_quiesce
      report_write > "$STATE_DIR/report-quiesce.json"
      ;;
    data)
      case "$stage" in
        PREFLIGHT|QUIESCED) : ;;
        *) die 1 "state.stage=$stage (data requires state=PREFLIGHT or QUIESCED)" ;;
      esac
      phase_data
      report_write > "$STATE_DIR/report-data.json"
      ;;
    iam)
      [ "$stage" = "DATA" ] || die 1 "state.stage=$stage (iam requires state=DATA - after an IAM failure add --iam-secrets-file and resume with this phase)"
      phase_iam
      report_write > "$STATE_DIR/report-iam.json"
      ;;
    verify)
      [ "$stage" = "IAM" ] || die 1 "state.stage=$stage (verify requires state=IAM)"
      phase_verify
      report_write > "$STATE_DIR/report-verify.json"
      ;;
    cutover)
      case "$stage" in
        VERIFIED|CONFIG_SWITCHED|CONSUMERS_RECREATED|COMPONENT_VERIFIED) : ;;
        *) die 1 "state.stage=$stage (cutover requires state=VERIFIED - verify must be green first; if the cutover already started, --resume continues it)" ;;
      esac
      phase_cutover
      report_write > "$STATE_DIR/report-cutover.json"
      ;;
    mark-cutover-done)
      # Contract (pinned by tests/storage-migrate-state-machine.sh #9): the
      # manual acknowledgement is only valid while the cutover is PENDING. The
      # automated flow records completion itself in P8, so any other stage -
      # VERIFIED included - is a misuse and must be rejected.
      case "$stage" in
        CUTOVER_PENDING) : ;;
        *) die 1 "state.stage=$stage (mark-cutover-done requires state=CUTOVER_PENDING; the automated flow records completion in P8)" ;;
      esac
      state_stage "CUTOVER_DONE"
      report_add "cutover" "done" "pass" "operator acknowledgement recorded at $(now_iso) (it does not verify restarts or configuration)"
      report_write > "$STATE_DIR/report-final.json"
      log_info "state=CUTOVER_DONE - the $SHADOW_DAYS-day shadow period starts (keep the source read-only, do not delete)"
      ;;
    *) die 4 "unknown --phase=$PHASE (all|quiesce|data|iam|verify|cutover|mark-cutover-done)" ;;
  esac
}

main "$@"
