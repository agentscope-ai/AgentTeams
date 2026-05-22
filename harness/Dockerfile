# ============================================================
# HiClaw Harness Worker — delegates to Claude Code / Gemini CLI / OpenCode / Codex
# ============================================================
#
# Build args:
#   HIGRESS_REGISTRY        - base image registry for mc + python
#   HICLAW_CONTROLLER_IMAGE - source for the hiclaw CLI binary
#   APT_MIRROR             - Debian mirror (default: official)
#   PIP_INDEX_URL          - PyPI mirror (default: official)
#   NPM_REGISTRY           - npm registry (default: official)
#
# Build context: ./harness/ (HiClaw/harness/ when built from HiClaw/)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG HICLAW_CONTROLLER_IMAGE=registry-outsource.momocdn.net/momovn-dev/hiclaw-controller:latest

# ============ Stage 1: mc (MinIO Client) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Stage 2: hiclaw CLI (from controller image) ============
FROM ${HICLAW_CONTROLLER_IMAGE} AS hiclaw-controller

# ============ Final Image ============
FROM ${HIGRESS_REGISTRY}/higress/python:3.11-slim

ARG APT_MIRROR=
ARG PIP_INDEX_URL=https://pypi.org/simple/
ARG NPM_REGISTRY=https://registry.npmjs.org/

RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        libolm-dev \
        libffi-dev \
        curl \
        git \
        jq \
        ripgrep \
        tzdata && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Install Node.js + npm (needed for all CLI agents)
RUN apt-get update && \
    apt-get install -y --no-install-recommends nodejs npm && \
    rm -rf /var/lib/apt/lists/*

# Install all four CLI agents + mcporter via npm
RUN npm config set registry "${NPM_REGISTRY}" && \
    npm install -g \
        @anthropic-ai/claude-code \
        @google/gemini-cli \
        opencode-ai \
        @openai/codex \
        mcporter && \
    rm -rf /root/.npm

# Python venv
RUN python3 -m venv /opt/venv/harness
ENV PATH="/opt/venv/harness/bin:$PATH"

# Python deps - use matrix-nio instead of nio
RUN /opt/venv/harness/bin/pip install \
        --index-url "${PIP_INDEX_URL}" \
        typer rich httpx matrix-nio

# Copy mc binary (mc:20260216 puts it at /usr/bin/mc)
COPY --from=mc /usr/bin/mc /usr/local/bin/mc

# Copy hiclaw CLI binary
COPY --from=hiclaw-controller /usr/local/bin/hiclaw /usr/local/bin/hiclaw

# Copy harness-worker source directly to site-packages
COPY src/harness_worker /opt/venv/harness/lib/python3.11/site-packages/harness_worker

# Create entrypoint wrapper script
RUN echo '#!/bin/bash\nexec /opt/venv/harness/bin/python -m harness_worker.cli "$@"' > /usr/local/bin/harness-worker && chmod +x /usr/local/bin/harness-worker

# Working directory
WORKDIR /root/hiclaw-fs/agents

# Scripts entrypoint
COPY scripts/harness-worker-entrypoint.sh /opt/hiclaw/scripts/
RUN chmod +x /opt/hiclaw/scripts/harness-worker-entrypoint.sh

ENTRYPOINT ["/opt/hiclaw/scripts/harness-worker-entrypoint.sh"]
