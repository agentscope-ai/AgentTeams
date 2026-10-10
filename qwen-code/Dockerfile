ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

FROM node:22.22-bookworm-slim

ARG QWEN_CODE_VERSION=0.24.7

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        jq \
        python3 \
        tini \
        tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin
COPY shared/lib/ /opt/agentteams/scripts/lib/

RUN npm install --global "@qwen-code/qwen-code@${QWEN_CODE_VERSION}" \
    && npm cache clean --force

COPY qwen-code/scripts/qwen-code-worker-entrypoint.sh /opt/agentteams/scripts/qwen-code-worker-entrypoint.sh
COPY qwen-code/scripts/serve_bridge.py /opt/agentteams/scripts/serve_bridge.py

RUN chmod +x /opt/agentteams/scripts/lib/mc-wrapper.sh \
        /opt/agentteams/scripts/qwen-code-worker-entrypoint.sh \
        /opt/agentteams/scripts/serve_bridge.py \
    && ln -sf /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc \
    && mkdir -p /root/agentteams-fs/agents

WORKDIR /root/agentteams-fs/agents
EXPOSE 8088
EXPOSE 8089
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/agentteams/scripts/qwen-code-worker-entrypoint.sh"]
