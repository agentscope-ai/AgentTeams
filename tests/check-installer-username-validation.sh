#!/usr/bin/env bash
set -eo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
eval "$(sed -n '/^step_admin()/,/^}/p' "$INSTALLER")"
minio_block="$(sed -n '/^    AGENTTEAMS_MINIO_USER=/,/^    AGENTTEAMS_MINIO_PASSWORD=/p' "$INSTALLER")"
msg() { printf '%s' "$1"; }
log() { :; }
error() { :; }
die() { echo "$*" >&2; exit 1; }
# Keep the real step logic while replacing terminal input with deterministic answers.
prompt() {
    calls=$((calls + 1))
    [ "$calls" -le 2 ] || exit 1
    if [ "$back" = 1 ]; then STEP_RESULT=back; return 1; fi
    if [ "$calls" = 1 ] && [ -n "$first_input" ]; then AGENTTEAMS_ADMIN_USER="$first_input"; fi
    if [ -z "${AGENTTEAMS_ADMIN_USER:-}" ]; then
        AGENTTEAMS_ADMIN_USER="${answer:-admin}"
    fi
}
run_admin() (
    AGENTTEAMS_ADMIN_USER="$1"
    AGENTTEAMS_NON_INTERACTIVE="$2"
    answer="$3"
    back="${4:-0}"
    first_input="${5:-}"
    calls=0
    AGENTTEAMS_ADMIN_PASSWORD=password
    step_admin
    if [ "$back" = 1 ]; then [ "$STEP_RESULT" = back ]; else
        [ "$AGENTTEAMS_ADMIN_USER" = "${answer:-admin}" ]
    fi
)
if run_admin ss 1 ss; then echo 'FAIL: two-character preset accepted'; exit 1; fi
run_admin ABC 1 abc
run_admin '' 1 ''
run_admin ss 0 valid
run_admin '' 0 valid 0 ss
run_admin '' 0 '' 1
for user in ss abc; do
    if (AGENTTEAMS_ADMIN_USER=admin; AGENTTEAMS_MINIO_USER="$user"; AGENTTEAMS_ADMIN_PASSWORD=password; eval "$minio_block"); then
        [ "$user" = abc ] || { echo 'FAIL: short MinIO override accepted'; exit 1; }
    else
        [ "$user" = ss ] || exit 1
    fi
done
(AGENTTEAMS_ADMIN_USER=abc; unset AGENTTEAMS_MINIO_USER; AGENTTEAMS_ADMIN_PASSWORD=password; eval "$minio_block"; [ "$AGENTTEAMS_MINIO_USER" = abc ])
echo 'PASS: Bash installer username validation'
