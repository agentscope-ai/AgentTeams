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
ARG QWENPAW_PIP_SPEC=qwenpaw==2.2.2.post1
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
        socat \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

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

# Overlay real qwenpaw-worker source without rebuilding the dependency-heavy venv.
COPY qwenpaw/src/ /tmp/qwenpaw-worker/src/
RUN SITE=/opt/venv/qwenpaw/lib/python3.11/site-packages && \
    cp -a /tmp/qwenpaw-worker/src/qwenpaw_worker/* "$SITE/qwenpaw_worker/" && \
    rm -rf /tmp/qwenpaw-worker

# Opaque QwenPaw plugin artifacts. Each plugin build owns its contents.
RUN mkdir -p /opt/agentteams/plugins
COPY dist/adapters/qwenpaw/teamharness-qwenpaw.zip /opt/agentteams/plugins/teamharness-qwenpaw.zip
COPY dist/adapters/qwenpaw/workerflow-qwenpaw.zip /opt/agentteams/plugins/workerflow-qwenpaw.zip
COPY plugins/agentteams-matrix-channel/ /opt/agentteams/plugins/agentteams-matrix-channel/
COPY plugins/agentteams-upgrade/ /opt/agentteams/plugins/agentteams-upgrade/
COPY qwenpaw/scripts/install-builtin-qwenpaw-plugins.py /tmp/install-builtin-qwenpaw-plugins.py
RUN /opt/venv/qwenpaw/bin/python /tmp/install-builtin-qwenpaw-plugins.py \
        --working-dir /opt/agentteams/qwenpaw-builtin \
        /opt/agentteams/plugins/agentteams-matrix-channel \
        /opt/agentteams/plugins/agentteams-upgrade \
        /opt/agentteams/plugins/teamharness-qwenpaw.zip \
        /opt/agentteams/plugins/workerflow-qwenpaw.zip \
    && rm -f /tmp/install-builtin-qwenpaw-plugins.py

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

# QwenPaw 2.2.1 reload must not overwrite a concurrent policy update.
# The patch is also KEPT in the image (not /tmp): the in-place runtime upgrade
# orchestrator re-applies it after an S3 package swap (the pristine wheel does
# not carry the fix). The build-time run below applies it for the 2.2.2.post1 image.
COPY qwenpaw/scripts/patch-qwenpaw-driver-policy-reload.py /opt/agentteams/scripts/patch-qwenpaw-driver-policy-reload.py
COPY qwenpaw/scripts/patch-qwenpaw-matrix-cron-final.py /opt/agentteams/scripts/patch-qwenpaw-matrix-cron-final.py
# In-place runtime upgrade: two-phase orchestrator + checker CLI, kept next to
# the patch script so the orchestrator's `$(dirname $0)` resolution finds both
# the patch (re-applied after an S3 package swap) and the checker.
COPY --chmod=755 qwenpaw/scripts/upgrade_qwenpaw_runtime.sh /opt/agentteams/scripts/upgrade_qwenpaw_runtime.sh
COPY --chmod=755 qwenpaw/scripts/check_qwenpaw_upgrade.py /opt/agentteams/scripts/check_qwenpaw_upgrade.py
COPY qwenpaw/tests/check_driver_policy_reload.py /tmp/check_driver_policy_reload.py
RUN /opt/venv/qwenpaw/bin/python /opt/agentteams/scripts/patch-qwenpaw-driver-policy-reload.py && \
    /opt/venv/qwenpaw/bin/python /opt/agentteams/scripts/patch-qwenpaw-matrix-cron-final.py && \
    /opt/venv/qwenpaw/bin/python /tmp/check_driver_policy_reload.py && \
    rm /tmp/check_driver_policy_reload.py

WORKDIR /root/agentteams-fs/agents

ENTRYPOINT ["/opt/agentteams/scripts/qwenpaw-worker-entrypoint.sh"]
