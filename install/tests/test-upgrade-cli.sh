#!/usr/bin/env bash
# ============================================================================
# test-upgrade-cli.sh — regression suite for install/agentteams-upgrade.sh (P1)
#
# Pure bash + coreutils. No real docker, no network: a stub `docker` is
# PATH-prepended and answers inspect/exec/logs/cp/stop/start/rename/run.
# ============================================================================
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
UPGRADE="${REPO_ROOT}/install/agentteams-upgrade.sh"

if [ ! -f "${UPGRADE}" ]; then
    echo "FAIL setup: ${UPGRADE} not found"
    exit 1
fi

TMP="$(mktemp -d)"
cleanup() {
    rm -rf "${TMP}"
}
trap cleanup EXIT

BIN="${TMP}/bin"
mkdir -p "${BIN}"
REC="${TMP}/rec"
export REC

cat > "${BIN}/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${REC}"
case "$1" in
    info)
        exit 0
        ;;
    inspect)
        fmt="$3"
        case "$fmt" in
            *"len .HostConfig.PortBindings"*) echo 4 ;;
            *PortBindings*) printf '8080/tcp 0.0.0.0 18080\n9090/tcp 0.0.0.0 19090\n' ;;
            *"len .Mounts"*) echo 4 ;;
            *"/data"*) echo "agentteams-data" ;;
            *".Mounts"*) printf 'volume|agentteams-data|/var/lib/docker/volumes/agentteams-data/_data|/data|true|rprivate|local\nbind||/host/config|/config|true|rprivate|\n' ;;
            *"json .Config.Entrypoint"*) echo '["/entrypoint.sh"]' ;;
            *Config.Entrypoint*) echo "/entrypoint.sh" ;;
            *Config.Cmd*) printf 'arg1\n' ;;
            *NetworkMode*) echo "agentteams-net" ;;
            *RestartPolicy.Name*) echo "unless-stopped" ;;
            *"len .Config.Env"*) echo 2 ;;
            *Config.Env*) printf 'A=1\nB=2\n' ;;
            *Config.Image*) echo "example/controller:old" ;;
            *State.Running*) echo "true" ;;
            *RepoDigests*) echo "example/controller@sha256:old" ;;
            *".Id"*) echo "sha256:old" ;;
            *) echo "" ;;
        esac
        ;;
    exec)
        case "$*" in
            *"printenv AGENTTEAMS_FS_ENDPOINT"*)
                case "$*" in
                    *"agentteams-manager"*) echo "http://agentteams-controller:9000" ;;
                    *)
                        [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo "http://127.0.0.1:9000" || echo "http://rustfs.example:9000"
                        ;;
                esac
                ;;
            *"printenv AGENTTEAMS_STORAGE_PROVIDER"*)
                case "$*" in
                    *"agentteams-manager"*) echo "minio" ;;
                    *) [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo "" || echo "s3" ;;
                esac
                ;;
            *"printenv AGENTTEAMS_FS_BUCKET"*)
                case "$*" in
                    *"agentteams-manager"*) echo "agentteams-storage" ;;
                    *) [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo "" || echo "agentteams-storage" ;;
                esac
                ;;
            *"8080/v1/models"*) [ "${STUB_SCENARIO:-ok}" = "gateway_down" ] && echo "000" || echo "200" ;;
            *healthz*) [ "${STUB_SCENARIO:-ok}" = "health_fail" ] && exit 1 || exit 0 ;;
            *"agt get workers -o json"*)
                [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo '{"workers":[]}' || echo '{"workers":[{"name":"w1","soul":"full"}]}'
                ;;
            *"agt get teams"*)
                [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo "No teams found." || echo "team-a"
                ;;
            *"agt get workers"*)
                [ "${STUB_SCENARIO:-ok}" = "storage_embedded" ] && echo "No workers found." || echo "worker-a"
                ;;
            *du*) echo "1024" ;;
            *) exit 0 ;;
        esac
        ;;
    logs)
        [ "${STUB_SCENARIO:-ok}" = "log_panic" ] && echo "panic: boom" || true
        ;;
    stop|start|rename|cp|run|rm)
        exit 0
        ;;
    ps)
        echo "agentteams-controller"
        case "${STUB_SCENARIO:-ok}" in storage_split|storage_embedded) echo "agentteams-manager" ;; esac
        ;;
    *)
        exit 0
        ;;
esac
STUB
chmod +x "${BIN}/docker"
export PATH="${BIN}:${PATH}"

RC=0
OUT="${TMP}/out"

run_case() {
    local scenario="$1"
    shift
    : > "${REC}"
    STUB_SCENARIO="${scenario}" bash "${UPGRADE}" "$@" > "${OUT}" 2>&1
    RC=$?
}

assert_eq() {
    if [ "$1" != "$2" ]; then
        echo "FAIL $3: expected [$2], got [$1]"
        echo "--- output ---"
        cat "${OUT}"
        exit 1
    fi
}

assert_contains() {
    if ! grep -q -- "$1" "$2"; then
        echo "FAIL $3: [$1] not found in $2"
        echo "--- output ---"
        cat "${OUT}"
        exit 1
    fi
}

assert_not_contains() {
    if grep -q -- "$1" "$2"; then
        echo "FAIL $3: [$1] unexpectedly found in $2"
        echo "--- output ---"
        cat "${OUT}"
        exit 1
    fi
}

ENV_FILE="${TMP}/agentteams-manager.env"
cat > "${ENV_FILE}" <<'EOF'
AGENTTEAMS_DATA_DIR=agentteams-data
AGENTTEAMS_VERSION=v1.2.5
EOF

# 1. preflight happy path
run_case ok preflight --env-file "${ENV_FILE}" --backup-dir "${TMP}/b1" --json
assert_eq "${RC}" "0" "preflight happy rc"
assert_contains '"ok":true' "${OUT}" "preflight happy ok"
assert_contains '"ports":4' "${OUT}" "preflight happy ports"
assert_contains '"mounts":4' "${OUT}" "preflight happy mounts"
assert_contains '"backend":"unknown"' "${OUT}" "preflight happy storage backend"

# 2. missing env
run_case ok preflight --env-file "${TMP}/missing.env" --backup-dir "${TMP}/b2" --json
assert_eq "${RC}" "3" "preflight missing env rc"
assert_contains 'env file not found' "${OUT}" "preflight missing env message"

# 3. data-dir mismatch
cat > "${TMP}/mismatch.env" <<'EOF'
AGENTTEAMS_DATA_DIR=custom-vol
EOF
run_case ok preflight --env-file "${TMP}/mismatch.env" --backup-dir "${TMP}/b3" --json
assert_eq "${RC}" "3" "preflight mismatch rc"
assert_contains 'mismatch' "${OUT}" "preflight mismatch message"

# 3.5 external S3 / RustFS backend
cat > "${TMP}/external.env" <<'EOF'
AGENTTEAMS_DATA_DIR=agentteams-data
AGENTTEAMS_FS_BUCKET=agentteams-storage
AGENTTEAMS_FS_ENDPOINT=https://s3.example.com
EOF
run_case ok preflight --env-file "${TMP}/external.env" --backup-dir "${TMP}/b3e" --json
assert_eq "${RC}" "0" "preflight external rc"
assert_contains '"backend":"external-s3"' "${OUT}" "preflight external backend"

# 3.6 embedded MinIO backend
cat > "${TMP}/embedded.env" <<'EOF'
AGENTTEAMS_DATA_DIR=agentteams-data
AGENTTEAMS_FS_BUCKET=agentteams-storage
AGENTTEAMS_FS_ENDPOINT=http://agentteams-controller:9000
EOF
run_case ok preflight --env-file "${TMP}/embedded.env" --backup-dir "${TMP}/b3i" --json
assert_eq "${RC}" "0" "preflight embedded rc"
assert_contains '"backend":"embedded-minio"' "${OUT}" "preflight embedded backend"

# 3.7 external RustFS via AGENTTEAMS_FS_DOMAIN
cat > "${TMP}/domain.env" <<'EOF'
AGENTTEAMS_DATA_DIR=agentteams-data
AGENTTEAMS_FS_DOMAIN=rustfs-poc
EOF
run_case ok preflight --env-file "${TMP}/domain.env" --backup-dir "${TMP}/b3d" --json
assert_eq "${RC}" "0" "preflight domain rc"
assert_contains '"backend":"external-s3"' "${OUT}" "preflight domain backend"

# 3.8 live storage components are consistent (controller only in the stub)
run_case ok preflight --env-file "${TMP}/external.env" --backup-dir "${TMP}/b3s1" --json
assert_eq "${RC}" "0" "preflight storage consistent rc"
assert_contains '"consistent":true' "${OUT}" "preflight storage consistent flag"
assert_contains '"components":' "${OUT}" "preflight storage components array"

# 3.9 live split-brain fails closed
run_case storage_split preflight --env-file "${TMP}/external.env" --backup-dir "${TMP}/b3s2" --json
assert_eq "${RC}" "3" "preflight storage split rc"
assert_contains '"consistent":false' "${OUT}" "preflight storage split flag"
assert_contains 'storage split-brain' "${OUT}" "preflight storage split message"

# 3.10 explicit migration-window override
run_case storage_split preflight --env-file "${TMP}/external.env" --backup-dir "${TMP}/b3s3" --allow-storage-split --json
assert_eq "${RC}" "0" "preflight storage split allow rc"
assert_contains '"consistent":false' "${OUT}" "preflight storage split allow flag"
assert_contains 'continuing because --allow-storage-split' "${OUT}" "preflight storage split allow message"
# 3.11 fresh embedded MinIO via two endpoint aliases + empty cluster
run_case storage_embedded preflight --env-file "${TMP}/embedded.env" --backup-dir "${TMP}/b3s4" --json
assert_eq "${RC}" "0" "preflight embedded aliases rc"
assert_contains '"consistent":true' "${OUT}" "preflight embedded aliases flag"
assert_contains '"teams":0' "${OUT}" "preflight empty teams count"
assert_contains '"workers":{"total":0' "${OUT}" "preflight empty workers count"
assert_contains 'returned no rows (fresh install?)' "${OUT}" "preflight empty cluster warning"

# 4. backup dry-run: no stop/cp
run_case ok backup --dry-run --env-file "${ENV_FILE}" --backup-dir "${TMP}/b4"
assert_eq "${RC}" "0" "backup dry rc"
assert_not_contains '^stop ' "${REC}" "backup dry no stop"
assert_not_contains '^cp ' "${REC}" "backup dry no cp"

# 5. backup --yes: stop + cp + state
run_case ok backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5" --state-file "${TMP}/b5/state.json"
assert_eq "${RC}" "0" "backup yes rc"
assert_contains '^stop ' "${REC}" "backup yes stop"
assert_contains '^cp ' "${REC}" "backup yes cp"
[ -f "${TMP}/b5/state.json" ] || { echo "FAIL backup yes: state.json missing"; cat "${OUT}"; exit 1; }
assert_contains '"status":"done"' "${TMP}/b5/state.json" "backup yes state"

# 6. controller dry-run: no run/rename
run_case ok controller --dry-run --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b6"
assert_eq "${RC}" "0" "controller dry rc"
assert_not_contains '^run ' "${REC}" "controller dry no run"
assert_not_contains '^rename ' "${REC}" "controller dry no rename"
assert_contains '--entrypoint /entrypoint.sh' "${OUT}" "controller dry entrypoint"

# 7. controller health failure: auto rollback
run_case health_fail controller --yes --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b7" --state-file "${TMP}/b7/state.json" --timeout 4
[ "${RC}" = "3" ] || [ "${RC}" = "4" ] || { echo "FAIL controller fail rc: expected 3/4, got ${RC}"; cat "${OUT}"; exit 1; }
assert_contains '^rename ' "${REC}" "controller fail rollback rename"
if ! grep -qE '"status":"(rolled_back|rollback_failed)"' "${TMP}/b7/state.json"; then
    echo "FAIL controller fail state"
    cat "${TMP}/b7/state.json"
    exit 1
fi
assert_contains '"controller_image":"example/controller:old"' "${TMP}/b7/state.json" "controller fail restored image"

# 7.5 env drift: controller must fail closed without --env-from-container
cat > "${TMP}/drift.env" <<'EOF'
AGENTTEAMS_DATA_DIR=agentteams-data
AGENTTEAMS_VERSION=v1.2.5
AGENTTEAMS_EXTRA=1
EOF
run_case ok controller --yes --controller-image example/controller:new --env-file "${TMP}/drift.env" --backup-dir "${TMP}/b75" --timeout 2
assert_eq "${RC}" "3" "controller env drift rc"
assert_contains 'env drift' "${OUT}" "controller env drift message"
assert_not_contains '^run ' "${REC}" "controller env drift no run"
assert_not_contains '^rename ' "${REC}" "controller env drift no rename"

# 7.6 --env-from-container dry-run: print controller.env, no values
run_case ok controller --dry-run --env-from-container --controller-image example/controller:new --env-file "${TMP}/drift.env" --backup-dir "${TMP}/b76" --timeout 2
assert_eq "${RC}" "0" "controller env-from-container dry rc"
assert_contains 'controller.env' "${OUT}" "controller env-from-container path"
assert_not_contains 'A=1' "${OUT}" "controller env-from-container no values"

# 7.7 logs panic but healthz/agt OK: warn, do not roll back
run_case log_panic controller --yes --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b77" --state-file "${TMP}/b77/state.json" --timeout 4
assert_eq "${RC}" "0" "controller log-panic rc"
assert_contains 'logs contain panic|fatal' "${OUT}" "controller log-panic warning"
assert_contains '"status":"done"' "${TMP}/b77/state.json" "controller log-panic state"

# 7.8 gateway down: default fail/rollback
run_case gateway_down controller --yes --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b78" --state-file "${TMP}/b78/state.json" --timeout 4
[ "${RC}" = "3" ] || [ "${RC}" = "4" ] || { echo "FAIL gateway down rc: ${RC}"; cat "${OUT}"; exit 1; }
assert_contains 'gateway data plane' "${OUT}" "gateway down message"
assert_contains '"status":"rolled_back"' "${TMP}/b78/state.json" "gateway down rolled back"

# 7.9 gateway down + --allow-gateway-down: proceed with warning
run_case gateway_down controller --yes --allow-gateway-down --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b79" --state-file "${TMP}/b79/state.json" --timeout 4
assert_eq "${RC}" "0" "gateway allow rc"
assert_contains 'continuing because --allow-gateway-down' "${OUT}" "gateway allow warning"
assert_contains '"status":"done"' "${TMP}/b79/state.json" "gateway allow state"

# 7.10 Higress backfill dir: copy + restart, allow gateway down
mkdir -p "${TMP}/backfill/ai-statistics/1.0.0"
: > "${TMP}/backfill/ai-statistics/1.0.0/plugin.wasm"
run_case gateway_down controller --yes --allow-gateway-down --higress-backfill-dir "${TMP}/backfill" --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b710" --state-file "${TMP}/b710/state.json" --timeout 4
assert_eq "${RC}" "0" "backfill controller rc"
assert_contains 'backfill applied' "${OUT}" "backfill message"
assert_contains '^cp ' "${REC}" "backfill cp"
assert_contains '^restart ' "${REC}" "backfill restart"

# 8. status --json
run_case ok status --state-file "${TMP}/b5/state.json" --json
assert_eq "${RC}" "0" "status json rc"
assert_contains '"step"' "${OUT}" "status json step"

# 9. --json purity: stdout is pure JSON; warnings go to stderr
STUB_SCENARIO=ok bash "${UPGRADE}" preflight --env-file "${ENV_FILE}" --backup-dir "${TMP}/b9" --json > "${TMP}/json.out" 2> "${TMP}/json.err"
RC=$?
assert_eq "${RC}" "0" "preflight json purity rc"
first="$(head -c 1 "${TMP}/json.out")"
assert_eq "${first}" "{" "preflight json purity first char"
assert_not_contains 'No such file' "${TMP}/json.err" "preflight json no log-dir noise"

echo "PASS test-upgrade-cli (22 scenarios)"
