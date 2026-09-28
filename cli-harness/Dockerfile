# ============================================================
# AgentTeams CLI-harness Worker - shared image for CLI coding agents
# ============================================================
# One image, several Worker runtimes: atomcode, codex, claude-code,
# kimi-code, pi, dsh. Each runtime value selects a different headless CLI
# adapter inside the shared Python worker (see src/cli_harness_worker/),
# which bridges Matrix rooms to the CLI: MinIO sync + Matrix transport +
# provider env are AgentTeams-provided; the CLI is the agent loop.
#
# Build args:
#   HIGRESS_REGISTRY          - base image registry for mc + python
#   AGENTTEAMS_CONTROLLER_IMAGE - source for the agt CLI binary
#   APT_MIRROR                - Debian mirror (default: empty / official)
#   PIP_INDEX_URL             - PyPI mirror (default: official PyPI)
#   NPM_REGISTRY              - npm registry (default: official)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest

# ============ Stage 1: mc (MinIO Client) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Stage 2: AgentTeams CLI (from controller image) ============
FROM ${AGENTTEAMS_CONTROLLER_IMAGE} AS agentteams-controller

# ============ Final Image ============
FROM ${HIGRESS_REGISTRY}/higress/python:3.11-slim

ARG APT_MIRROR=
ARG PIP_INDEX_URL=https://pypi.org/simple/
ARG NPM_REGISTRY=https://registry.npmjs.org/

# python:3.11-slim is Debian-based, uses /etc/apt/sources.list.d/debian.sources
RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        libffi-dev \
        curl \
        git \
        jq \
        ripgrep \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# mc (MinIO Client) — real binary; wrapper installed after shared libs are copied
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin

# AgentTeams CLI — for worker readiness reporting (agt worker report-ready)
COPY --from=agentteams-controller /usr/local/bin/agt /usr/local/bin/agt

# Install Node.js 22 — required by the npm-distributed CLI coding agents and
# the shared Worker CLIs (mcporter, skills CLI compatibility)
RUN apt-get update && \
    apt-get install -y --no-install-recommends nodejs npm && \
    rm -rf /var/lib/apt/lists/*

RUN npm config set registry ${NPM_REGISTRY} && \
    npm install -g mcporter skills @nacos-group/cli && \
    rm -rf /root/.npm

# --- Heavy install layer (cached unless pyproject.toml changes) ---
COPY pyproject.toml README.md /tmp/cli-harness/
RUN mkdir -p /tmp/cli-harness/src/cli_harness_worker \
    && echo '__version__ = "0.0.0"' > /tmp/cli-harness/src/cli_harness_worker/__init__.py

RUN python -m venv /opt/venv/cli-harness \
    && /opt/venv/cli-harness/bin/pip install --upgrade pip setuptools wheel \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
    && /opt/venv/cli-harness/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        "matrix-nio[e2e]>=0.24.0" \
        "typer>=0.9" \
        "rich>=13.0" \
        "httpx>=0.25" \
    && /opt/venv/cli-harness/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /tmp/cli-harness/

# --- CLI coding agents (best-effort per channel; see install script) ---
COPY scripts/install-cli-agents.sh /tmp/install-cli-agents.sh
RUN chmod +x /tmp/install-cli-agents.sh && \
    PIP_INDEX_URL="${PIP_INDEX_URL}" /tmp/install-cli-agents.sh && \
    rm -f /tmp/install-cli-agents.sh

# --- Overlay real cli-harness source (high-frequency changes) ---
COPY src/ /tmp/cli-harness/src/
RUN SITE=/opt/venv/cli-harness/lib/python3.11/site-packages \
    && cp -a /tmp/cli-harness/src/cli_harness_worker/* "$SITE/cli_harness_worker/" \
    && rm -rf /tmp/cli-harness/ \
    && ln -sf /opt/venv/cli-harness/bin/cli-harness-worker /usr/local/bin/cli-harness-worker

# Entrypoint script
COPY --chmod=755 scripts/cli-harness-worker-entrypoint.sh /opt/agentteams/scripts/cli-harness-worker-entrypoint.sh

# Shared environment bootstrap and credential management
# (agentteams-env.sh, oss-credentials.sh, mc-wrapper.sh)
COPY --from=shared . /opt/agentteams/scripts/lib/

RUN chmod +x /opt/agentteams/scripts/lib/mc-wrapper.sh && \
    ln -sf /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc

# Workspace lives under the controller-injected HOME=/root/agentteams-fs/agents/<name>
# so the layout matches the other runtimes (HOME == workspace == MinIO mirror
# root). The per-worker subdir is created by the entrypoint at startup.
RUN mkdir -p /root/agentteams-fs/agents

WORKDIR /root/agentteams-fs/agents

ENTRYPOINT ["/opt/agentteams/scripts/cli-harness-worker-entrypoint.sh"]
