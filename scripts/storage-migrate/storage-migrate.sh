#!/usr/bin/env bash
# storage-migrate.sh - MinIO -> RustFS storage-plane migration (backfill + cutover)
# Design: docs/design/storage-external-s3-backend.md (migration section)
# Phases: 0 preflight, 1 data, 2 IAM replay, 3 verify, 4 cutover (human gate)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"

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

# restore the bucket list from state (cross-phase; preflight wrote src.buckets)
get_buckets() {
  state_get '.src.buckets | join(" ")'
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
      jq --arg n "$pname" '.policies[$n] | if .Policy then .Policy else . end' "$snap" > "$pfile"
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

phase_verify() {
  log_info "phase: verify start (V1-V9)"
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
    keys=$({ mc_raw ls --recursive "$SRC_ALIAS/$b" 2>/dev/null || true; } | awk '{print $NF}' | head -10)
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
  elif [ -z "$(jq -r '.lastModified // empty' <<<"$lmod_raw" | head -1)" ]; then
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
  log_info "verify green (V1-V9)"
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
      *) die 4 "unexpected argument with --print-checklist: $a (print mode accepts only --deploy-mode, --src-endpoint, --dst-endpoint)" ;;
    esac
  done
}

render_cutover_checklist() {
  cat <<EOF
#!/usr/bin/env bash
# cutover-checklist.sh - generated at $(now_iso) (human-executed; tick each item)
# deploy-mode=$DEPLOY_MODE | source=${SRC_EP:-<source endpoint>} | target=${DST_EP:-<target endpoint>} | shadow=$SHADOW_DAYS days
# Preconditions: verify green (V1-V9) + final delta applied + target write probe ok (see report-cutover.json)
# Step tags: [tool] = done by the tool run, [manual] = an operator runs it, [verify] = check the evidence.
# [1] [tool]  final delta + verify already done by this run (see report-cutover.json)
#     on a RustFS target the ODM source fallback stays off after cutover (RUSTFS_ON_DEMAND_MIGRATION_ENABLED=false; RUSTFS_DURABILITY_MODE=$DST_DURABILITY unchanged)
# [2] [manual] freeze ALL writers — external clients -> members -> controller
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     embedded:
#       agt update worker <name> --state Stopped   (repeat per worker, or your loop)
#       agt update manager <name> --state Stopped
#       # do NOT delete pods/containers to freeze: they are recreated immediately and keep writing
#       # controller: no CR/config changes during the window; final delta must run BEFORE the controller stops (embedded MinIO lives in it)
EOF
      ;;
  esac
  case "$DEPLOY_MODE" in
    helm|both)
      cat <<'EOF'
#     helm:
#       kubectl -n <ns> patch worker <name> --type merge -p '{"spec":{"state":"Stopped"}}'
#       kubectl -n <ns> patch manager <name> --type merge -p '{"spec":{"state":"Stopped"}}'
#       # strict tier (optional): kubectl -n <ns> scale deploy <release>-controller --replicas=0
EOF
      ;;
  esac
  cat <<'EOF'
# [verify] no writes: object counts stable on both ends for N seconds
# [3] [manual] switch deployment config atomically (provider + endpoint + bucket + credentials in ONE change)
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     embedded (manual container recreate; installer does not support external S3 yet):
#       docker inspect agentteams-controller --format '{{json .Config.Env}}'   # capture the full current env first
#       docker rm -f agentteams-controller
#       docker run -d --name agentteams-controller --network agentteams-net \
#         --network-alias matrix-local.agentteams.io --network-alias aigw-local.agentteams.io --network-alias fs-local.agentteams.io \
#         <all previous -e flags> \
#         -e AGENTTEAMS_STORAGE_PROVIDER=s3 \
#         -e AGENTTEAMS_FS_ENDPOINT=<dst endpoint> -e AGENTTEAMS_FS_BUCKET=<bucket> \
#         -e AGENTTEAMS_FS_ACCESS_KEY=<key> -e AGENTTEAMS_FS_SECRET_KEY=<secret> \
#         -v /var/run/docker.sock:/var/run/docker.sock --security-opt label=disable \
#         -v agentteams-data:/data -v <workspace>:/root/agentteams-fs/agents/manager \
#         -p 8080:8080 -p 8001:8001 -p 18088:8088 --restart unless-stopped <embedded image>
#       # WARNING: never use the installer reinstall path to change storage config (it deletes agentteams-data, the workspace and the env file)
EOF
      ;;
  esac
  case "$DEPLOY_MODE" in
    helm|both)
      cat <<'EOF'
#     helm:
#       helm upgrade --install <release> <chart> -n <ns> \
#         --set storage.provider=s3 --set storage.mode=existing --set storage.bucket=<bucket> \
#         --set storage.s3.endpoint=<dst endpoint> --set storage.s3.existingSecret=<secret>
#       # the MinIO StatefulSet stops rendering after the switch; do NOT delete its PVC (it is the rollback data source)
EOF
      ;;
  esac
  cat <<'EOF'
# [verify] controller: storage provider=s3 (external) in logs; env inside the new container/pod shows the new AGENTTEAMS_FS_*
# [4] [manual] recreate every consumer (env is baked at creation; "restart" does NOT re-read env)
EOF
  case "$DEPLOY_MODE" in
    embedded|both)
      cat <<'EOF'
#     embedded: docker rm -f <prefix><manager> <prefix><worker-1> ... ; controller recreates them from current config
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
  cat <<'EOF'
# [verify] inside a recreated consumer: env | grep AGENTTEAMS_FS_  -> new endpoint, same bucket as controller
# [5] [manual] per-component read/write + permission checks
#     controller: trigger one config deploy, confirm the object lands on the target
#     manager: write + read one object
#     each worker: write + read with its own credentials (proves the per-worker IAM policy exists on the target)
#     negative: worker A must get 403 reading worker B's prefix
# [6] [manual] resume traffic: agt update worker --state Running (or kubectl patch back); watch errors/audit
# [7] [manual] keep the source read-only for the shadow window; do not delete it
# [8] [manual] rollback (preserves target-side writes)
#     freeze target writers first, then per bucket:
#       mc mirror --overwrite --remove <dst>/<bucket> <src>/<bucket>
#     --remove is safe ONLY while the source took no writes (Gate A empty); otherwise reconcile key by key and do NOT use --remove
#     then flip the config back (provider=minio / previous env), recreate all consumers again and re-run [5]
# [9] [manual] record completion: --mark-cutover-done records an operator ACKNOWLEDGEMENT only; it does not verify restarts or configuration
EOF
}

# ============ Phase 4: cutover (three steps) ============
phase_cutover() {
  log_info "phase: cutover start"
  local buckets
  buckets=$(get_buckets)

  # --- (1) final delta (source changes since the last mirror pass -> target) ---
  local since=""
  # take the timestamp of the most recent mirror pass
  local i n_pass
  n_pass=$(state_get '.stage_detail.passes // 1')
  for i in $(seq "${n_pass:-1}" -1 1); do
    since=$(state_get ".stage_detail.mirror_pass_$i // empty")
    [ -n "$since" ] && break
  done
  if [ "$MODE" = "inplace" ]; then
    log_info "inplace: final delta is a no-op (data imported on first start)"
  elif [ -n "$since" ]; then
    log_info "final delta mirror --newer-than $since"
    for b in $buckets; do
      mc_raw mirror "$SRC_ALIAS/$b" "$DST_ALIAS/$b" --newer-than "$since" 2>&1 | tail -1 >&2 || die 1 "final delta failed bucket=$b"
    done
    report_add "cutover" "final-delta" "pass" "since=$since"
  else
    log_warn "no mirror timestamp to anchor on (odm mode) - rely on the V3 md5 sampling and confirm the source is frozen before continuing"
    report_add "cutover" "final-delta" "warn" "odm mode has no delta anchor; relies on the V6 write-freeze assertion"
  fi

  # --- (2) write probes on both sides ---
  local pb="${buckets%% *}" pk="cutover-probe-$(date +%s).txt"
  mc_put src-probe "$SRC_ALIAS/$pb/$pk" && mc_raw rm --force "$SRC_ALIAS/$pb/$pk" >/dev/null 2>&1 \
    && report_add "cutover" "probe-src" "pass" "source write probe OK" || report_add "cutover" "probe-src" "fail" "source write probe failed (frozen? if expected, continue)"
  mc_put dst-probe "$DST_ALIAS/$pb/$pk" && mc_get "$DST_ALIAS/$pb/$pk" 2>/dev/null | grep -q dst-probe \
    && mc_raw rm --force "$DST_ALIAS/$pb/$pk" >/dev/null 2>&1 \
    && report_add "cutover" "probe-dst" "pass" "target write probe OK" || die 1 "target write probe failed - cutover aborted (target not writable)"

  # --- (3) cutover checklist (human-executed; the script never changes production) ---
  state_set "stage_detail.deploy_mode" "\"$DEPLOY_MODE\""
  report_add "cutover" "deploy_mode" "info" "cutover checklist deploy-mode=$DEPLOY_MODE (recorded in state and report)"
  local list="$STATE_DIR/cutover-checklist.sh"
  render_cutover_checklist > "$list"
  chmod +x "$list"
  report_add "cutover" "checklist" "pass" "cutover checklist -> $list (human-executed; deploy-mode=$DEPLOY_MODE)"
  log_info "cutover steps done: final delta ok, both write probes ok, checklist -> $list"
  log_warn "after completing [2]-[9] in $list, record the state with: $0 <same args> --state-dir $(dirname "$STATE_FILE") --mark-cutover-done (records an operator acknowledgement only; it does not verify restarts or configuration)"
  state_set "stage" '"CUTOVER_PENDING"'
  report_write > "$STATE_DIR/report-cutover.json"
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
  jq -n --arg s "$schema" --arg st "$stage" --arg up "$updated" \
    --argjson pre "$pre" --argjson ver "$ver" \
    --arg sd "$STATE_DIR" --arg sf "$sf" --arg rf "$rf" \
    --arg ck "$STATE_DIR/cutover-checklist.sh" --arg pl "$STATE_DIR/dryrun-plan.json" \
    '{schema:$s, stage:$st, updated_at:$up, preflight:$pre, verify:$ver,
      artifacts:{state:$sf, report:$rf, checklist:$ck, plan:$pl}}'
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

  local stage
  stage=$(state_get '.stage')
  case "$PHASE" in
    all)
      [ "$stage" = "INIT" ] || die 1 "state.stage=$stage (not INIT): resume per phase with --phase, or delete $STATE_FILE to start over"
      state_stage "PREFLIGHT"
      phase_preflight
      if [ "$DRY_RUN" = true ]; then
        report_write > "$STATE_DIR/dryrun-plan.json"
        log_info "dry-run complete: plan and preflight report -> $STATE_DIR/dryrun-plan.json (drop --dry-run to execute for real)"
        exit 0
      fi
      phase_data
      phase_iam
      phase_verify
      log_info "main flow complete (preflight+data+IAM+verify green). cutover is a human gate: --phase cutover"
      report_write > "$STATE_DIR/report-main.json"
      ;;
    data)
      [ "$stage" = "PREFLIGHT" ] || die 1 "state.stage=$stage (data requires state=PREFLIGHT - run --phase all preflight first, or delete the state)"
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
      [ "$stage" = "VERIFIED" ] || die 1 "state.stage=$stage (cutover requires state=VERIFIED - verify must be green)"
      phase_cutover
      ;;
    mark-cutover-done)
      [ "$stage" = "CUTOVER_PENDING" ] || die 1 "state.stage=$stage (run --phase cutover and complete cutover-checklist.sh first)"
      state_stage "CUTOVER_DONE"
      report_add "cutover" "done" "pass" "operator acknowledgement recorded at $(now_iso) (it does not verify restarts or configuration)"
      report_write > "$STATE_DIR/report-final.json"
      log_info "state=CUTOVER_DONE - the $SHADOW_DAYS-day shadow period starts (keep the source read-only, do not delete)"
      ;;
    *) die 4 "unknown --phase=$PHASE (all|data|iam|verify|cutover|mark-cutover-done)" ;;
  esac
}

main "$@"
