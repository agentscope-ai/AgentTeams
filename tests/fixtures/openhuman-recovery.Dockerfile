# Build with tests/fixtures as the context. This layer is only for local recovery tests.
ARG OPENHUMAN_TEST_BASE=recovery-1631/openhuman:local
FROM ${OPENHUMAN_TEST_BASE}

RUN marker="$(printf '\177ELF')" && \
    if [ "$(head -c4 /usr/local/bin/openhuman-core)" = "$marker" ]; then \
        mv /usr/local/bin/openhuman-core /usr/local/bin/openhuman-core.native; \
    fi && \
    test "$(head -c4 /usr/local/bin/openhuman-core.native)" = "$marker"
COPY --chmod=755 openhuman_recovery_policy.py /usr/local/bin/openhuman-core
COPY --chmod=755 runtime_recovery_step.py /usr/local/bin/agentteams-recovery-step
