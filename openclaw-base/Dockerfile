# ============================================================
# OpenClaw Base Image - Shared base for Manager and Worker
# ============================================================
# Based on higress/all-in-one so that Manager and Worker share
# the same base layers, avoiding ~900MB duplicate downloads.
#
# Includes: all-in-one (Higress) + Node.js 22 + OpenClaw + mcporter
#           + common tools (git, jq, nginx, etc.)
#
# Build args:
#   HIGRESS_REGISTRY  - base image registry (default: cn-hangzhou)
#   APT_MIRROR        - APT mirror host (default: mirrors.aliyun.com, set to empty to use official)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

# Based on all-in-one (Ubuntu 22.04) for layer sharing with Manager
FROM ${HIGRESS_REGISTRY}/higress/all-in-one:sha-d32debd

# Build args (must be declared BEFORE any RUN that uses them)
ARG APT_MIRROR=mirrors.aliyun.com
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG http_proxy
ARG https_proxy
ARG NPM_REGISTRY=https://registry.npmjs.org/

# Set proxy as environment variables to ensure all tools can use them
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    http_proxy=${http_proxy} \
    https_proxy=${https_proxy}

# all-in-one is based on Ubuntu 22.04, uses sources.list (not ubuntu.sources)
RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i "s|archive.ubuntu.com|${APT_MIRROR}|g; s|security.ubuntu.com|${APT_MIRROR}|g" \
            /etc/apt/sources.list 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y \
    git python3 make g++ curl \
    jq nginx gettext-base openssh-client ca-certificates procps tzdata \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 22 (all-in-one doesn't include Node.js)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y nodejs && \
    npm install -g pnpm && \
    rm -rf /var/lib/apt/lists/*

# Clone, build, and optimize OpenClaw
# 1. Clone with single branch to minimize .git size
# 2. Install dependencies and build
# 3. Remove unnecessary files (tests, .git, etc.) - keep all node_modules for extensions
RUN git clone --depth 1 --single-branch -b hiclaw-v1 \
        https://github.com/johnlanni/openclaw.git /opt/openclaw && \
    cd /opt/openclaw && \
    git fetch --depth 1 origin 86dad1383c12629ad3aa48575f2dd350fc0775d4 && \
    git checkout 86dad1383c12629ad3aa48575f2dd350fc0775d4 && \
    pnpm config set registry ${NPM_REGISTRY} && \
    pnpm install && \
    pnpm build && \
    pnpm ui:build && \
    echo "==> Verifying critical native addons..." && \
    ADDON_DIR=/opt/openclaw/node_modules/.pnpm/@matrix-org+matrix-sdk-crypto-nodejs@0.4.0/node_modules/@matrix-org/matrix-sdk-crypto-nodejs && \
    ls "$ADDON_DIR"/matrix-sdk-crypto.linux-*.node 2>/dev/null | grep -q . \
        || (echo "ERROR: matrix-sdk-crypto native addon missing (pnpm install may have silently failed on this arch)" && exit 1) && \
    echo "==> Native addon check passed: $(ls $ADDON_DIR/matrix-sdk-crypto.linux-*.node)" && \
    rm -rf \
        .git .github \
        test \
        docker-compose.yml Dockerfile* \
        scripts/git-hooks \
        assets README-header.png \
        .secrets.baseline .pre-commit-config.yaml \
        .dockerignore .gitignore .gitattributes \
        .oxfmtrc.jsonc .oxlintrc.json .shellcheckrc \
        .detect-secrets.cfg .npmrc \
        .env.example appcast.xml \
        vitest*.config.ts tsdown.config.ts fly*.toml render.yaml && \
    rm -rf /root/.local/share/pnpm /root/.npm

# Install mcporter
RUN npm install -g mcporter && \
    rm -rf /root/.npm

# Install skills CLI for agent skills ecosystem (skills.sh)
# This enables workers to discover and install skills from the open agent skills ecosystem
RUN npm install -g skills && \
    rm -rf /root/.npm

# Clear proxy env vars (they're only needed during build, not at runtime)
# Without this, curl in the container would try to use the proxy for localhost
ENV HTTP_PROXY= \
    HTTPS_PROXY= \
    http_proxy= \
    https_proxy=
