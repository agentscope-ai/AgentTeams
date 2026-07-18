#!/bin/bash
# hiclaw-verify.sh - Post-install shallow verification for AgentTeams
#
# Usage:
#   bash install/hiclaw-verify.sh [container_name]   # default: agentteams-manager
#   AGENTTEAMS_VERIFY_INFRA_CONTAINER=<name>         # default: agentteams-controller
#
# Runs 6 read-only reachability checks and prints PASS/FAIL per check.
# Exit code: 0 if all pass, 1 if any fail.
#
# ── Extension notes ────────────────────────────────────────────────────────────
#
# Kubernetes migration (TODO when K8s support is planned):
#   This script currently assumes a single-container Docker/Podman deployment.
#   Three areas need rework for K8s:
#
#   1. Runtime detection
#      Replace the Docker/Podman block below with a three-way check:
#        docker  → EXEC_CMD="docker exec ${CONTAINER}"
#        podman  → EXEC_CMD="podman exec ${CONTAINER}"
#        kubectl → EXEC_CMD="kubectl exec <pod-name> --namespace <ns> --"
#      Pod name is dynamic; discover it with:
#        kubectl get pod -l app=agentteams-manager -o jsonpath='{.items[0].metadata.name}'
#
#   2. Internal service checks (checks #2, #3, #6)
#      Local embedded installs route infrastructure checks to the controller
#      container and the Agent check to the Manager container. Legacy installs
#      fall back to the Manager container for all three checks.
#      In K8s each service is a separate Pod/Service; replace with:
#        `kubectl exec <manager-pod> -- curl http://<service-name>.<ns>.svc:PORT`
#      or use `kubectl port-forward svc/<name> LOCAL:REMOTE` for a one-shot probe.
#
#   3. External access checks (checks #4, #5)
#      Currently reads PORT_GATEWAY / PORT_CONSOLE from container env (host port
#      mappings). In K8s these become NodePort / Ingress / LoadBalancer addresses.
#      Replace printenv-based detection with:
#        kubectl get svc agentteams-gateway -o jsonpath='{.spec.ports[0].nodePort}'
#      or accept GATEWAY_URL / CONSOLE_URL as environment variables for flexibility.
#
# ───────────────────────────────────────────────────────────────────────────────

# No set -e: each check is independent; failures do not abort subsequent checks.

MANAGER_CONTAINER="${1:-agentteams-manager}"
INFRA_CONTAINER="${AGENTTEAMS_VERIFY_INFRA_CONTAINER:-agentteams-controller}"

# ---------- Docker/Podman detection ----------
# TODO(k8s): extend to three-way detection (docker / podman / kubectl)
#   and set EXEC_CMD accordingly (see extension notes above).

DOCKER_CMD="docker"
if ! docker version >/dev/null 2>&1; then
    if podman version >/dev/null 2>&1; then
        DOCKER_CMD="podman"
    fi
fi

# ---------- Local topology and port/config detection ----------
# TODO(k8s): replace printenv-based detection with kubectl-based service
#   discovery, or accept GATEWAY_URL / CONSOLE_URL env vars directly.

running_containers=$("${DOCKER_CMD}" ps --format '{{.Names}}' 2>/dev/null) || running_containers=""
SERVICE_CONTAINER="${MANAGER_CONTAINER}"
if printf '%s\n' "${running_containers}" | grep -Fxq "${INFRA_CONTAINER}"; then
    SERVICE_CONTAINER="${INFRA_CONTAINER}"
fi

manager_env=$("${DOCKER_CMD}" exec "${MANAGER_CONTAINER}" printenv 2>/dev/null) || manager_env=""
if [ "${SERVICE_CONTAINER}" = "${MANAGER_CONTAINER}" ]; then
    service_env="${manager_env}"
else
    service_env=$("${DOCKER_CMD}" exec "${SERVICE_CONTAINER}" printenv 2>/dev/null) || service_env=""
fi

mapped_host_port() {
    local container="$1"
    local container_port="$2"
    local mapping

    mapping=$("${DOCKER_CMD}" port "${container}" "${container_port}/tcp" 2>/dev/null | head -1) || mapping=""
    printf '%s\n' "${mapping}" | awk -F: 'NF > 1 { print $NF; exit }'
}

PORT_GATEWAY=$(echo "${service_env}" | grep ^AGENTTEAMS_PORT_GATEWAY= | cut -d= -f2-)
PORT_CONSOLE=$(echo "${service_env}" | grep ^AGENTTEAMS_PORT_CONSOLE= | cut -d= -f2-)
PORT_GATEWAY="${PORT_GATEWAY:-$(mapped_host_port "${SERVICE_CONTAINER}" 8080)}"
PORT_CONSOLE="${PORT_CONSOLE:-$(mapped_host_port "${SERVICE_CONTAINER}" 8001)}"
PORT_GATEWAY="${PORT_GATEWAY:-18080}"
PORT_CONSOLE="${PORT_CONSOLE:-18001}"

# ---------- Result tracking ----------

PASS=0
FAIL=0

check_pass() {
    echo "  [PASS] $1"
    PASS=$((PASS + 1))
}

check_fail() {
    echo "  [FAIL] $1"
    FAIL=$((FAIL + 1))
}

# ---------- Checks ----------

echo ""
echo "==> AgentTeams Post-Install Verification"

# 1. Manager container running
# TODO(k8s): replace with `kubectl get pod -l app=agentteams-manager` and check
#   that at least one pod is in Running phase (not just Pending/CrashLoopBackOff).
if printf '%s\n' "${running_containers}" | grep -Fxq "${MANAGER_CONTAINER}"; then
    check_pass "Manager container running"
else
    check_fail "Manager container running (container '${MANAGER_CONTAINER}' not found in ${DOCKER_CMD} ps)"
fi

# 2. MinIO health check (internal via infrastructure container)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- curl http://minio.<ns>.svc:9000/minio/health/live`
#   or probe the MinIO Service ClusterIP directly if network policy allows.
minio_status=$("${DOCKER_CMD}" exec "${SERVICE_CONTAINER}" \
    curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:9000/minio/health/live" 2>/dev/null) || minio_status="000"
if [ "${minio_status}" = "200" ]; then
    check_pass "MinIO health check"
else
    check_fail "MinIO health check (HTTP ${minio_status})"
fi

# 3. Matrix API reachable (internal via infrastructure container)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- curl http://matrix.<ns>.svc:6167/_matrix/client/versions`
matrix_status=$("${DOCKER_CMD}" exec "${SERVICE_CONTAINER}" \
    curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:6167/_matrix/client/versions" 2>/dev/null) || matrix_status="000"
if [ "${matrix_status}" = "200" ]; then
    check_pass "Matrix API reachable"
else
    check_fail "Matrix API reachable (HTTP ${matrix_status})"
fi

# 4. Higress Gateway reachable (external host port, any non-000 response is ok)
# TODO(k8s): replace 127.0.0.1:PORT with the Ingress/NodePort/LoadBalancer address.
#   Suggested: accept AGENTTEAMS_VERIFY_GATEWAY_URL env var as override, fall back to
#   auto-detected NodePort via `kubectl get svc`.
gateway_status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:${PORT_GATEWAY}/" 2>/dev/null) || gateway_status="000"
if [ "${gateway_status}" != "000" ]; then
    check_pass "Higress Gateway reachable"
else
    check_fail "Higress Gateway reachable (no response on port ${PORT_GATEWAY})"
fi

# 5. Higress Console reachable (external host port, HTTP 200)
# TODO(k8s): same as check #4 — use Ingress/NodePort address or AGENTTEAMS_VERIFY_CONSOLE_URL override.
console_status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:${PORT_CONSOLE}/" 2>/dev/null) || console_status="000"
if [ "${console_status}" = "200" ]; then
    check_pass "Higress Console reachable"
else
    check_fail "Higress Console reachable (HTTP ${console_status} on port ${PORT_CONSOLE})"
fi

# 6. Manager Agent healthy (runtime-aware check)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- <health-check-command>`
#   Pod name must be resolved dynamically before this call.
MANAGER_RUNTIME=$(echo "${manager_env}" | grep ^AGENTTEAMS_MANAGER_RUNTIME= | cut -d= -f2-)
MANAGER_RUNTIME="${MANAGER_RUNTIME:-openclaw}"

if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
    # CoPaw: check app API health endpoint
    agent_status=$("${DOCKER_CMD}" exec "${MANAGER_CONTAINER}" \
        curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
        "http://127.0.0.1:18799/health" 2>/dev/null) || agent_status="000"
    if [ "${agent_status}" = "200" ]; then
        check_pass "CoPaw Agent healthy"
    else
        check_fail "CoPaw Agent healthy (HTTP ${agent_status})"
    fi
else
    # OpenClaw: check gateway health
    agent_output=$("${DOCKER_CMD}" exec "${MANAGER_CONTAINER}" \
        openclaw gateway health --json 2>/dev/null) || agent_output=""
    if echo "${agent_output}" | grep -q '"ok"'; then
        check_pass "OpenClaw Agent healthy"
    else
        check_fail "OpenClaw Agent healthy (output: ${agent_output:-<empty>})"
    fi
fi

# ---------- Summary ----------

TOTAL=$((PASS + FAIL))
echo "==> Result: ${PASS}/${TOTAL} passed"
echo ""

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
