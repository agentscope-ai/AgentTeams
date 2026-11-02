#!/usr/bin/env bash
# An installation created against an external S3 must upgrade as the same
# backend. The five storage settings have to survive the save -> fresh-process
# -> load -> resolve round trip; explicit environment values still win.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
INSTALLER=install/agentteams-install.sh
TMP="$(mktemp -d)"; trap 'rm -rf "${TMP}"' EXIT
fails=0; passes=0
pass(){ passes=$((passes+1)); echo "PASS: $*"; }
fail(){ fails=$((fails+1)); echo "FAIL: $*"; }

# Extract one function body so the test drives the real code, not a copy.
extract(){ awk -v fn="$1" 'index($0, fn"()")==1{p=1} p{print} p&&/^}/{exit}' "${INSTALLER}"; }
extract load_current_params_from_env > "${TMP}/load.sh"
extract _resolve_controller_storage  > "${TMP}/resolve.sh"
grep -q 'load_current_params_from_env()' "${TMP}/load.sh" || { echo "FAIL: could not extract loader"; exit 1; }
grep -q '_resolve_controller_storage()'  "${TMP}/resolve.sh" || { echo "FAIL: could not extract resolver"; exit 1; }

die(){ echo "DIE: $*" >&2; return 1; }
log_info(){ :; }; log_warn(){ :; }

# A fresh process: only the saved file is available.
fresh(){ # envfile [VAR=value ...]
    local envfile="$1"; shift
    ( set +u; export AGENTTEAMS_ENV_FILE="${envfile}"
      unset AGENTTEAMS_STORAGE_PROVIDER AGENTTEAMS_FS_ENDPOINT AGENTTEAMS_FS_BUCKET             AGENTTEAMS_FS_ACCESS_KEY AGENTTEAMS_FS_SECRET_KEY
      for kv in "$@"; do export "${kv}"; done
      source "${TMP}/load.sh"; load_current_params_from_env
      source "${TMP}/resolve.sh"
      _resolve_controller_storage >/dev/null 2>&1 || true
      echo "${_storage_provider}|${_storage_endpoint}|${_storage_bucket}|${_storage_ak}" )
}

# 1) explicit S3 first install -> saved file
cat > "${TMP}/s3.env" <<ENV
AGENTTEAMS_STORAGE_PROVIDER=s3
AGENTTEAMS_FS_ENDPOINT=https://oss.example.com
AGENTTEAMS_FS_BUCKET=team-data
AGENTTEAMS_FS_ACCESS_KEY=AKIAEXAMPLE
AGENTTEAMS_FS_SECRET_KEY=c2VjcmV0
ENV
got="$(fresh "${TMP}/s3.env")"
[ "${got%%|*}" = "s3" ] && pass "upgrade keeps the external backend" || fail "backend reverted: ${got}"
case "${got}" in *"oss.example.com|team-data|AKIAEXAMPLE") pass "endpoint/bucket/access key restored" ;; *) fail "values lost: ${got}" ;; esac

# 2) control: default installation saved without the keys -> embedded MinIO
printf 'AGENTTEAMS_LLM_PROVIDER=qwen\n' > "${TMP}/minio.env"
got="$(fresh "${TMP}/minio.env")"
[ "${got%%|*}" = "minio" ] && pass "control: default MinIO install still resolves to minio" || fail "control: ${got}"
case "${got}" in *"127.0.0.1:9000"*) pass "control: embedded endpoint" ;; *) fail "control endpoint: ${got}" ;; esac

# 3) control: an explicit environment value overrides the saved file
got="$(fresh "${TMP}/s3.env" AGENTTEAMS_FS_BUCKET=override-bucket)"
case "${got}" in *"override-bucket"*) pass "explicit env overrides the saved value" ;; *) fail "override ignored: ${got}" ;; esac

# 4) the saved file really carries all five keys (guards the writer, not just the reader)
for k in AGENTTEAMS_STORAGE_PROVIDER AGENTTEAMS_FS_ENDPOINT AGENTTEAMS_FS_BUCKET AGENTTEAMS_FS_ACCESS_KEY AGENTTEAMS_FS_SECRET_KEY; do
    grep -q "^${k}=" "${INSTALLER}" && pass "installer persists ${k}" || fail "installer does not persist ${k}"
done

echo
[ "${fails}" -eq 0 ] && { echo "ALL PASS: storage round trip (${passes} checks)"; exit 0; }
echo "${fails} FAILURES (${passes} passed)"; exit 1
