#!/bin/bash
set -eo pipefail
source "$(dirname "$0")/lib/minio-client.sh"
state=$(mktemp -d)
trap 'rm -rf "$state"' EXIT
sleep() { :; }
minio_file_exists() { return 0; }
minio_read_file() {
    if [ -f "$state/read" ]; then
        echo 'completed current-task'
    else
        touch "$state/read"
        echo 'draft'
    fi
}
minio_wait_for_file 'task/README.md' 10 'current-task'
test -f "$state/read"
minio_read_file() { echo 'permanently wrong content'; }
if minio_wait_for_file 'task/README.md' 10 'current-task'; then
    echo 'Wrong content unexpectedly passed' >&2
    exit 1
fi
minio_read_file() { return 1; }
minio_wait_for_file 'legacy/file' 5
echo 'MinIO content readiness regressions: PASS'
