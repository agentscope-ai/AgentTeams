# ============================================================
# HiClaw CoPaw Worker Agent - Python-based Container
# ============================================================
# Lightweight Python container for running CoPaw-based Worker Agents.
# Uses the same MinIO-backed config/sync model as OpenClaw workers.
#
# Build args:
#   HIGRESS_REGISTRY  - base image registry for mc (MinIO Client)
#   APT_MIRROR        - Debian mirror (default: mirrors.aliyun.com)
#   PIP_INDEX_URL     - PyPI mirror (default: Aliyun)
#   COPAW_VERSION     - CoPaw version to install from PyPI (default: 1.0.2)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG HICLAW_CONTROLLER_IMAGE=hiclaw/hiclaw-controller:latest

# ============ Stage 1: mc (MinIO Client) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Stage 2: hiclaw CLI (from controller image) ============
FROM ${HICLAW_CONTROLLER_IMAGE} AS hiclaw-controller

# ============ Final Image ============
FROM ${HIGRESS_REGISTRY}/higress/python:3.11-slim

ARG APT_MIRROR=
ARG PIP_INDEX_URL=https://pypi.org/simple/
ARG COPAW_VERSION=1.0.2

# python:3.11-slim is Debian-based, uses /etc/apt/sources.list.d/debian.sources
RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        libolm-dev \
        libjemalloc2 \
        curl \
        git \
        jq \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# jemalloc reduces Python memory fragmentation (~10-20% RSS savings).
# Detect the arch-specific .so path so the image works on both amd64 and arm64.
RUN JEMALLOC=$(find /usr/lib -name 'libjemalloc.so.2' -print -quit) \
    && echo "LD_PRELOAD=${JEMALLOC}" >> /etc/environment \
    && echo "${JEMALLOC}" > /etc/ld.so.preload

# mc (MinIO Client) — real binary; wrapper installed after shared libs are copied
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin

# hiclaw CLI — for worker/team/human management via controller REST API
COPY --from=hiclaw-controller /usr/local/bin/hiclaw /usr/local/bin/hiclaw

ARG NPM_REGISTRY=https://registry.npmjs.org/

# Install Node.js 22 for shared Worker CLIs
RUN apt-get update && \
    apt-get install -y --no-install-recommends nodejs npm && \
    rm -rf /var/lib/apt/lists/*

RUN npm config set registry ${NPM_REGISTRY} && \
    npm install -g mcporter skills @nacos-group/cli && \
    rm -rf /root/.npm

# Stage copaw-worker dependency metadata only (src/ copied later for layer caching).
# A minimal __init__.py placeholder lets pip resolve the package without the real source,
# so that dependency-heavy venv layers (~2 GB) are rebuilt only when pyproject.toml changes.
COPY pyproject.toml README.md /tmp/copaw-worker/
RUN mkdir -p /tmp/copaw-worker/src/copaw_worker \
    && mkdir -p /tmp/copaw-worker/src/matrix \
    && echo '__version__ = "0.0.0"' > /tmp/copaw-worker/src/copaw_worker/__init__.py \
    && echo '__version__ = "0.0.0"' > /tmp/copaw-worker/src/matrix/__init__.py

# --- Single venv: copaw from PyPI + copaw-worker ---
RUN python -m venv /opt/venv/copaw \
    && /opt/venv/copaw/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        copaw==${COPAW_VERSION} \
    && /opt/venv/copaw/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /tmp/copaw-worker/ \
    && /opt/venv/copaw/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        "wrapt<2.0" \
        loongsuite-site-bootstrap \
        loongsuite-distro \
        opentelemetry-exporter-otlp \
        loongsuite-instrumentation-copaw \
        loongsuite-instrumentation-agentscope

# --- Overlay real copaw-worker source (high-frequency changes) ---
# This layer is rebuilt on every src/ change but does NOT invalidate the
# heavy venv layers above, saving ~2 GB of re-download on typical upgrades.
COPY src/ /tmp/copaw-worker/src/
RUN SITE=/opt/venv/copaw/lib/python3.11/site-packages \
    && cp -a /tmp/copaw-worker/src/copaw_worker/* "$SITE/copaw_worker/" \
    && rm -rf "$SITE/copaw/app/channels/matrix" \
    && cp -a /tmp/copaw-worker/src/matrix "$SITE/copaw/app/channels/" \
    && if [ -f /tmp/copaw-worker/src/matrix/config.py ]; then \
         cp /tmp/copaw-worker/src/matrix/config.py "$SITE/copaw/config/config.py"; \
       fi \
    && rm -rf /tmp/copaw-worker/

# CoPaw runtime CLIs (make them available in PATH for agents)
RUN ln -sf /opt/venv/copaw/bin/copaw /usr/local/bin/copaw && \
    ln -sf /opt/venv/copaw/bin/copaw-sync /usr/local/bin/copaw-sync

# Entrypoint script
COPY --chmod=755 scripts/copaw-worker-entrypoint.sh /opt/hiclaw/scripts/copaw-worker-entrypoint.sh

# Shared environment bootstrap and credential management (hiclaw-env.sh, oss-credentials.sh)
COPY --from=shared . /opt/hiclaw/scripts/lib/

# mc wrapper: auto-refreshes STS credentials before every mc invocation (no-op in local mode)
RUN chmod +x /opt/hiclaw/scripts/lib/mc-wrapper.sh && \
    ln -sf /opt/hiclaw/scripts/lib/mc-wrapper.sh /usr/local/bin/mc

# Create workspace directory
RUN mkdir -p /root/.hiclaw-worker

WORKDIR /root/.hiclaw-worker

ENTRYPOINT ["/opt/hiclaw/scripts/copaw-worker-entrypoint.sh"]
