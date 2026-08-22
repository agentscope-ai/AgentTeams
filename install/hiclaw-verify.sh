#!/bin/bash
# hiclaw-verify.sh - Post-install shallow verification for HiClaw
#
# Usage:
#   bash install/hiclaw-verify.sh [container_name]   # default: hiclaw-manager
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
#        kubectl get pod -l app=hiclaw-manager -o jsonpath='{.items[0].metadata.name}'
#
#   2. Internal service checks (checks #2, #3, #6)
#      These use `docker exec ... curl 127.0.0.1:PORT` which works because all
#      services share a single container network namespace.
#      In K8s each service is a separate Pod/Service; replace with:
#        `kubectl exec <manager-pod> -- curl http://<service-name>.<ns>.svc:PORT`
#      or use `kubectl port-forward svc/<name> LOCAL:REMOTE` for a one-shot probe.
#
#   3. External access checks (checks #4, #5)
#      Currently reads PORT_GATEWAY / PORT_CONSOLE from container env (host port
#      mappings). In K8s these become NodePort / Ingress / LoadBalancer addresses.
#      Replace printenv-based detection with:
#        kubectl get svc hiclaw-gateway -o jsonpath='{.spec.ports[0].nodePort}'
#      or accept GATEWAY_URL / CONSOLE_URL as environment variables for flexibility.
#
# ───────────────────────────────────────────────────────────────────────────────

# No set -e: each check is independent; failures do not abort subsequent checks.

CONTAINER="${1:-hiclaw-manager}"

# ---------- Docker/Podman detection ----------
# TODO(k8s): extend to three-way detection (docker / podman / kubectl)
#   and set EXEC_CMD accordingly (see extension notes above).

DOCKER_CMD="docker"
if ! docker version >/dev/null 2>&1; then
    if podman version >/dev/null 2>&1; then
        DOCKER_CMD="podman"
    fi
fi

# ---------- Port/config detection from container env ----------
# TODO(k8s): replace printenv-based detection with kubectl-based service
#   discovery, or accept GATEWAY_URL / CONSOLE_URL env vars directly.

container_env=$("${DOCKER_CMD}" exec "${CONTAINER}" printenv 2>/dev/null) || container_env=""
PORT_GATEWAY=$(echo "$container_env" | grep ^HICLAW_PORT_GATEWAY= | cut -d= -f2-)
PORT_CONSOLE=$(echo "$container_env" | grep ^HICLAW_PORT_CONSOLE= | cut -d= -f2-)
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
echo "==> HiClaw Post-Install Verification"

# 1. Manager container running
# TODO(k8s): replace with `kubectl get pod -l app=hiclaw-manager` and check
#   that at least one pod is in Running phase (not just Pending/CrashLoopBackOff).
if "${DOCKER_CMD}" ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTAINER}$"; then
    check_pass "Manager container running"
else
    check_fail "Manager container running (container '${CONTAINER}' not found in docker ps)"
fi

# 2. MinIO health check (internal via docker exec)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- curl http://minio.<ns>.svc:9000/minio/health/live`
#   or probe the MinIO Service ClusterIP directly if network policy allows.
minio_status=$("${DOCKER_CMD}" exec "${CONTAINER}" \
    curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:9000/minio/health/live" 2>/dev/null) || minio_status="000"
if [ "${minio_status}" = "200" ]; then
    check_pass "MinIO health check"
else
    check_fail "MinIO health check (HTTP ${minio_status})"
fi

# 3. Matrix API reachable (internal via docker exec)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- curl http://matrix.<ns>.svc:6167/_matrix/client/versions`
matrix_status=$("${DOCKER_CMD}" exec "${CONTAINER}" \
    curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:6167/_matrix/client/versions" 2>/dev/null) || matrix_status="000"
if [ "${matrix_status}" = "200" ]; then
    check_pass "Matrix API reachable"
else
    check_fail "Matrix API reachable (HTTP ${matrix_status})"
fi

# 4. Higress Gateway reachable (external host port, any non-000 response is ok)
# TODO(k8s): replace 127.0.0.1:PORT with the Ingress/NodePort/LoadBalancer address.
#   Suggested: accept HICLAW_VERIFY_GATEWAY_URL env var as override, fall back to
#   auto-detected NodePort via `kubectl get svc`.
gateway_status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:${PORT_GATEWAY}/" 2>/dev/null) || gateway_status="000"
if [ "${gateway_status}" != "000" ]; then
    check_pass "Higress Gateway reachable"
else
    check_fail "Higress Gateway reachable (no response on port ${PORT_GATEWAY})"
fi

# 5. Higress Console reachable (external host port, HTTP 200)
# TODO(k8s): same as check #4 — use Ingress/NodePort address or HICLAW_VERIFY_CONSOLE_URL override.
console_status=$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    "http://127.0.0.1:${PORT_CONSOLE}/" 2>/dev/null) || console_status="000"
if [ "${console_status}" = "200" ]; then
    check_pass "Higress Console reachable"
else
    check_fail "Higress Console reachable (HTTP ${console_status} on port ${PORT_CONSOLE})"
fi

# 6. OpenClaw Agent healthy (internal via docker exec)
# TODO(k8s): replace with `kubectl exec <manager-pod> -- openclaw gateway health --json`
#   Pod name must be resolved dynamically before this call.
agent_output=$("${DOCKER_CMD}" exec "${CONTAINER}" \
    openclaw gateway health --json 2>/dev/null) || agent_output=""
if echo "${agent_output}" | grep -q '"ok"'; then
    check_pass "OpenClaw Agent healthy"
else
    check_fail "OpenClaw Agent healthy (output: ${agent_output:-<empty>})"
fi

# ---------- Summary ----------

TOTAL=$((PASS + FAIL))
echo "==> Result: ${PASS}/${TOTAL} passed"
echo ""

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
