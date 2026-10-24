#!/usr/bin/env bash
# Reproducible end-to-end test for the external S3-compatible storage
# backend (storage.provider=s3).
#
# Brings up two real engines in Docker and exercises on both:
#   * the S3 data-plane operation matrix (mb/cp/ls/stat/cat/du/rm/mirror)
#   * the controller's mc admin user/policy lifecycle (add/dup/
#     policy-create/dup/attach/dup/detach/remove/dup-remove)
# and prints the tested version matrix.
#
# Requirements: docker, network access to the image registries below.
# Ports 19020/19021 (MinIO src) and 19210 (RustFS dst) must be free.
#
# Usage:  bash test/e2e/external-s3-backend.sh
# Exit:   0 = all assertions passed, 1 = failure (details on stdout)
#
# Tested versions (2026-10-19, Raspberry Pi 5 arm64 + Docker):
#   MinIO  RELEASE.2025-09-07T16-13-09Z (commit 07c3a429) — migration source
#   RustFS 1.0.3 (rustfs/rustfs:1.0.3)                    — migration target
#   mc     RELEASE.2025-08-13T08-35-41Z (commit 7394ce0d) — same binary the
#          controller image bundles (higress/mc:20260216)

set -uo pipefail

MINIO_IMG="higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/minio:20260216"
RUSTFS_IMG="rustfs/rustfs:1.0.3"
MC_IMG="higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/mc:20260216"

WORKDIR="${E2E_WORKDIR:-$(mktemp -d "${TMPDIR:-/tmp}/e2e-externals3.XXXXXX")}"
# Pre-create the scratch dirs NOW, before any docker invocation: a bind
# mount to a missing path makes docker create the directory as root,
# which would break the host-side writes below.
mkdir -p "${WORKDIR}/src" "${WORKDIR}/dst"
SRC_NAME="e2e-externals3-src"
DST_NAME="e2e-externals3-dst"
SRC_PORT=19020
DST_PORT=19210
SRC_ENDPOINT="http://127.0.0.1:${SRC_PORT}"
DST_ENDPOINT="http://127.0.0.1:${DST_PORT}"
SRC_AK="srcadmin"; SRC_SK="srcpass12345"
DST_AK="dstadmin"; DST_SK="dstpass12345"
BUCKET="agentteams-storage"

FAILURES=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES+1)); }
expect_ok() { # expect_ok <desc> <cmd...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc (cmd: $*)"; fi
}
expect_fail() { # expect_fail <desc> <cmd...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then fail "$desc (expected failure)"; else pass "$desc"; fi
}

# fetch_obj <ak> <sk> <endpoint> <alias> <object> <outfile>
# Captures the raw object bytes into <outfile> and returns mc's exit code.
# Never compare object bytes through ``$(mc cat ...)``: command substitution
# strips trailing newlines, so b"keep\n" and b"keep" would compare equal
# (review P2). Capture to a file, then cmp/hash the file.
fetch_obj() {
  local ak="$1" sk="$2" ep="$3" alias="$4" obj="$5" out="$6"
  run_mc "${ak}" "${sk}" "${ep}" "${alias}" cat "${obj}" >"${out}" 2>/dev/null
}

# _cmp_obj <ak> <sk> <endpoint> <alias> <object> <expected-file>
# Silent byte comparison: 0 = identical, 1 = bytes differ, 2 = read failure.
_cmp_obj() {
  local ak="$1" sk="$2" ep="$3" alias="$4" obj="$5" want="$6" got rc
  got="${WORKDIR}/cmp-object.bin"
  if ! fetch_obj "${ak}" "${sk}" "${ep}" "${alias}" "${obj}" "${got}"; then
    return 2
  fi
  if cmp -s "${got}" "${want}"; then rc=0; else rc=1; fi
  rm -f "${got}"
  return "${rc}"
}

# obj_equals_file <desc> <ak> <sk> <endpoint> <alias> <object> <expected-file>
# Asserting wrapper around _cmp_obj (raw bytes, read failure is a failure).
obj_equals_file() {
  local desc="$1"
  shift
  _cmp_obj "$@"
  case $? in
    0) pass "${desc}" ;;
    1) fail "${desc} (bytes differ from the recorded value)" ;;
    *) fail "${desc} (object unreadable)" ;;
  esac
}

# mc invocation against a live engine via the same mc image the controller
# bundles. The alias set and the command run in ONE container (a throwaway
# container would lose the alias otherwise).
run_mc() { # run_mc <ak> <sk> <endpoint> <alias> <args...>
  local ak="$1" sk="$2" ep="$3" alias="$4"; shift 4
  local args=""
  local a
  for a in "$@"; do args+=$(printf '%q ' "$a"); done
  docker run --rm --network host \
    -v ${WORKDIR}/src:${WORKDIR}/src \
    -v ${WORKDIR}/dst:${WORKDIR}/dst \
    --entrypoint sh "${MC_IMG}" -c \
    "mc -q alias set '${alias}' '${ep}' '${ak}' '${sk}' >/dev/null 2>&1 && mc ${args}"
}

cleanup() {
  docker rm -f "${SRC_NAME}" "${DST_NAME}" >/dev/null 2>&1
  rm -rf ${WORKDIR}/*
}
trap cleanup EXIT

echo "=== pulling images (first run) ==="
docker pull -q "${MINIO_IMG}" || exit 1
docker pull -q "${RUSTFS_IMG}" || exit 1
docker pull -q "${MC_IMG}" || exit 1

echo "=== port preflight ==="
for p in "$SRC_PORT" "$((SRC_PORT+1))" "$DST_PORT"; do
  if ss -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}$"; then
    echo "FAIL: port ${p} is already in use (free it or override *_PORT)"; exit 1
  fi
done

echo "=== starting engines ==="
docker rm -f "${SRC_NAME}" "${DST_NAME}" >/dev/null 2>&1
docker run -d --name "${SRC_NAME}" --network host \
  -e MINIO_ROOT_USER="${SRC_AK}" -e MINIO_ROOT_PASSWORD="${SRC_SK}" \
  "${MINIO_IMG}" server /data --address ":${SRC_PORT}" --console-address ":$((SRC_PORT+1))" >/dev/null || exit 1
docker run -d --name "${DST_NAME}" --network host \
  -e RUSTFS_ACCESS_KEY="${DST_AK}" -e RUSTFS_SECRET_KEY="${DST_SK}" \
  -e RUSTFS_ADDRESS=":${DST_PORT}" -e RUSTFS_CONSOLE_ENABLE=false \
  "${RUSTFS_IMG}" >/dev/null || exit 1

wait_health() { # wait_health <ak> <sk> <endpoint> <name>
  # Health = admin plane reachable (admin user list). Note: `mc admin info`
  # is NOT supported identically across engines (RustFS rejects it), so the
  # probe uses the call both engines answer — the same call family the
  # controller's admin client uses.
  local ak="$1" sk="$2" ep="$3" name="$4"
  for _ in $(seq 1 60); do
    if run_mc "${ak}" "${sk}" "${ep}" probe "admin" "user" "list" probe >/dev/null 2>&1; then
      pass "${name} healthy"
      return 0
    fi
    sleep 1
  done
  fail "${name} did not become healthy in 60s"
  return 1
}
wait_health "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" "MinIO source" || exit 1
wait_health "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" "RustFS target" || exit 1

echo "=== S3 data-plane matrix (both engines) ==="
for side in src dst; do
  case "${side}" in
    src) AK="${SRC_AK}"; SK="${SRC_SK}"; EP="${SRC_ENDPOINT}" ;;
    dst) AK="${DST_AK}"; SK="${DST_SK}"; EP="${DST_ENDPOINT}" ;;
  esac
  AL="e2e${side}"
  mkdir -p "${WORKDIR}/${side}"
  head -c 1048576 /dev/urandom > "${WORKDIR}/${side}/blob.bin"

  # Every operation carries the alias prefix: mc's first-set alias is NOT
  # the implicit target of bare-bucket commands (verified on mc
  # RELEASE.2025-08-13: bare `mc mb` created a bucket elsewhere while
  # `mc cp alias/bucket/...` reported "bucket does not exist").
  expect_ok "${side}: mb"            run_mc "${AK}" "${SK}" "${EP}" "${AL}" mb "${AL}/${BUCKET}"
  expect_ok "${side}: cp put"        run_mc "${AK}" "${SK}" "${EP}" "${AL}" cp "${WORKDIR}/${side}/blob.bin" "${AL}/${BUCKET}/agents/w1/blob.bin"
  expect_ok "${side}: ls"            run_mc "${AK}" "${SK}" "${EP}" "${AL}" ls --recursive "${AL}/${BUCKET}"
  expect_ok "${side}: stat"          run_mc "${AK}" "${SK}" "${EP}" "${AL}" stat "${AL}/${BUCKET}/agents/w1/blob.bin"
  local_src=$(sha256sum "${WORKDIR}/${side}/blob.bin" | cut -d" " -f1)
  if ! fetch_obj "${AK}" "${SK}" "${EP}" "${AL}" "${AL}/${BUCKET}/agents/w1/blob.bin" "${WORKDIR}/${side}/blob.back"; then
    fail "${side}: cat roundtrip (object unreadable)"
  elif cmp -s "${WORKDIR}/${side}/blob.bin" "${WORKDIR}/${side}/blob.back"; then
    local_dst=$(sha256sum "${WORKDIR}/${side}/blob.back" | cut -d" " -f1)
    pass "${side}: cat roundtrip (byte-identical, sha256 ${local_dst:0:12}… = ${local_src:0:12}…)"
  else
    fail "${side}: cat roundtrip (bytes differ)"
  fi
  expect_ok "${side}: du"            run_mc "${AK}" "${SK}" "${EP}" "${AL}" du "${AL}/${BUCKET}"
  # cross-engine mirror (src -> dst) is covered once below; rm here
  expect_ok "${side}: rm object"     run_mc "${AK}" "${SK}" "${EP}" "${AL}" rm "${AL}/${BUCKET}/agents/w1/blob.bin"
  expect_fail "${side}: stat after rm" run_mc "${AK}" "${SK}" "${EP}" "${AL}" stat "${AL}/${BUCKET}/agents/w1/blob.bin"
done

echo "=== cross-engine mirror (src -> dst) ==="
head -c 524288 /dev/urandom > ${WORKDIR}/src/blob2.bin
run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc cp ${WORKDIR}/src/blob2.bin e2esrc/${BUCKET}/shared/blob2.bin >/dev/null 2>&1
# mirror needs both engine aliases in one container (mc mirror src dst)
mirror_out=$(docker run --rm --network host \
    -v "${WORKDIR}/src":"${WORKDIR}/src" -v "${WORKDIR}/dst":"${WORKDIR}/dst" \
    --entrypoint sh "${MC_IMG}" -c \
    "mc -q alias set e2esrc '${SRC_ENDPOINT}' '${SRC_AK}' '${SRC_SK}' >/dev/null 2>&1 && \
     mc -q alias set e2edst '${DST_ENDPOINT}' '${DST_AK}' '${DST_SK}' >/dev/null 2>&1 && \
     mc mirror --overwrite e2esrc/${BUCKET}/shared e2edst/${BUCKET}/shared" 2>&1)
if [ $? -eq 0 ]; then pass "mirror src->dst"; else fail "mirror src->dst (out: $mirror_out)"; fi
expect_ok "mirror object present on dst" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat e2edst/${BUCKET}/shared/blob2.bin

echo "=== mc admin user/policy lifecycle (both engines) ==="
POLICY_FILE="${WORKDIR}/policy.json"
cat > "${POLICY_FILE}" <<'JSON'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetBucketLocation"],"Resource":["arn:aws:s3:::agentteams-storage"]},{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::agentteams-storage"]},{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::agentteams-storage/agents/w1","arn:aws:s3:::agentteams-storage/agents/w1/","arn:aws:s3:::agentteams-storage/agents/w1/*"]}]}
JSON
mount_policy_file() { # policy file must be visible to the mc container
  local side="$1"
  cp "${POLICY_FILE}" "${WORKDIR}/${side}/policy.json"
}

for side in src dst; do
  case "${side}" in
    src) AK="${SRC_AK}"; SK="${SRC_SK}"; EP="${SRC_ENDPOINT}" ;;
    dst) AK="${DST_AK}"; SK="${DST_SK}"; EP="${DST_ENDPOINT}" ;;
  esac
  AL="e2e${side}"
  # mc admin needs the policy file inside the container: bind the per-side
  # scratch dir read-only and run alias+command in one container.
  mca() { # mca <side> <args...>
    local s="$1"; shift
    local ep ak sk
    case "${s}" in
      src) ep="${SRC_ENDPOINT}"; ak="${SRC_AK}"; sk="${SRC_SK}" ;;
      dst) ep="${DST_ENDPOINT}"; ak="${DST_AK}"; sk="${DST_SK}" ;;
    esac
    local args="" a
    for a in "$@"; do args+=$(printf '%q ' "$a"); done
    docker run --rm --network host \
      -v "${WORKDIR}/${s}:/scratch:ro" \
      -v ${WORKDIR}/src:${WORKDIR}/src \
      -v ${WORKDIR}/dst:${WORKDIR}/dst \
      --entrypoint sh "${MC_IMG}" -c \
      "mc -q alias set 'e2e${s}' '${ep}' '${ak}' '${sk}' >/dev/null 2>&1 && mc ${args}"
  }
  mount_policy_file "${side}"
  # Idempotency styles differ per engine and BOTH are valid inputs to the
  # controller's idempotent admin client (covered by the fake-mc unit
  # tests): MinIO rejects duplicates with "already ..." errors, RustFS
  # succeeds silently. Assert the engine behaviour matrix here.
  # user add is idempotent on BOTH engines (MinIO 2025-09-07 updates the
  # password; RustFS no-ops). Only the attach style differs.
  # Verified 2026-10-19: user add AND policy attach are silent no-ops on
  # BOTH engines (MinIO 2025-09-07 updates password / RustFS no-ops). The
  # controller's "already" error-string tolerance is defensive for older
  # engine versions.
  DUP_USER_ADD=expect_ok
  DUP_ATTACH=expect_ok
  expect_ok "${side}: admin user add"        mca "${side}" admin user add e2e${side} worker-e2e workerpass1
  ${DUP_USER_ADD} "${side}: admin user add dup (${side} style)" mca "${side}" admin user add e2e${side} worker-e2e workerpass1
  expect_ok "${side}: admin policy create"   mca "${side}" admin policy create e2e${side} worker-e2e /scratch/policy.json
  expect_ok "${side}: admin policy create dup (overwrite)" mca "${side}" admin policy create e2e${side} worker-e2e /scratch/policy.json
  expect_ok "${side}: admin policy attach"   mca "${side}" admin policy attach e2e${side} worker-e2e --user worker-e2e
  ${DUP_ATTACH} "${side}: admin policy attach dup (${side} style)" mca "${side}" admin policy attach e2e${side} worker-e2e --user worker-e2e
  expect_ok "${side}: admin policy detach"   mca "${side}" admin policy detach e2e${side} worker-e2e --user worker-e2e
  expect_ok "${side}: admin user remove"     mca "${side}" admin user remove e2e${side} worker-e2e
  expect_fail "${side}: admin user remove dup (absent user)" mca "${side}" admin user remove e2e${side} worker-e2e
done

echo "=== migration shadow regression (docs/design/storage-external-s3-backend.md) ==="
# The migration guide used to finish with "mc mirror --overwrite src dst"
# at cutover. Workers write to the TARGET during shadow (the source is
# read-only), so that blanket copy reverts newer target-side data — the
# review reproduced a stale state.txt overwriting the shadow version.
# This regression pins the corrected procedure end to end:
#   baseline manifest -> shadow writes on the target -> Gate A (source
#   unchanged) -> cutover performs NO source->target copy -> shadow
#   overwrite/create survive and the deletion is preserved -> reverse
#   rollback (--remove) carries all three back to the source.
MIG="shared/mig"
mirror_mig() { # mirror_mig <from:src|dst> <to:src|dst> [--remove]
  # Alias NAMES are fixed to their actual endpoints: mfsrc = source,
  # mfdst = target. Only the operands move (mf${from} -> mf${to}).
  # Binding the aliases to the dynamic from/to endpoints and then using
  # mf${from}/mf${to} reverses the direction twice: the rollback
  # (mirror_mig dst src --remove) then copied source->target instead of
  # target->source — clobbering the shadow update, deleting the
  # shadow-created object and resurrecting the shadow deletion it was
  # meant to carry back to the source.
  local from="$1" to="$2" extra="${3:-}"
  case "${from}" in src | dst) ;; *) echo "mirror_mig: bad from '${from}'" >&2; return 2 ;; esac
  case "${to}" in src | dst) ;; *) echo "mirror_mig: bad to '${to}'" >&2; return 2 ;; esac
  docker run --rm --network host \
    -v "${WORKDIR}/src":"${WORKDIR}/src" -v "${WORKDIR}/dst":"${WORKDIR}/dst" \
    --entrypoint sh "${MC_IMG}" -c \
    "mc -q alias set mfsrc '${SRC_ENDPOINT}' '${SRC_AK}' '${SRC_SK}' >/dev/null 2>&1 && \
     mc -q alias set mfdst '${DST_ENDPOINT}' '${DST_AK}' '${DST_SK}' >/dev/null 2>&1 && \
     mc mirror --overwrite ${extra} mf${from}/${BUCKET}/${MIG} mf${to}/${BUCKET}/${MIG}"
}

# 1) baseline: three objects on the source, bulk-copied to the target
printf 'v1\n'    > "${WORKDIR}/src/mig-state.txt"
printf 'keep\n'  > "${WORKDIR}/src/mig-keep.txt"
printf 'delete\n' > "${WORKDIR}/src/mig-del.txt"
expect_ok "mig: seed source state" run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc cp "${WORKDIR}/src/mig-state.txt" "e2esrc/${BUCKET}/${MIG}/state.txt"
expect_ok "mig: seed source keep"  run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc cp "${WORKDIR}/src/mig-keep.txt"  "e2esrc/${BUCKET}/${MIG}/keep.txt"
expect_ok "mig: seed source del"   run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc cp "${WORKDIR}/src/mig-del.txt"   "e2esrc/${BUCKET}/${MIG}/del.txt"
if mirror_mig src dst >/dev/null 2>&1; then pass "mig: bulk copy src->dst"; else fail "mig: bulk copy src->dst"; fi
BASELINE=$(run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc ls --recursive --json "e2esrc/${BUCKET}/${MIG}" 2>/dev/null | sort)
if [ -n "${BASELINE}" ]; then pass "mig: baseline manifest captured"; else fail "mig: baseline manifest empty"; fi

# 2) shadow writes on the TARGET (workers point at it; source read-only)
printf 'v2-shadow\n' > "${WORKDIR}/dst/mig-state-v2.txt"
expect_ok "mig: shadow overwrite on target" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/dst/mig-state-v2.txt" "e2edst/${BUCKET}/${MIG}/state.txt"
expect_ok "mig: shadow delete on target"    run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst rm "e2edst/${BUCKET}/${MIG}/del.txt"
printf 'new\n' > "${WORKDIR}/dst/mig-new.txt"
expect_ok "mig: shadow create on target"    run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/dst/mig-new.txt" "e2edst/${BUCKET}/${MIG}/new.txt"

# 3) Gate A: the source is unchanged since the baseline
AFTER=$(run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc ls --recursive --json "e2esrc/${BUCKET}/${MIG}" 2>/dev/null | sort)
if [ "${BASELINE}" = "${AFTER}" ]; then pass "mig: Gate A — source unchanged since baseline"; else fail "mig: Gate A — source drifted during shadow"; fi

# 4) Gate B: the target holds the baseline plus the *recorded* shadow delta.
#    A deletion during shadow is intended state, not a gate failure — the
#    old "every baseline key present" check failed on the very deletion this
#    regression performs (and its "same size" wording also failed on the
#    intended overwrite). The delta is recorded explicitly:
#      update state.txt -> "v2-shadow" / delete del.txt / create new.txt
DELTA_UPDATE="state.txt"
DELTA_DELETE="del.txt"
DELTA_CREATE="new.txt"
# mc ls --recursive --json <alias>/<bucket>/<prefix> returns keys relative
# to <prefix>; re-attach it so every downstream object path is unambiguous.
# (Idempotent: keys that already carry the prefix pass through unchanged.)
key_rel() { sed -E 's/.*"key":"([^"]+)".*/\1/' | awk -v p="${MIG}/" 'index($0, p) == 1 { print; next } { print p $0 }'; }
TGT_MANIFEST=$(run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst ls --recursive --json "e2edst/${BUCKET}/${MIG}" 2>/dev/null | sort)
TGT_KEYS=$(printf '%s\n' "${TGT_MANIFEST}" | key_rel | sort)
BASE_KEYS=$(printf '%s\n' "${BASELINE}" | key_rel | sort)

# 4a) every baseline key the shadow did not delete is still on the target
GATE_B_MISSING=""
for k in ${BASE_KEYS}; do
  [ "${k##*/}" = "${DELTA_DELETE}" ] && continue
  printf '%s\n' "${TGT_KEYS}" | grep -qxF -- "${k}" || GATE_B_MISSING="${GATE_B_MISSING} ${k}"
done
if [ -z "${GATE_B_MISSING}" ]; then pass "mig: Gate B — non-deleted baseline keys present on target"; else fail "mig: Gate B — missing on target:${GATE_B_MISSING}"; fi

# 4b) the recorded deletion is preserved on the target (not resurrected)
if run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat "e2edst/${BUCKET}/${MIG}/${DELTA_DELETE}" >/dev/null 2>&1; then
  fail "mig: Gate B — recorded deletion was resurrected on the target"
else
  pass "mig: Gate B — recorded deletion preserved"
fi

# gate_b_content_probe [<key>...]
# Byte-integrity probe for untouched baseline objects (default key set = the
# non-delta baseline keys; explicit keys let the control tests exercise the
# same code path). 0 = all identical and readable; otherwise prints
# "<key>(<reason>)" items and returns 1.
gate_b_content_probe() {
  local bad="" k keys=""
  if [ "$#" -gt 0 ]; then
    keys="$*"
  else
    for k in ${BASE_KEYS}; do
      case "${k##*/}" in "${DELTA_UPDATE}" | "${DELTA_DELETE}") continue ;; esac
      keys="${keys} ${k}"
    done
  fi
  for k in ${keys}; do
    if ! fetch_obj "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc "e2esrc/${BUCKET}/${k}" "${WORKDIR}/probe-src.bin"; then
      bad="${bad} ${k}(unreadable-on-source)"; continue
    fi
    if ! fetch_obj "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst "e2edst/${BUCKET}/${k}" "${WORKDIR}/probe-dst.bin"; then
      bad="${bad} ${k}(missing-on-target)"; continue
    fi
    cmp -s "${WORKDIR}/probe-src.bin" "${WORKDIR}/probe-dst.bin" || bad="${bad} ${k}(content)"
  done
  if [ -z "${bad}" ]; then return 0; fi
  printf '%s' "${bad}"
  return 1
}

# 4c) untouched baseline objects are byte-identical on both sides.
#     Raw bytes are captured into files and compared with cmp: `$(mc cat)`
#     strips trailing newlines (b"keep\n" vs b"keep" would compare equal —
#     review P2) and hashing a failed read compares empty against empty.
if GATE_B_DRIFT=$(gate_b_content_probe); then
  pass "mig: Gate B — untouched baseline objects byte-identical"
else
  fail "mig: Gate B — content drift:${GATE_B_DRIFT}"
fi

# 4d) the recorded update holds the shadow content on the target — compared
#     as raw bytes against the exact file uploaded during shadow
obj_equals_file "mig: Gate B — recorded update intact" "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst "e2edst/${BUCKET}/${MIG}/${DELTA_UPDATE}" "${WORKDIR}/dst/mig-state-v2.txt"

# 4e) no keys outside baseline ∪ recorded-create appear on the target
EXTRA_KEYS=$(comm -13 <(printf '%s\n' "${BASE_KEYS}") <(printf '%s\n' "${TGT_KEYS}") | sed '/^$/d' | tr '\n' ' ' | sed 's/ *$//')
if [ "${EXTRA_KEYS}" = "${MIG}/${DELTA_CREATE}" ]; then pass "mig: Gate B — target keys = baseline + recorded create"; else fail "mig: Gate B — unexpected target keys: '${EXTRA_KEYS}'"; fi

# 5) cutover performs NO source->target copy: shadow data must survive
obj_equals_file "mig: cutover preserves the shadow overwrite" "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst "e2edst/${BUCKET}/${MIG}/state.txt" "${WORKDIR}/dst/mig-state-v2.txt"
if run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat "e2edst/${BUCKET}/${MIG}/del.txt" >/dev/null 2>&1; then
  fail "mig: cutover resurrected the deleted object"
else
  pass "mig: cutover preserves the deletion"
fi
expect_ok "mig: cutover keeps the shadow-created object" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat "e2edst/${BUCKET}/${MIG}/new.txt"

# 6) rollback: reverse sync must carry overwrite + create + deletion back
if mirror_mig dst src --remove >/dev/null 2>&1; then pass "mig: rollback reverse sync (--remove)"; else fail "mig: rollback reverse sync"; fi
STATE_SRC_DESC="mig: rollback preserves the shadow overwrite"
obj_equals_file "${STATE_SRC_DESC}" "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc "e2esrc/${BUCKET}/${MIG}/state.txt" "${WORKDIR}/dst/mig-state-v2.txt"
if run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc stat "e2esrc/${BUCKET}/${MIG}/del.txt" >/dev/null 2>&1; then
  fail "mig: rollback did not propagate the deletion"
else
  pass "mig: rollback propagates the deletion"
fi
expect_ok "mig: rollback imports the shadow-created object" run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc stat "e2esrc/${BUCKET}/${MIG}/new.txt"
expect_ok "mig: rollback keeps untouched baseline objects"  run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc stat "e2esrc/${BUCKET}/${MIG}/keep.txt"

# 6b) the reverse sync must not touch the target — it still holds the shadow
#     state. These assertions are what catch a reversed helper: the bug where
#     the "rollback" copies source->target reverts the shadow overwrite,
#     deletes the shadow-created object and resurrects the shadow deletion.
obj_equals_file "mig: rollback left the target's shadow overwrite intact" "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst "e2edst/${BUCKET}/${MIG}/${DELTA_UPDATE}" "${WORKDIR}/dst/mig-state-v2.txt"
expect_ok "mig: rollback left the target's shadow-created object intact" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat "e2edst/${BUCKET}/${MIG}/${DELTA_CREATE}"
if run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst stat "e2edst/${BUCKET}/${MIG}/${DELTA_DELETE}" >/dev/null 2>&1; then
  fail "mig: rollback resurrected the target's deletion"
else
  pass "mig: rollback left the target's deletion intact"
fi

echo "=== byte-integrity control tests (the probes must reject divergences) ==="
# N1) trailing-newline-only divergence: b"keep" vs b"keep\n". This is the
#     exact normalisation gap the review reproduced: through $(mc cat ...)
#     both sides become "keep" and the gate passes.
printf 'keep' > "${WORKDIR}/dst/neg-keep-nonl.txt"
expect_ok "ctrl: seed a trailing-newline-only divergence on the target" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/dst/neg-keep-nonl.txt" "e2edst/${BUCKET}/${MIG}/keep.txt"
if gate_b_content_probe >/dev/null; then
  fail "ctrl: probe accepted a trailing-newline-only divergence"
else
  pass "ctrl: probe rejects a trailing-newline-only divergence"
fi
expect_ok "ctrl: restore keep.txt with its recorded bytes" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/src/mig-keep.txt" "e2edst/${BUCKET}/${MIG}/keep.txt"
if gate_b_content_probe >/dev/null; then pass "ctrl: probe green again after restore"; else fail "ctrl: probe still red after restore"; fi

# N2) binary single-byte difference
printf '\x00\x01\x02\x03\x04\xff\xfe\x80\x7f\x00' > "${WORKDIR}/src/neg-bin.bin"
printf '\x00\x01\x02\x03\x04\xff\xfe\x80\x7f\x01' > "${WORKDIR}/dst/neg-bin-flip.bin"
expect_ok "ctrl: seed a binary object on the source" run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc cp "${WORKDIR}/src/neg-bin.bin" "e2esrc/${BUCKET}/${MIG}/neg-bin.bin"
expect_ok "ctrl: seed the identical binary object on the target" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/src/neg-bin.bin" "e2edst/${BUCKET}/${MIG}/neg-bin.bin"
if gate_b_content_probe "${MIG}/neg-bin.bin" >/dev/null; then pass "ctrl: probe green on identical binary objects"; else fail "ctrl: probe red on identical binary objects"; fi
expect_ok "ctrl: flip one byte on the target" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst cp "${WORKDIR}/dst/neg-bin-flip.bin" "e2edst/${BUCKET}/${MIG}/neg-bin.bin"
if gate_b_content_probe "${MIG}/neg-bin.bin" >/dev/null; then fail "ctrl: probe accepted a one-byte binary difference"; else pass "ctrl: probe rejects a one-byte binary difference"; fi

# N3) an unreadable object must fail the probe (never "empty == empty")
expect_ok "ctrl: delete the target copy" run_mc "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst rm "e2edst/${BUCKET}/${MIG}/neg-bin.bin"
if gate_b_content_probe "${MIG}/neg-bin.bin" >/dev/null; then fail "ctrl: probe passed with an unreadable target object"; else pass "ctrl: probe fails on an unreadable target object"; fi
expect_ok "ctrl: cleanup the binary fixture on the source" run_mc "${SRC_AK}" "${SRC_SK}" "${SRC_ENDPOINT}" e2esrc rm "e2esrc/${BUCKET}/${MIG}/neg-bin.bin"

# N4) the recorded-value check must fail on a read error even when the
#     recorded value file is empty (empty == empty is not a match)
: > "${WORKDIR}/neg-empty.txt"
if _cmp_obj "${DST_AK}" "${DST_SK}" "${DST_ENDPOINT}" e2edst "e2edst/${BUCKET}/${MIG}/does-not-exist.bin" "${WORKDIR}/neg-empty.txt"; then
  fail "ctrl: value check passed on a missing object with an empty expected file"
else
  pass "ctrl: value check fails on a missing object (empty expected file included)"
fi

echo "=== tested version matrix ==="
echo "minio : $(docker run --rm --entrypoint minio "${MINIO_IMG}" --version 2>/dev/null | head -1)"
echo "rustfs: $(docker run --rm --entrypoint /usr/bin/rustfs "${RUSTFS_IMG}" --version 2>/dev/null | head -1)"
echo "mc    : $(docker run --rm --entrypoint mc "${MC_IMG}" --version 2>/dev/null | head -1)"
echo "agentteams: ${AGENTTEAMS_REF:-$(git -C "$(dirname "${BASH_SOURCE[0]}")/../.." rev-parse --short HEAD 2>/dev/null || echo 'unspecified')}"

echo
if [ "${FAILURES}" -eq 0 ]; then
  echo "E2E RESULT: PASS (all assertions green on both engines)"
  exit 0
fi
echo "E2E RESULT: FAIL (${FAILURES} assertion(s) failed)"
exit 1
