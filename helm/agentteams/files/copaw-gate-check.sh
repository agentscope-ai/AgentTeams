#!/bin/sh
# Legacy CoPaw migration gate — Helm pre-upgrade hook (issue #1310 / PR #1335).
#
# Runs in-cluster as a Job in the controller image (which bundles kubectl and
# POSIX sh). Compares the AGENTTEAMS_COPAW_WORKER_IMAGE the running controller
# deployment(s) currently resolve against the image this upgrade will render
# (GATE_NEW_COPAW_IMAGE, injected by the chart at render time).
#
# Verdicts:
#   no live legacy image                -> exit 0 (safe to upgrade)
#   live image kept verbatim            -> exit 0
#   live image deliberately changed     -> exit 0 (WARN: explicit pin)
#   live image dropped by the upgrade   -> exit 1 (aborts the upgrade),
#                                          unless GATE_FORCE=true
#   any kubectl read failure (RBAC/API) -> exit 2 (aborts the upgrade): an
#                                          unreadable cluster must never pass
#
# A kubectl non-zero exit code is a READ FAILURE, not an empty result:
# treating it as "no deployment" would let a Forbidden/API-down cluster pass
# the gate (fail-open). Every lookup therefore distinguishes rc from output.
#
# Environment:
#   GATE_NAMESPACE     release namespace (downward API on the hook pod)
#   GATE_NEW_COPAW_IMAGE  image the upgrade renders ("" when values do not pin)
#   GATE_FORCE          "true" explicitly acks dropping the legacy image

set -u

NS="${GATE_NAMESPACE:-default}"
NEW_IMAGE="${GATE_NEW_COPAW_IMAGE:-}"
FORCE="${GATE_FORCE:-false}"

ERR_FILE="$(mktemp)"
trap 'rm -f "${ERR_FILE}"' EXIT

DEPLOYS="$(kubectl -n "${NS}" get deployment -l 'app.kubernetes.io/component=controller' \
    -o jsonpath='{.items[*].metadata.name}' 2>"${ERR_FILE}")" || {
    echo "FAIL-CLOSED: cannot read deployments in namespace '${NS}':" >&2
    head -c 400 "${ERR_FILE}" >&2
    echo "" >&2
    echo "The migration gate must not pass on an unreadable cluster. Fix cluster access (RBAC/API) and retry the upgrade." >&2
    exit 2
}

if [ -z "${DEPLOYS}" ]; then
    echo "OK: no agentteams controller deployment in namespace '${NS}' — no legacy CoPaw image to preserve"
    exit 0
fi

OLD_IMAGES=""
for DEPLOY in ${DEPLOYS}; do
    LIVE="$(kubectl -n "${NS}" get deployment "${DEPLOY}" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="controller")].env[?(@.name=="AGENTTEAMS_COPAW_WORKER_IMAGE")].value}' \
        2>"${ERR_FILE}")" || {
        echo "FAIL-CLOSED: cannot read the controller env of deployment '${DEPLOY}':" >&2
        head -c 400 "${ERR_FILE}" >&2
        echo "" >&2
        echo "The migration gate must not pass on an unreadable cluster. Fix cluster access (RBAC/API) and retry the upgrade." >&2
        exit 2
    }
    if [ -n "${LIVE}" ]; then
        OLD_IMAGES="${OLD_IMAGES} ${DEPLOY}=${LIVE}"
    fi
done

if [ -z "${OLD_IMAGES# }" ]; then
    echo "OK: no live controller deployment carries AGENTTEAMS_COPAW_WORKER_IMAGE — safe to upgrade"
    exit 0
fi

echo "live controller deployment(s) resolve legacy CoPaw image:${OLD_IMAGES}"

if [ -z "${NEW_IMAGE}" ]; then
    if [ "${FORCE}" = "true" ]; then
        echo "WARN: GATE_FORCE=true (copawGate.force) — allowing the upgrade to drop the legacy CoPaw image"
        exit 0
    fi
    {
        echo "FAIL: this upgrade drops AGENTTEAMS_COPAW_WORKER_IMAGE, but the running deployment resolves:${OLD_IMAGES}"
        echo "Empty-spec.image CoPaw workers would be recreated on a built-in image this release no longer builds."
        echo "Pin worker.defaultImage.copaw.repository/.tag to the live value, migrate the CoPaw workers to QwenPaw first,"
        echo "or set copawGate.force=true to explicitly ack the drop."
    } >&2
    exit 1
fi

CHANGED=""
for PAIR in ${OLD_IMAGES}; do
    DEPLOY="${PAIR%%=*}"
    LIVE="${PAIR#*=}"
    if [ "${LIVE}" != "${NEW_IMAGE}" ]; then
        CHANGED="${CHANGED} ${DEPLOY}: ${LIVE} -> ${NEW_IMAGE}"
    fi
done

if [ -n "${CHANGED# }" ]; then
    echo "WARN: upgrade changes the legacy CoPaw image (explicit pin, proceeding):${CHANGED}"
fi
echo "OK: legacy CoPaw image preserved by the upgrade"
exit 0
