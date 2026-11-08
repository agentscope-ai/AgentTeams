#!/usr/bin/env bash
# Contract: the embedded Controller must advertise its own in-network URL so
# the Manager and Workers it provisions inherit a reachable address. The image
# ENV default (http://127.0.0.1:8090) only resolves inside the Controller
# container itself, and the worker side fails closed when it cannot reach the
# conditional task-meta endpoint.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
install_sh="$root/install/agentteams-install.sh"
helm_ctrl="$root/helm/agentteams/templates/controller/deployment.yaml"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

[[ -f "$install_sh" ]] || fail "missing $install_sh"
[[ -f "$helm_ctrl" ]] || fail "missing $helm_ctrl"

expected='CONTROLLER_URL=http://agentteams-controller:8090'
count="$(grep -c -- "$expected" "$install_sh" || true)"
[[ "${count:-0}" -ge 2 ]] || fail "installer must set $expected for the Manager and the Controller (found ${count:-0})"

if grep -q 'CONTROLLER_URL=http://127.0.0.1:8090' "$install_sh"; then
  fail "installer must never hand a loopback controller URL to provisioned containers"
fi

grep -q 'AGENTTEAMS_CONTROLLER_URL' "$helm_ctrl" || fail "helm controller deployment must set AGENTTEAMS_CONTROLLER_URL"
grep -q 'agentteams.controller.internalURL' "$helm_ctrl" || fail "helm controller deployment must use the internal service URL helper"

echo "ok: installer controller url"
