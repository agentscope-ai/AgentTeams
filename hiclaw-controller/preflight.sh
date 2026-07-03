#!/bin/sh
# ============================================================
# HiClaw Embedded Image — manual kine SQLite integrity check
# ============================================================
# The REAL pre-flight (run automatically, every boot, before kine starts) is
# implemented in Go: hiclaw-controller/internal/store/kine.go
# (checkSQLiteIntegrity, called from StartKine before endpoint.Listen). It
# uses the CGo-linked github.com/mattn/go-sqlite3 driver that is already
# compiled into the hiclaw-controller binary (CGO_ENABLED=1, see
# hiclaw-controller/Dockerfile) — it does NOT rely on a `sqlite3` CLI binary
# being present in the image, so this script is not part of the container
# startup path.
#
# This script exists as an operator convenience for the MANUAL corruption
# check described in docs/implementation-milestone-1.md Step 3 (S3b
# acceptance criteria): run it by hand, inside or against a copy of the
# running container's /data volume, to sanity-check a DB outside of the
# controller's own boot sequence — e.g. after copying hiclaw.db off a
# volume for offline inspection.
#
# Manual corruption-recovery drill (documented per Step 3 acceptance):
#   1. docker cp hiclaw:/data/hiclaw-controller/hiclaw.db /tmp/hiclaw.db.bak
#   2. Corrupt a copy to rehearse the failure path:
#        cp /tmp/hiclaw.db.bak /tmp/hiclaw.db.corrupt
#        truncate -s 4096 /tmp/hiclaw.db.corrupt   # chop the file mid-page
#   3. ./preflight.sh /tmp/hiclaw.db.corrupt   -> exits 1, prints FAIL
#   4. Restart the real container with the corrupt file swapped into the
#      volume -> hiclaw-controller refuses to start and prints the
#      "KINE SQLITE INTEGRITY CHECK FAILED — REFUSING TO START" banner
#      (see internal/store/kine.go logKineCorruption) instead of silently
#      booting against an empty re-initialized DB.
#
# Usage: preflight.sh [path-to-hiclaw.db]  (default: /data/hiclaw-controller/hiclaw.db)
# Requires the sqlite3 CLI, which is NOT installed in the embedded image by
# default (verified: only nginx/jq/curl/zip/unzip/git are apt-get installed
# in Dockerfile.embedded). Install it ad hoc for a one-off manual check:
#   docker exec -u root hiclaw apt-get update && apt-get install -y sqlite3
# ============================================================
set -eu

DB_PATH="${1:-/data/hiclaw-controller/hiclaw.db}"

if [ ! -f "$DB_PATH" ]; then
    echo "preflight: $DB_PATH does not exist (fresh volume) — nothing to check" >&2
    exit 0
fi

if ! command -v sqlite3 >/dev/null 2>&1; then
    echo "preflight: sqlite3 CLI not found on PATH. Install it (apt-get install -y sqlite3)" >&2
    echo "preflight: or rely on the automatic Go-based check in internal/store/kine.go" >&2
    exit 2
fi

result="$(sqlite3 "$DB_PATH" 'PRAGMA integrity_check;' 2>&1)" || {
    echo "########################################################################" >&2
    echo "# KINE SQLITE INTEGRITY CHECK FAILED (manual run)" >&2
    echo "# db_path=$DB_PATH" >&2
    echo "# detail=sqlite3 invocation error: $result" >&2
    echo "########################################################################" >&2
    exit 1
}

if [ "$result" != "ok" ]; then
    echo "########################################################################" >&2
    echo "# KINE SQLITE INTEGRITY CHECK FAILED (manual run)" >&2
    echo "# db_path=$DB_PATH" >&2
    echo "# detail=$result" >&2
    echo "########################################################################" >&2
    exit 1
fi

echo "preflight: ok $DB_PATH"
exit 0
