#!/usr/bin/env bash
# Contract (#1501): presigned download URLs carry the public endpoint host to
# the caller's browser, so the installers must never invent an unreachable
# origin. The embedded Controller publishes only 8080/8001/8088 — the
# in-container 127.0.0.1:9000 is not a browser endpoint — therefore presign is
# disabled by default and the operator must supply a reachable origin.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash_installer="$root/install/agentteams-install.sh"
ps_installer="$root/install/agentteams-install.ps1"
en_docs="$root/docs/usage/resource-management.md"
zh_docs="$root/docs/zh-cn/usage/resource-management.md"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

for f in "$bash_installer" "$ps_installer" "$en_docs" "$zh_docs"; do
  [[ -f "$f" ]] || fail "missing $f"
done

# 1. No loopback public endpoint may be baked into either installer.
if grep -q 'FS_PUBLIC_ENDPOINT=http://127.0.0.1:9000' "$bash_installer" "$ps_installer"; then
  fail "installer hardcodes an unreachable loopback FS_PUBLIC_ENDPOINT"
fi

# 2. Both installers propagate an operator-supplied origin.
grep -q 'AGENTTEAMS_FS_PUBLIC_ENDPOINT=' "$bash_installer" ||
  fail "bash installer does not pass AGENTTEAMS_FS_PUBLIC_ENDPOINT through"
grep -q 'FS_PUBLIC_ENDPOINT=${AGENTTEAMS_FS_PUBLIC_ENDPOINT}' "$bash_installer" ||
  fail "bash installer does not forward the operator value into the controller env"
grep -q 'AGENTTEAMS_FS_PUBLIC_ENDPOINT' "$ps_installer" ||
  fail "PowerShell installer does not read AGENTTEAMS_FS_PUBLIC_ENDPOINT"

# 3. Presign must stay conditional (no unconditional -e either).
if grep -qE '^\s*-e .*FS_PUBLIC_ENDPOINT' "$bash_installer"; then
  fail "bash installer still sets FS_PUBLIC_ENDPOINT unconditionally"
fi

# 4. Docs must not claim the embedded install publishes the storage port.
if grep -q 'publishes MinIO on the host at `127.0.0.1:9000`' "$en_docs"; then
  fail "EN docs still claim the embedded install publishes 127.0.0.1:9000"
fi
if grep -q '将 MinIO 发布到宿主机的 `127.0.0.1:9000`' "$zh_docs"; then
  fail "ZH docs still claim the embedded install publishes 127.0.0.1:9000"
fi
grep -q 'disabled by default' "$en_docs" ||
  fail "EN docs must state that presigning is disabled by default in embedded installs"
grep -q '默认\*\*关闭\*\*' "$zh_docs" ||
  fail "ZH docs must state that presigning is disabled by default in embedded installs"

echo "ok: installer presign endpoint contract"
