#!/usr/bin/env bash
# Fail when docs/usage/<name>.md is present but docs/zh-cn/usage/<name>.md is not.
# A drift here means an English-only doc landed without its Chinese counterpart;
# the #1354 translation automation only covers new docs, so legacy drift
# surfaces via this guard.
#
# Intentionally English-only pages are exempt through an explicit -en suffix:
# a file named <base>-en.md is skipped by the loop below, so the suffix is a
# real exemption enforced by the same script that recommends it.
#
# --self-test runs fixture-based cases instead of checking the repository:
#   - a missing Chinese counterpart must make the guard exit non-zero;
#   - a complete pairing (including an -en suffixed English-only file) passes;
#   - an -en sibling must not mask a genuinely missing counterpart.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

EN_DIR="docs/usage"
ZH_DIR="docs/zh-cn/usage"

run_check() {
    local root="$1"
    if [ ! -d "${root}/${EN_DIR}" ]; then
        echo "no ${EN_DIR} directory; nothing to check"
        exit 0
    fi

    local missing=()
    local checked=0
    local en base zh
    shopt -s nullglob
    for en in "${root}/${EN_DIR}"/*.md; do
        base="$(basename "${en}")"
        # Explicit exemption: <name>-en.md marks an intentionally
        # English-only page, so it is excluded from the parity loop.
        case "${base}" in
            *-en.md) continue ;;
        esac
        checked=$((checked + 1))
        zh="${root}/${ZH_DIR}/${base}"
        if [ ! -f "${zh}" ]; then
            missing+=("${base}")
        fi
    done

    if [ "${#missing[@]}" -gt 0 ]; then
        echo "ERROR: English-only docs in ${EN_DIR} missing their zh-cn counterpart:" >&2
        local m
        for m in "${missing[@]}"; do
            echo "  - ${EN_DIR}/${m}  (no ${ZH_DIR}/${m})" >&2
        done
        echo >&2
        echo "Translate the doc or, if the file is intentionally English-only," >&2
        echo "rename it with an -en suffix (e.g. internal-notes-en.md);" >&2
        echo "this check skips such files." >&2
        exit 1
    fi

    echo "ok: ${EN_DIR} and ${ZH_DIR} are in sync (${checked} files checked)"
}

run_self_test() {
    local tmp script guard
    guard="$(basename "${BASH_SOURCE[0]}")"
    script="${SCRIPT_DIR}/${guard}"
    tmp="$(mktemp -d)"
    trap 'rm -rf "${tmp}"' RETURN

    # Case 1: an English doc without its Chinese counterpart must fail.
    mkdir -p "${tmp}/missing/tests" "${tmp}/missing/${EN_DIR}" "${tmp}/missing/${ZH_DIR}"
    cp "${script}" "${tmp}/missing/tests/"
    printf '# Alpha\n' > "${tmp}/missing/${EN_DIR}/alpha.md"
    printf '# Alpha (zh)\n' > "${tmp}/missing/${ZH_DIR}/alpha.md"
    printf '# Beta (no zh counterpart)\n' > "${tmp}/missing/${EN_DIR}/beta.md"
    if bash "${tmp}/missing/tests/${guard}" >/dev/null 2>&1; then
        echo "self-test FAIL: missing Chinese counterpart did not fail the guard" >&2
        return 1
    fi

    # Case 2: complete pairing plus an -en suffixed English-only file passes.
    mkdir -p "${tmp}/complete/tests" "${tmp}/complete/${EN_DIR}" "${tmp}/complete/${ZH_DIR}"
    cp "${script}" "${tmp}/complete/tests/"
    printf '# Alpha\n' > "${tmp}/complete/${EN_DIR}/alpha.md"
    printf '# Alpha (zh)\n' > "${tmp}/complete/${ZH_DIR}/alpha.md"
    printf '# Internal notes, intentionally English-only\n' > "${tmp}/complete/${EN_DIR}/internal-notes-en.md"
    if ! bash "${tmp}/complete/tests/${guard}" >/dev/null 2>&1; then
        echo "self-test FAIL: complete pairing with an -en exemption did not pass" >&2
        return 1
    fi

    # Case 3: the exemption must not mask a real missing counterpart --
    # beta.md (no -en suffix) still has to fail even when beta-en.md exists.
    mkdir -p "${tmp}/suffix-shadow/tests" "${tmp}/suffix-shadow/${EN_DIR}" "${tmp}/suffix-shadow/${ZH_DIR}"
    cp "${script}" "${tmp}/suffix-shadow/tests/"
    printf '# Alpha\n' > "${tmp}/suffix-shadow/${EN_DIR}/alpha.md"
    printf '# Alpha (zh)\n' > "${tmp}/suffix-shadow/${ZH_DIR}/alpha.md"
    printf '# Beta\n' > "${tmp}/suffix-shadow/${EN_DIR}/beta.md"
    printf '# Beta (en-only variant)\n' > "${tmp}/suffix-shadow/${EN_DIR}/beta-en.md"
    if bash "${tmp}/suffix-shadow/tests/${guard}" >/dev/null 2>&1; then
        echo "self-test FAIL: -en exemption masked a genuinely missing counterpart" >&2
        return 1
    fi

    echo "self-test ok: missing counterpart fails, complete pairing with -en exemption passes"
}

case "${1:-}" in
    --self-test)
        run_self_test
        ;;
    "")
        run_check "${REPO_ROOT}"
        ;;
    *)
        echo "usage: $0 [--self-test]" >&2
        exit 2
        ;;
esac
