#!/bin/bash
# smoke-test.sh - Post-startup health check for Manager container
# Verifies all core services are running and accessible.
# Usage: bash /manager/tests/smoke-test.sh

set -e

PASS=0
FAIL=0

check() {
    local name="$1"
    local cmd="$2"

    if eval "${cmd}" > /dev/null 2>&1; then
        echo -e "\033[32m[PASS]\033[0m ${name}"
        PASS=$((PASS + 1))
    else
        echo -e "\033[31m[FAIL]\033[0m ${name}"
        FAIL=$((FAIL + 1))
    fi
}

echo "=== AgentTeams Manager Smoke Test ==="
echo ""

# Core services
check "MinIO (port 9000)" \
    "curl -sf http://127.0.0.1:9000/minio/health/live"

check "MinIO Console (port 9001)" \
    "curl -sf http://127.0.0.1:9001/"

check "Tuwunel Matrix (port 6167)" \
    "curl -sf http://127.0.0.1:6167/_matrix/client/versions"

check "Higress Gateway (port 8080)" \
    "curl -sf -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/ | grep -q '404\|200'"

check "Higress Console (port 8001)" \
    "curl -sf http://127.0.0.1:8001/"

check "Element Web (port 8088)" \
    "curl -sf http://127.0.0.1:8088/"

# File system
check "MinIO bucket exists" \
    "mc alias set smoketest http://127.0.0.1:9000 ${AGENTTEAMS_MINIO_USER} ${AGENTTEAMS_MINIO_PASSWORD} && mc ls smoketest/agentteams-storage/"

check "Manager SOUL.md in MinIO" \
    "mc cat smoketest/agentteams-storage/agents/manager/SOUL.md | head -1"

# Processes
check "supervisord running" \
    "pgrep -f supervisord"

check "minio process running" \
    "pgrep -f 'minio server'"

check "tuwunel process running" \
    "pgrep -f tuwunel"

# Summary
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="

if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
