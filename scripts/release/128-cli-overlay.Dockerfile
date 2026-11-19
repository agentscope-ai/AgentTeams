ARG CONTROLLER_IMAGE
ARG INPUT_IMAGE
FROM ${CONTROLLER_IMAGE} AS controller
FROM ${INPUT_IMAGE} AS harness
ARG AGT_PATH=/usr/local/bin/agt
COPY --from=controller /usr/local/bin/agt ${AGT_PATH}
FROM harness AS embedded
COPY --from=controller /usr/local/bin/agentteams-controller /usr/local/bin/agentteams-controller
