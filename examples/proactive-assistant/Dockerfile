ARG CONTROLLER_IMAGE
ARG EMBEDDED_IMAGE
ARG QWENPAW_IMAGE
FROM ${CONTROLLER_IMAGE} AS controller
FROM ${EMBEDDED_IMAGE} AS embedded
COPY --from=controller /usr/local/bin/agentteams-controller /usr/local/bin/agentteams-controller
COPY --from=controller /usr/local/bin/agt /usr/local/bin/agt
COPY --from=controller /opt/agentteams/config/crd/ /opt/agentteams/config/crd/
COPY --from=controller /opt/agentteams/agent/ /opt/agentteams/agent/
COPY --from=controller /opt/agentteams/plugins/ /opt/agentteams/plugins/
FROM ${QWENPAW_IMAGE} AS qwenpaw
COPY --from=controller /usr/local/bin/agt /usr/local/bin/agt
