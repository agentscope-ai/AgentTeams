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
RUNNING_FILE="${REC}.running"
export REC

cat > "${BIN}/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${REC}"
RUNNING_FILE="${REC}.running"
running_state() { if [ -f "${RUNNING_FILE}" ]; then cat "${RUNNING_FILE}"; else echo true; fi }
shm_value() {
    # $1 = container name. The original container reports its own size; a
    # container created in this run reports the daemon default unless the
    # upgrade passed --shm-size explicitly.
    local name="$1"
    case "${STUB_SCENARIO:-ok}" in
        shm_drift|shm_verify_mismatch)
            if [ -f "${REC}.create" ] && grep -q -- "--name ${name} " "${REC}.create"; then
                if [ "${STUB_SCENARIO}" = "shm_verify_mismatch" ]; then echo 8403288064; return; fi
                if grep -q -- "--shm-size 67108864" "${REC}.create"; then echo 67108864; else echo 8403288064; fi
            else
                echo 67108864
            fi
            ;;
        boundary) echo 134217728 ;;
        *) echo 67108864 ;;
    esac
}
case "$1" in
    info)
        exit 0
        ;;
    image)
        # docker image inspect --format <fmt> <ref>  -> $4 = fmt, $5 = ref
        fmt="$4"
        ref="$5"
        case "${STUB_SCENARIO:-ok}" in
            image_inspect_fail|image_inspect_fail_health) exit 1 ;;
            image_retag)
                # The test tag was moved to another cached image: inspecting
                # the tag returns the other image, inspecting the container's
                # immutable ID keeps the original.
                if [ "${ref}" = "sha256:stableimageid" ]; then
                    case "$fmt" in
                        *"{{.Id}}"*) echo "sha256:stableimageid" ;;
                        *RepoDigests*) printf 'example/controller@sha256:stable\n' ;;
                    esac
                else
                    case "$fmt" in
                        *"{{.Id}}"*) echo "sha256:retagged" ;;
                        *RepoDigests*) printf 'example/controller@sha256:retagged\n' ;;
                    esac
                fi
                exit 0
                ;;
        esac
        case "$fmt" in
            *"{{.Id}}"*) echo "sha256:imageid" ;;
            *RepoDigests*)
                [ "${STUB_SCENARIO:-ok}" = "image_no_digest" ] && exit 0
                printf 'example/controller@sha256:aaa\nexample/controller@sha256:bbb\n'
                ;;
        esac
        exit 0
        ;;
    inspect)
        fmt="$3"
        # OomKillDisable defaults: an unset *bool renders <nil>, docker create
        # stores false. Keep these scenarios separate from the full boundary set.
        if [ "${STUB_SCENARIO:-ok}" = "boundary_nil_false" ] || [ "${STUB_SCENARIO:-ok}" = "boundary_true_false" ]; then
            case "$fmt" in
                *"{{.HostConfig.OomKillDisable}}"*)
                    case "$4" in
                        *-old*)
                            [ "${STUB_SCENARIO:-ok}" = "boundary_true_false" ] && echo "true" || echo "<nil>"
                            ;;
                        *) echo "false" ;;
                    esac
                    exit 0
                    ;;
            esac
        fi
        # scenario overrides (boundary / unsupported_sysctl); everything else
        # falls through to the standard answers below
        if [ "${STUB_SCENARIO:-ok}" = "boundary" ]; then
            case "$fmt" in
                *"{{.Config.User}}"*) echo "1000:1000"; exit 0 ;;
                *"{{.Config.WorkingDir}}"*) echo "/work"; exit 0 ;;
                *"{{.HostConfig.ReadonlyRootfs}}"*) echo "true"; exit 0 ;;
                *"{{.HostConfig.Memory}}"*) echo "268435456"; exit 0 ;;
                *"{{.HostConfig.NanoCpus}}"*) echo "500000000"; exit 0 ;;
                *"{{.HostConfig.PidsLimit}}"*) echo "128"; exit 0 ;;
                *"{{.HostConfig.Init}}"*) echo "true"; exit 0 ;;
                *"HostConfig.GroupAdd"*) echo "1000"; exit 0 ;;
                *"HostConfig.SecurityOpt"*) printf 'label=disable\nno-new-privileges:true\n'; exit 0 ;;
                *"HostConfig.CapDrop"*) echo "ALL"; exit 0 ;;
                *"{{.Config.Domainname}}"*) echo "example.com"; exit 0 ;;
                *"{{.Config.StopSignal}}"*) echo "SIGQUIT"; exit 0 ;;
                *"{{.Config.StopTimeout}}"*) echo "30"; exit 0 ;;
                *"{{.HostConfig.OomKillDisable}}"*)
                    case "${STUB_SCENARIO:-ok}" in
                        boundary_nil_false)
                            case "$4" in *-old) echo "<nil>" ;; *) echo "false" ;; esac
                            ;;
                        boundary_true_false)
                            case "$4" in *-old) echo "true" ;; *) echo "false" ;; esac
                            ;;
                        *) echo "true" ;;
                    esac
                    exit 0
                    ;;
                *"{{.HostConfig.CgroupnsMode}}"*) echo "host"; exit 0 ;;
            esac
        elif [ "${STUB_SCENARIO:-ok}" = "unsupported_sysctl" ]; then
            case "$fmt" in
                *"{{range .HostConfig.Sysctls}}"*) echo "net.ipv4.ip_forward=1"; exit 0 ;;
            esac
        elif [ "${STUB_SCENARIO:-ok}" = "unsupported_mac" ]; then
            case "$fmt" in
                *"{{.Config.MacAddress}}"*) echo "02:42:ac:11:00:02"; exit 0 ;;
            esac
        elif [ "${STUB_SCENARIO:-ok}" = "unsupported_hostname" ]; then
            case "$fmt" in
                *"{{.Config.Hostname}}"*) echo "pinned-host"; exit 0 ;;
            esac
        fi
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
            *HostConfig.ShmSize*) shm_value "$4" ;;
            *Config.Image*) echo "example/controller:old" ;;
            *"{{.Image}}"*)
                [ "${STUB_SCENARIO:-ok}" = "image_retag" ] && echo "sha256:stableimageid" || echo "sha256:imageid"
                ;;
            *State.Running*) running_state ;;
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
            *healthz*)
                case "${STUB_SCENARIO:-ok}" in
                    health_fail|image_inspect_fail_health) exit 1 ;;
                    *) exit 0 ;;
                esac
                ;;
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
    stop)
        case "${STUB_SCENARIO:-ok}" in
            stop_fail) exit 1 ;;
            stop_noop) exit 0 ;;
            *) echo false > "${RUNNING_FILE}"; exit 0 ;;
        esac
        ;;
    start)
        case "${STUB_SCENARIO:-ok}" in
            start_fail) exit 1 ;;
            *) echo true > "${RUNNING_FILE}"; exit 0 ;;
        esac
        ;;
    cp)
        [ "${STUB_SCENARIO:-ok}" = "cp_fail" ] && exit 1
        exit 0
        ;;
    create)
        printf '%s\n' "$*" >> "${REC}.create"
        exit 0
        ;;
    rename|run|rm) exit 0 ;;
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
    rm -f "${RUNNING_FILE}" "${REC}.create"
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

assert_contains_fixed() {
    # fixed-string contains (BRE metacharacters like [ ] in the needle)
    if ! grep -qF -- "$1" "$2"; then
        echo "FAIL $3: [$1] not found in $2"
        echo "--- output ---"
        cat "${OUT}"
        exit 1
    fi
}

assert_not_contains_fixed() {
    if grep -qF -- "$1" "$2"; then
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
# 5a. the image inventory records the image reference, the image ID and the
# repository digests (never a container ID / never an empty reference)
assert_contains 'agentteams-controller: image=example/controller:old id=sha256:imageid digests=example/controller@sha256:aaa,example/controller@sha256:bbb' "${TMP}/b5/images.txt" "backup inventory line"

# 5b. a custom --controller-name is inventoried under that name
run_case ok backup --yes --controller-name owned-ctrl --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5c" --state-file "${TMP}/b5c/state.json"
assert_eq "${RC}" "0" "backup custom-name rc"
assert_contains 'owned-ctrl: image=example/controller:old id=sha256:imageid' "${TMP}/b5c/images.txt" "custom-name inventory line"

# 5c. a local-only image (no repo digest) records digests=none
run_case image_no_digest backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5n" --state-file "${TMP}/b5n/state.json"
assert_eq "${RC}" "0" "backup no-digest rc"
assert_contains 'digests=none' "${TMP}/b5n/images.txt" "no-digest inventory"

# 5d. an image-inspect failure fails the backup instead of writing an empty line,
# and a controller that was stopped for the copy is started again (recovery
# gate) even though the backup status stays failed.
run_case image_inspect_fail backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5f" --state-file "${TMP}/b5f/state.json"
assert_eq "${RC}" "3" "backup image-fail rc"
assert_contains 'image inventory failed' "${OUT}" "image-fail message"
assert_contains '"status":"failed"' "${TMP}/b5f/state.json" "image-fail state"
assert_contains '^stop ' "${REC}" "image-fail stop before the copy"
assert_contains '^cp ' "${REC}" "image-fail copy ran"
assert_contains '^start ' "${REC}" "image-fail recovery start"
assert_contains 'exec agentteams-controller curl -fsS' "${REC}" "image-fail recovery health gate"
assert_contains 'recovery ok' "${TMP}/b5f/state.json" "image-fail recovery detail"
assert_not_contains '"status":"done"' "${TMP}/b5f/state.json" "image-fail stays failed"

# 5d2. the inventory records the container's immutable image ID, not the
# (mutable) tag it was created with: moving the tag to another cached image
# must not change the recorded ID/digests.
run_case image_retag backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5r" --state-file "${TMP}/b5r/state.json"
assert_eq "${RC}" "0" "backup retagged-tag rc"
assert_contains 'agentteams-controller: image=example/controller:old id=sha256:stableimageid digests=example/controller@sha256:stable'     "${TMP}/b5r/images.txt" "inventory follows the container image ID"
assert_not_contains 'retagged' "${TMP}/b5r/images.txt" "inventory ignores the moved tag"
assert_contains_fixed 'sha256:stableimageid' "${REC}" "image inspect used the immutable ID"
assert_not_contains_fixed 'sha256:retagged' "${REC}" "image inspect never followed the moved tag"

# 5d3. inventory failure with a health gate that also fails: the controller is
# started, the failed gate is recorded, and the backup stays failed.
run_case image_inspect_fail_health backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b5fh" --state-file "${TMP}/b5fh/state.json" --timeout 2
assert_eq "${RC}" "3" "inventory+health rc"
assert_contains '^start ' "${REC}" "inventory+health recovery start"
assert_contains 'recovery FAILED' "${TMP}/b5fh/state.json" "inventory+health recovery detail"
assert_contains '"status":"failed"' "${TMP}/b5fh/state.json" "inventory+health stays failed"
assert_not_contains '"status":"done"' "${TMP}/b5fh/state.json" "inventory+health not done"

# 5.1 docker stop fails: no copy, nonzero, failed state, controller left as-is
run_case stop_fail backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b51" --state-file "${TMP}/b51/state.json"
assert_eq "${RC}" "3" "backup stop-fail rc"
assert_contains 'refusing to copy' "${OUT}" "backup stop-fail message"
assert_contains '^stop ' "${REC}" "backup stop-fail stop attempted"
assert_not_contains '^cp ' "${REC}" "backup stop-fail no copy"
assert_contains '"status":"failed"' "${TMP}/b51/state.json" "backup stop-fail state"
assert_contains 'stop failed' "${TMP}/b51/state.json" "backup stop-fail detail"
assert_not_contains '^start ' "${REC}" "backup stop-fail no restart needed"

# 5.2 stop returns 0 but the container is still running: verified-stop check aborts the copy
run_case stop_noop backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b52" --state-file "${TMP}/b52/state.json"
assert_eq "${RC}" "3" "backup stop-noop rc"
assert_contains 'still running after stop' "${OUT}" "backup stop-noop message"
assert_not_contains '^cp ' "${REC}" "backup stop-noop no copy"
assert_contains '"status":"failed"' "${TMP}/b52/state.json" "backup stop-noop state"

# 5.3 a later step fails (docker cp): the controller is still restarted, exit nonzero
run_case cp_fail backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b53" --state-file "${TMP}/b53/state.json"
assert_eq "${RC}" "3" "backup cp-fail rc"
assert_contains '^cp ' "${REC}" "backup cp-fail cp attempted"
assert_contains '^start ' "${REC}" "backup cp-fail recovery start"
assert_contains '"status":"failed"' "${TMP}/b53/state.json" "backup cp-fail state"
assert_contains 'recovery ok' "${TMP}/b53/state.json" "backup cp-fail recovery detail"

# 5.4 restart fails after a successful copy: nonzero, DOWN recorded
run_case start_fail backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b54" --state-file "${TMP}/b54/state.json"
assert_eq "${RC}" "3" "backup start-fail rc"
assert_contains 'is DOWN' "${OUT}" "backup start-fail message"
assert_contains '"status":"failed"' "${TMP}/b54/state.json" "backup start-fail state"

# 5.5 post-backup health gate fails: nonzero, status failed (NOT done), restart attempted
run_case health_fail backup --yes --env-file "${ENV_FILE}" --backup-dir "${TMP}/b55" --state-file "${TMP}/b55/state.json" --timeout 2
assert_eq "${RC}" "3" "backup health-fail rc"
assert_contains '^start ' "${REC}" "backup health-fail restart"
assert_contains '"status":"failed"' "${TMP}/b55/state.json" "backup health-fail state"
assert_not_contains '"status":"done"' "${TMP}/b55/state.json" "backup health-fail not done"
assert_contains 'health gate' "${TMP}/b55/state.json" "backup health-fail detail"

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

# 7.11 boundary: the dry-run create command carries the preserved boundary
run_case boundary controller --dry-run --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b711"
assert_eq "${RC}" "0" "boundary dry rc"
assert_contains_fixed '--user 1000:1000' "${OUT}" "boundary dry user"
assert_contains_fixed '--workdir /work' "${OUT}" "boundary dry workdir"
assert_contains_fixed '--read-only' "${OUT}" "boundary dry readonly"
assert_contains_fixed '--memory 268435456' "${OUT}" "boundary dry memory"
assert_contains_fixed '--cpus 0.5' "${OUT}" "boundary dry cpus"
assert_contains_fixed '--pids-limit 128' "${OUT}" "boundary dry pids"
assert_contains_fixed '--init' "${OUT}" "boundary dry init"
assert_contains_fixed '--group-add 1000' "${OUT}" "boundary dry group-add"
assert_contains_fixed '--security-opt label=disable' "${OUT}" "boundary dry seopt label"
assert_contains_fixed '--security-opt no-new-privileges' "${OUT}" "boundary dry seopt nnp"
assert_contains_fixed '--cap-drop ALL' "${OUT}" "boundary dry cap-drop"
assert_contains_fixed '--shm-size 134217728' "${OUT}" "boundary dry shm-size"
assert_contains_fixed '--domainname example.com' "${OUT}" "boundary dry domainname"
assert_contains_fixed '--stop-signal SIGQUIT' "${OUT}" "boundary dry stop-signal"
assert_contains_fixed '--stop-timeout 30' "${OUT}" "boundary dry stop-timeout"
assert_contains_fixed '--oom-kill-disable' "${OUT}" "boundary dry oom-kill-disable"
assert_contains_fixed '--cgroupns host' "${OUT}" "boundary dry cgroupns"

# 7.12 default container: no boundary flags are invented
run_case ok controller --dry-run --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b712"
assert_eq "${RC}" "0" "no-boundary dry rc"
assert_not_contains_fixed '--user' "${OUT}" "no-boundary no user"
assert_not_contains_fixed '--read-only' "${OUT}" "no-boundary no readonly"
assert_not_contains_fixed '--memory' "${OUT}" "no-boundary no memory"
assert_not_contains_fixed '--cpus' "${OUT}" "no-boundary no cpus"
assert_not_contains_fixed '--pids-limit' "${OUT}" "no-boundary no pids"

# 7.13 unsupported sysctls: preflight fails closed; the controller refuses before rename
run_case unsupported_sysctl preflight --env-file "${ENV_FILE}" --backup-dir "${TMP}/b713" --json
assert_eq "${RC}" "3" "sysctl preflight rc"
assert_contains 'unsupported non-default runtime settings' "${OUT}" "sysctl preflight message"
assert_contains_fixed '"unsupported_runtime":["sysctls"]' "${OUT}" "sysctl preflight json"
run_case unsupported_sysctl controller --dry-run --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b713c"
assert_eq "${RC}" "3" "sysctl controller rc"
assert_contains 'unsupported non-default runtime settings' "${OUT}" "sysctl controller message"
assert_not_contains '^rename ' "${OUT}" "sysctl controller no rename"
assert_not_contains '^stop ' "${OUT}" "sysctl controller no stop"

# 7.14 shared-memory size is carried explicitly (never inherited from the daemon default)
run_case boundary controller --dry-run --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b714"
assert_eq "${RC}" "0" "shm dry rc"
assert_contains_fixed '--shm-size 134217728' "${OUT}" "shm dry flag"

# 7.15 daemon default differs: without the explicit flag the new container would
# get 8403288064; with it the boundary verify passes
run_case shm_drift controller --yes --prepare-only --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b715" --state-file "${TMP}/b715/state.json" --timeout 4
assert_eq "${RC}" "0" "shm drift prepare rc"
assert_contains 'boundary preserved' "${OUT}" "shm drift preserved"
assert_contains_fixed 'shm_size' "${OUT}" "shm drift summary key"

# 7.16 a replacement that still drifts is rejected before the old container is started
run_case shm_verify_mismatch controller --yes --prepare-only --controller-image example/controller:new --env-file "${ENV_FILE}" --backup-dir "${TMP}/b716" --state-file "${TMP}/b716/state.json" --timeout 4
[ "${RC}" = "3" ] || { echo "FAIL shm mismatch rc: ${RC}"; cat "${OUT}"; exit 1; }
assert_contains 'boundary mismatch: shm_size' "${OUT}" "shm mismatch message"
assert_not_contains '^start ' "${REC}" "shm mismatch never starts the replacement"

# 7.17 explicit MAC and custom hostname cannot be reproduced safely: refuse before rename
run_case unsupported_mac preflight --env-file "${ENV_FILE}" --backup-dir "${TMP}/b717" --json
assert_eq "${RC}" "3" "mac preflight rc"
assert_contains 'unsupported non-default runtime settings' "${OUT}" "mac preflight message"
assert_contains_fixed '"unsupported_runtime":["mac_address"]' "${OUT}" "mac preflight json"
run_case unsupported_hostname preflight --env-file "${ENV_FILE}" --backup-dir "${TMP}/b718" --json
assert_eq "${RC}" "3" "hostname preflight rc"
assert_contains_fixed '"unsupported_runtime":["hostname"]' "${OUT}" "hostname preflight json"

# 7.18 old docker-run default renders <nil>, docker create stores false: same boundary
run_case boundary_nil_false controller --yes --prepare-only --controller-image example/controller:new \
    --env-file "${ENV_FILE}" --backup-dir "${TMP}/b718" --state-file "${TMP}/b718/state.json" --timeout 4
assert_eq "${RC}" "0" "oom nil/false prepare rc"
assert_contains 'boundary preserved' "${OUT}" "oom nil/false preserved"
assert_not_contains 'boundary mismatch: oom_kill_disable' "${OUT}" "oom nil/false no mismatch"

# 7.19 explicit true vs false is still a real mismatch
run_case boundary_true_false controller --yes --prepare-only --controller-image example/controller:new \
    --env-file "${ENV_FILE}" --backup-dir "${TMP}/b719" --state-file "${TMP}/b719/state.json" --timeout 4
assert_eq "${RC}" "3" "oom true/false rc"
assert_contains 'boundary mismatch: oom_kill_disable' "${OUT}" "oom true/false mismatch"
assert_not_contains '^start ' "${REC}" "oom true/false never starts replacement"

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

echo "PASS test-upgrade-cli ($(grep -c '^run_case ' "${BASH_SOURCE[0]}") scenarios)"
