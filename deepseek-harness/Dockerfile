ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

FROM node:22.22-bookworm-slim

ARG DSH_VERSION=0.1.1-rc.2

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

RUN npm install --global "pnpm@11.7.0" "@deepseek-ai/dsh@${DSH_VERSION}" \
    && npm cache clean --force

COPY plugins/teamharness/ /opt/agentteams/plugins/teamharness/
RUN mkdir -p /opt/agentteams/dsh-template /tmp/adapter-pack \
    && DSH_HOME=/opt/agentteams/dsh-template pnpm --dir /opt/agentteams/plugins/teamharness/adapters/deepseek-harness pack --pack-destination /tmp/adapter-pack \
    && DSH_HOME=/opt/agentteams/dsh-template dsh plugin --profile headless add /tmp/adapter-pack/*.tgz \
    && rm -rf /tmp/adapter-pack

COPY deepseek-harness/scripts/deepseek-harness-worker-entrypoint.sh /opt/agentteams/scripts/deepseek-harness-worker-entrypoint.sh
COPY deepseek-harness/scripts/agentteams-dsh /usr/local/bin/agentteams-dsh
COPY deepseek-harness/scripts/bridge_state.py /opt/agentteams/scripts/bridge_state.py
COPY deepseek-harness/scripts/matrix_channel.py /opt/agentteams/scripts/matrix_channel.py
COPY deepseek-harness/scripts/matrix_bridge.py /opt/agentteams/scripts/matrix_bridge.py
COPY deepseek-harness/scripts/runtime_env.py /opt/agentteams/scripts/runtime_env.py

RUN chmod +x /opt/agentteams/scripts/lib/mc-wrapper.sh \
        /opt/agentteams/scripts/deepseek-harness-worker-entrypoint.sh \
        /opt/agentteams/scripts/matrix_bridge.py \
        /opt/agentteams/scripts/runtime_env.py \
        /usr/local/bin/agentteams-dsh \
    && ln -sf /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc \
    && mkdir -p /root/agentteams-fs/agents

WORKDIR /root/agentteams-fs/agents
EXPOSE 8088
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/agentteams/scripts/deepseek-harness-worker-entrypoint.sh"]
