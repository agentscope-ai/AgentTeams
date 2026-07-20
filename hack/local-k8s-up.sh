#!/bin/bash
# local-k8s-up.sh — Create a kind cluster and deploy HiClaw via Helm.
#
# Prerequisites:
#   - kind: https://kind.sigs.k8s.io/
#   - helm: https://helm.sh/
#   - kubectl
#
# Required environment variables:
#   HICLAW_LLM_API_KEY          LLM API key
#
# Optional environment variables:
#   HICLAW_REGISTRATION_TOKEN   Matrix registration token (auto-generated if empty)
#   HICLAW_ADMIN_PASSWORD       Admin password (auto-generated if empty)
#   HICLAW_CLUSTER_NAME         kind cluster name (default: hiclaw)
#   HICLAW_NAMESPACE            K8s namespace (default: hiclaw)
#   HICLAW_SKIP_KIND            Skip kind cluster creation (default: 0)
#   HICLAW_SKIP_BUILD           Skip local image build (default: 0, set to 1 to use remote images)
#   HICLAW_BUILD_K8S_IMAGE      Build lightweight k8s manager image instead of all-in-one (default: 0)
#
# Usage:
#   HICLAW_LLM_API_KEY=sk-xxx ./hack/local-k8s-up.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER_NAME="${HICLAW_CLUSTER_NAME:-hiclaw}"
NAMESPACE="${HICLAW_NAMESPACE:-hiclaw}"
SKIP_KIND="${HICLAW_SKIP_KIND:-0}"
SKIP_BUILD="${HICLAW_SKIP_BUILD:-0}"
BUILD_K8S_IMAGE="${HICLAW_BUILD_K8S_IMAGE:-0}"
CONTROLLER_REPLICAS="${HICLAW_CONTROLLER_REPLICAS:-1}"

LLM_API_KEY="${HICLAW_LLM_API_KEY:-}"
REGISTRATION_TOKEN="${HICLAW_REGISTRATION_TOKEN:-}"
ADMIN_PASSWORD="${HICLAW_ADMIN_PASSWORD:-}"

log() { echo -e "\033[36m[HiClaw K8s]\033[0m $1"; }
error() { echo -e "\033[31m[HiClaw K8s ERROR]\033[0m $1" >&2; exit 1; }

# ── Preflight checks ──────────────────────────────────────────────────────

for cmd in kind helm kubectl docker; do
    command -v "$cmd" >/dev/null 2>&1 || error "$cmd is required but not found"
done

if [ -z "$LLM_API_KEY" ]; then
    error "HICLAW_LLM_API_KEY is required. Example: HICLAW_LLM_API_KEY=sk-xxx $0"
fi

# ── Step 1: Create kind cluster ───────────────────────────────────────────

if [ "$SKIP_KIND" = "0" ]; then
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log "kind cluster '${CLUSTER_NAME}' already exists, skipping creation"
    else
        log "Creating kind cluster '${CLUSTER_NAME}'..."
        kind create cluster --name "$CLUSTER_NAME" --config "${PROJECT_ROOT}/hack/kind-config.yaml"
    fi
    kubectl cluster-info --context "kind-${CLUSTER_NAME}"
else
    log "Skipping kind cluster creation (HICLAW_SKIP_KIND=1)"
fi

# ── Step 2: Build & load local images ──────────────────────────────────────

MANAGER_IMAGE="hiclaw/manager:local"
COPAW_MANAGER_IMAGE="hiclaw/manager-copaw:local"
CONTROLLER_IMAGE="agentteams/agentteams-controller:local"
WORKER_IMAGE="hiclaw/worker-agent:local"
COPAW_WORKER_IMAGE="hiclaw/copaw-worker:local"
HERMES_WORKER_IMAGE="hiclaw/hermes-worker:local"
OPENHUMAN_WORKER_IMAGE="hiclaw/openhuman-worker:local"
HELM_IMAGE_OVERRIDES=""

if [ "$SKIP_BUILD" = "0" ]; then
    log "Building local images..."

    # Controller
    log "Building controller image..."
    docker build -t "$CONTROLLER_IMAGE" -f "${PROJECT_ROOT}/agentteams-controller/Dockerfile" "${PROJECT_ROOT}/agentteams-controller"

    # Manager (choose between all-in-one and k8s-lightweight)
    if [ "$BUILD_K8S_IMAGE" = "1" ]; then
        log "Building manager image (lightweight k8s)..."
        docker build -t "$MANAGER_IMAGE" \
            --build-arg OPENCLAW_BASE_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base:20260423-8359cbc \
            --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
            -f "${PROJECT_ROOT}/manager/Dockerfile.k8s" "${PROJECT_ROOT}"
    else
        log "Building manager image (all-in-one)..."
        docker build -t "$MANAGER_IMAGE" \
            --build-arg OPENCLAW_BASE_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base:20260423-8359cbc \
            --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
            -f "${PROJECT_ROOT}/manager/Dockerfile" "${PROJECT_ROOT}"
    fi

    # CoPaw Manager (lightweight Python image with copaw_worker for bridge/sync)
    log "Building CoPaw manager image..."
    docker build -t "$COPAW_MANAGER_IMAGE" \
        --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
        -f "${PROJECT_ROOT}/manager/Dockerfile.copaw" "${PROJECT_ROOT}"

    # Worker images (openclaw + copaw)
    log "Building worker image (openclaw)..."
    docker build -t "$WORKER_IMAGE" \
        --build-arg OPENCLAW_BASE_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base:20260423-8359cbc \
        --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
        --build-context shared="${PROJECT_ROOT}/shared/lib" \
        -f "${PROJECT_ROOT}/worker/Dockerfile" "${PROJECT_ROOT}/worker"

    log "Building worker image (copaw)..."
    docker build -t "$COPAW_WORKER_IMAGE" \
        --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
        --build-context shared="${PROJECT_ROOT}/shared/lib" \
        -f "${PROJECT_ROOT}/copaw/Dockerfile" "${PROJECT_ROOT}/copaw"

    log "Building worker image (hermes)..."
    docker build -t "$HERMES_WORKER_IMAGE" \
        --build-arg HICLAW_CONTROLLER_IMAGE="$CONTROLLER_IMAGE" \
        --build-context shared="${PROJECT_ROOT}/shared/lib" \
        -f "${PROJECT_ROOT}/hermes/Dockerfile" "${PROJECT_ROOT}/hermes"

    log "Building worker image (openhuman)..."
    docker build -t "$OPENHUMAN_WORKER_IMAGE" \
        -f "${PROJECT_ROOT}/openhuman/Dockerfile" "${PROJECT_ROOT}"

    log "Loading images into kind cluster..."
    kind load docker-image "$MANAGER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$COPAW_MANAGER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$CONTROLLER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$WORKER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$COPAW_WORKER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$HERMES_WORKER_IMAGE" --name "$CLUSTER_NAME"
    kind load docker-image "$OPENHUMAN_WORKER_IMAGE" --name "$CLUSTER_NAME"

    # Pre-load Docker Hub images that Kind nodes may not be able to pull directly
    # (e.g., behind GFW or with unreliable Docker Hub access)
    log "Pre-loading Docker Hub images into kind cluster..."
    PRELOAD_IMAGES=(
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/minio:20260216"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/mc:20260216"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/tuwunel:20260216"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/element-web:20260216"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/console:2.2.0"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/higress:2.2.0"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/pilot:2.2.0"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/gateway:2.2.0"
        "higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/proxyv2:2.2.0"
    )
    PLATFORM="linux/$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')"
    for img in "${PRELOAD_IMAGES[@]}"; do
        # Remove existing image first to avoid stale multi-arch manifest
        docker rmi "$img" 2>/dev/null || true
        docker pull --platform "$PLATFORM" "$img" 2>/dev/null || log "WARN: failed to pull $img (may already exist locally)"
        # Bypass kind entirely: pipe docker save directly into containerd import
        # without --all-platforms to avoid multi-arch manifest digest issues
        docker save "$img" | docker exec --privileged -i "$CLUSTER_NAME-control-plane" \
            ctr --namespace=k8s.io images import --snapshotter=overlayfs -
    done

    HELM_IMAGE_OVERRIDES="--set manager.image.repository=hiclaw/manager-copaw --set manager.image.tag=local --set manager.image.pullPolicy=Never"
    HELM_IMAGE_OVERRIDES="${HELM_IMAGE_OVERRIDES} --set controller.image.repository=agentteams/agentteams-controller --set controller.image.tag=local --set controller.image.pullPolicy=Never"
    HELM_IMAGE_OVERRIDES="${HELM_IMAGE_OVERRIDES} --set worker.defaultImage.openclaw.repository=hiclaw/worker-agent --set worker.defaultImage.openclaw.tag=local"
    HELM_IMAGE_OVERRIDES="${HELM_IMAGE_OVERRIDES} --set worker.defaultImage.copaw.repository=hiclaw/copaw-worker --set worker.defaultImage.copaw.tag=local"
    HELM_IMAGE_OVERRIDES="${HELM_IMAGE_OVERRIDES} --set worker.defaultImage.hermes.repository=hiclaw/hermes-worker --set worker.defaultImage.hermes.tag=local"
    HELM_IMAGE_OVERRIDES="${HELM_IMAGE_OVERRIDES} --set worker.defaultImage.openhuman.repository=hiclaw/openhuman-worker --set worker.defaultImage.openhuman.tag=local"

    log "Local images built and loaded"
else
    log "Skipping local build (HICLAW_SKIP_BUILD=1), using remote images"
fi

# ── Step 3: Build Helm dependencies ────────────────────────────────────────

CHART_DIR="${PROJECT_ROOT}/helm/agentteams"

log "Building Helm dependencies..."
helm dependency build "$CHART_DIR"

# ── Step 4: Helm install / upgrade ──────────────────────────────────────────

HELM_SET_OVERRIDES=""
if [ -n "$REGISTRATION_TOKEN" ]; then
    HELM_SET_OVERRIDES="${HELM_SET_OVERRIDES} --set credentials.registrationToken=${REGISTRATION_TOKEN}"
fi
if [ -n "$ADMIN_PASSWORD" ]; then
    HELM_SET_OVERRIDES="${HELM_SET_OVERRIDES} --set credentials.adminPassword=${ADMIN_PASSWORD}"
fi
if [ "$CONTROLLER_REPLICAS" != "1" ]; then
    HELM_SET_OVERRIDES="${HELM_SET_OVERRIDES} --set controller.replicaCount=${CONTROLLER_REPLICAS}"
fi

log "Installing HiClaw via Helm..."
helm upgrade --install hiclaw "$CHART_DIR" \
    --namespace "$NAMESPACE" --create-namespace \
    --set gateway.publicURL="http://localhost:18080" \
    --set credentials.llmApiKey="$LLM_API_KEY" \
    ${HELM_SET_OVERRIDES} \
    ${HELM_IMAGE_OVERRIDES} \
    --timeout 10m \
    --wait=false

# ── Step 5: Wait for core infrastructure ──────────────────────────────────

log "Waiting for Tuwunel (StatefulSet)..."
kubectl rollout status statefulset -l app.kubernetes.io/component=tuwunel \
    -n "$NAMESPACE" --timeout=120s 2>/dev/null || log "Tuwunel not ready yet (may still be pulling image)"

log "Waiting for MinIO (StatefulSet)..."
kubectl rollout status statefulset -l app.kubernetes.io/component=minio \
    -n "$NAMESPACE" --timeout=120s 2>/dev/null || log "MinIO not ready yet"

log "Waiting for Controller..."
kubectl wait --for=condition=available deployment -l app.kubernetes.io/component=controller \
    -n "$NAMESPACE" --timeout=120s 2>/dev/null || log "Controller not ready yet"

# ── Step 6: Print access information ──────────────────────────────────────

echo ""
log "========================================="
log " HiClaw Local K8s Deployment"
log "========================================="
echo ""
log "Cluster:   kind-${CLUSTER_NAME}"
log "Namespace: ${NAMESPACE}"
echo ""
log "Admin credentials:"
log "  Username: admin"
if [ -n "$ADMIN_PASSWORD" ]; then
    log "  Password: ${ADMIN_PASSWORD}"
else
    AUTO_ADMIN_PASSWORD=$(kubectl get secret -n "${NAMESPACE}" hiclaw-runtime-env -o jsonpath='{.data.HICLAW_ADMIN_PASSWORD}' 2>/dev/null | base64 -d 2>/dev/null)
    if [ -n "$AUTO_ADMIN_PASSWORD" ]; then
        log "  Password: ${AUTO_ADMIN_PASSWORD}"
    else
        log "  Password: (unable to retrieve, check secret hiclaw-runtime-env in namespace ${NAMESPACE})"
    fi
fi
echo ""
if [ -n "$REGISTRATION_TOKEN" ]; then
    log "Registration token: ${REGISTRATION_TOKEN}"
else
    AUTO_REG_TOKEN=$(kubectl get secret -n "${NAMESPACE}" hiclaw-runtime-env -o jsonpath='{.data.HICLAW_REGISTRATION_TOKEN}' 2>/dev/null | base64 -d 2>/dev/null)
    if [ -n "$AUTO_REG_TOKEN" ]; then
        log "Registration token: ${AUTO_REG_TOKEN}"
    else
        log "Registration token: (unable to retrieve, check secret hiclaw-runtime-env in namespace ${NAMESPACE})"
    fi
fi
echo ""
log "Access Element Web:"
log "  Then open: http://localhost:18080"
echo ""
log "View Controller logs:"
log "  kubectl logs -f deployment/agentteams-controller -n ${NAMESPACE}"
echo ""
log "View Manager logs (created by controller via Manager CRD):"
log "  kubectl get managers -n ${NAMESPACE}"
log "  kubectl logs -f \$(kubectl get pods -l agentteams.io/manager -n ${NAMESPACE} -o name | head -1) -n ${NAMESPACE}"
echo ""
log "View all pods:"
log "  kubectl get pods -n ${NAMESPACE}"
echo ""
