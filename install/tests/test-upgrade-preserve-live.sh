#!/usr/bin/env bash
# ============================================================================
# test-upgrade-preserve-live.sh — real-engine regression for the controller
# step's boundary preservation (install/agentteams-upgrade.sh).
#
# Unlike test-upgrade-cli.sh (stubbed docker) this drives the real docker
# daemon with throwaway busybox fixtures. Needs a reachable daemon; without
# one it prints SKIP and exits 0 so CI/dev machines without docker stay
# green — the Pi is where this suite is meant to bite.
# ============================================================================
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
UPGRADE="${REPO_ROOT}/install/agentteams-upgrade.sh"

if [ ! -f "${UPGRADE}" ]; then
    echo "FAIL setup: ${UPGRADE} not found"
    exit 1
fi

if ! docker info >/dev/null 2>&1; then
    echo "SKIP test-upgrade-preserve-live (no docker daemon)"
    exit 0
fi

TMP="$(mktemp -d)"
FA="atc-boundary-a-$$"
FB="atc-boundary-b-$$"
VOL_A="atc-boundary-vol-a-$$"
VOL_B="atc-boundary-vol-b-$$"
cleanup() {
    local n
    for n in $(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^(${FA}|${FB}|${FC})" || true); do
        docker rm -f "${n}" >/dev/null 2>&1 || true
    done
    docker volume rm "${VOL_A}" "${VOL_B}" "${VOL_C}" >/dev/null 2>&1 || true
    rm -rf "${TMP}"
}
trap cleanup EXIT

fail() {
    echo "FAIL $1"
    echo "--- output ---"
    cat "${TMP}/last.out" 2>/dev/null
    exit 1
}

run_live() {
    # $1 label (unused, for symmetry with the stub suite); rest: upgrade args
    shift
    bash "${UPGRADE}" "$@" > "${TMP}/last.out" 2>&1
    RC=$?
}

docker image inspect busybox:latest >/dev/null 2>&1 \
    || docker pull busybox:latest >/dev/null 2>&1 \
    || { echo "FAIL setup: busybox:latest not available locally and pull failed"; exit 1; }

# ---------------------------------------------------------------------------
# Fixture A: a created (never started) controller-like container carrying a
# security/resource boundary: user, workdir, readonly rootfs, memory, cpus.
# ---------------------------------------------------------------------------
docker volume create "${VOL_A}" >/dev/null || fail "volume A"
docker create --name "${FA}" \
    --user 1000:1000 --workdir /work --read-only \
    --memory 256m --cpus 0.5 \
    -v "${VOL_A}:/data" \
    busybox:latest sleep 300 >/dev/null || fail "fixture A create"
FA_ID="$(docker inspect --format '{{.Id}}' "${FA}")"
# Expectations come from the fixture itself: some kernels/cgroups discard
# memory or pids limits at create time, and the upgrade must reproduce what
# the old container actually has, not what was requested.
EXP_USER="$(docker inspect --format '{{.Config.User}}' "${FA}")"
EXP_WORKDIR="$(docker inspect --format '{{.Config.WorkingDir}}' "${FA}")"
EXP_READONLY="$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "${FA}")"
EXP_MEM="$(docker inspect --format '{{.HostConfig.Memory}}' "${FA}")"
EXP_NANO="$(docker inspect --format '{{.HostConfig.NanoCpus}}' "${FA}")"
docker stop "${FA}" >/dev/null 2>&1 || true

ENV_A="${TMP}/env-a"
printf 'AGENTTEAMS_DATA_DIR=%s\nAGENTTEAMS_VERSION=latest\n' "${VOL_A}" > "${ENV_A}"

# preflight: boundary is read out, nothing unsupported
# --allow-storage-split: the live stack in this environment is deliberately
# split (unrelated to the fixture); the test is about the fixture boundary.
run_live preflight preflight --json --allow-storage-split --env-file "${ENV_A}" --backup-dir "${TMP}/a" --controller-name "${FA}"
[ "${RC}" = "0" ] || fail "preflight A rc=${RC}"
grep -qF "\"user\":\"${EXP_USER}\"" "${TMP}/last.out" || fail "preflight A user"
grep -qF "\"workdir\":\"${EXP_WORKDIR}\"" "${TMP}/last.out" || fail "preflight A workdir"
grep -qF "\"readonly_rootfs\":${EXP_READONLY}" "${TMP}/last.out" || fail "preflight A readonly"
grep -qF "\"memory\":${EXP_MEM}" "${TMP}/last.out" || fail "preflight A memory"
grep -qF '"unsupported_runtime":[]' "${TMP}/last.out" || fail "preflight A unsupported empty"

# controller --prepare-only --dry-run: the create line carries the boundary
run_live dryA controller --dry-run --prepare-only \
    --controller-image busybox:latest --env-file "${ENV_A}" --allow-env-drift \
    --backup-dir "${TMP}/a" --controller-name "${FA}"
[ "${RC}" = "0" ] || fail "dry A rc=${RC}"
[ -n "${EXP_USER}" ] && { grep -qF -- "--user ${EXP_USER}" "${TMP}/last.out" || fail "dry A user"; }
[ -n "${EXP_WORKDIR}" ] && { grep -qF -- "--workdir ${EXP_WORKDIR}" "${TMP}/last.out" || fail "dry A workdir"; }
[ "${EXP_READONLY}" = "true" ] && { grep -qF -- "--read-only" "${TMP}/last.out" || fail "dry A read-only"; }
[ "${EXP_MEM}" != "0" ] && { grep -qF -- "--memory ${EXP_MEM}" "${TMP}/last.out" || fail "dry A memory"; }
[ "${EXP_NANO}" != "0" ] && { grep -qF -- "--cpus" "${TMP}/last.out" || fail "dry A cpus"; }

# controller --prepare-only --yes: create, boundary-verify, discard the new
# container, restore the old one under its original name — fixture intact
run_live prepareA controller --yes --prepare-only --allow-storage-split \
    --controller-image busybox:latest --env-file "${ENV_A}" --allow-env-drift \
    --backup-dir "${TMP}/a" --controller-name "${FA}"
[ "${RC}" = "0" ] || fail "prepare A rc=${RC}"
grep -qF 'boundary preserved:' "${TMP}/last.out" || fail "prepare A preserved line"
[ "$(docker inspect --format '{{.Id}}' "${FA}")" = "${FA_ID}" ] || fail "prepare A id changed"
[ "$(docker inspect --format '{{.Config.User}}' "${FA}")" = "${EXP_USER}" ] || fail "prepare A user lost"
[ "$(docker inspect --format '{{.State.Running}}' "${FA}")" = "false" ] || fail "prepare A left running"
if docker ps -a --format '{{.Names}}' | grep -q "^${FA}-"; then
    fail "prepare A leftover container"
fi

# ---------------------------------------------------------------------------
# Fixture C: a non-default shared-memory size. Daemons differ in their default
# (64 MiB vs several GiB), so the upgrade must pass --shm-size explicitly and
# the boundary verify must compare it.
# ---------------------------------------------------------------------------
FC="atc-boundary-c-$$"
VOL_C="atc-boundary-vol-c-$$"
docker volume create "${VOL_C}" >/dev/null || fail "volume C"
docker create --name "${FC}" --shm-size 134217728 -v "${VOL_C}:/data" \
    busybox:latest sleep 300 >/dev/null || fail "fixture C create"
FC_ID="$(docker inspect --format '{{.Id}}' "${FC}")"
FC_SHM="$(docker inspect --format '{{.HostConfig.ShmSize}}' "${FC}")"
docker stop "${FC}" >/dev/null 2>&1 || true
ENV_C="${TMP}/env-c"
printf 'AGENTTEAMS_DATA_DIR=%s\nAGENTTEAMS_VERSION=latest\n' "${VOL_C}" > "${ENV_C}"
run_live prepareC controller --yes --prepare-only --allow-storage-split \
    --controller-image busybox:latest --env-file "${ENV_C}" --allow-env-drift \
    --backup-dir "${TMP}/c" --controller-name "${FC}"
[ "${RC}" = "0" ] || fail "prepare C rc=${RC}"
grep -qF 'boundary preserved:' "${TMP}/last.out" || fail "prepare C preserved line"
grep -qF 'shm_size' "${TMP}/last.out" || fail "prepare C shm summary"
[ "$(docker inspect --format '{{.Id}}' "${FC}")" = "${FC_ID}" ] || fail "prepare C id changed"
[ "$(docker inspect --format '{{.HostConfig.ShmSize}}' "${FC}")" = "${FC_SHM}" ] || fail "prepare C shm changed"
docker rm -f "${FC}" >/dev/null 2>&1 || true
docker volume rm "${VOL_C}" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Fixture B: an unsupported runtime setting (sysctl) — the upgrade must
# refuse before anything is renamed or stopped.
# ---------------------------------------------------------------------------
docker volume create "${VOL_B}" >/dev/null || fail "volume B"
docker create --name "${FB}" \
    --sysctl net.ipv4.ip_forward=1 \
    -v "${VOL_B}:/data" \
    busybox:latest sleep 300 >/dev/null || fail "fixture B create"
FB_ID="$(docker inspect --format '{{.Id}}' "${FB}")"
FB_STATUS="$(docker inspect --format '{{.State.Status}}' "${FB}")"
docker stop "${FB}" >/dev/null 2>&1 || true

ENV_B="${TMP}/env-b"
printf 'AGENTTEAMS_DATA_DIR=%s\nAGENTTEAMS_VERSION=latest\n' "${VOL_B}" > "${ENV_B}"

run_live preflightB preflight --json --allow-storage-split --env-file "${ENV_B}" --backup-dir "${TMP}/b" --controller-name "${FB}"
[ "${RC}" = "3" ] || fail "preflight B rc=${RC} (want 3)"
grep -q 'unsupported non-default runtime settings' "${TMP}/last.out" || fail "preflight B message"
grep -qF 'sysctls' "${TMP}/last.out" || fail "preflight B sysctls name"

run_live prepareB controller --yes --prepare-only --allow-storage-split \
    --controller-image busybox:latest --env-file "${ENV_B}" --allow-env-drift \
    --backup-dir "${TMP}/b" --controller-name "${FB}"
[ "${RC}" = "3" ] || fail "prepare B rc=${RC} (want 3)"
[ "$(docker inspect --format '{{.Id}}' "${FB}")" = "${FB_ID}" ] || fail "prepare B id changed"
[ "$(docker inspect --format '{{.State.Status}}' "${FB}")" = "${FB_STATUS}" ] || fail "prepare B status changed"

echo "PASS test-upgrade-preserve-live"
