# ---------------------------------------------------------------------------
# OpenHuman Worker — HiClaw-integrated multi-stage Docker build
#
# Based on the upstream OpenHuman Dockerfile with the following changes:
#   - Enables `channel-matrix` feature flag for native Matrix support
#   - Installs mc (MinIO Client) for file sync with HiClaw centralized storage
#   - Installs jq for JSON processing in entrypoint scripts
#   - Copies HiClaw shared scripts and OpenHuman worker entrypoint
#   - Copies OpenHuman worker agent template (AGENTS.md, skills)
#
# License notice:
#   This image contains openhuman-core, licensed under GPL-3.0.
#   Source: https://github.com/tinyhumansai/openhuman (see OPENHUMAN_GIT_REF).
#   The GPL-3.0 license text is preserved at /usr/share/licenses/openhuman/LICENSE
#   inside the built image.
#
# Build context: HiClaw repo root (to access shared/lib/ and manager/agent/).
# OpenHuman upstream source is fetched via git inside the builder stage —
# no `openhuman-src/` directory is required in the build context.
#
# Build:   docker build -t hiclaw/openhuman-worker:latest -f openhuman/Dockerfile .
#          # (optional) pin to a specific upstream ref:
#          docker build --build-arg OPENHUMAN_GIT_REF=v0.1.0 \
#                       -t hiclaw/openhuman-worker:latest -f openhuman/Dockerfile .
# Run:     docker run --env-file .env hiclaw/openhuman-worker:latest
# ---------------------------------------------------------------------------

# Top-level build args (must be declared before any FROM that references them).
ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com
ARG AGENTTEAMS_CONTROLLER_IMAGE=agentteams/agentteams-controller:latest

# ==========================================================================
# Stage 0: mc (MinIO Client) — pulled from internal image instead of dl.min.io
# Aligned with hermes / copaw / worker Dockerfiles to avoid public CDN latency.
# ==========================================================================
FROM ${HIGRESS_REGISTRY}/higress/mc:20260216 AS mc

# ==========================================================================
# Stage 1: hiclaw CLI (from controller image)
# ==========================================================================
FROM ${AGENTTEAMS_CONTROLLER_IMAGE} AS agentteams-controller

# ==========================================================================
# Stage 2: Build the Rust binary with channel-matrix enabled
# ==========================================================================
FROM rust:1.93-bookworm AS builder

ENV DEBIAN_FRONTEND=noninteractive

# Optional Debian mirror for environments where deb.debian.org is slow / blocked.
# Pass `--build-arg APT_MIRROR=mirrors.aliyun.com` (or any working mirror host)
# to redirect apt to that mirror. Default empty = use the image's original source.
ARG APT_MIRROR=

# System dependencies required for compilation.
# ALSA / X11 / input headers are needed because cpal, enigo, arboard,
# and rdev are unconditional dependencies. matrix-sdk requires libssl-dev.
RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    pkg-config \
    libssl-dev \
    libasound2-dev \
    libxdo-dev \
    libxtst-dev \
    libx11-dev \
    libevdev-dev \
    clang \
    mold \
    ca-certificates \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# OpenHuman upstream source.
# Override via `docker --build-arg OPENHUMAN_REPO_URL=... OPENHUMAN_GIT_REF=...`
# to use a fork or a different release tag / commit.
#
# Default pinned to v0.54.0 (released 2026-05-19), the latest stable
# release of tinyhumansai/openhuman as of integration. Bump deliberately,
# don't drift to `main`, so builds stay reproducible.
ARG OPENHUMAN_REPO_URL=https://github.com/tinyhumansai/openhuman.git
ARG OPENHUMAN_GIT_REF=v0.54.0

# Pre-fetch openhuman source via git so the build is self-contained
# (no requirement for an `openhuman-src/` directory in the build context).
# Pinning to a tag / commit makes builds reproducible.
RUN git clone --depth 1 --branch "${OPENHUMAN_GIT_REF}" "${OPENHUMAN_REPO_URL}" /build/openhuman-src

WORKDIR /build/openhuman-src

# Build with channel-matrix feature enabled (native Matrix support).
RUN cargo build --release --bin openhuman-core --features channel-matrix

# ==========================================================================
# Stage 3: Minimal runtime image with HiClaw integration
# ==========================================================================
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Re-declare APT_MIRROR for this stage (ARGs do not cross stages).
ARG APT_MIRROR=

RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list.d/debian.sources 2>/dev/null || true; \
        sed -i "s|deb.debian.org|${APT_MIRROR}|g; s|security.debian.org|${APT_MIRROR}|g" \
            /etc/apt/sources.list 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libssl3 \
    libasound2 \
    libxdo3 \
    libxtst6 \
    libx11-6 \
    libevdev2 \
    curl \
    jq \
    openssl \
    gosu \
    bash \
    && rm -rf /var/lib/apt/lists/*

# Install mc (MinIO Client) — pulled from the internal mc stage so the
# build does not depend on dl.min.io public CDN reachability.
COPY --from=mc /usr/bin/mc /usr/local/bin/mc.bin
RUN chmod +x /usr/local/bin/mc.bin

# Non-root user — fixed UID/GID for stable volume ownership
RUN groupadd --gid 10001 openhuman \
 && useradd --uid 10001 --gid 10001 --create-home --shell /bin/bash openhuman

ENV HOME=/home/openhuman

# Pre-create workspace directory
RUN mkdir -p /home/openhuman/.openhuman/skills \
             /home/openhuman/.openhuman/memory \
             /home/openhuman/.openhuman/shared \
             /home/openhuman/.openhuman/config \
             /home/openhuman/.openhuman/agent-config \
 && chown -R openhuman:openhuman /home/openhuman

# Copy HiClaw shared scripts
COPY shared/lib/ /opt/agentteams/scripts/lib/
RUN chmod +x /opt/agentteams/scripts/lib/*.sh 2>/dev/null || true

# Install mc-wrapper as /usr/local/bin/mc (STS credential refresh proxy)
RUN cp /opt/agentteams/scripts/lib/mc-wrapper.sh /usr/local/bin/mc \
 && chmod +x /usr/local/bin/mc

# Copy OpenHuman worker entrypoint
COPY openhuman/scripts/openhuman-worker-entrypoint.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/openhuman-worker-entrypoint.sh

# Copy OpenHuman worker agent template (AGENTS.md + skills)
COPY manager/agent/openhuman-worker-agent/ /opt/agentteams/agent/openhuman-worker-agent/

# AgentTeams CLI — for worker lifecycle management via controller REST API
COPY --from=agentteams-controller /usr/local/bin/agt /usr/local/bin/agt
RUN ln -sf /usr/local/bin/agt /usr/local/bin/hiclaw

# Copy the built binary and upstream license (GPL-3.0 compliance: recipients
# must be able to identify the license of the included GPL component).
COPY --from=builder /build/openhuman-src/target/release/openhuman-core /usr/local/bin/openhuman-core
COPY --from=builder /build/openhuman-src/LICENSE /usr/share/licenses/openhuman/LICENSE

# The entrypoint runs as root to set up mc credentials and volumes,
# then runs openhuman-core as the main process.
USER root

# Default environment
ENV OPENHUMAN_WORKSPACE=/home/openhuman/.openhuman
ENV OPENHUMAN_CORE_HOST=0.0.0.0
ENV OPENHUMAN_CORE_PORT=7788
ENV RUST_LOG=info

EXPOSE 7788

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -sf http://localhost:7788/health || exit 1

ENTRYPOINT ["/usr/local/bin/openhuman-worker-entrypoint.sh"]
