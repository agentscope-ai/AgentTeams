ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

FROM node:24-bookworm-slim

ARG DSH_VERSION=0.1.1-rc.2
ARG NODE_MIN_VERSION=24.22.1
# Reject a stale/floating Node 24 that is below the 2026-10-21 security floor.
# The base tag is floating; use docker build --pull to refresh it.
RUN node -e 'const [a,b,c]=process.versions.node.split(".").map(Number); const [x,y,z]=process.argv[1].split(".").map(Number); if (a<x || (a===x&&b<y) || (a===x&&b===y&&c<z)) { console.error(`Node ${process.versions.node} < required ${process.argv[1]}`); process.exit(1) }' "${NODE_MIN_VERSION}"


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
