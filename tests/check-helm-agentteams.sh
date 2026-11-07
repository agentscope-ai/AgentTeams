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

# ---------------------------------------------------------------------------
# Managed RustFS storage engine (storage.engine=rustfs, chart-hosted)
# ---------------------------------------------------------------------------
# engine=rustfs requires provider=s3: the chart deploys a single-node
# RustFS StatefulSet (image pinned with the release, same policy as MinIO)
# and wires the controller to it — endpoint derived from the in-chart
# StatefulSet, credentials via secretKeyRef on the RustFS secret.
# engine=external (default) must keep rendering exactly as before.
# ---------------------------------------------------------------------------

RUSTFS_ARGS=(
    "${COMMON_ARGS[@]}"
    --set storage.provider=s3
    --set storage.engine=rustfs
)
rustfs_render="$(mktemp)"
rustfs_ext_render="$(mktemp)"
trap 'rm -f "${render}" "${copaw_render}" "${upg_render}" "${drift_err}" "${s3_render}" "${rustfs_render}" "${rustfs_ext_render}"' EXIT

# 1. engine=rustfs renders the RustFS three-piece set and the controller
#    env wiring; the MinIO StatefulSet must NOT render.
if ! helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}" > "${rustfs_render}"; then
    echo "FAIL: helm template (engine=rustfs) failed"
    exit 1
fi

if ! grep -q "name: agentteams-rustfs" "${rustfs_render}"; then
    echo "FAIL: managed RustFS resources missing (expected name: agentteams-rustfs)"
    exit 1
fi
# Count only RustFS-owned documents; the higress dependency may render its
# own Statefulsets/Services, so a global kind count is not meaningful.
count_rustfs_docs() {
    awk -v kind="$1" 'BEGIN{RS="---"} $0 ~ ("kind: " kind) && /-rustfs/ {n++} END{print n+0}' "${rustfs_render}"
}
for kind in StatefulSet Service Secret; do
    if [ "$(count_rustfs_docs "${kind}")" = "0" ]; then
        echo "FAIL: engine=rustfs did not render a RustFS ${kind}"
        exit 1
    fi
done
stss="$(count_rustfs_docs StatefulSet)"
if [ "${stss}" -ne 1 ]; then
    echo "FAIL: expected exactly 1 StatefulSet (RustFS) in engine=rustfs render, got: ${stss}"
    exit 1
fi
if grep -qF "agentteams-minio" "${rustfs_render}"; then
    echo "FAIL: MinIO resources must not render when storage.engine=rustfs"
    exit 1
fi
grep -q 'name: AGENTTEAMS_STORAGE_PROVIDER' "${rustfs_render}"
grep -q 'value: "s3"' "${rustfs_render}"
grep -q 'value: "http://agentteams-rustfs:9000"' "${rustfs_render}" || {
    echo "FAIL: AGENTTEAMS_FS_ENDPOINT not derived from the in-chart RustFS (want http://agentteams-rustfs:9000)"
    exit 1
}
grep -qF -- "key: RUSTFS_ACCESS_KEY" "${rustfs_render}" || {
    echo "FAIL: controller env must take RUSTFS_ACCESS_KEY via secretKeyRef"
    exit 1
}
grep -qF -- "key: RUSTFS_SECRET_KEY" "${rustfs_render}" || {
    echo "FAIL: controller env must take RUSTFS_SECRET_KEY via secretKeyRef"
    exit 1
}
grep -qF -- 'RUSTFS_ACCESS_KEY: "rustfsadmin"' "${rustfs_render}" || {
    echo "FAIL: chart-rendered RustFS secret missing the default root credentials"
    exit 1
}
echo "PASS: managed RustFS renders StatefulSet/Service/Secret + controller env wiring, no MinIO"

# 2. engine=external (default) must render no RustFS resource at all
#    (existing behaviour unchanged — checked against the default render).
if grep -qi "rustfs" "${render}"; then
    echo "FAIL: default render (engine=external) must contain no RustFS resource"
    exit 1
fi
echo "PASS: default render (engine=external) keeps rendering without RustFS"

# 3. engine=rustfs with a non-s3 provider must fail the render.
if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}"     --set storage.engine=rustfs > /dev/null 2> "${drift_err}"; then
    echo "FAIL: engine=rustfs with provider=minio must not render"
    exit 1
fi
if ! grep -qF "storage.engine=rustfs requires storage.provider=s3" "${drift_err}"; then
    echo "FAIL: engine=rustfs + provider=minio error is not explicit:"
    cat "${drift_err}" || true
    exit 1
fi
echo "PASS: engine=rustfs fails the render unless provider=s3"

# 4. engine=rustfs + existingSecret: no chart-rendered secret, and the
#    env (controller + RustFS container) references the user secret.
if ! helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}"     --set storage.rustfs.auth.existingSecret=my-rustfs-creds > "${rustfs_ext_render}"; then
    echo "FAIL: helm template (engine=rustfs, existingSecret) failed"
    exit 1
fi
grep -qF -- "name: my-rustfs-creds" "${rustfs_ext_render}" || {
    echo "FAIL: engine=rustfs deployment does not reference the user-supplied existingSecret"
    exit 1
}
grep -qF -- "key: RUSTFS_ACCESS_KEY" "${rustfs_ext_render}" || {
    echo "FAIL: existingSecret wiring must use the RUSTFS_ACCESS_KEY key"
    exit 1
}
if grep -qF -- 'RUSTFS_ACCESS_KEY: "rustfsadmin"' "${rustfs_ext_render}"; then
    echo "FAIL: chart RustFS secret must not render when existingSecret is set"
    exit 1
fi
echo "PASS: managed RustFS honours existingSecret (no chart secret, env via secretKeyRef)"

# 5. storage.rustfs.image.tag override must reach the rendered image.
if ! helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}"     --set storage.rustfs.image.tag=1.1.1 > "${rustfs_ext_render}"; then
    echo "FAIL: helm template (engine=rustfs, tag override) failed"
    exit 1
fi
grep -qF "docker.io/rustfs/rustfs:1.1.1" "${rustfs_ext_render}" || {
    echo "FAIL: storage.rustfs.image.tag override not rendered into the RustFS image"
    exit 1
}
echo "PASS: storage.rustfs.image.tag override renders the pinned image"

# 6. engine=rustfs + a non-empty storage.s3.endpoint must fail the render:
#    the managed engine deploys its own in-cluster RustFS and the endpoint
#    is derived from it — the old "endpoint still wins" override semantics
#    is gone. 00-validate fails before any manifest is emitted, so no RustFS
#    StatefulSet can render either.
rustfs_bad_render="$(mktemp)"
trap 'rm -f "${render}" "${copaw_render}" "${upg_render}" "${drift_err}" "${s3_render}" "${rustfs_render}" "${rustfs_ext_render}" "${rustfs_bad_render}"' EXIT
if helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}" \
    --set storage.s3.endpoint=http://external.example:9000 > "${rustfs_bad_render}" 2> "${drift_err}"; then
    echo "FAIL: engine=rustfs with a non-empty storage.s3.endpoint must not render"
    exit 1
fi
if ! grep -qF "storage.engine=rustfs deploys its own in-cluster RustFS; storage.s3.endpoint must be empty" "${drift_err}"; then
    echo "FAIL: engine=rustfs + s3.endpoint error is not explicit:"
    cat "${drift_err}" || true
    exit 1
fi
if ! grep -qF "storage.engine=external (the default), storage.mode=existing" "${drift_err}"; then
    echo "FAIL: engine=rustfs + s3.endpoint error does not point at the external fix:"
    cat "${drift_err}" || true
    exit 1
fi
if [ "$(awk 'BEGIN{RS="---"} /kind: StatefulSet/ && /-rustfs/ {n++} END{print n+0}' "${rustfs_bad_render}")" -ne 0 ]; then
    echo "FAIL: the failed render must not emit the RustFS StatefulSet"
    exit 1
fi
echo "PASS: engine=rustfs rejects a non-empty storage.s3.endpoint (external fix in the error, no RustFS manifests)"

# 7. engine=rustfs + storage.mode=existing must fail the render and name
#    both legal combinations in the error.
if helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}" \
    --set storage.mode=existing > /dev/null 2> "${drift_err}"; then
    echo "FAIL: engine=rustfs with storage.mode=existing must not render"
    exit 1
fi
if ! grep -qF "storage.engine=rustfs cannot be combined with storage.mode=existing" "${drift_err}"; then
    echo "FAIL: engine=rustfs + mode=existing error is not explicit:"
    cat "${drift_err}" || true
    exit 1
fi
if ! grep -qF "storage.engine=rustfs + storage.mode=managed" "${drift_err}"; then
    echo "FAIL: engine=rustfs + mode=existing error does not name the managed combination:"
    cat "${drift_err}" || true
    exit 1
fi
if ! grep -qF "storage.engine=external + storage.mode=existing" "${drift_err}"; then
    echo "FAIL: engine=rustfs + mode=existing error does not name the external combination:"
    cat "${drift_err}" || true
    exit 1
fi
echo "PASS: engine=rustfs + mode=existing fails the render naming both legal combinations"

# 8. Security context: the RustFS 1.0.4 image runs as uid/gid 10001
#    (USER=rustfs) and a default (root-owned) PVC is unwritable for it —
#    the server FATALs with "Permission denied". The StatefulSet must run
#    non-root and let kubelet apply fsGroup to the PVC. Asserted on the
#    RustFS StatefulSet document only (a whole-render grep would false-
#    positive on other components' security contexts).
rustfs_sts="$(awk 'BEGIN{RS="---"} /kind: StatefulSet/ && /-rustfs/ {print}' "${rustfs_render}")"
if [ -z "${rustfs_sts}" ]; then
    echo "FAIL: no RustFS StatefulSet document found in the engine=rustfs render"
    exit 1
fi
for needle in \
    "fsGroup: 10001" \
    "fsGroupChangePolicy: OnRootMismatch" \
    "runAsUser: 10001" \
    "runAsGroup: 10001" \
    "runAsNonRoot: true" \
    "allowPrivilegeEscalation: false"
do
    if ! grep -qF "${needle}" <<<"${rustfs_sts}"; then
        echo "FAIL: managed RustFS StatefulSet is missing ${needle}"
        exit 1
    fi
done
echo "PASS: managed RustFS StatefulSet runs non-root (uid/gid 10001) with fsGroup on the PVC"

# 9. Image repository: the default is the upstream multi-arch image
#    (docker.io/rustfs/rustfs), directly pullable; the higress mirror is
#    an override (pullable only after `make mirror-images` has run).
grep -qF "image: \"docker.io/rustfs/rustfs:1.0.4\"" "${rustfs_render}" || {
    echo "FAIL: default storage.rustfs.image.repository is not the upstream docker.io/rustfs/rustfs"
    exit 1
}
if ! helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}"     --set storage.rustfs.image.repository=higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/rustfs > "${rustfs_ext_render}"; then
    echo "FAIL: helm template (engine=rustfs, mirror repository override) failed"
    exit 1
fi
grep -qF "image: \"higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/rustfs:1.0.4\"" "${rustfs_ext_render}" || {
    echo "FAIL: higress mirror repository override not rendered into the RustFS image"
    exit 1
}
echo "PASS: default RustFS image is the upstream repository; the higress mirror override renders"

# 10. podSecurityContext must be user-overridable (fsGroup pass-through).
if ! helm template agentteams "${CHART}" "${RUSTFS_ARGS[@]}"     --set storage.rustfs.podSecurityContext.fsGroup=2000 > "${rustfs_ext_render}"; then
    echo "FAIL: helm template (engine=rustfs, fsGroup override) failed"
    exit 1
fi
rustfs_sts="$(awk 'BEGIN{RS="---"} /kind: StatefulSet/ && /-rustfs/ {print}' "${rustfs_ext_render}")"
if ! grep -qF "fsGroup: 2000" <<<"${rustfs_sts}"; then
    echo "FAIL: storage.rustfs.podSecurityContext.fsGroup override not rendered into the StatefulSet"
    exit 1
fi
if grep -qF "fsGroup: 10001" <<<"${rustfs_sts}"; then
    echo "FAIL: fsGroup override must replace the default, not coexist with it"
    exit 1
fi
echo "PASS: podSecurityContext.fsGroup override passes through to the StatefulSet"

# ---------------------------------------------------------------------------
# Central storage.engine / storage.mode enum validation (render-time gate)
# ---------------------------------------------------------------------------
# 00-validate checks engine in {external, rustfs} and mode in {managed,
# existing} before any combination logic, so an unknown value fails the
# render instead of falling through a branch (any non-rustfs engine used
# to be silently treated as external). Unknown values must fail before
# any manifest is emitted.

# 11. Unknown mode (the maintainer's example): must fail the render and
#     emit no RustFS resource.
rustfs_enum_render="$(mktemp)"
trap 'rm -f "${render}" "${copaw_render}" "${upg_render}" "${drift_err}" "${s3_render}" "${rustfs_render}" "${rustfs_ext_render}" "${rustfs_bad_render}" "${rustfs_enum_render}"' EXIT
if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set storage.provider=s3 \
    --set storage.engine=rustfs \
    --set storage.mode=invalid-review-mode > "${rustfs_enum_render}" 2> "${drift_err}"; then
    echo "FAIL: unknown storage.mode must not render"
    exit 1
fi
if ! grep -qF 'unsupported storage.mode "invalid-review-mode"' "${drift_err}"; then
    echo "FAIL: unknown storage.mode error is not explicit:"
    cat "${drift_err}" || true
    exit 1
fi
if ! grep -qF '"managed" or "existing"' "${drift_err}"; then
    echo "FAIL: unknown storage.mode error does not list the legal values:"
    cat "${drift_err}" || true
    exit 1
fi
for kind in StatefulSet Service Secret; do
    if [ "$(awk -v kind="${kind}" 'BEGIN{RS="---"} $0 ~ ("kind: " kind) && /-rustfs/ {n++} END{print n+0}' "${rustfs_enum_render}")" -ne 0 ]; then
        echo "FAIL: failed render (unknown storage.mode) must not emit a RustFS ${kind}"
        exit 1
    fi
done
echo "PASS: unknown storage.mode fails the render before any RustFS resource is emitted"

# 12. Unknown engine: must fail the render even when the rest of the
#     s3 + existing combination is fully valid (endpoint + credentials).
if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set storage.provider=s3 \
    --set storage.engine=bogus \
    --set storage.mode=existing \
    --set storage.s3.endpoint=http://x:9000 \
    --set storage.s3.accessKey=a --set storage.s3.secretKey=b > "${rustfs_enum_render}" 2> "${drift_err}"; then
    echo "FAIL: unknown storage.engine must not render"
    exit 1
fi
if ! grep -qF 'unsupported storage.engine "bogus"' "${drift_err}"; then
    echo "FAIL: unknown storage.engine error is not explicit:"
    cat "${drift_err}" || true
    exit 1
fi
if ! grep -qF '"external" or "rustfs"' "${drift_err}"; then
    echo "FAIL: unknown storage.engine error does not list the legal values:"
    cat "${drift_err}" || true
    exit 1
fi
echo "PASS: unknown storage.engine fails the render before any combination logic runs"

# 13. The documented managed combination (engine=rustfs + mode=managed)
#     must still render the RustFS three-piece set with fsGroup 10001.
if ! helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set storage.provider=s3 \
    --set storage.engine=rustfs \
    --set storage.mode=managed > "${rustfs_enum_render}" 2> "${drift_err}"; then
    echo "FAIL: helm template (engine=rustfs + explicit mode=managed) failed:"
    cat "${drift_err}" || true
    exit 1
fi
for kind in StatefulSet Service Secret; do
    if [ "$(awk -v kind="${kind}" 'BEGIN{RS="---"} $0 ~ ("kind: " kind) && /-rustfs/ {n++} END{print n+0}' "${rustfs_enum_render}")" = "0" ]; then
        echo "FAIL: engine=rustfs + mode=managed did not render a RustFS ${kind}"
        exit 1
    fi
done
rustfs_enum_sts="$(awk 'BEGIN{RS="---"} /kind: StatefulSet/ && /-rustfs/ {print}' "${rustfs_enum_render}")"
if ! grep -qF "fsGroup: 10001" <<<"${rustfs_enum_sts}"; then
    echo "FAIL: engine=rustfs + mode=managed StatefulSet is missing fsGroup: 10001"
    exit 1
fi
echo "PASS: engine=rustfs + mode=managed still renders the RustFS three-piece set (fsGroup 10001)"
