#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SCRIPT="${ROOT_DIR}/install/agentteams-install.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agentteams-higress-diagnostics.XXXXXX")"
trap 'rm -rf "${TEST_ROOT}"' EXIT

HELPER="${TEST_ROOT}/helper.sh"
OUTPUT="${TEST_ROOT}/output.log"

sed -n '/^        _report_higress_gateway_timeout() {/,/^        }/p' \
    "${INSTALL_SCRIPT}" >"${HELPER}"
source "${HELPER}"

cat >"${TEST_ROOT}/docker" <<'MOCK'
#!/usr/bin/env bash
set -e
test "${1:-}" = exec
shift 2
case "$*" in
    "uname -m") printf 'arm64\n' ;;
    "cat /proc/sys/fs/inotify/max_user_instances") printf '128\n' ;;
    "cat /proc/sys/vm/max_map_count") printf '65530\n' ;;
    "cat /proc/sys/vm/overcommit_memory") printf '0\n' ;;
    "sh -c tail -80 /var/log/agentteams/higress-gateway.log 2>/dev/null")
        printf 'Fatal error: Check failed: 12\n'
        ;;
    *) echo "unexpected docker invocation: $*" >&2; exit 1 ;;
esac
MOCK
chmod +x "${TEST_ROOT}/docker"

DOCKER_CMD="${TEST_ROOT}/docker"
log() { printf '%s\n' "$*" >>"${OUTPUT}"; }

_report_higress_gateway_timeout agentteams-controller >>"${OUTPUT}"

for expected in \
    "architecture=arm64" \
    "fs.inotify.max_user_instances=128" \
    "vm.max_map_count=65530" \
    "vm.overcommit_memory=0" \
    "gateway-log: Fatal error: Check failed: 12" \
    "Detected Envoy/V8 fatal exit" \
    "reported on ARM64"; do
    grep -Fq "${expected}" "${OUTPUT}" || {
        echo "missing diagnostic: ${expected}" >&2
        cat "${OUTPUT}" >&2
        exit 1
    }
done

grep -Fq '_report_higress_gateway_timeout "${ctr}"' "${INSTALL_SCRIPT}"
echo "PASS: Higress timeout diagnostics include actionable evidence"
