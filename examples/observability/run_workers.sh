#!/usr/bin/env bash
set -euo pipefail
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QWENPAW_IMAGE="${QWENPAW_IMAGE:-agentteams/qwenpaw-worker:local-20261106003000-otel}"
HERMES_IMAGE="${HERMES_IMAGE:-agentteams/hermes-worker:local-20261106003000-otel}"
for runtime in qwenpaw hermes; do
    image="${QWENPAW_IMAGE}"
    platform=linux/arm64
    if [ "${runtime}" = hermes ]; then
        image="${HERMES_IMAGE}"
        platform="${HERMES_PLATFORM:-linux/amd64}"
    fi
    docker run --rm --platform "${platform}" --network at-tracing-demo_default \
        -v "${DEMO_DIR}:/demo:ro" \
        -e AGENTTEAMS_OTEL_ENABLED=true \
        -e "AGENTTEAMS_OTEL_RUNTIME=${runtime}" \
        -e "AGENTTEAMS_WORKER_NAME=${runtime}-demo" \
        -e OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318 \
        -e TRACELOOP_TRACE_CONTENT=true \
        --entrypoint "/opt/venv/${runtime}/bin/python" \
        "${image}" /demo/runtime_demo.py
done
