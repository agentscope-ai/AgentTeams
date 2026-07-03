#!/bin/sh
# ============================================================
# HiClaw Embedded Image — composite Docker HEALTHCHECK
# ============================================================
# Wired in via Dockerfile.embedded's HEALTHCHECK CMD. Exits 0 (healthy) only
# if ALL of the following respond successfully:
#   1. hiclaw-controller  GET http://127.0.0.1:8090/healthz
#   2. MinIO               GET http://127.0.0.1:9000/minio/health/live
#   3. Tuwunel (Matrix)    GET http://127.0.0.1:6167/_matrix/client/versions
#      (CONDUWUIT_PORT in manager/scripts/init/start-tuwunel.sh — Tuwunel's
#      internal listen port; 8080 is the Higress gateway's public port, not
#      Tuwunel's own.)
#
# Any single failure makes the whole container report unhealthy — Docker's
# HEALTHCHECK model has no notion of "partially up", so callers (orchestration,
# `docker ps`, `docker inspect`) get one aggregate signal.
#
# Uses curl (already installed by Dockerfile.embedded's apt-get layer for the
# base higress/all-in-one:2.2.1 image; verified present, wget is not).
# ============================================================
set -eu

CONTROLLER_URL="${HICLAW_HEALTHCHECK_CONTROLLER_URL:-http://127.0.0.1:8090/healthz}"
MINIO_URL="${HICLAW_HEALTHCHECK_MINIO_URL:-http://127.0.0.1:9000/minio/health/live}"
TUWUNEL_URL="${HICLAW_HEALTHCHECK_TUWUNEL_URL:-http://127.0.0.1:6167/_matrix/client/versions}"

check() {
    name="$1"
    url="$2"
    if ! curl -fsS --max-time 3 -o /dev/null "$url"; then
        echo "healthcheck: FAIL ${name} (${url})" >&2
        return 1
    fi
    echo "healthcheck: ok ${name}"
    return 0
}

status=0
check "controller"  "$CONTROLLER_URL" || status=1
check "minio"        "$MINIO_URL"      || status=1
check "tuwunel"      "$TUWUNEL_URL"    || status=1

exit "$status"
