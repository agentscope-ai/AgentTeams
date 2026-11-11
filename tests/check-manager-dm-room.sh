#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_MATRIX_DIRECT_URL=http://matrix
eval "$(sed -n '/^matrix_find_dm_room()/,/^}/p' "$ROOT_DIR/tests/lib/matrix-client.sh")"
log_info() { :; }
_encode_room_id() { printf '%s' "$1"; }
matrix_joined_rooms() { printf '%s' '{"joined_rooms":["!worker","!manager"]}'; }
exec_in_agent() {
    printf '%s' '{"managers":[{"matrixUserID":"@manager:test","roomID":"!manager"}]}';
}
exec_in_manager() {
    printf '%s' '{"chunk":[{"state_key":"@admin:test","content":{"membership":"join"}},{"state_key":"@manager:test","content":{"membership":"join"}},{"state_key":"@alice:test","content":{"membership":"join"}}]}';
}
room=$(matrix_find_dm_room token '@manager:test' || true)
[[ "$room" == '!manager' ]] || { echo 'FAIL: canonical Manager room must remain discoverable after a Worker joins'; exit 1; }
exec_in_agent() { return 1; }
exec_in_manager() {
    case "$*" in
        *'!worker'*) printf '%s' '{"chunk":[{"state_key":"@admin:test","content":{"membership":"join"}},{"state_key":"@other:test","content":{"membership":"join"}}]}' ;;
        *) printf '%s' '{"chunk":[{"state_key":"@admin:test","content":{"membership":"join"}},{"state_key":"@manager:test","content":{"membership":"join"}},{"state_key":"@alice:test","content":{"membership":"leave"}}]}' ;;
    esac
}
[[ "$(matrix_find_dm_room token '@manager:test')" == '!manager' ]]
if matrix_find_dm_room token '@manager:tes' >/dev/null; then
    echo 'FAIL: user IDs must match exactly'; exit 1
fi
echo 'PASS: canonical Manager room, historical membership and exact identity'
