#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval "$(sed -n '/^matrix_login()/,/^}/p' "$ROOT_DIR/tests/lib/matrix-client.sh")"
SESSION_TEST_DIR=$(mktemp -d)
trap 'rm -r "$SESSION_TEST_DIR"' EXIT
export SESSION_TEST_DIR TEST_MATRIX_DIRECT_URL=http://matrix
export TEST_ADMIN_USER=admin TEST_ADMIN_PASSWORD=fixture-password
echo 0 > "$SESSION_TEST_DIR/logins"
exec_in_manager() {
    local count body username password
    count=$(cat "$SESSION_TEST_DIR/logins")
    count=$((count + 1))
    echo "$count" > "$SESSION_TEST_DIR/logins"
    body="${!#}"
    username=$(jq -r '.identifier.user' <<< "$body")
    password=$(jq -r '.password' <<< "$body")
    if [ "$count" -gt 5 ] || [ "$password" != fixture-password ]; then
        return 22
    fi
    jq -cn --arg token "fixture-$username" '{access_token:$token}'
}
export -f matrix_login exec_in_manager

login=$(matrix_login "$TEST_ADMIN_USER" "$TEST_ADMIN_PASSWORD")
export TEST_MATRIX_ADMIN_SESSION=$(jq -r '.access_token' <<< "$login")
for _ in {1..8}; do
    if ! bash -c 'matrix_login "$TEST_ADMIN_USER" "$TEST_ADMIN_PASSWORD"' \
        | jq -e '.access_token == "fixture-admin"' >/dev/null; then
        echo 'FAIL: repeated Admin logins exhausted the successful-login allowance' >&2
        exit 1
    fi
done
test "$(cat "$SESSION_TEST_DIR/logins")" -eq 1
if matrix_login admin wrong-password >/dev/null; then
    echo 'FAIL: cached session must not accept a different password' >&2
    exit 1
fi
matrix_login alice fixture-password | jq -e '.access_token == "fixture-alice"' >/dev/null
test "$(cat "$SESSION_TEST_DIR/logins")" -eq 3
echo 'PASS: child tests reuse the Admin session; other credentials still log in'
