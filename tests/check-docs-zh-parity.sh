#!/usr/bin/env bash
# Fail when docs/usage/<name>.md is present but docs/zh-cn/usage/<name>.md is not.
# A drift here means an English-only doc landed without its Chinese counterpart;
# the #1354 translation automation only covers new docs, so legacy drift
# surfaces via this guard.
#
# Skips index / topic pages that are intentionally English-only (README,
# CHANGELOG, design notes that live under docs/design/).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

EN_DIR="docs/usage"
ZH_DIR="docs/zh-cn/usage"

if [ ! -d "${EN_DIR}" ]; then
    echo "no ${EN_DIR} directory; nothing to check"
    exit 0
fi

missing=()
shopt -s nullglob
for en in "${EN_DIR}"/*.md; do
    base="$(basename "${en}")"
    zh="${ZH_DIR}/${base}"
    if [ ! -f "${zh}" ]; then
        missing+=("${base}")
    fi
done

if [ "${#missing[@]}" -gt 0 ]; then
    echo "ERROR: English-only docs in ${EN_DIR} missing their zh-cn counterpart:" >&2
    for m in "${missing[@]}"; do
        echo "  - ${EN_DIR}/${m}  (no ${ZH_DIR}/${m})" >&2
    done
    echo >&2
    echo "Translate the doc or, if the file is intentionally English-only," >&2
    echo "rename it (e.g. add a -en suffix) and re-run this check." >&2
    exit 1
fi

echo "ok: ${EN_DIR} and ${ZH_DIR} are in sync ($(ls "${EN_DIR}"/*.md | wc -l) files)"
