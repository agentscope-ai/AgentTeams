# Build with tests/fixtures as the context. This layer is only for local recovery tests.
ARG OPENHUMAN_TEST_BASE
FROM ${OPENHUMAN_TEST_BASE}

RUN if [ ! -x /usr/local/bin/openhuman-core.native ]; then \
        mv /usr/local/bin/openhuman-core /usr/local/bin/openhuman-core.native; \
    fi
COPY --chmod=755 openhuman_recovery_policy.py /usr/local/bin/openhuman-core
COPY --chmod=755 runtime_recovery_step.py /usr/local/bin/agentteams-recovery-step
