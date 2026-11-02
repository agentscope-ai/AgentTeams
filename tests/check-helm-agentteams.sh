#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${ROOT_DIR}/helm/agentteams"
COMMON_ARGS=(
    --set credentials.registrationToken=test
    --set credentials.adminPassword=test
    --set credentials.llmApiKey=test
    --set gateway.publicURL=http://localhost:18080
)

render="$(mktemp)"
copaw_render="$(mktemp)"
upg_render="$(mktemp)"
drift_err="$(mktemp)"
trap 'rm -f "${render}" "${copaw_render}" "${upg_render}" "${drift_err}"' EXIT

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" > "${render}"

grep -q 'name: agentteams-controller' "${render}"
grep -q 'app.kubernetes.io/name: agentteams' "${render}"

echo "PASS: AgentTeams Helm release renders canonical resource names"

# ---------------------------------------------------------------------------
# Legacy CoPaw worker image (issue #1310 / PR #1335)
# ---------------------------------------------------------------------------
# New releases must NOT inject AGENTTEAMS_COPAW_WORKER_IMAGE (the CoPaw
# worker image is gone from fresh installs). Existing CoPaw deployments
# keep their image only by explicitly pinning worker.defaultImage.copaw.*
# in values, in which case the pinned repository:tag must be rendered into
# the controller env so workers with an empty spec.image keep pulling it.
# ---------------------------------------------------------------------------

# The env-entry form only: the pre-upgrade gate hook's ConfigMap legitimately
# embeds the env var name inside its shell script, so a bare string match
# would false-positive on the default render.
if grep -qF -- "- name: AGENTTEAMS_COPAW_WORKER_IMAGE" "${render}"; then
    echo "FAIL: default values must not inject AGENTTEAMS_COPAW_WORKER_IMAGE"
    grep -n "AGENTTEAMS_COPAW_WORKER_IMAGE" "${render}" || true
    exit 1
fi

if ! helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set worker.defaultImage.copaw.repository=private.registry.example/agentteams-copaw-worker \
    --set worker.defaultImage.copaw.tag=v1.2.3 > "${copaw_render}"; then
    echo "FAIL: helm template (copaw image values) failed:"
    cat "${copaw_render}" || true
    exit 1
fi

if ! grep -q "name: AGENTTEAMS_COPAW_WORKER_IMAGE" "${copaw_render}"; then
    echo "FAIL: explicitly pinned copaw image is not injected as AGENTTEAMS_COPAW_WORKER_IMAGE"
    grep -n "AGENTTEAMS_COPAW_WORKER_IMAGE" "${copaw_render}" || true
    exit 1
fi

if ! grep -q "value: \"private.registry.example/agentteams-copaw-worker:v1.2.3\"" "${copaw_render}"; then
    echo "FAIL: AGENTTEAMS_COPAW_WORKER_IMAGE value not rendered from pinned repository:tag"
    grep -n "AGENTTEAMS_COPAW_WORKER_IMAGE" "${copaw_render}" || true
    exit 1
fi

echo "PASS: AgentTeams Helm release keeps legacy CoPaw worker image opt-in (no default injection)"

# ---------------------------------------------------------------------------
# Pre-upgrade migration gate hook (in-chart enforcement of the same contract)
# ---------------------------------------------------------------------------
# The supported upgrade path is a plain `helm upgrade` of the published
# chart, so the gate must live in the chart: a pre-upgrade hook Job in the
# controller image (which bundles kubectl + POSIX sh) compares the live
# AGENTTEAMS_COPAW_WORKER_IMAGE against the image the upgrade renders
# (GATE_NEW_COPAW_IMAGE) and fails the upgrade when it would be dropped or
# when the cluster cannot be read. The hook's decision logic is covered by
# tests/check-copaw-helm-upgrade-gate.sh (stub-driven failure modes).
# ---------------------------------------------------------------------------

for needle in \
    '"helm.sh/hook": pre-upgrade' \
    'kind: Job' \
    'kind: ServiceAccount' \
    'kind: Role' \
    'kind: RoleBinding' \
    'kind: ConfigMap' \
    'app.kubernetes.io/component: copaw-gate' \
    'GATE_NEW_COPAW_IMAGE'
do
    if ! grep -qF "${needle}" "${render}"; then
        echo "FAIL: pre-upgrade CoPaw gate hook is incomplete in the default render (missing): ${needle}"
        exit 1
    fi
done

# The hook may only list/get deployments (least privilege for the gate).
role_block="$(awk '/^kind: Role$/,/^---/' "${render}")"
if ! grep -qF -- "- deployments" <<<"${role_block}"; then
    echo "FAIL: pre-upgrade CoPaw gate Role does not grant deployments access"
    echo "${role_block}" || true
    exit 1
fi

# Default values: the hook must see an empty incoming image (nothing pinned).
gate_new="$(sed -n '/name: GATE_NEW_COPAW_IMAGE/{n; s/^ *value: "\(.*\)"$/\1/p;}' "${render}")"
if [ -n "${gate_new}" ]; then
    echo "FAIL: default values must render an empty GATE_NEW_COPAW_IMAGE for the pre-upgrade gate, got: ${gate_new}"
    exit 1
fi

# Pinned values: the hook must see exactly the pinned image.
gate_new="$(sed -n '/name: GATE_NEW_COPAW_IMAGE/{n; s/^ *value: "\(.*\)"$/\1/p;}' "${copaw_render}")"
if [ "${gate_new}" != "private.registry.example/agentteams-copaw-worker:v1.2.3" ]; then
    echo "FAIL: pinned values must render the pinned image into GATE_NEW_COPAW_IMAGE, got: ${gate_new}"
    exit 1
fi

echo "PASS: pre-upgrade CoPaw gate hook renders with the correct env wiring"

# ---------------------------------------------------------------------------
# Legacy CoPaw upgrade path (old-chart install -> new-chart upgrade)
# ---------------------------------------------------------------------------
# Previous chart versions resolved the copaw worker image from chart
# defaults (higress-registry.../agentteams-copaw-worker + the release's
# global image tag), so legacy deployments carry no explicit pin in their
# values. This chart's default repository is empty, so a plain upgrade drops
# the env and empty-spec.image workers would fall back to a built-in image
# this release no longer builds. The upgrade must pin the *resolved* old
# image, tag included (tests/copaw-helm-upgrade-gate.sh pre-checks live
# clusters before `helm upgrade`; the pre-upgrade hook above enforces the
# same contract on the upgrade path itself).
# ---------------------------------------------------------------------------
LEGACY_COPAW_IMAGE="higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4"
LEGACY_COPAW_TAG="${LEGACY_COPAW_IMAGE##*:}"
LEGACY_COPAW_REPO="${LEGACY_COPAW_IMAGE%:*}"

if ! helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set worker.defaultImage.copaw.repository="${LEGACY_COPAW_REPO}" \
    --set worker.defaultImage.copaw.tag="${LEGACY_COPAW_TAG}" > "${upg_render}"; then
    echo "FAIL: helm template (resolved legacy image pin) failed:"
    cat "${upg_render}" || true
    exit 1
fi

if ! grep -q "name: AGENTTEAMS_COPAW_WORKER_IMAGE" "${upg_render}"; then
    echo "FAIL: upgrade pinning the resolved legacy image does not inject AGENTTEAMS_COPAW_WORKER_IMAGE"
    exit 1
fi

if ! grep -q "value: \"${LEGACY_COPAW_IMAGE}\"" "${upg_render}"; then
    echo "FAIL: upgrade must preserve the resolved legacy image verbatim (tag included):"
    grep -n "AGENTTEAMS_COPAW_WORKER_IMAGE" "${upg_render}" || true
    exit 1
fi

echo "PASS: old-chart resolved CoPaw image (tag included) survives upgrade when pinned"

# A partial pin (repository only) must fail the render: the global image
# tag no longer applies to CoPaw, otherwise an upgrade with a changed
# global tag would select a CoPaw image this release no longer builds.
if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set worker.defaultImage.copaw.repository="${LEGACY_COPAW_REPO}" \
    --set global.imageTag=v9.9.9 > /dev/null 2> "${drift_err}"; then
    echo "FAIL: repository-only copaw pin must not render (global image tag must not select a CoPaw image)"
    exit 1
fi

if ! grep -Fq "worker.defaultImage.copaw.tag is required" "${drift_err}"; then
    echo "FAIL: repository-only copaw pin did not fail with the explicit-tag requirement:"
    cat "${drift_err}" || true
    exit 1
fi

echo "PASS: repository-only CoPaw pin fails the render (no global-tag image drift)"

# ---------------------------------------------------------------------------
# External S3-compatible storage backend (storage.provider=s3, e.g. RustFS)
# ---------------------------------------------------------------------------
# provider=s3 + mode=existing: fixed root credentials, MinIO-compatible
# admin API — the controller keeps managing per-worker users/policies via
# mc. The MinIO StatefulSet must NOT render, and no credential-provider
# sidecar is required (fixed credentials, not STS).
# ---------------------------------------------------------------------------

S3_ARGS=(
    "${COMMON_ARGS[@]}"
    --set storage.provider=s3
    --set storage.mode=existing
    --set storage.s3.endpoint=http://rustfs:9000
)
s3_render="$(mktemp)"
trap 'rm -f "${render}" "${copaw_render}" "${upg_render}" "${drift_err}" "${s3_render}"' EXIT

if ! helm template agentteams "${CHART}" "${S3_ARGS[@]}"     --set storage.s3.accessKey=rustfsadmin     --set storage.s3.secretKey=rustfsadmin > "${s3_render}"; then
    echo "FAIL: helm template (s3 provider, inline credentials) failed"
    cat "${s3_render}" || true
    exit 1
fi

grep -q 'name: AGENTTEAMS_STORAGE_PROVIDER' "${s3_render}"
grep -q 'value: "s3"' "${s3_render}"
grep -q 'value: "http://rustfs:9000"' "${s3_render}" || {
    echo "FAIL: s3 endpoint not rendered into AGENTTEAMS_FS_ENDPOINT"; exit 1;
}
grep -qF -- "name: agentteams-s3" "${s3_render}" || {
    echo "FAIL: inline s3 credentials did not render the chart secret agentteams-s3"; exit 1;
}
grep -qF -- "accesskey: \"rustfsadmin\"" "${s3_render}" || {
    echo "FAIL: s3 access key missing from rendered secret"; exit 1;
}
if grep -qE '^kind: StatefulSet' -A3 "${s3_render}" | grep -q minio; then
    echo "FAIL: MinIO StatefulSet must not render when storage.provider=s3"
    exit 1
fi
if grep -qF "agentteams-credential-provider" "${s3_render}"; then
    echo "FAIL: s3 provider must not deploy the credential-provider sidecar (fixed credentials)"
    exit 1
fi
echo "PASS: external S3 backend (inline credentials) renders endpoint + secret, no MinIO StatefulSet, no sidecar"

# existingSecret: no chart-rendered secret, deployment references the user secret.
if ! helm template agentteams "${CHART}" "${S3_ARGS[@]}"     --set storage.s3.existingSecret=my-rustfs-creds     --set storage.s3.accessKeySecretKey=ak     --set storage.s3.secretKeySecretKey=sk > "${s3_render}"; then
    echo "FAIL: helm template (s3 provider, existingSecret) failed"
    exit 1
fi
grep -qF -- "name: my-rustfs-creds" "${s3_render}" || {
    echo "FAIL: s3 deployment does not reference the user-supplied existingSecret"; exit 1;
}
grep -qF -- "key: ak" "${s3_render}" || {
    echo "FAIL: custom access key secret key not honoured"; exit 1;
}
if grep -qF -- "name: agentteams-s3" "${s3_render}"; then
    echo "FAIL: chart s3 secret must not render when existingSecret is set"
    exit 1
fi
echo "PASS: external S3 backend honours existingSecret + custom key names"

# Failure modes: missing endpoint / missing credentials / managed mode.
if helm template agentteams "${CHART}" "${S3_ARGS[@]}"     --set storage.s3.endpoint= 2> "${drift_err}"     --set storage.s3.accessKey=a --set storage.s3.secretKey=b > /dev/null; then
    echo "FAIL: s3 provider without endpoint must not render"; exit 1
fi
grep -qF "storage.s3.endpoint is required" "${drift_err}" || {
    echo "FAIL: s3 missing-endpoint error is not explicit:"; cat "${drift_err}" || true; exit 1;
}

if helm template agentteams "${CHART}" "${S3_ARGS[@]}" > /dev/null 2> "${drift_err}"; then
    echo "FAIL: s3 provider without credentials must not render"; exit 1
fi
grep -qF "storage.s3.existingSecret or storage.s3.accessKey" "${drift_err}" || {
    echo "FAIL: s3 missing-credentials error is not explicit:"; cat "${drift_err}" || true; exit 1;
}

if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}"     --set storage.provider=s3 --set storage.mode=managed     --set storage.s3.endpoint=http://rustfs:9000     --set storage.s3.accessKey=a --set storage.s3.secretKey=b > /dev/null 2> "${drift_err}"; then
    echo "FAIL: s3 provider with mode=managed must not render"; exit 1
fi
# The explicit failure can surface either from 00-validate (preferred) or
# from the storage endpoint helper (render order), both are valid guards.
if ! grep -qE "storage.provider=s3 requires storage.mode=existing|unsupported storage combination s3/managed" "${drift_err}"; then
    echo "FAIL: s3 mode=managed error is not explicit:"; cat "${drift_err}" || true; exit 1;
fi

echo "PASS: s3 provider failure modes fail the render with explicit errors"
