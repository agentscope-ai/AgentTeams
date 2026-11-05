#!/usr/bin/env bash
set -euo pipefail
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QWENPAW_IMAGE="${QWENPAW_IMAGE:-agentteams/qwenpaw-worker:local-20261106003000-otel}"
docker volume create at-tracing-demo_collector >/dev/null
docker compose -f "${DEMO_DIR}/compose.yaml" up -d --wait --wait-timeout 120 postgres redis minio clickhouse
docker run --rm -v at-tracing-demo_collector:/queue --entrypoint /bin/sh \
    "${QWENPAW_IMAGE}" -c 'chown 10001:10001 /queue'
docker run --rm --network at-tracing-demo_default --entrypoint /bin/sh \
    "${QWENPAW_IMAGE}" -c '/usr/local/bin/mc.bin alias set tracing http://minio:9000 minio tracing-local >/dev/null && /usr/local/bin/mc.bin mb --ignore-existing tracing/langfuse'
docker compose -f "${DEMO_DIR}/compose.yaml" up -d
echo 'Langfuse: http://localhost:13100 — wait for /api/public/health to return 200'
