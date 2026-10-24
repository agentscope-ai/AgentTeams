#!/usr/bin/env bash
# ============================================================================
# agentteams-upgrade.sh — safe, layered AgentTeams upgrade (P1)
#
# P1 scope: preflight / plan / backup / controller / status / --dry-run.
# Workers, manager, dashboard and memory are P2/P3.
#
# Safety:
#   - preflight is read-only and emits JSON;
#   - backup stops the controller for a cold kine copy, then restarts it;
#   - controller renames the old container, runs the new image with the old
#     ports/mounts/network/restart policy, health-gates it, and keeps the old
#     container for rollback;
#   - --dry-run prints every command without mutating;
#   - --yes is required for backup/controller; without it only the plan prints.
#
# Exit codes: 0 ok / 2 usage / 3 gate fail / 4 rollback failed.
# ============================================================================
set -u

EXIT_OK=0
EXIT_USAGE=2
EXIT_GATE=3
EXIT_ROLLBACK=4

PROG="$(basename "$0")"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

ENV_FILE="${AGENTTEAMS_ENV_FILE:-$HOME/agentteams-manager.env}"
BACKUP_DIR=""
STATE_FILE=""
TARGET=""
CONTROLLER_IMAGE=""
DRY_RUN=0
YES=0
JSON=0
TIMEOUT=300
LOG_FILE=""
DOCKER_CMD=""
CONTROLLER_NAME="agentteams-controller"
OLD_NAME=""
ENV_FROM_CONTAINER=0
ALLOW_ENV_DRIFT=0
KEEP_FAILED=0
ALLOW_GATEWAY_DOWN=0
ALLOW_STORAGE_SPLIT=0
HIGRESS_BACKFILL_DIR=""
EFFECTIVE_ENV_FILE=""
ENV_SOURCE="file"
OLD_IMAGE=""

WARN_LIST=()
ERR_LIST=()

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
log() {
    local msg="[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*"
    if [ "${JSON}" = "1" ]; then
        printf '%s\n' "$msg" >&2
    else
        printf '%s\n' "$msg"
    fi
    if [ -n "${LOG_FILE}" ] && [ -d "$(dirname "${LOG_FILE}")" ]; then
        printf '%s\n' "$msg" >> "${LOG_FILE}" 2>/dev/null || true
    fi
}

warn() {
    WARN_LIST+=("$*")
    log "WARN: $*"
}

err() {
    ERR_LIST+=("$*")
    log "ERROR: $*"
}

die() {
    log "ERROR: $*"
    exit "${2:-$EXIT_GATE}"
}

json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n\r' '  '
}

json_list() {
    local first=1 item
    printf '['
    for item in "$@"; do
        [ "$first" = "1" ] || printf ','
        first=0
        printf '"%s"' "$(json_escape "$item")"
    done
    printf ']'
}

# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: ${PROG} <command> [options]

Commands:
  preflight     read-only checks; JSON with --json
  plan          print the upgrade plan
  backup        cold kine backup + env + image record + state
  controller    upgrade the controller only (rename/run + health gate + rollback)
  status        show the last state (JSON with --json)
  help          show this help

Options:
  --env-file PATH          default: \${AGENTTEAMS_ENV_FILE:-\$HOME/agentteams-manager.env}
  --env-from-container     use the running controller's env for the new container (recommended)
  --allow-env-drift        proceed even if the env-file key set differs from the container
  --keep-failed            keep the failed new controller (stopped) for log inspection
  --allow-gateway-down     continue even if 8080/v1/models is 000 (default: fail/rollback)
  --allow-storage-split    continue even if live components use different storage endpoints/providers
                           (migration-window override; default: fail closed)
  --higress-backfill-dir DIR  copy plugin files into the new controller and restart it (Wasm version backfill)
  --backup-dir DIR         default: \$HOME/agentteams-upgrade-backups/<UTC>
  --state-file PATH        default: <backup-dir>/state.json
  --target VERSION         target version (recorded in the plan/state)
  --controller-image IMG   target controller image (required for controller)
  --dry-run                print commands; do not mutate
  --yes                    allow mutations (required for backup/controller)
  --json                   machine-readable output (preflight/status)
  --timeout SEC            health-gate timeout (default 300)
  --log-file PATH          default: <backup-dir>/upgrade.log
  -h, --help               show this help

Exit codes: 0 ok / 2 usage / 3 gate fail / 4 rollback failed.
EOF
}

resolve_paths() {
    if [ -z "${BACKUP_DIR}" ]; then
        if [ -n "${STATE_FILE}" ]; then
            BACKUP_DIR="$(dirname "${STATE_FILE}")"
        else
            BACKUP_DIR="${HOME}/agentteams-upgrade-backups/$(date -u +%Y%m%dT%H%M%SZ)"
        fi
    fi
    if [ -z "${STATE_FILE}" ]; then
        STATE_FILE="${BACKUP_DIR}/state.json"
    fi
    if [ -z "${LOG_FILE}" ]; then
        LOG_FILE="${BACKUP_DIR}/upgrade.log"
    fi
}

# ---------------------------------------------------------------------------
# Docker / inspect helpers
# ---------------------------------------------------------------------------
detect_docker() {
    if command -v docker >/dev/null 2>&1; then
        DOCKER_CMD="docker"
    elif command -v podman >/dev/null 2>&1; then
        DOCKER_CMD="podman"
    else
        die "neither docker nor podman found" "${EXIT_GATE}"
    fi
    if ! "${DOCKER_CMD}" info >/dev/null 2>&1; then
        die "container runtime is not running: ${DOCKER_CMD}" "${EXIT_GATE}"
    fi
}

container_exists() {
    "${DOCKER_CMD}" inspect "$1" >/dev/null 2>&1
}

inspect_fmt() {
    "${DOCKER_CMD}" inspect --format "$1" "$2" 2>/dev/null || true
}

read_env() {
    [ -f "${ENV_FILE}" ] || return 0
    grep "^$1=" "${ENV_FILE}" 2>/dev/null | head -n 1 | cut -d= -f2- | tr -d '\r'
}

data_mount() {
    inspect_fmt '{{range .Mounts}}{{if eq .Destination "/data"}}{{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}{{end}}{{end}}' "${CONTROLLER_NAME}"
}

ports_count() {
    inspect_fmt '{{len .HostConfig.PortBindings}}' "${CONTROLLER_NAME}"
}

mounts_count() {
    inspect_fmt '{{len .Mounts}}' "${CONTROLLER_NAME}"
}

mount_lines() {
    inspect_fmt '{{range .Mounts}}{{.Type}}|{{.Name}}|{{.Source}}|{{.Destination}}|{{.RW}}|{{.Propagation}}|{{.Driver}}{{println}}{{end}}' "${CONTROLLER_NAME}"
}

fs_type_of() {
    local path="$1" fs=""
    if command -v findmnt >/dev/null 2>&1; then
        fs="$(findmnt -T "${path}" -o FSTYPE -n 2>/dev/null | head -n 1 || true)"
    fi
    if [ -z "${fs}" ] && command -v stat >/dev/null 2>&1; then
        fs="$(stat -f -c %T "${path}" 2>/dev/null || true)"
    fi
    printf '%s' "${fs}"
}

is_remote_fs() {
    case "$1" in
        nfs|nfs4|cifs|smb|smb3|sshfs|fuse.sshfs|glusterfs|ceph|9p|virtiofs|afs|lustre|gpfs) return 0 ;;
        fuse.*) return 0 ;;
        *) return 1 ;;
    esac
}

mount_json() {
    local mtype="$1" mname="$2" msrc="$3" mdst="$4" mrw="$5" mprop="$6" mdriver="$7" mfs="$8" mremote="$9"
    local rw="false"
    [ "${mrw}" = "true" ] && rw="true"
    printf '{"type":"%s","name":"%s","source":"%s","destination":"%s","rw":%s,"propagation":"%s","driver":"%s","fs_type":"%s","remote":%s}' \
        "$(json_escape "${mtype}")" "$(json_escape "${mname}")" "$(json_escape "${msrc}")" "$(json_escape "${mdst}")" \
        "${rw}" "$(json_escape "${mprop}")" "$(json_escape "${mdriver}")" "$(json_escape "${mfs}")" "${mremote}"
}

container_running() {
    local v
    v="$(inspect_fmt '{{.State.Running}}' "${CONTROLLER_NAME}")"
    [ "${v}" = "true" ]
}

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------
write_state() {
    local step="$1" status="$2" now
    now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    mkdir -p "$(dirname "${STATE_FILE}")" 2>/dev/null || true
    {
        echo "STEP=${step}"
        echo "STATUS=${status}"
        echo "UPDATED_AT=${now}"
        echo "BACKUP_DIR=${BACKUP_DIR}"
        echo "CONTROLLER_IMAGE=${CONTROLLER_IMAGE}"
        echo "ENV_SOURCE=${ENV_SOURCE}"
    } > "${STATE_FILE%.json}.env"
    printf '{"step":"%s","status":"%s","updated_at":"%s","backup_dir":"%s","controller_image":"%s","env_source":"%s"}\n' \
        "${step}" "${status}" "${now}" \
        "$(json_escape "${BACKUP_DIR}")" "$(json_escape "${CONTROLLER_IMAGE}")" "$(json_escape "${ENV_SOURCE}")" \
        > "${STATE_FILE}"
}

# ---------------------------------------------------------------------------
# Health gate
# ---------------------------------------------------------------------------
healthz_ok() {
    local url
    if "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" sh -c 'command -v curl >/dev/null 2>&1'; then
        for url in \
            http://127.0.0.1:8090/healthz \
            http://127.0.0.1:8001/healthz \
            http://127.0.0.1:8080/healthz; do
            if "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" curl -fsS "${url}" >/dev/null 2>&1; then
                return 0
            fi
        done
        return 1
    fi
    "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get teams >/dev/null 2>&1
}

agt_ok() {
    "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get teams >/dev/null 2>&1 \
        && "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get workers >/dev/null 2>&1
}

logs_ok() {
    local n since
    since="$(inspect_fmt '{{.State.StartedAt}}' "${CONTROLLER_NAME}")"
    if [ -n "${since}" ]; then
        n="$("${DOCKER_CMD}" logs --since "${since}" "${CONTROLLER_NAME}" 2>&1 | grep -cE 'panic|fatal' || true)"
    else
        n="$("${DOCKER_CMD}" logs --tail 200 "${CONTROLLER_NAME}" 2>&1 | grep -cE 'panic|fatal' || true)"
    fi
    [ "${n}" = "0" ]
}

gateway_http() {
    local code
    code="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/models 2>/dev/null || true)"
    [ -n "${code}" ] || code="000"
    printf '%s' "${code}"
}

wait_gateway() {
    local waited=0 code
    while [ "${waited}" -lt "${TIMEOUT}" ]; do
        code="$(gateway_http)"
        [ "${code}" != "000" ] && return 0
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

higress_plugin_check() {
    local refs ref path missing=0
    refs="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" sh -c 'grep -Rho "plugins/[a-z-]*/[0-9.]*/plugin.wasm" /data 2>/dev/null | sort -u' 2>/dev/null || true)"
    for ref in ${refs}; do
        path="/usr/share/nginx/html/${ref}"
        if ! "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" test -f "${path}" >/dev/null 2>&1; then
            missing=$((missing + 1))
            warn "Higress plugin missing in image: ${ref}"
        fi
    done
    printf '%s' "${missing}"
}

wait_health() {
    local waited=0
    while [ "${waited}" -lt "${TIMEOUT}" ]; do
        if healthz_ok && agt_ok; then
            if ! logs_ok; then
                warn "controller logs contain panic|fatal; healthz and agt are OK, continuing"
            fi
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
cmd_preflight() {
    detect_docker
    resolve_paths

    local data_env data_mount_val data_dir data_source
    data_env="$(read_env AGENTTEAMS_DATA_DIR)"
    data_mount_val="$(data_mount)"

    if [ ! -f "${ENV_FILE}" ]; then
        err "env file not found: ${ENV_FILE}"
    fi

    if [ -n "${data_env}" ] && [ -n "${data_mount_val}" ] && [ "${data_env}" != "${data_mount_val}" ]; then
        err "data dir mismatch: env=${data_env} mounted=${data_mount_val}"
    fi
    if [ -n "${data_env}" ]; then
        data_dir="${data_env}"; data_source="env"
    elif [ -n "${data_mount_val}" ]; then
        data_dir="${data_mount_val}"; data_source="mounted"
    else
        data_dir="agentteams-data"; data_source="default"
        warn "cannot read AGENTTEAMS_DATA_DIR from env or container; defaulting to agentteams-data"
    fi

    local ctrl_exists="false" running="false" image="" ports=0 mounts=0 entry="" net="" restart=""
    if container_exists "${CONTROLLER_NAME}"; then
        ctrl_exists="true"
        image="$(inspect_fmt '{{.Config.Image}}' "${CONTROLLER_NAME}")"
        ports="$(ports_count)"
        mounts="$(mounts_count)"
        entry="$(inspect_fmt '{{json .Config.Entrypoint}}' "${CONTROLLER_NAME}")"
        net="$(inspect_fmt '{{.HostConfig.NetworkMode}}' "${CONTROLLER_NAME}")"
        restart="$(inspect_fmt '{{.HostConfig.RestartPolicy.Name}}' "${CONTROLLER_NAME}")"
        container_running && running="true"
    else
        err "controller container not found: ${CONTROLLER_NAME}"
    fi

    local old_env_count=0 file_env_count=0 env_drift="false"
    if [ "${ctrl_exists}" = "true" ]; then
        old_env_count="$(inspect_fmt '{{len .Config.Env}}' "${CONTROLLER_NAME}")"
        case "${old_env_count}" in ''|*[!0-9]*) old_env_count=0 ;; esac
        file_env_count="$(grep -c '^[A-Za-z_][A-Za-z0-9_]*=' "${ENV_FILE}" 2>/dev/null || true)"
        case "${file_env_count}" in ''|*[!0-9]*) file_env_count=0 ;; esac
        if [ "${old_env_count}" -gt 0 ] && [ "${file_env_count}" -gt 0 ] && [ "${old_env_count}" -ne "${file_env_count}" ]; then
            env_drift="true"
            warn "env drift: container=${old_env_count} env-file=${file_env_count}; use --env-from-container (recommended) or --allow-env-drift"
        fi
    fi

    local mount_items="" mounts_json="[]" data_model="volume"
    local has_volume=0 has_bind=0 has_remote=0
    local mtype mname msrc mdst mrw mprop mdriver mfs mremote
    while IFS='|' read -r mtype mname msrc mdst mrw mprop mdriver; do
        [ -n "${mdst}" ] || continue
        mfs=""
        mremote="false"
        if [ "${mtype}" = "bind" ]; then
            mfs="$(fs_type_of "${msrc}")"
        elif [ "${mtype}" = "volume" ] && [ -d "${msrc}" ]; then
            mfs="$(fs_type_of "${msrc}")"
        fi
        if [ -n "${mfs}" ] && is_remote_fs "${mfs}"; then
            mremote="true"
            warn "mount ${mdst} uses remote fs ${mfs}; preserved as-is (latency/locking may differ)"
        fi
        case "${mtype}" in
            volume) has_volume=1 ;;
            bind) has_bind=1 ;;
            tmpfs) ;;
            *) warn "unknown mount type ${mtype} at ${mdst}" ;;
        esac
        [ "${mremote}" = "true" ] && has_remote=1
        mount_items="${mount_items}${mount_items:+,}$(mount_json "${mtype}" "${mname}" "${msrc}" "${mdst}" "${mrw}" "${mprop}" "${mdriver}" "${mfs}" "${mremote}")"
    done < <(mount_lines)
    mounts_json="[${mount_items}]"
    if [ "${has_remote}" = "1" ]; then
        data_model="remote"
    elif [ "${has_volume}" = "1" ] && [ "${has_bind}" = "1" ]; then
        data_model="mixed"
    elif [ "${has_bind}" = "1" ]; then
        data_model="bind"
    else
        data_model="volume"
    fi

    local s3_configured="false" s3_bucket="" s3_endpoint="" s3_backend="unknown" has_data_minio=0
    s3_bucket="$(read_env AGENTTEAMS_FS_BUCKET)"
    [ -n "${s3_bucket}" ] || s3_bucket="$(read_env AGENTTEAMS_MINIO_BUCKET)"
    [ -n "${s3_bucket}" ] && s3_configured="true"
    s3_endpoint="$(read_env AGENTTEAMS_FS_ENDPOINT)"
    [ -n "${s3_endpoint}" ] || s3_endpoint="$(read_env AGENTTEAMS_MINIO_ENDPOINT)"
    [ -n "${s3_endpoint}" ] || s3_endpoint="$(read_env AGENTTEAMS_FS_DOMAIN)"
    if mount_lines | cut -d'|' -f4 | grep -qx "/data/minio"; then
        has_data_minio=1
    fi
    case "${s3_endpoint}" in
        *"127.0.0.1:9000"*|*"localhost:9000"*|*"agentteams-controller:9000"*) s3_backend="embedded-minio" ;;
        "")
            if [ "${has_data_minio}" = "1" ]; then s3_backend="embedded-minio"; else s3_backend="unknown"; fi
            ;;
        *) s3_backend="external-s3" ;;
    esac
    if [ "${s3_backend}" = "external-s3" ] && [ "${has_data_minio}" = "1" ]; then
        warn "external S3 endpoint but /data/minio mount is present; verify the storage migration state"
    fi
    if [ "${s3_backend}" = "embedded-minio" ] && [ "${has_data_minio}" = "0" ]; then
        warn "embedded MinIO endpoint but no /data/minio mount; verify the data model"
    fi
    [ "${s3_backend}" = "unknown" ] && warn "storage backend unknown; set AGENTTEAMS_FS_ENDPOINT or check /data/minio"
    [ -n "${s3_endpoint}" ] && s3_configured="true"
    [ "${s3_configured}" = "true" ] || warn "S3 endpoint/bucket not found in env; worker state may rely on local mounts"
    [ -z "${s3_bucket}" ] && [ -n "${s3_endpoint}" ] && warn "S3 bucket not in env; verify AGENTTEAMS_FS_BUCKET (default may be agentteams-storage)"

    # The env file describes the intended backend; it cannot prove what the
    # running components actually use. Compare the live endpoint/provider/
    # bucket across the controller, Manager and Workers. A split here means
    # one component still writes to the old engine, which is exactly the
    # state an upgrade/migration must not silently accept.
    local storage_components="" storage_consistent="true" storage_ref="" storage_bucket_ref="" c ep provider bucket comp ckey
    while IFS= read -r c; do
        [ -n "${c}" ] || continue
        case "${c}" in rustfs*|minio*|*minio*|agentteams-dashboard|agentteams-docker-proxy|*dashboard*|*proxy*) continue ;; esac
        ep="$("${DOCKER_CMD}" exec "${c}" printenv AGENTTEAMS_FS_ENDPOINT 2>/dev/null || true)"
        [ -n "${ep}" ] || continue
        provider="$("${DOCKER_CMD}" exec "${c}" printenv AGENTTEAMS_STORAGE_PROVIDER 2>/dev/null || true)"
        bucket="$("${DOCKER_CMD}" exec "${c}" printenv AGENTTEAMS_FS_BUCKET 2>/dev/null || true)"
        ep="${ep%/}"
        comp="{\"name\":\"$(json_escape "${c}")\",\"endpoint\":\"$(json_escape "${ep}")\",\"provider\":\"$(json_escape "${provider}")\",\"bucket\":\"$(json_escape "${bucket}")\"}"
        storage_components="${storage_components}${storage_components:+,}${comp}"
        # The controller may expose the embedded MinIO as 127.0.0.1:9000
        # while the Manager reaches the same engine as agentteams-controller:9000.
        # Normalize only the embedded aliases; an explicit external provider
        # (s3/external-s3) is never folded into embedded-minio.
        ckey="${ep}|${provider}"
        case "${provider}" in
            s3|external-s3) ;;
            *)
                case "${ep}" in
                    *"127.0.0.1:9000"*|*"localhost:9000"*|*"agentteams-controller:9000"*)
                        ckey="embedded-minio"
                        ;;
                esac
                ;;
        esac
        if [ -z "${storage_ref}" ]; then
            storage_ref="${ckey}"
        elif [ "${ckey}" != "${storage_ref}" ]; then
            storage_consistent="false"
        fi
        # An empty bucket means the component did not expose the env name;
        # compare only when both sides actually name a bucket.
        if [ -n "${bucket}" ]; then
            if [ -z "${storage_bucket_ref}" ]; then
                storage_bucket_ref="${bucket}"
            elif [ "${bucket}" != "${storage_bucket_ref}" ]; then
                storage_consistent="false"
            fi
        fi
    done < <("${DOCKER_CMD}" ps --format '{{.Names}}' 2>/dev/null || true)
    storage_components="[${storage_components}]"
    if [ "${storage_components}" = "[]" ]; then
        warn "no live component storage env found; storage consistency not verified"
    fi
    if [ "${storage_consistent}" = "false" ]; then
        if [ "${ALLOW_STORAGE_SPLIT}" = "1" ]; then
            warn "storage split-brain: live components use different endpoints/providers; continuing because --allow-storage-split"
        else
            err "storage split-brain: live components use different storage endpoints/providers; complete the migration or pass --allow-storage-split"
        fi
    fi

    local gw_code gw_ok="false"
    gw_code="$(gateway_http)"
    if [ "${gw_code}" != "000" ]; then
        gw_ok="true"
    else
        warn "gateway data plane 8080/v1/models=000; Higress plugin/data-plane may be down"
    fi

    local plugin_missing
    plugin_missing="$(higress_plugin_check)"
    [ "${plugin_missing}" -gt 0 ] && warn "Higress plugin refs missing from image: ${plugin_missing} (backfill may be needed)"

    local free_kb=0 kine_kb=0 free_bytes=0 kine_bytes=0
    local check_dir="${BACKUP_DIR}"
    while [ ! -d "${check_dir}" ] && [ "${check_dir}" != "/" ]; do
        check_dir="$(dirname "${check_dir}")"
    done
    free_kb="$(df -Pk "${check_dir}" 2>/dev/null | awk 'NR==2 {print $4}' || true)"
    case "${free_kb}" in ''|*[!0-9]*) free_kb=0 ;; esac
    free_bytes=$((free_kb * 1024))
    local backup_fs
    backup_fs="$(fs_type_of "${check_dir}")"
    if [ "${backup_fs}" = "tmpfs" ]; then
        warn "backup dir is on tmpfs (${check_dir}); choose a persistent filesystem for kine backups"
    fi
    if [ "${running}" = "true" ]; then
        kine_kb="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" du -sk /data/agentteams-controller 2>/dev/null | awk '{print $1}' || true)"
        case "${kine_kb}" in ''|*[!0-9]*) kine_kb=0 ;; esac
        kine_bytes=$((kine_kb * 1024))
        if [ "${kine_kb}" -gt 0 ] && [ "${free_kb}" -lt $((kine_kb * 2)) ]; then
            err "insufficient disk for kine backup: free=${free_kb}KB need>=${kine_kb}KB*2"
        fi
    else
        warn "controller is not running; kine size and agt checks are skipped"
    fi

    local teams=0 workers=0 teams_out="" workers_out=""
    if [ "${running}" = "true" ]; then
        if ! teams_out="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get teams 2>/dev/null)"; then
            err "agt get teams failed"
            teams_out=""
        fi
        if ! workers_out="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get workers 2>/dev/null)"; then
            err "agt get workers failed"
            workers_out=""
        fi
        # A fresh install has no teams/workers yet; the CLI prints a
        # human-readable sentinel instead of an empty list. Count real rows,
        # and treat zero as a valid fresh-install state (warn, not fail).
        if [ -z "${teams_out}" ] || printf '%s\n' "${teams_out}" | grep -qx 'No teams found\.'; then
            teams=0
        else
            teams="$(printf '%s\n' "${teams_out}" | wc -l | tr -d ' ')"
        fi
        if [ -z "${workers_out}" ] || printf '%s\n' "${workers_out}" | grep -qx 'No workers found\.'; then
            workers=0
        else
            workers="$(printf '%s\n' "${workers_out}" | wc -l | tr -d ' ')"
        fi
        case "${teams}" in ''|*[!0-9]*) teams=0 ;; esac
        case "${workers}" in ''|*[!0-9]*) workers=0 ;; esac
        [ "${teams}" -gt 0 ] || warn "agt get teams returned no rows (fresh install?)"
        [ "${workers}" -gt 0 ] || warn "agt get workers returned no rows (fresh install?)"
    fi

    local soul_set=-1 soul_missing=-1 workers_json=""
    workers_json="$("${DOCKER_CMD}" exec "${CONTROLLER_NAME}" agt get workers -o json 2>/dev/null || true)"
    if [ -n "${workers_json}" ] && printf '%s' "${workers_json}" | grep -q '"soul"'; then
        soul_set="$(printf '%s' "${workers_json}" | grep -o '"soul"' | wc -l | tr -d ' ')"
        soul_missing="$(printf '%s' "${workers_json}" | grep -o '"soul"[[:space:]]*:[[:space:]]*""' | wc -l | tr -d ' ')"
    else
        warn "spec.soul audit skipped: agt does not expose the soul field"
    fi

    local ok="true"
    [ "${#ERR_LIST[@]}" -gt 0 ] && ok="false"

    if [ "${JSON}" = "1" ]; then
        local warnings_json errors_json
        if [ "${#WARN_LIST[@]}" -gt 0 ]; then warnings_json="$(json_list "${WARN_LIST[@]}")"; else warnings_json="[]"; fi
        if [ "${#ERR_LIST[@]}" -gt 0 ]; then errors_json="$(json_list "${ERR_LIST[@]}")"; else errors_json="[]"; fi
        printf '{"ok":%s,"env_file":"%s","data_dir":"%s","data_dir_source":"%s","controller":{"name":"%s","exists":%s,"image":"%s","ports":%s,"mounts":%s,"entrypoint":"%s","network":"%s","restart":"%s","running":%s},"env_drift":%s,"data_model":"%s","mounts":%s,"s3":{"backend":"%s","configured":%s,"bucket":"%s","data_minio_mount":%s,"components":%s,"consistent":%s},"gateway":{"http_code":"%s","ok":%s,"plugin_missing":%s},"docker":{"cmd":"%s"},"disk_free_bytes":%s,"kine_bytes":%s,"workers":{"total":%s,"soul_set":%s,"soul_missing":%s},"teams":%s,"warnings":%s,"errors":%s}\n' \
            "${ok}" "$(json_escape "${ENV_FILE}")" "$(json_escape "${data_dir}")" "${data_source}" \
            "${CONTROLLER_NAME}" "${ctrl_exists}" "$(json_escape "${image}")" "${ports}" "${mounts}" \
            "$(json_escape "${entry}")" "$(json_escape "${net}")" "$(json_escape "${restart}")" "${running}" "${env_drift}" \
            "${data_model}" "${mounts_json}" "${s3_backend}" "${s3_configured}" "$(json_escape "${s3_bucket}")" "$([ "${has_data_minio}" = "1" ] && echo true || echo false)" \
            "${storage_components}" "${storage_consistent}" \
            "${gw_code}" "${gw_ok}" "${plugin_missing}" "${DOCKER_CMD}" "${free_bytes}" "${kine_bytes}" "${workers}" "${soul_set}" "${soul_missing}" "${teams}" \
            "${warnings_json}" "${errors_json}"
    else
        log "preflight: env=${ENV_FILE} data_dir=${data_dir} (${data_source})"
        log "controller: exists=${ctrl_exists} running=${running} image=${image} ports=${ports} mounts=${mounts}"
        log "storage: backend=${s3_backend} consistent=${storage_consistent} components=${storage_components}"
        log "docker: ${DOCKER_CMD}; teams=${teams} workers=${workers}"
    fi

    [ "${ok}" = "true" ] && return "${EXIT_OK}"
    return "${EXIT_GATE}"
}

# ---------------------------------------------------------------------------
# plan
# ---------------------------------------------------------------------------
cmd_plan() {
    resolve_paths
    log "AgentTeams safe upgrade plan (P1)"
    log "  env file       : ${ENV_FILE}"
    log "  backup dir     : ${BACKUP_DIR}"
    log "  target version : ${TARGET:-<current>}"
    log "  controller img : ${CONTROLLER_IMAGE:-<current>}"
    log "  1. preflight (read-only)"
    log "  2. backup (kine cold copy + env + image record)"
    log "  3. controller (rename/run + health gate; old container retained)"
    log "  P2: workers (canary) / P3: manager, dashboard, memory"
}

# ---------------------------------------------------------------------------
# backup
# ---------------------------------------------------------------------------
record_images() {
    local out="${BACKUP_DIR}/images.txt" name val
    : > "${out}"
    for name in agentteams-controller agentteams-manager agentteams-dashboard; do
        if container_exists "${name}"; then
            val="$(inspect_fmt '{{.Config.Image}} {{.Id}} {{index .RepoDigests 0}}' "${name}")"
            printf '%s: %s\n' "${name}" "${val}" >> "${out}"
        fi
    done
}

cmd_backup() {
    detect_docker
    resolve_paths

    if [ ! -f "${ENV_FILE}" ]; then
        die "env file not found: ${ENV_FILE}" "${EXIT_GATE}"
    fi
    if ! container_exists "${CONTROLLER_NAME}"; then
        die "controller container not found: ${CONTROLLER_NAME}" "${EXIT_GATE}"
    fi

    if [ "${DRY_RUN}" = "1" ]; then
        log "[dry-run] mkdir -p ${BACKUP_DIR} && chmod 700 ${BACKUP_DIR}"
        log "[dry-run] cp ${ENV_FILE} ${BACKUP_DIR}/agentteams-manager.env"
        log "[dry-run] export container env -> ${BACKUP_DIR}/controller.env (0600)"
        log "[dry-run] ${DOCKER_CMD} stop ${CONTROLLER_NAME}"
        log "[dry-run] ${DOCKER_CMD} cp ${CONTROLLER_NAME}:/data/agentteams-controller/. ${BACKUP_DIR}/kine/"
        log "[dry-run] record image IDs/digests to ${BACKUP_DIR}/images.txt"
        log "[dry-run] ${DOCKER_CMD} start ${CONTROLLER_NAME}"
        log "[dry-run] write state ${STATE_FILE}"
        return "${EXIT_OK}"
    fi
    [ "${YES}" = "1" ] || die "backup requires --yes (or use --dry-run)" "${EXIT_USAGE}"

    mkdir -p "${BACKUP_DIR}" && chmod 700 "${BACKUP_DIR}" || die "cannot create ${BACKUP_DIR}" "${EXIT_GATE}"
    cp "${ENV_FILE}" "${BACKUP_DIR}/agentteams-manager.env" 2>/dev/null || warn "could not copy env file"
    umask 077
    if "${DOCKER_CMD}" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "${CONTROLLER_NAME}" > "${BACKUP_DIR}/controller.env" 2>/dev/null; then
        chmod 600 "${BACKUP_DIR}/controller.env"
    else
        warn "could not export the controller env to ${BACKUP_DIR}/controller.env"
    fi

    local was_running=0
    if container_running; then
        was_running=1
        "${DOCKER_CMD}" stop "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
    fi

    mkdir -p "${BACKUP_DIR}/kine"
    if ! "${DOCKER_CMD}" cp "${CONTROLLER_NAME}:/data/agentteams-controller/." "${BACKUP_DIR}/kine/" >/dev/null 2>&1; then
        [ "${was_running}" = "1" ] && "${DOCKER_CMD}" start "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        write_state "backup" "failed"
        die "docker cp kine failed" "${EXIT_GATE}"
    fi
    record_images

    if [ "${was_running}" = "1" ]; then
        "${DOCKER_CMD}" start "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        if ! wait_health; then
            warn "controller did not pass the health gate after backup; backup is complete"
        fi
    fi

    write_state "backup" "done"
    log "backup done: ${BACKUP_DIR}"
    return "${EXIT_OK}"
}

# ---------------------------------------------------------------------------
# controller
# ---------------------------------------------------------------------------
build_run_args() {
    RUN_ARGS=(-d --name "${CONTROLLER_NAME}")
    local net restart
    net="$(inspect_fmt '{{.HostConfig.NetworkMode}}' "${CONTROLLER_NAME}")"
    restart="$(inspect_fmt '{{.HostConfig.RestartPolicy.Name}}' "${CONTROLLER_NAME}")"
    [ -n "${net}" ] && RUN_ARGS+=(--network "${net}")
    [ -n "${restart}" ] && RUN_ARGS+=(--restart "${restart}")
    if [ -n "${EFFECTIVE_ENV_FILE}" ] && { [ -f "${EFFECTIVE_ENV_FILE}" ] || [ "${ENV_FROM_CONTAINER}" = "1" ]; }; then
        RUN_ARGS+=(--env-file "${EFFECTIVE_ENV_FILE}")
    fi

    local p hostip hostport
    while read -r p hostip hostport; do
        [ -n "${p}" ] || continue
        if [ -n "${hostip}" ] && [ "${hostip}" != "0.0.0.0" ]; then
            RUN_ARGS+=(-p "${hostip}:${hostport}:${p}")
        elif [ -n "${hostport}" ]; then
            RUN_ARGS+=(-p "${hostport}:${p}")
        else
            RUN_ARGS+=(-p "${p}")
        fi
    done < <(inspect_fmt '{{range $p, $conf := .HostConfig.PortBindings}}{{range $conf}}{{println $p .HostIp .HostPort}}{{end}}{{end}}' "${CONTROLLER_NAME}")

    local mtype mname msrc mdst mrw mprop mdriver spec
    while IFS='|' read -r mtype mname msrc mdst mrw mprop mdriver; do
        [ -n "${mdst}" ] || continue
        case "${mtype}" in
            volume)
                spec="type=volume,src=${mname},dst=${mdst}"
                [ "${mrw}" = "false" ] && spec="${spec},readonly"
                [ -n "${mdriver}" ] && spec="${spec},volume-driver=${mdriver}"
                RUN_ARGS+=(--mount "${spec}")
                ;;
            bind)
                case "${msrc}" in
                    *,*) die "bind mount source contains a comma; run the controller step manually: ${msrc}" "${EXIT_GATE}" ;;
                esac
                spec="type=bind,src=${msrc},dst=${mdst}"
                [ "${mrw}" = "false" ] && spec="${spec},readonly"
                [ -n "${mprop}" ] && spec="${spec},bind-propagation=${mprop}"
                RUN_ARGS+=(--mount "${spec}")
                ;;
            tmpfs)
                RUN_ARGS+=(--mount "type=tmpfs,dst=${mdst}")
                ;;
            *)
                die "unsupported mount type: ${mtype} at ${mdst}" "${EXIT_GATE}"
                ;;
        esac
    done < <(mount_lines)

    local ep_first="" ep_item
    local ep_rest=()
    local cmd_args=()
    while IFS= read -r ep_item; do
        [ -n "${ep_item}" ] || continue
        if [ -z "${ep_first}" ]; then
            ep_first="${ep_item}"
        else
            ep_rest+=("${ep_item}")
        fi
    done < <(inspect_fmt '{{range .Config.Entrypoint}}{{println .}}{{end}}' "${CONTROLLER_NAME}")
    while IFS= read -r ep_item; do
        [ -n "${ep_item}" ] && cmd_args+=("${ep_item}")
    done < <(inspect_fmt '{{range .Config.Cmd}}{{println .}}{{end}}' "${CONTROLLER_NAME}")

    if [ -n "${ep_first}" ]; then
        RUN_ARGS+=(--entrypoint "${ep_first}")
    fi
    RUN_ARGS+=("${CONTROLLER_IMAGE}")
    if [ "${#ep_rest[@]}" -gt 0 ]; then
        RUN_ARGS+=("${ep_rest[@]}")
    fi
    if [ "${#cmd_args[@]}" -gt 0 ]; then
        RUN_ARGS+=("${cmd_args[@]}")
    fi
}

rollback_controller() {
    log "rolling back controller"
    if [ "${KEEP_FAILED}" = "1" ]; then
        "${DOCKER_CMD}" stop "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        local failed="agentteams-controller-failed-$(date -u +%Y%m%dT%H%M%SZ)"
        if "${DOCKER_CMD}" rename "${CONTROLLER_NAME}" "${failed}" >/dev/null 2>&1; then
            log "kept failed controller as ${failed}"
        else
            "${DOCKER_CMD}" rm "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        fi
    else
        "${DOCKER_CMD}" stop "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        "${DOCKER_CMD}" rm "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
    fi
    if ! "${DOCKER_CMD}" rename "${OLD_NAME}" "${CONTROLLER_NAME}" >/dev/null 2>&1; then
        CONTROLLER_IMAGE="${OLD_IMAGE:-${CONTROLLER_IMAGE}}"
        write_state "controller" "rollback_failed"
        die "rollback failed: cannot rename ${OLD_NAME} back" "${EXIT_ROLLBACK}"
    fi
    "${DOCKER_CMD}" start "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
    if wait_health; then
        CONTROLLER_IMAGE="${OLD_IMAGE:-${CONTROLLER_IMAGE}}"
        write_state "controller" "rolled_back"
        log "rollback done: old controller is healthy"
        exit "${EXIT_GATE}"
    fi
    CONTROLLER_IMAGE="${OLD_IMAGE:-${CONTROLLER_IMAGE}}"
    write_state "controller" "rollback_failed"
    die "rollback failed: old controller did not pass the health gate" "${EXIT_ROLLBACK}"
}

cmd_controller() {
    detect_docker
    resolve_paths

    [ -n "${CONTROLLER_IMAGE}" ] || die "--controller-image is required" "${EXIT_USAGE}"
    if [ -n "${HIGRESS_BACKFILL_DIR}" ]; then
        [ -d "${HIGRESS_BACKFILL_DIR}" ] || die "higress backfill dir not found: ${HIGRESS_BACKFILL_DIR}" "${EXIT_GATE}"
        if ! find "${HIGRESS_BACKFILL_DIR}" -name plugin.wasm 2>/dev/null | grep -q .; then
            die "higress backfill dir contains no plugin.wasm: ${HIGRESS_BACKFILL_DIR}" "${EXIT_GATE}"
        fi
    fi
    if [ "${ENV_FROM_CONTAINER}" = "1" ]; then
        EFFECTIVE_ENV_FILE="${BACKUP_DIR}/controller.env"
        ENV_SOURCE="container"
    else
        EFFECTIVE_ENV_FILE="${ENV_FILE}"
        ENV_SOURCE="file"
OLD_IMAGE=""
        [ -f "${ENV_FILE}" ] || die "env file not found: ${ENV_FILE}" "${EXIT_GATE}"
    fi
    container_exists "${CONTROLLER_NAME}" || die "controller container not found: ${CONTROLLER_NAME}" "${EXIT_GATE}"

    local old_image ports mounts
    old_image="$(inspect_fmt '{{.Config.Image}}' "${CONTROLLER_NAME}")"
    OLD_IMAGE="${old_image}"
    ports="$(ports_count)"
    mounts="$(mounts_count)"
    case "${ports}" in ''|*[!0-9]*) ports=0 ;; esac
    case "${mounts}" in ''|*[!0-9]*) mounts=0 ;; esac
    if [ "${ports}" -eq 0 ]; then
        die "unsupported controller shape: no published ports; run the controller step manually" "${EXIT_GATE}"
    fi
    if [ "${mounts}" -eq 0 ]; then
        die "unsupported controller shape: no mounts; run the controller step manually" "${EXIT_GATE}"
    fi

    if [ -n "${HIGRESS_BACKFILL_DIR}" ]; then
        [ -d "${HIGRESS_BACKFILL_DIR}" ] || die "higress backfill dir not found: ${HIGRESS_BACKFILL_DIR}" "${EXIT_GATE}"
        if ! find "${HIGRESS_BACKFILL_DIR}" -name plugin.wasm 2>/dev/null | grep -q .; then
            die "higress backfill dir contains no plugin.wasm: ${HIGRESS_BACKFILL_DIR}" "${EXIT_GATE}"
        fi
    fi
    if [ "${ENV_FROM_CONTAINER}" = "1" ]; then
        if [ "${DRY_RUN}" = "1" ]; then
            log "[dry-run] export container env -> ${EFFECTIVE_ENV_FILE} (0600)"
        else
            mkdir -p "${BACKUP_DIR}" && chmod 700 "${BACKUP_DIR}" || die "cannot create ${BACKUP_DIR}" "${EXIT_GATE}"
            umask 077
            "${DOCKER_CMD}" inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "${CONTROLLER_NAME}" > "${EFFECTIVE_ENV_FILE}" 2>/dev/null \
                || die "cannot export the controller env" "${EXIT_GATE}"
            chmod 600 "${EFFECTIVE_ENV_FILE}"
        fi
    elif [ "${ALLOW_ENV_DRIFT}" != "1" ]; then
        local ck fk
        ck="$(inspect_fmt '{{len .Config.Env}}' "${CONTROLLER_NAME}")"
        case "${ck}" in ''|*[!0-9]*) ck=0 ;; esac
        fk="$(grep -c '^[A-Za-z_][A-Za-z0-9_]*=' "${ENV_FILE}" 2>/dev/null || true)"
        case "${fk}" in ''|*[!0-9]*) fk=0 ;; esac
        if [ "${ck}" -gt 0 ] && [ "${fk}" -gt 0 ] && [ "${ck}" -ne "${fk}" ]; then
            die "env drift: container=${ck} env-file=${fk}; use --env-from-container (recommended) or --allow-env-drift" "${EXIT_GATE}"
        fi
    fi

    build_run_args

    if [ "${DRY_RUN}" = "1" ]; then
        log "[dry-run] ${DOCKER_CMD} stop ${CONTROLLER_NAME}"
        log "[dry-run] ${DOCKER_CMD} rename ${CONTROLLER_NAME} agentteams-controller-<old>-old"
        log "[dry-run] ${DOCKER_CMD} run ${RUN_ARGS[*]}"
        if [ -n "${HIGRESS_BACKFILL_DIR}" ]; then
            log "[dry-run] docker cp ${HIGRESS_BACKFILL_DIR}/. ${CONTROLLER_NAME}:/usr/share/nginx/html/plugins/"
            log "[dry-run] docker exec ${CONTROLLER_NAME} pkill envoy; docker restart ${CONTROLLER_NAME}"
        fi
        log "[dry-run] health gate: healthz + agt get teams/workers + logs panic|fatal=0"
        log "[dry-run] on failure: stop/rm new, rename old back, start, health-gate old"
        return "${EXIT_OK}"
    fi
    [ "${YES}" = "1" ] || die "controller requires --yes (or use --dry-run)" "${EXIT_USAGE}"

    OLD_NAME="agentteams-controller-$(printf '%s' "${old_image}" | tr '/:' '__')-old"
    if container_exists "${OLD_NAME}"; then
        OLD_NAME="${OLD_NAME}-$(date -u +%Y%m%dT%H%M%SZ)"
    fi

    "${DOCKER_CMD}" stop "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
    if ! "${DOCKER_CMD}" rename "${CONTROLLER_NAME}" "${OLD_NAME}" >/dev/null 2>&1; then
        "${DOCKER_CMD}" start "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        die "cannot rename the old controller; nothing changed" "${EXIT_GATE}"
    fi

    if ! "${DOCKER_CMD}" run "${RUN_ARGS[@]}"; then
        rollback_controller
    fi
    if [ -n "${HIGRESS_BACKFILL_DIR}" ]; then
        if ! "${DOCKER_CMD}" cp "${HIGRESS_BACKFILL_DIR}/." "${CONTROLLER_NAME}:/usr/share/nginx/html/plugins/" >/dev/null 2>&1; then
            warn "Higress plugin backfill copy failed"
            rollback_controller
        fi
        "${DOCKER_CMD}" exec "${CONTROLLER_NAME}" pkill envoy >/dev/null 2>&1 || true
        "${DOCKER_CMD}" restart "${CONTROLLER_NAME}" >/dev/null 2>&1 || true
        log "Higress plugin backfill applied from ${HIGRESS_BACKFILL_DIR}; controller restarted"
    fi
    if wait_health; then
        if [ "${ALLOW_GATEWAY_DOWN}" = "1" ]; then
            local gw_code
            gw_code="$(gateway_http)"
            [ "${gw_code}" = "000" ] && warn "gateway data plane 8080/v1/models=000; continuing because --allow-gateway-down"
            write_state "controller" "done"
            log "controller upgraded: ${CONTROLLER_IMAGE}; old container kept as ${OLD_NAME}"
            return "${EXIT_OK}"
        fi
        if wait_gateway; then
            write_state "controller" "done"
            log "controller upgraded: ${CONTROLLER_IMAGE}; old container kept as ${OLD_NAME}"
            return "${EXIT_OK}"
        fi
        warn "gateway data plane 8080/v1/models=000; rolling back (use --allow-gateway-down to override)"
        rollback_controller
    fi
    rollback_controller
}

# ---------------------------------------------------------------------------
# status
# ---------------------------------------------------------------------------
cmd_status() {
    resolve_paths
    if [ ! -f "${STATE_FILE}" ]; then
        die "state file not found: ${STATE_FILE}" "${EXIT_USAGE}"
    fi
    if [ "${JSON}" = "1" ]; then
        cat "${STATE_FILE}"
        return "${EXIT_OK}"
    fi
    sed -n 's/^/  /p' "${STATE_FILE%.json}.env" 2>/dev/null || cat "${STATE_FILE}"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
COMMAND="${1:-help}"
[ $# -gt 0 ] && shift

while [ $# -gt 0 ]; do
    case "$1" in
        --env-file) ENV_FILE="$2"; shift 2 ;;
        --env-from-container) ENV_FROM_CONTAINER=1; shift ;;
        --allow-env-drift) ALLOW_ENV_DRIFT=1; shift ;;
        --keep-failed) KEEP_FAILED=1; shift ;;
        --allow-gateway-down) ALLOW_GATEWAY_DOWN=1; shift ;;
        --allow-storage-split) ALLOW_STORAGE_SPLIT=1; shift ;;
        --higress-backfill-dir) HIGRESS_BACKFILL_DIR="$2"; shift 2 ;;
        --backup-dir) BACKUP_DIR="$2"; shift 2 ;;
        --state-file) STATE_FILE="$2"; shift 2 ;;
        --target) TARGET="$2"; shift 2 ;;
        --controller-image) CONTROLLER_IMAGE="$2"; shift 2 ;;
        --timeout) TIMEOUT="$2"; shift 2 ;;
        --log-file) LOG_FILE="$2"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        --yes) YES=1; shift ;;
        --json) JSON=1; shift ;;
        -h|--help) usage; exit "${EXIT_OK}" ;;
        *) die "unknown option: $1" "${EXIT_USAGE}" ;;
    esac
done

case "${COMMAND}" in
    preflight) cmd_preflight ;;
    plan) cmd_plan ;;
    backup) cmd_backup ;;
    controller) cmd_controller ;;
    status) cmd_status ;;
    help|-h|--help) usage ;;
    *) usage; exit "${EXIT_USAGE}" ;;
esac
