#!/usr/bin/env bash
# copaw-helm-upgrade-gate.sh - migration gate for legacy CoPaw
# deployments, run BEFORE `helm upgrade` (issue #1310 / PR #1335).
#
# Previous chart versions resolved the copaw worker image from chart
# defaults (higress-registry.../agentteams/agentteams-copaw-worker + the
# release's global image tag), so those deployments carry no explicit pin
# in their values. This chart's default worker.defaultImage.copaw.repository
# is empty: a plain `helm upgrade` drops AGENTTEAMS_COPAW_WORKER_IMAGE and
# empty-spec.image CoPaw workers would be recreated on a built-in image
# this release no longer builds. The in-chart pre-upgrade hook
# (helm/agentteams/templates/hook/legacy-copaw-upgrade-gate.yaml) enforces
# the same contract on the upgrade path itself; this script is the
# standalone pre-check for operators.
#
# Usage (on a machine with kubectl + helm, before `helm upgrade`):
#   bash tests/copaw-helm-upgrade-gate.sh --namespace <ns> \
#       --values <upgrade-values.yaml>
#
# The gate reads the image the live controller deployment currently
# resolves and compares it against the render of this chart with the
# upgrade values:
#   - verbatim in the render        -> OK (resolved image incl. tag kept)
#   - explicitly changed in values  -> WARN (deliberate override, exit 0)
#   - dropped by the render         -> FAIL (prints the exact pin to add)
#
# Fail-closed: a kubectl read failure (RBAC Forbidden, API down) is a
# non-zero exit code, NOT an empty result — the gate exits 2 instead of
# reporting "no deployment".
#
# Exit codes: 0 = safe to upgrade, 1 = migration required, 2 = usage or
# environment error (render failure of the upgrade values, or a failed
# kubectl read).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${ROOT_DIR}/helm/agentteams"

usage() {
    cat <<'EOF'
Usage: copaw-helm-upgrade-gate.sh --namespace <ns> [--values <upgrade-values.yaml>]

Checks that upgrading a legacy CoPaw deployment with this chart keeps the
resolved AGENTTEAMS_COPAW_WORKER_IMAGE (or deliberately replaces it).
Without --values, any live legacy image is a FAIL: the upgrade would drop
the env. Prints the exact values pin (repository + tag) to add.

Exit codes: 0 = safe to upgrade, 1 = migration required, 2 = usage or
environment error (render failure, or a failed kubectl read — the gate
fails closed and never passes on an unreadable cluster).
EOF
}

NS=""
VALUES=""
while [ $# -gt 0 ]; do
    case "$1" in
        --namespace)
            [ $# -ge 2 ] || { echo "ERROR: --namespace requires a value" >&2; exit 2; }
            NS="$2"; shift 2 ;;
        --values)
            [ $# -ge 2 ] || { echo "ERROR: --values requires a value" >&2; exit 2; }
            VALUES="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

[ -n "${NS}" ] || { echo "ERROR: --namespace is required" >&2; exit 2; }
if [ -n "${VALUES}" ] && [ ! -f "${VALUES}" ]; then
    echo "ERROR: values file not found: ${VALUES}" >&2
    exit 2
fi

command -v kubectl >/dev/null 2>&1 || { echo "ERROR: kubectl not found in PATH" >&2; exit 2; }
command -v helm >/dev/null 2>&1 || { echo "ERROR: helm not found in PATH" >&2; exit 2; }

kubectl_err="$(mktemp)"

# Component label keeps the lookup release-name agnostic. A non-zero kubectl
# rc (Forbidden, API down, ...) is a read failure, not an empty result —
# suppressing it with `|| true` would let a Forbidden cluster pass the gate.
if ! DEPLOY="$(kubectl -n "${NS}" get deployment -l 'app.kubernetes.io/component=controller' \
    -o jsonpath='{.items[0].metadata.name}' 2>"${kubectl_err}")"; then
    echo "ERROR: kubectl failed to read deployments in namespace '${NS}':" >&2
    head -c 400 "${kubectl_err}" >&2
    echo "" >&2
    echo "The gate cannot distinguish 'no deployment' from 'cannot read the cluster'; failing closed." >&2
    exit 2
fi
if [ -z "${DEPLOY}" ]; then
    echo "OK: no agentteams controller deployment in namespace '${NS}' — no legacy CoPaw image to preserve"
    exit 0
fi
echo "controller deployment: ${DEPLOY}"

if ! LIVE_IMAGE="$(kubectl -n "${NS}" get deployment "${DEPLOY}" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="controller")].env[?(@.name=="AGENTTEAMS_COPAW_WORKER_IMAGE")].value}' \
    2>"${kubectl_err}")"; then
    echo "ERROR: kubectl failed to read the controller env of deployment '${DEPLOY}':" >&2
    head -c 400 "${kubectl_err}" >&2
    echo "" >&2
    echo "The gate cannot distinguish 'no env' from 'cannot read the cluster'; failing closed." >&2
    exit 2
fi
LIVE_IMAGE="${LIVE_IMAGE//$'\r'/}"

if [ -z "${LIVE_IMAGE}" ]; then
    echo "OK: '${DEPLOY}' does not carry AGENTTEAMS_COPAW_WORKER_IMAGE — no legacy CoPaw image to preserve"
    exit 0
fi
echo "live deployment resolves legacy CoPaw image: ${LIVE_IMAGE}"

# Print the values pin that carries LIVE_IMAGE forward (tag = text after the
# last colon, only when that colon follows the last slash; port-safe).
print_pin_hint() {
    local img="$1" base repo tag
    base="${img##*/}"
    if [ "${base}" != "${base#*:}" ]; then
        tag="${base#*:}"
        repo="${img%:*}"
    else
        tag="latest"
        repo="${img}"
    fi
    cat <<EOF
worker:
  defaultImage:
    copaw:
      repository: "${repo}"
      tag: "${tag}"
EOF
}

if [ -z "${VALUES}" ]; then
    echo ""
    echo "FAIL: no --values given, so the upgrade cannot preserve the resolved legacy image."
    echo "Pass the values file used for 'helm upgrade', and if it does not pin"
    echo "worker.defaultImage.copaw.*, add:"
    echo ""
    print_pin_hint "${LIVE_IMAGE}"
    exit 1
fi

render="$(mktemp)"
render_err="$(mktemp)"
trap 'rm -f "${render}" "${render_err}" "${kubectl_err}"' EXIT

# Credential stubs only make the render succeed; they never touch
# worker.defaultImage.* (helm --set precedence).
if ! helm template agentteams "${CHART}" \
    --namespace "${NS}" \
    --set credentials.registrationToken=gate-check \
    --set credentials.adminPassword=gate-check \
    --set credentials.llmApiKey=gate-check \
    --set gateway.publicURL=http://localhost:18080 \
    -f "${VALUES}" \
    > "${render}" 2> "${render_err}"; then
    echo "FAIL: the upgrade values do not render against this chart — 'helm upgrade' would fail the same way:"
    tail -n 5 "${render_err}" >&2 || true
    exit 2
fi

NEW_IMAGE=""
if grep -q 'name: AGENTTEAMS_COPAW_WORKER_IMAGE' "${render}"; then
    NEW_IMAGE="$(sed -n '/name: AGENTTEAMS_COPAW_WORKER_IMAGE/{n; s/^ *value: "\(.*\)"$/\1/p;}' "${render}")"
fi

if [ -n "${NEW_IMAGE}" ] && [ "${NEW_IMAGE}" = "${LIVE_IMAGE}" ]; then
    echo "OK: upgrade values preserve the resolved legacy CoPaw image (${NEW_IMAGE}) — safe to upgrade"
    exit 0
fi

if [ -n "${NEW_IMAGE}" ]; then
    echo "WARN: upgrade values change the legacy CoPaw image:"
    echo "       live:      ${LIVE_IMAGE}"
    echo "       upgraded:  ${NEW_IMAGE}"
    echo "Empty-spec.image CoPaw workers will be recreated on the new image. Proceed only if that is intended."
    exit 0
fi

echo ""
echo "FAIL: this upgrade drops AGENTTEAMS_COPAW_WORKER_IMAGE, but the running deployment resolves:"
echo "       ${LIVE_IMAGE}"
echo ""
echo "Empty-spec.image CoPaw workers would be recreated on a built-in image this"
echo "release no longer builds. Before 'helm upgrade', add this pin to your"
echo "upgrade values (or complete the QwenPaw migration of those workers first):"
echo ""
print_pin_hint "${LIVE_IMAGE}"
exit 1
