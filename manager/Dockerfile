# ============================================================
# AgentTeams Manager Agent - OpenClaw Runtime
# ============================================================
# Lightweight Manager container based on openclaw-base.
# Does NOT include Higress, Tuwunel, MinIO, or Element Web.
# All infrastructure is provided by the controller (embedded)
# or K8s Pods (incluster). AGENTTEAMS_RUNTIME env var selects mode.
#
# Build from project root:
#   docker build -f manager/Dockerfile -t agentteams/manager:latest .
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG OPENCLAW_BASE_IMAGE=hiclaw/openclaw-base:20260423-8359cbc
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest

# ============ Stage 1: MinIO Client (mc) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Stage 2: AgentTeams CLI (from controller image) ============
FROM ${AGENTTEAMS_CONTROLLER_IMAGE} AS agentteams-controller

# ============ Final Image ============
FROM ${OPENCLAW_BASE_IMAGE}

# mc (MinIO Client) — direct binary, no STS wrapper needed in K8s mode
COPY --from=mc /usr/bin/mc /usr/local/bin/mc

# AgentTeams CLI — for worker/team/human management via controller REST API
COPY --from=agentteams-controller /usr/local/bin/agt /usr/local/bin/agt
RUN ln -sf /usr/local/bin/agt /usr/local/bin/hiclaw

# ---- Built-in observability plugin (bundled unconditionally, enabled at runtime) ----
# Placed before agent/configs COPY so that code changes do not invalidate this layer.
ARG OPENCLAW_CMS_PLUGIN_URL=https://arms-apm-cn-hangzhou-pre.oss-cn-hangzhou.aliyuncs.com/openclaw-cms-plugin/0.1.2/openclaw-cms-plugin.tar.gz
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

# Ensure openclaw binary is on PATH
ENV PATH="/opt/openclaw/packages/clawdbot/node_modules/.bin:${PATH}"

# ---- Copy Manager agent definitions ----
COPY manager/agent/ /opt/agentteams/agent/
COPY manager/configs/ /opt/agentteams/configs/

# ---- Copy scripts: shared libs first, then manager scripts ----
COPY shared/lib/ /opt/agentteams/scripts/lib/
COPY manager/scripts/ /opt/agentteams/scripts/

ARG BUILTIN_VERSION=latest
RUN echo "${BUILTIN_VERSION}" > /opt/agentteams/agent/.builtin-version

# Make all scripts executable
RUN find /opt/agentteams/scripts -name '*.sh' -exec chmod +x {} + && \
    find /opt/agentteams/agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/agentteams/agent/worker-skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true && \
    find /opt/agentteams/agent/worker-agent/skills/*/scripts -name '*.sh' -exec chmod +x {} + 2>/dev/null || true

# AGENTTEAMS_RUNTIME is NOT hardcoded — injected by Helm (k8s) or SAE (aliyun) at deploy time

# Workspace directory
ENV HOME=/root/manager-workspace
RUN mkdir -p /root/manager-workspace /root/agentteams-fs /root/.openclaw

WORKDIR /root/manager-workspace

# Workaround: lsof hangs in Kind (Docker-in-Docker) when scanning /proc.
# OpenClaw Gateway uses lsof to check port availability before binding;
# replace with a stub that always reports "port not in use" (exit 1).
RUN rm -f /usr/bin/lsof && printf '#!/bin/sh\nexit 1\n' > /usr/bin/lsof && chmod +x /usr/bin/lsof

# OpenClaw gateway port
EXPOSE 18799

# start-manager-agent.sh branches on AGENTTEAMS_RUNTIME env var
ENTRYPOINT ["/opt/agentteams/scripts/init/start-manager-agent.sh"]
