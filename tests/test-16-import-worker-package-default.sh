#!/bin/bash
# test-16-import-worker-package-default.sh - Case 16: agentteams-import.sh package defaulting
#
# Verifies the host-side import wrapper assembles the expected `agt` CLI
# arguments before delegating into the Manager container:
#   1. --name without --package defaults to --package <name>
#   2. explicit --package is preserved
#   3. --zip imports do not inject --package

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TEST_TMP_DIR="$(mktemp -d /tmp/agentteams-import-default-XXXXXX)"
trap 'rm -rf "${TEST_TMP_DIR}"' EXIT

FAKE_BIN="${TEST_TMP_DIR}/bin"
FAKE_DOCKER_CAPTURE="${TEST_TMP_DIR}/docker-cmd.txt"
mkdir -p "${FAKE_BIN}"
export FAKE_DOCKER_CAPTURE

cat > "${FAKE_BIN}/docker" <<'EOF'
#!/bin/bash
set -e

capture_file="${FAKE_DOCKER_CAPTURE:?}"

case "$1" in
    info)
        exit 0
        ;;
    ps)
        echo "agentteams-manager"
        exit 0
        ;;
    cp)
        exit 0
        ;;
    exec)
        shift
        if [ "${1:-}" = "agentteams-manager" ]; then
            shift
        fi
        case "${1:-}" in
            mkdir)
                exit 0
                ;;
            printenv)
                exit 0
                ;;
            agt)
                shift
                printf '%s\n' "$@" > "${capture_file}"
                exit 0
                ;;
        esac
        exit 0
        ;;
esac

echo "unexpected docker invocation: $*" >&2
exit 1
EOF
chmod +x "${FAKE_BIN}/docker"

export PATH="${FAKE_BIN}:${PATH}"

source "${SCRIPT_DIR}/lib/test-helpers.sh"

test_setup "16-import-worker-package-default"

run_import() {
    : > "${FAKE_DOCKER_CAPTURE}"
    bash "${PROJECT_ROOT}/install/agentteams-import.sh" "$@" >/dev/null 2>&1
}

captured_cmd() {
    tr '\n' ' ' < "${FAKE_DOCKER_CAPTURE}" | sed 's/  */ /g; s/^ //; s/ $//'
}

count_package_flags() {
    grep -c '^--package$' "${FAKE_DOCKER_CAPTURE}" 2>/dev/null || true
}

log_section "Default Package from Name"
run_import worker --name alice
CMD="$(captured_cmd)"
assert_contains "${CMD}" "apply worker --name alice --package alice" "import wrapper defaults package to worker name"
assert_eq "1" "$(count_package_flags)" "default package is injected exactly once"

log_section "Explicit Package Preserved"
run_import worker --name alice --package reviewer
CMD="$(captured_cmd)"
assert_contains "${CMD}" "apply worker --name alice --package reviewer" "explicit package is forwarded unchanged"
assert_eq "1" "$(count_package_flags)" "explicit package does not duplicate package flag"

log_section "Zip Import Skips Package Default"
run_import worker --name alice --zip /tmp/alice.zip
CMD="$(captured_cmd)"
assert_contains "${CMD}" "apply worker --name alice --zip /tmp/import/alice.zip" "zip import forwards copied zip path"
assert_eq "0" "$(count_package_flags)" "zip import does not auto-inject package"

test_teardown "16-import-worker-package-default"
test_summary
