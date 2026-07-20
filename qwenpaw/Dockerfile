# ============================================================
# AgentTeams QwenPaw Worker - Python-based Container
# ============================================================
# Lightweight Python container for running QwenPaw-based Worker Agents.
# Uses the same MinIO-backed config/sync model as CoPaw workers, but reads
# controller-projected runtime.yaml as the worker contract.
#
# Build args:
#   HIGRESS_REGISTRY  - base image registry for mc (MinIO Client)
#   APT_MIRROR        - Debian mirror
#   PIP_INDEX_URL     - PyPI mirror
#   QWENPAW_PIP_SPEC  - QwenPaw package spec to install from PyPI
#   NPM_REGISTRY      - npm mirror
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

# ============ Stage 1: mc (MinIO Client) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Final Image ============
FROM ${HIGRESS_REGISTRY}/higress/python:3.11-slim

ARG APT_MIRROR=
ARG PIP_INDEX_URL=https://pypi.org/simple/
ARG PIP_TIMEOUT=300
ARG QWENPAW_PIP_SPEC=qwenpaw==1.1.11
ARG NPM_REGISTRY=https://registry.npmjs.org/

RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        curl \
        git \
        jq \
        libjemalloc2 \
        nodejs \
        npm \
        socat \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# jemalloc reduces Python memory fragmentation and matches the CoPaw worker base.
RUN JEMALLOC=$(find /usr/lib -name 'libjemalloc.so.2' -print -quit) \
    && echo "LD_PRELOAD=${JEMALLOC}" >> /etc/environment \
    && echo "${JEMALLOC}" > /etc/ld.so.preload

# mc (MinIO Client) - real binary; wrapper installed after shared libs are copied.
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin

# Stage qwenpaw-worker dependency metadata only (src/ copied later for layer caching).
COPY qwenpaw/pyproject.toml qwenpaw/README.md /tmp/qwenpaw-worker/
RUN mkdir -p /tmp/qwenpaw-worker/src/qwenpaw_worker && \
    echo '__version__ = "0.0.0"' > /tmp/qwenpaw-worker/src/qwenpaw_worker/__init__.py

# Single venv: qwenpaw runtime + qwenpaw-worker.
RUN --mount=type=cache,target=/root/.cache/pip,sharing=locked \
    python -m venv /opt/venv/qwenpaw \
    && /opt/venv/qwenpaw/bin/pip install \
        --retries 5 \
        --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed -E 's|^https?://||;s|/.*||')" \
        "${QWENPAW_PIP_SPEC}" \
    && /opt/venv/qwenpaw/bin/pip install \
        --retries 5 \
        --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed -E 's|^https?://||;s|/.*||')" \
        /tmp/qwenpaw-worker/

RUN /opt/venv/qwenpaw/bin/pip install \
        --retries 5 \
        --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed -E 's|^https?://||;s|/.*||')" \
        "wrapt<2.0" \
        loongsuite-site-bootstrap==0.6.1 \
        loongsuite-distro==0.6.0 \
        loongsuite-otel-util-genai==0.6.1 \
        loongsuite-instrumentation-qwenpaw==0.6.1 \
        loongsuite-instrumentation-agentscope==0.6.1 \
        opentelemetry-exporter-otlp

# Keep Node CLI tooling out of the qwenpaw pip dependency cache key.
RUN npm config set registry ${NPM_REGISTRY} && \
    npm install -g mcporter skills @nacos-group/cli@1.1.2 && \
    rm -rf /root/.npm

# Overlay AgentTeams Matrix channel behavior while keeping QwenPaw as the base runtime.
COPY qwenpaw/src/matrix/ /tmp/qwenpaw-matrix-overlay/
RUN SITE=/opt/venv/qwenpaw/lib/python3.11/site-packages && \
    cp -a /tmp/qwenpaw-matrix-overlay/channel.py "$SITE/qwenpaw/app/channels/matrix/channel.py" && \
    rm -rf /tmp/qwenpaw-matrix-overlay

# Overlay real qwenpaw-worker source without rebuilding the dependency-heavy venv.
COPY qwenpaw/src/ /tmp/qwenpaw-worker/src/
RUN SITE=/opt/venv/qwenpaw/lib/python3.11/site-packages && \
    cp -a /tmp/qwenpaw-worker/src/qwenpaw_worker/* "$SITE/qwenpaw_worker/" && \
    rm -rf /tmp/qwenpaw-worker

# Opaque QwenPaw plugin artifacts. Each plugin build owns its contents.
RUN mkdir -p /opt/agentteams/plugins
COPY dist/adapters/qwenpaw/teamharness-qwenpaw.zip /opt/agentteams/plugins/teamharness-qwenpaw.zip
COPY dist/adapters/qwenpaw/workerflow-qwenpaw.zip /opt/agentteams/plugins/workerflow-qwenpaw.zip
COPY qwenpaw/scripts/install-builtin-qwenpaw-plugins.py /tmp/install-builtin-qwenpaw-plugins.py
RUN /opt/venv/qwenpaw/bin/python /tmp/install-builtin-qwenpaw-plugins.py \
        --working-dir /opt/agentteams/qwenpaw-builtin \
        /opt/agentteams/plugins/teamharness-qwenpaw.zip \
        /opt/agentteams/plugins/workerflow-qwenpaw.zip \
    && rm -f /tmp/install-builtin-qwenpaw-plugins.py

# Enable QwenPaw's native deferred MCP startup for app workspaces.
COPY qwenpaw/scripts/patch-qwenpaw-defer-mcp-startup.py /tmp/patch-qwenpaw-defer-mcp-startup.py
RUN /opt/venv/qwenpaw/bin/python /tmp/patch-qwenpaw-defer-mcp-startup.py && \
    rm -f /tmp/patch-qwenpaw-defer-mcp-startup.py

COPY --chmod=755 qwenpaw/scripts/qwenpaw-worker-entrypoint.sh /opt/agentteams/scripts/qwenpaw-worker-entrypoint.sh
COPY --chmod=755 qwenpaw/scripts/qwenpaw-teamharness-plugin-reload.sh /opt/agentteams/scripts/qwenpaw-teamharness-plugin-reload.sh
COPY --from=shared . /opt/agentteams/scripts/lib/

# mc wrapper: auto-refreshes STS credentials before every mc invocation.
RUN chmod +x /opt/agentteams/scripts/lib/mc-wrapper.sh && \
    ln -sf /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc && \
    ln -sf /opt/venv/qwenpaw/bin/qwenpaw /usr/local/bin/qwenpaw && \
    ln -sf /opt/venv/qwenpaw/bin/qwenpaw-worker /usr/local/bin/qwenpaw-worker && \
    ln -sf /opt/agentteams/scripts/qwenpaw-teamharness-plugin-reload.sh /usr/local/bin/qwenpaw-teamharness-plugin-reload && \
    mkdir -p /root/agentteams-fs/agents

WORKDIR /root/agentteams-fs/agents

ENTRYPOINT ["/opt/agentteams/scripts/qwenpaw-worker-entrypoint.sh"]
