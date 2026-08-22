# ============================================================
# HiClaw Worker Agent - Lightweight Container
# ============================================================
# Based on openclaw-base (which includes all-in-one + Node.js + OpenClaw + mcporter)
# Workers are stateless -- all config/state stored in centralized MinIO.
#
# Build args:
#   HIGRESS_REGISTRY           - base image registry (default: cn-hangzhou)
#   OPENCLAW_BASE_IMAGE        - openclaw-base image (default: hiclaw/openclaw-base:latest)
#   OPENCLAW_CMS_PLUGIN_URL    - observability plugin tarball URL (bundled unconditionally)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG OPENCLAW_BASE_IMAGE=hiclaw/openclaw-base:20260327-74de2f2

# ============ Stage 1: mc (MinIO Client) ============
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ============ Final Image: Based on openclaw-base ============
# openclaw-base already includes: all-in-one + Node.js 22 + OpenClaw + mcporter + common tools
FROM ${OPENCLAW_BASE_IMAGE}

# mc (MinIO Client) — real binary; wrapper installed after shared libs are copied
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin

# ---- Built-in observability plugin (bundled unconditionally, enabled at runtime) ----
# Placed before scripts/lib COPY so that code changes do not invalidate this layer.
ARG OPENCLAW_CMS_PLUGIN_URL=https://arms-apm-cn-hangzhou-pre.oss-cn-hangzhou.aliyuncs.com/openclaw-cms-plugin/0.1.1/openclaw-cms-plugin.tar.gz
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
# OPENCLAW_CONFIG_PATH is intentionally NOT set here:
# - HOME is set to the workspace via docker run -e HOME=...
# - entrypoint creates ~/.openclaw/openclaw.json symlink in that workspace
# - openclaw falls back to ~/.openclaw/openclaw.json when OPENCLAW_CONFIG_PATH is unset
#   which correctly resolves to <workspace>/.openclaw/openclaw.json
ENV PATH="/opt/openclaw/packages/clawdbot/node_modules/.bin:${PATH}"

# Shared environment bootstrap and credential management
COPY --from=shared . /opt/hiclaw/scripts/lib/

# mc wrapper: auto-refreshes STS credentials before every mc invocation (no-op in local mode)
RUN chmod +x /opt/hiclaw/scripts/lib/mc-wrapper.sh && \
    ln -sf /opt/hiclaw/scripts/lib/mc-wrapper.sh /usr/local/bin/mc

# Worker entrypoint and sync scripts
COPY --chmod=755 scripts/ /opt/hiclaw/scripts/
RUN printf '#!/bin/bash\nexec "/root/hiclaw-fs/agents/${HICLAW_WORKER_NAME}/skills/file-sync/scripts/hiclaw-sync.sh" "$@"\n' \
    > /usr/local/bin/hiclaw-sync && chmod +x /usr/local/bin/hiclaw-sync

# hiclaw-fs root (mirrors Manager's ~/hiclaw-fs/ layout)
RUN mkdir -p /root/hiclaw-fs /root/.openclaw

WORKDIR /root/hiclaw-fs

ENTRYPOINT ["/opt/hiclaw/scripts/worker-entrypoint.sh"]
