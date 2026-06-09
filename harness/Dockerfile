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
ARG HICLAW_CONTROLLER_IMAGE=hiclaw/hiclaw-controller:latest

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
        openssh-client \
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

# Install Bazelisk as native binary named `bazel` (no Node.js wrapper overhead)
RUN ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') && \
    curl -fsSL "https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-${ARCH}" \
         -o /usr/local/bin/bazel && \
    chmod +x /usr/local/bin/bazel

# Python venv
RUN python3 -m venv /opt/venv/harness
ENV PATH="/opt/venv/harness/bin:$PATH"

# Install hiclaw_common (policies, sync, mautrix relay) from shared build context.
# shared-python maps to HiClaw/shared/python/ via --build-context shared-python=./shared/python
COPY --from=shared-python hiclaw_common /tmp/hiclaw-common
RUN /opt/venv/harness/bin/pip install \
        --index-url "${PIP_INDEX_URL}" \
        '/tmp/hiclaw-common[matrix]'

# Python runtime deps (harness-worker itself; hiclaw_common covers mautrix)
RUN /opt/venv/harness/bin/pip install \
        --index-url "${PIP_INDEX_URL}" \
        typer rich httpx

# Copy mc binary (mc:20260216 puts it at /usr/bin/mc)
COPY --from=mc /usr/bin/mc /usr/local/bin/mc

# Copy hiclaw CLI binary
COPY --from=hiclaw-controller /usr/local/bin/hiclaw /usr/local/bin/hiclaw

# Copy harness-worker source directly to site-packages
COPY src/harness_worker /opt/venv/harness/lib/python3.11/site-packages/harness_worker

# Overlay hiclaw_common source (picks up changes without invalidating the heavy pip-install layer)
COPY --from=shared-python hiclaw_common/src/hiclaw_common /opt/venv/harness/lib/python3.11/site-packages/hiclaw_common

# markdown-it-py: required by hiclaw_common.matrix for HTML formatting.
# Explicit step so adding it to hiclaw_common[matrix] doesn't bust the heavy pip cache above.
RUN /opt/venv/harness/bin/pip install --no-cache-dir 'markdown-it-py>=3.0'

# Create entrypoint wrapper script
RUN echo '#!/bin/bash\nexec /opt/venv/harness/bin/python -m harness_worker.cli "$@"' > /usr/local/bin/harness-worker && chmod +x /usr/local/bin/harness-worker

# Working directory
WORKDIR /root/hiclaw-fs/agents

# Scripts entrypoint
COPY scripts/harness-worker-entrypoint.sh /opt/hiclaw/scripts/
RUN chmod +x /opt/hiclaw/scripts/harness-worker-entrypoint.sh

ENTRYPOINT ["/opt/hiclaw/scripts/harness-worker-entrypoint.sh"]
