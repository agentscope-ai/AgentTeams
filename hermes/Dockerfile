# ============================================================
# AgentTeams Hermes Worker Agent - Python-based Container
# ============================================================
# Lightweight Python container running hermes-agent v0.10.0 as a
# AgentTeams Worker. Uses the same MinIO-backed config/sync model as the
# OpenClaw / CoPaw workers, with hermes-agent providing the agent loop
# and tools. The Matrix gateway adapter keeps hermes-agent's native
# mautrix implementation and overlays only AgentTeams-specific policy
# hooks (allowlists, mention enrichment, history, vision gating).
#
# Build args:
#   HIGRESS_REGISTRY        - base image registry for mc + python
#   AGENTTEAMS_CONTROLLER_IMAGE - source for the agt CLI binary
#   APT_MIRROR              - Debian mirror (default: empty / official)
#   PIP_INDEX_URL           - PyPI mirror (default: official PyPI)
#   NPM_REGISTRY            - npm registry (default: official)
#   HERMES_GIT_REF          - git ref of hermes-agent to install
#                             (default: v2026.4.16, which is the
#                             "Hermes Agent v0.10.0" release)
#   HERMES_REPO_URL         - hermes-agent git repo URL
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
# v2026.4.16 is the "Hermes Agent v0.10.0 (2026.4.16)" calendar tag
# (commit 1dd6b5d5 on the upstream repo).
ARG HERMES_GIT_REF=v2026.4.16
ARG HERMES_REPO_URL=https://github.com/NousResearch/hermes-agent.git

# python:3.11-slim is Debian-based, uses /etc/apt/sources.list.d/debian.sources
RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        libolm-dev \
        libffi-dev \
        libjemalloc2 \
        curl \
        git \
        jq \
        ripgrep \
        ffmpeg \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

# jemalloc reduces Python memory fragmentation (~10-20% RSS savings).
RUN JEMALLOC=$(find /usr/lib -name 'libjemalloc.so.2' -print -quit) \
    && echo "LD_PRELOAD=${JEMALLOC}" >> /etc/environment \
    && echo "${JEMALLOC}" > /etc/ld.so.preload

# mc (MinIO Client) — real binary; wrapper installed after shared libs are copied
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin

# AgentTeams CLI — for worker/team/human management via controller REST API
COPY --from=agentteams-controller /usr/local/bin/agt /usr/local/bin/agt

# Install the latest Node.js 24 LTS (floor: 24.22.1 for the 2026-10-21 security release).
# By default the build resolves the latest 24.x LTS from nodejs.org and verifies its SHA-256.
# For a byte-reproducible build, pass --build-arg NODE_VERSION=24.22.1 (or any 24.x >= floor).
# NODE_MAJOR is fixed to 24: a 25.x/26.x override is rejected even if NODE_MAJOR=24 is passed.
# Docker may reuse this layer: use --no-cache for a forced refresh to the latest 24 LTS.
ARG NODE_MAJOR=24
ARG NODE_MIN_VERSION=24.22.1
ARG NODE_VERSION=
ARG NODE_SHA256=
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl xz-utils jq; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) node_arch="x64" ;; \
      arm64) node_arch="arm64" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    required_major="24"; \
    if [ "${NODE_MAJOR}" != "${required_major}" ]; then \
      echo "NODE_MAJOR=${NODE_MAJOR} is not Node ${required_major} LTS; refusing to build" >&2; exit 1; \
    fi; \
    case "${NODE_MIN_VERSION}" in 24.*) ;; *) echo "NODE_MIN_VERSION=${NODE_MIN_VERSION} is not Node 24.x" >&2; exit 1 ;; esac; \
    ver="${NODE_VERSION}"; \
    if [ -z "$ver" ]; then \
      ver="$(curl -fsSL https://nodejs.org/dist/index.json | jq -r --arg p "v${NODE_MAJOR}." '[.[] | select(.lts != false) | select(.version | startswith($p))][0].version' | sed 's/^v//')"; \
    fi; \
    ver="${ver#v}"; \
    test -n "$ver"; \
    case "$ver" in \
      *[!0-9.]*|*..*|.*|*.) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    case "$ver" in \
      *.*.*) ;; \
      *) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    case "$ver" in \
      *.*.*.*) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    major="${ver%%.*}"; \
    if [ "$major" != "$required_major" ]; then \
      echo "Node $ver is not a Node ${required_major}.x release; refusing a non-${required_major} major" >&2; exit 1; \
    fi; \
    if [ "$(printf '%s\n%s\n' "$NODE_MIN_VERSION" "$ver" | sort -V | head -n1)" != "$NODE_MIN_VERSION" ]; then \
      echo "Node $ver is older than required floor $NODE_MIN_VERSION" >&2; exit 1; \
    fi; \
    tarball="node-v${ver}-linux-${node_arch}.tar.xz"; \
    sha="${NODE_SHA256}"; \
    if [ -z "$sha" ]; then \
      sha="$(curl -fsSL "https://nodejs.org/dist/v${ver}/SHASUMS256.txt" | awk -v f="$tarball" '$2 == f {print $1}' | head -n1)"; \
    fi; \
    test -n "$sha" || { echo "no SHASUMS256.txt entry for ${tarball}" >&2; exit 1; }; \
    curl -fsSLO "https://nodejs.org/dist/v${ver}/${tarball}"; \
    echo "${sha}  ${tarball}" | sha256sum -c -; \
    tar -xJf "$tarball" -C /usr/local --strip-components=1 --no-same-owner; \
    rm -f "$tarball"; \
    node -v; npm -v; \
    test "$(node -v)" = "v${ver}"; \
    rm -rf /var/lib/apt/lists/*

RUN npm config set registry ${NPM_REGISTRY} && \
    npm install -g mcporter skills @nacos-group/cli && \
    rm -rf /root/.npm

# --- Heavy install layer (cached unless HERMES_GIT_REF or hermes-worker
#     pyproject.toml change) ---
#
# Stage hermes-worker dependency metadata for layer caching: pyproject.toml
# changes invalidate the heavy install layer below; src/ changes don't.
COPY pyproject.toml README.md /tmp/hermes-worker/
RUN mkdir -p /tmp/hermes-worker/src/hermes_worker \
    && mkdir -p /tmp/hermes-worker/src/hermes_matrix \
    && echo '__version__ = "0.0.0"' > /tmp/hermes-worker/src/hermes_worker/__init__.py \
    && echo '__version__ = "0.0.0"' > /tmp/hermes-worker/src/hermes_matrix/__init__.py

# Pre-fetch hermes-agent into a local checkout. Pinning to a specific tag
# makes the build reproducible. Two-step (clone + install) avoids re-fetching
# when only hermes-worker source changes.
RUN git clone --depth 1 --branch "${HERMES_GIT_REF}" "${HERMES_REPO_URL}" /opt/hermes-src

RUN python -m venv /opt/venv/hermes \
    && /opt/venv/hermes/bin/pip install --upgrade pip setuptools wheel \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
    && /opt/venv/hermes/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /opt/hermes-src \
    && /opt/venv/hermes/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
         "mautrix[encryption]" \
        "matrix-nio[e2e]>=0.24.0" \
        "markdown-it-py>=3.0" \
        "linkify-it-py>=2.0" \
        "Markdown>=3.6" \
        "typer>=0.9" \
        "rich>=13.0" \
        "httpx>=0.25" \
        "pyyaml>=6.0" \
    && /opt/venv/hermes/bin/pip install --no-cache-dir \
        --index-url "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /tmp/hermes-worker/

# --- Overlay real hermes-worker source (high-frequency changes) ---
# This layer is rebuilt on every src/ change but does NOT invalidate the
# heavy install layers above, saving ~1-2 GB of re-download on typical edits.
COPY src/ /tmp/hermes-worker/src/
RUN SITE=/opt/venv/hermes/lib/python3.11/site-packages \
    && cp -a /tmp/hermes-worker/src/hermes_worker/* "$SITE/hermes_worker/" \
    && cp -a /tmp/hermes-worker/src/hermes_matrix/* "$SITE/hermes_matrix/" \
    && rm -rf /tmp/hermes-worker/

# --- Overlay our Matrix shim into hermes-agent's gateway/platforms/ ---
# Keep hermes-agent's native mautrix transport by renaming its original
# gateway/platforms/matrix.py to _matrix_native.py, then install a shim at
# the original import path.  The shim re-exports the native module and swaps
# in hermes_matrix.adapter.MatrixAdapter, a thin subclass that only adds
# AgentTeams policy hooks (mentions, allowlists, history, vision gating).
RUN SITE=/opt/venv/hermes/lib/python3.11/site-packages \
    && mv "$SITE/gateway/platforms/matrix.py" "$SITE/gateway/platforms/_matrix_native.py" \
    && cp "$SITE/hermes_matrix/_shim.py" "$SITE/gateway/platforms/matrix.py"

# Symlinks for hermes CLI on PATH (used by entrypoint and skills)
RUN if [ -f /opt/venv/hermes/bin/hermes ]; then \
        ln -sf /opt/venv/hermes/bin/hermes /usr/local/bin/hermes; \
    fi \
    && if [ -f /opt/venv/hermes/bin/hermes-agent ]; then \
        ln -sf /opt/venv/hermes/bin/hermes-agent /usr/local/bin/hermes-agent; \
    fi \
    && ln -sf /opt/venv/hermes/bin/hermes-worker /usr/local/bin/hermes-worker

# Entrypoint script
COPY --chmod=755 scripts/hermes-worker-entrypoint.sh /opt/agentteams/scripts/hermes-worker-entrypoint.sh

# Shared environment bootstrap and credential management
# (agentteams-env.sh, oss-credentials.sh) — same named build-context as copaw.
COPY --from=shared . /opt/agentteams/scripts/lib/

# mc wrapper: auto-refreshes STS credentials before every mc invocation
# (no-op in local mode).
RUN chmod +x /opt/agentteams/scripts/lib/mc-wrapper.sh && \
    ln -sf /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc

# Workspace lives under the controller-injected HOME=/root/agentteams-fs/agents/<name>
# so the layout matches the openclaw worker (HOME == workspace == MinIO mirror
# root). The per-worker subdir is created by the entrypoint at startup.
RUN mkdir -p /root/agentteams-fs/agents

WORKDIR /root/agentteams-fs/agents

# HERMES_HOME is set per-worker by the entrypoint (<workspace>/.hermes); we
# deliberately leave it unset in the image so `docker exec` can't see a wrong
# default that points at a non-existent path.

ENTRYPOINT ["/opt/agentteams/scripts/hermes-worker-entrypoint.sh"]
