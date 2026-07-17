#!/bin/bash
# Security regression test for install/hiclaw-install.sh prompt helpers.
#
# The prompt/prompt_optional helpers used to assign values via `eval`, which
# executed any shell metacharacters in a preset or default value. They now use
# `printf -v` (write) and `${!var}` indirect expansion (read). This test proves
# that values containing single quotes, command substitution, and backticks are
# preserved verbatim and never executed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
INSTALLER="${REPO_ROOT}/install/hiclaw-install.sh"

fail() { echo "FAIL: $1" >&2; exit 1; }

# Extract just the prompt() and prompt_optional() functions so we can exercise
# them without triggering the installer's top-level command dispatch.
prompt_src="$(sed -n '/^prompt() {/,/^}/p' "${INSTALLER}")"
[ -n "${prompt_src}" ] || fail "could not extract prompt() from installer"
prompt_optional_src="$(sed -n '/^prompt_optional() {/,/^}/p' "${INSTALLER}")"
[ -n "${prompt_optional_src}" ] || fail "could not extract prompt_optional() from installer"

TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

# A payload that would create the sentinel file (and mangle the value) if the
# helper ever re-parsed it through eval or a subshell.
sentinel="${TMPDIR_ROOT}/pwned"
payload="a'b\$(touch ${sentinel})\`touch ${sentinel}\`c"

# Case 1: value already set in the environment (read path via ${!var}).
# Non-interactive + preset value -> prompt should log "preset" and leave the
# value untouched, never re-parsing it.
out1="$(
    env -i PATH="${PATH}" MY_VALUE="${payload}" bash -c '
        log() { :; }
        msg() { :; }
        read_secret() { :; }
        read() { return 0; }
        die() { echo "die: $*" >&2; exit 7; }
        '"${prompt_src}"'
        AGENTTEAMS_NON_INTERACTIVE=1
        prompt MY_VALUE "Some value" "unused-default"
        printf "%s" "${MY_VALUE}"
    '
)"
[ "${out1}" = "${payload}" ] || fail "preset value not preserved verbatim: got [${out1}] want [${payload}]"
[ ! -e "${sentinel}" ] || fail "preset value executed a command substitution"

# Case 2: value unset -> non-interactive default path (write path via printf -v).
out2="$(
    env -i PATH="${PATH}" PAYLOAD="${payload}" bash -c '
        log() { :; }
        msg() { :; }
        read_secret() { :; }
        read() { return 0; }
        die() { echo "die: $*" >&2; exit 7; }
        '"${prompt_src}"'
        AGENTTEAMS_NON_INTERACTIVE=1
        prompt MY_DEFAULT "Some value" "$PAYLOAD"
        printf "%s" "${MY_DEFAULT}"
    '
)"
[ "${out2}" = "${payload}" ] || fail "default value not assigned verbatim: got [${out2}] want [${payload}]"
[ ! -e "${sentinel}" ] || fail "default value executed a command substitution"

# Case 3: prompt_optional with a preset value must also preserve it verbatim.
out3="$(
    env -i PATH="${PATH}" MY_OPT="${payload}" bash -c '
        log() { :; }
        msg() { :; }
        read_secret() { :; }
        read() { return 0; }
        die() { echo "die: $*" >&2; exit 7; }
        '"${prompt_optional_src}"'
        AGENTTEAMS_NON_INTERACTIVE=1
        prompt_optional MY_OPT "Optional value"
        printf "%s" "${MY_OPT}"
    '
)"
[ "${out3}" = "${payload}" ] || fail "prompt_optional preset value not preserved verbatim: got [${out3}] want [${payload}]"
[ ! -e "${sentinel}" ] || fail "prompt_optional preset value executed a command substitution"

echo "PASS: installer prompt helpers preserve values without executing them"
