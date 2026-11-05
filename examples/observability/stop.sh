#!/usr/bin/env bash
set -euo pipefail
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for container in at-tracing-worker-qwenpaw at-tracing-worker-hermes at-tracing-model-proxy at-tracing-demo-matrix at-tracing-demo-dashboard; do
    docker stop --time 15 "${container}" >/dev/null 2>&1 || true
done
docker compose -f "${DEMO_DIR}/compose.yaml" down
