# ============================================================
# AgentTeams Manager — unified multi-target Dockerfile
# ============================================================
# Shared manager-agent bundle + runtime-specific final stages.
#
# Build OpenClaw Manager (default target):
#   docker build -f manager/Dockerfile -t agentteams/manager:latest .
#
# Build CoPaw Manager:
#   docker build -f manager/Dockerfile --target manager-copaw \
#     -t agentteams/manager-copaw:latest .
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG OPENCLAW_BASE_IMAGE=hiclaw/openclaw-base:20260423-8359cbc
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest

# ============ Stage 1: MinIO Client (mc) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Stage 2: hiclaw CLI (from controller image) ============
FROM ${AGENTTEAMS_CONTROLLER_IMAGE} AS agentteams-controller

# ============ Stage 3: Shared manager agent tree + scripts (G9.7) ============
FROM scratch AS manager-agent-bundle
COPY manager/agent/ /opt/hiclaw/agent/
COPY manager/configs/ /opt/hiclaw/configs/
COPY shared/lib/ /opt/hiclaw/scripts/lib/
COPY manager/scripts/ /opt/hiclaw/scripts/

# ============ CoPaw Manager runtime base ============
FROM ${HIGRESS_REGISTRY}/higress/python:3.11-slim AS manager-copaw-base

ARG APT_MIRROR=
ARG PIP_INDEX_URL=https://pypi.org/simple/
ARG NPM_REGISTRY=https://registry.npmjs.org/
ARG COPAW_VERSION=1.0.2

RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
        libolm-dev \
        libjemalloc2 \
        git curl jq tzdata ca-certificates openssh-client gettext-base procps \
        nodejs npm \
    && rm -rf /var/lib/apt/lists/*

RUN JEMALLOC=$(find /usr/lib -name 'libjemalloc.so.2' -print -quit) \
    && echo "LD_PRELOAD=${JEMALLOC}" >> /etc/environment \
    && echo "${JEMALLOC}" > /etc/ld.so.preload

RUN npm config set registry ${NPM_REGISTRY} && \
    npm install -g mcporter skills @nacos-group/cli && \
    rm -rf /root/.npm

RUN python3 -m venv /opt/copaw-venv
ENV PATH="/opt/copaw-venv/bin:${PATH}"

RUN pip install --no-cache-dir \
        -i "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        "copaw==${COPAW_VERSION}" \
        "matrix-nio[e2e]>=0.24.0"

RUN pip install --no-cache-dir \
        -i "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        "wrapt<2.0" \
        loongsuite-site-bootstrap \
        loongsuite-distro \
        opentelemetry-exporter-otlp \
        loongsuite-instrumentation-copaw \
        loongsuite-instrumentation-agentscope

COPY --from=agentteams-protocol . /tmp/agentteams-protocol/
COPY --from=agentteams-openclaw-merge . /tmp/agentteams-openclaw-merge/
COPY --from=agentteams-sync . /tmp/agentteams-sync/
RUN pip install --no-cache-dir \
        -i "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /tmp/agentteams-protocol/ /tmp/agentteams-openclaw-merge/ /tmp/agentteams-sync/

COPY copaw/src/matrix/ /tmp/matrix-overlay/
RUN SITE=$(python3 -c "import copaw; import os; print(os.path.dirname(copaw.__file__))") \
    && SP=/opt/copaw-venv/lib/python3.11/site-packages \
    && rm -rf "$SITE/app/channels/matrix" \
    && mkdir -p "$SITE/app/channels/matrix" \
    && cp /tmp/matrix-overlay/*.py "$SITE/app/channels/matrix/" \
    && rm -rf "$SP/matrix" \
    && mkdir -p "$SP/matrix" \
    && cp /tmp/matrix-overlay/*.py "$SP/matrix/" \
    && if [ -f /tmp/matrix-overlay/config.py ]; then \
         cp /tmp/matrix-overlay/config.py "$SITE/config/config.py"; \
       fi \
    && rm -rf /tmp/matrix-overlay

COPY copaw/pyproject.toml copaw/README.md /tmp/copaw-worker/
RUN mkdir -p /tmp/copaw-worker/src/copaw_worker \
    && mkdir -p /tmp/copaw-worker/src/matrix \
    && echo '__version__ = "0.0.0"' > /tmp/copaw-worker/src/copaw_worker/__init__.py \
    && echo '__version__ = "0.0.0"' > /tmp/copaw-worker/src/matrix/__init__.py

RUN pip install --no-cache-dir \
        -i "${PIP_INDEX_URL}" \
        --trusted-host "$(echo ${PIP_INDEX_URL} | sed 's|https\?://||;s|/.*||')" \
        /tmp/copaw-worker/

COPY copaw/src/ /tmp/copaw-worker/src/
RUN SITE=/opt/copaw-venv/lib/python3.11/site-packages \
    && cp -a /tmp/copaw-worker/src/copaw_worker/* "$SITE/copaw_worker/" \
    && COPAW_SITE=$(python3 -c "import copaw; import os; print(os.path.dirname(copaw.__file__))") \
    && rm -rf "$COPAW_SITE/app/channels/matrix" "$SITE/matrix" \
    && mkdir -p "$COPAW_SITE/app/channels/matrix" "$SITE/matrix" \
    && cp /tmp/copaw-worker/src/matrix/*.py "$COPAW_SITE/app/channels/matrix/" \
    && cp /tmp/copaw-worker/src/matrix/*.py "$SITE/matrix/" \
    && if [ -f /tmp/copaw-worker/src/matrix/config.py ]; then \
         cp /tmp/copaw-worker/src/matrix/config.py "$COPAW_SITE/config/config.py"; \
       fi \
    && rm -rf /tmp/copaw-worker/

# ============ CoPaw Manager ============
FROM manager-copaw-base AS manager-copaw

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest
ARG BUILTIN_VERSION=latest

COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY --from=agentteams-controller /usr/local/bin/hiclaw /usr/local/bin/hiclaw

COPY --from=manager-agent-bundle /opt/hiclaw/agent /opt/hiclaw/agent
COPY --from=manager-agent-bundle /opt/hiclaw/configs /opt/hiclaw/configs
COPY --from=manager-agent-bundle /opt/hiclaw/scripts /opt/hiclaw/scripts

RUN bash /opt/hiclaw/scripts/lib/sync-shared-worker-skills.sh /opt/hiclaw/agent && \
    bash /opt/hiclaw/scripts/lib/render-manager-prompts.sh all /opt/hiclaw/agent && \
    echo "${BUILTIN_VERSION}" > /opt/hiclaw/agent/.builtin-version && \
    find /opt/hiclaw/scripts -name '*.sh' -exec chmod +x {} + && \
    find /opt/hiclaw/agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/hiclaw/agent/worker-skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/hiclaw/agent/worker-agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true

ENV HOME=/root/manager-workspace
RUN mkdir -p /root/manager-workspace /root/hiclaw-fs /root/.openclaw

WORKDIR /root/manager-workspace
ENTRYPOINT ["/opt/hiclaw/scripts/init/start-manager-agent.sh"]

# ============ OpenClaw Manager (default target — last stage) ============
FROM ${OPENCLAW_BASE_IMAGE} AS manager-openclaw

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest
ARG OPENCLAW_CMS_PLUGIN_URL=https://arms-apm-cn-hangzhou-pre.oss-cn-hangzhou.aliyuncs.com/openclaw-cms-plugin/0.1.2/openclaw-cms-plugin.tar.gz
ARG BUILTIN_VERSION=latest

COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY --from=agentteams-controller /usr/local/bin/hiclaw /usr/local/bin/hiclaw

ENV OPENCLAW_CMS_PLUGIN_DIR="/opt/openclaw/extensions/openclaw-cms-plugin"
RUN tmp_dir="$(mktemp -d)" && \
    curl -fsSL "${OPENCLAW_CMS_PLUGIN_URL}" -o "${tmp_dir}/plugin.tar.gz" && \
    mkdir -p "${OPENCLAW_CMS_PLUGIN_DIR}" && \
    tar -xzf "${tmp_dir}/plugin.tar.gz" -C "${tmp_dir}" && \
    if [ -d "${tmp_dir}/openclaw-cms-plugin" ]; then \
      cp -rf "${tmp_dir}/openclaw-cms-plugin/." "${OPENCLAW_CMS_PLUGIN_DIR}/"; \
    else \
      cp -rf "${tmp_dir}/." "${OPENCLAW_CMS_PLUGIN_DIR}/"; \
    fi && \
    cd "${OPENCLAW_CMS_PLUGIN_DIR}" && \
    npm install --omit=dev --ignore-scripts && \
    rm -rf "${tmp_dir}"

ENV PATH="/opt/openclaw/packages/clawdbot/node_modules/.bin:${PATH}"

COPY --from=manager-agent-bundle /opt/hiclaw/agent /opt/hiclaw/agent
COPY --from=manager-agent-bundle /opt/hiclaw/configs /opt/hiclaw/configs
COPY --from=manager-agent-bundle /opt/hiclaw/scripts /opt/hiclaw/scripts

COPY --from=agentteams-openclaw-merge . /tmp/agentteams-openclaw-merge/
RUN apt-get update && apt-get install -y --no-install-recommends python3-pip \
    && python3 -m pip install --no-cache-dir --break-system-packages \
        /tmp/agentteams-openclaw-merge/ \
    && rm -rf /tmp/agentteams-openclaw-merge /var/lib/apt/lists/*

RUN bash /opt/hiclaw/scripts/lib/sync-shared-worker-skills.sh /opt/hiclaw/agent && \
    bash /opt/hiclaw/scripts/lib/render-manager-prompts.sh all /opt/hiclaw/agent && \
    echo "${BUILTIN_VERSION}" > /opt/hiclaw/agent/.builtin-version && \
    find /opt/hiclaw/scripts -name '*.sh' -exec chmod +x {} + && \
    find /opt/hiclaw/agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/hiclaw/agent/worker-skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/hiclaw/agent/worker-agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true

ENV HOME=/root/manager-workspace
RUN mkdir -p /root/manager-workspace /root/hiclaw-fs /root/.openclaw

WORKDIR /root/manager-workspace

RUN rm -f /usr/bin/lsof && printf '#!/bin/sh\nexit 1\n' > /usr/bin/lsof && chmod +x /usr/bin/lsof

EXPOSE 18799
ENTRYPOINT ["/opt/hiclaw/scripts/init/start-manager-agent.sh"]
