# syntax=docker/dockerfile:1.7

ARG PYTHON_IMAGE=python:3.12.13-slim-bookworm
ARG UV_VERSION=0.11.32

FROM ${PYTHON_IMAGE} AS worker-builder
ARG UV_VERSION
RUN python -m pip install --no-cache-dir "uv==${UV_VERSION}" \
    && python -m venv /opt/venv
COPY deepagents/ /src/deepagents/
COPY deepagents-agentteams/ /src/deepagents-agentteams/
RUN UV_PROJECT_ENVIRONMENT=/opt/venv uv sync \
      --project /src/deepagents-agentteams \
      --frozen \
      --no-dev \
      --no-editable \
    && /opt/venv/bin/python -c \
      "import deepagents, deepagents_agentteams; assert deepagents.__version__.startswith('0.7.3')"

FROM ${PYTHON_IMAGE} AS runner-builder
RUN python -m venv /opt/runner-venv \
    && /opt/runner-venv/bin/pip install --no-cache-dir \
      "fastapi==0.141.1" \
      "uvicorn==0.52.1" \
      "pydantic==2.13.4"
COPY deepagents-agentteams/ /src/deepagents-agentteams/
RUN /opt/runner-venv/bin/pip install --no-cache-dir --no-deps /src/deepagents-agentteams

FROM ${PYTHON_IMAGE} AS worker
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libolm3 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --create-home --home-dir /var/lib/agentteams --shell /usr/sbin/nologin agentteams \
    && mkdir -p /var/lib/agentteams/deepagents \
    && chown -R 65532:65532 /var/lib/agentteams
COPY --from=worker-builder /opt/venv /opt/venv
ENV PATH="/opt/venv/bin:${PATH}" \
    HOME=/var/lib/agentteams \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    AGENTTEAMS_DEEPAGENTS_STATE_DIR=/var/lib/agentteams/deepagents
USER 65532:65532
WORKDIR /var/lib/agentteams
ENTRYPOINT ["deepagents-agentteams-worker"]

FROM ${PYTHON_IMAGE} AS runner
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      jq \
      ripgrep \
      unzip \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --create-home --home-dir /workspace --shell /usr/sbin/nologin agentteams \
    && mkdir -p /workspace \
    && chown 65532:65532 /workspace
COPY --from=runner-builder /opt/runner-venv /opt/runner-venv
ENV PATH="/opt/runner-venv/bin:${PATH}" \
    HOME=/workspace \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    AGENTTEAMS_RUNNER_WORKSPACE=/workspace \
    AGENTTEAMS_RUNNER_STATE_DIR=/tmp/agentteams-runner-state \
    AGENTTEAMS_RUNNER_PORT=8080
USER 65532:65532
WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["deepagents-agentteams-runner"]
