# ============================================================
# OpenClaw Base Image - Shared base for Manager and Worker
# ============================================================
# Based on a plain Ubuntu 24.04 image (mirrored from the higress
# registry for in-China pull speed). The previous all-in-one base
# carried Higress + Envoy + Pilot + Console (~1.7 GB) which the
# modern Manager and Worker do not run — Higress lives in the
# embedded controller image, not here.
#
# Includes: Ubuntu 24.04 + Node.js 22 + OpenClaw + mcporter
#           + common tools (git, jq, etc.)
#
# Build args:
#   HIGRESS_REGISTRY  - base image registry (default: cn-hangzhou)
#   APT_MIRROR        - APT mirror host (default: empty; set to mirrors.aliyun.com for China)
#   OPENCLAW_REPO_URL - OpenClaw git repository URL (default: https://github.com/higress-group/openclaw.git)
# ============================================================

ARG HIGRESS_REGISTRY=higress-registry.cn-hangzhou.cr.aliyuncs.com

# Plain Ubuntu 24.04 (Noble), ~100MB instead of all-in-one's ~1.79GB
FROM ${HIGRESS_REGISTRY}/higress/ubuntu:24.04

# Build args (must be declared BEFORE any RUN that uses them)
ARG APT_MIRROR=
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG http_proxy
ARG https_proxy
ARG NPM_REGISTRY=https://registry.npmjs.org/
ARG OPENCLAW_REPO_URL=https://github.com/higress-group/openclaw.git

# Set proxy as environment variables to ensure all tools can use them
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    http_proxy=${http_proxy} \
    https_proxy=${https_proxy}

# Ubuntu 24.04 ships APT sources in deb822 format at
# /etc/apt/sources.list.d/ubuntu.sources. Older Ubuntu (22.04 and
# earlier) used a single-line /etc/apt/sources.list. Rewrite both
# so the mirror flag stays compatible if the base image is ever
# downgraded or rebased.
RUN if [ -n "${APT_MIRROR}" ]; then \
        for _src in /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources; do \
            [ -f "${_src}" ] && sed -i \
                "s|archive.ubuntu.com|${APT_MIRROR}|g; \
                 s|security.ubuntu.com|${APT_MIRROR}|g; \
                 s|ports.ubuntu.com|${APT_MIRROR}|g" "${_src}" || true; \
        done; \
    fi && \
    apt-get update && apt-get install -y \
    git python3 make g++ curl \
    jq gettext-base openssh-client ca-certificates procps tzdata \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 22 via NodeSource (the base image does not ship Node.js).
# setup_22.x auto-installs gnupg / apt-transport-https if missing.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y nodejs && \
    npm install -g pnpm && \
    rm -rf /var/lib/apt/lists/*

# Clone, build, and optimize OpenClaw
# 1. Clone with single branch to minimize .git size
# 2. Install dependencies and build
# 3. Remove unnecessary files (tests, .git, etc.) - keep all node_modules for extensions
RUN git clone --depth 1 --single-branch -b hiclaw-2026.4.14 \
        ${OPENCLAW_REPO_URL} /opt/openclaw && \
    cd /opt/openclaw && \
    git fetch --depth 1 origin 2f35b6fa6b65d012e3b0c9f24af3f8a4b617a6e0 && \
    git checkout 2f35b6fa6b65d012e3b0c9f24af3f8a4b617a6e0 && \
    pnpm config set registry ${NPM_REGISTRY} && \
    pnpm install && \
    pnpm build && \
    pnpm ui:build && \
    echo "==> Verifying critical native addons..." && \
    MSC_DIR=/opt/openclaw/node_modules/@matrix-org/matrix-sdk-crypto-nodejs && \
    MSC_VER=$(jq -r .version ${MSC_DIR}/package.json) && \
    MSC_ARCH=$(uname -m) && \
    case "${MSC_ARCH}" in \
        x86_64)  MSC_FILE=matrix-sdk-crypto.linux-x64-gnu.node ;; \
        aarch64) MSC_FILE=matrix-sdk-crypto.linux-arm64-gnu.node ;; \
        *)       echo "Unsupported arch: ${MSC_ARCH}" && exit 1 ;; \
    esac && \
    if [ ! -f "${MSC_DIR}/${MSC_FILE}" ]; then \
        echo "==> matrix-sdk-crypto native addon missing — fetching ${MSC_FILE} v${MSC_VER}..."; \
        for i in 1 2 3 4 5; do \
            curl -fsSL --retry 3 --retry-delay 2 \
                "https://github.com/matrix-org/matrix-rust-sdk-crypto-nodejs/releases/download/v${MSC_VER}/${MSC_FILE}" \
                -o "${MSC_DIR}/${MSC_FILE}" && break; \
            echo "==> attempt $i failed, retrying..."; \
            sleep $((i * 3)); \
        done; \
    fi && \
    if [ ! -f "${MSC_DIR}/${MSC_FILE}" ] || [ ! -s "${MSC_DIR}/${MSC_FILE}" ]; then \
        echo "ERROR: failed to fetch matrix-sdk-crypto native addon"; \
        ls -la ${MSC_DIR}/ 2>/dev/null | head -20; \
        exit 1; \
    fi && \
    echo "==> Native addon present: ${MSC_DIR}/${MSC_FILE} ($(stat -c%s ${MSC_DIR}/${MSC_FILE}) bytes)" && \
    chmod +x /opt/openclaw/openclaw.mjs && \
    ln -sf /opt/openclaw/openclaw.mjs /usr/local/bin/openclaw && \
    echo "==> Linked openclaw CLI to /usr/local/bin/openclaw" && \
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

# Install shared CLIs for Worker runtimes:
# - mcporter: MCP tool invocation
# - skills: skills.sh ecosystem
# - @nacos-group/cli: enterprise Nacos skill/agentspec discovery
RUN npm install -g mcporter skills @nacos-group/cli && \
    rm -rf /root/.npm

# Clear proxy env vars (they're only needed during build, not at runtime)
# Without this, curl in the container would try to use the proxy for localhost
ENV HTTP_PROXY= \
    HTTPS_PROXY= \
    http_proxy= \
    https_proxy=
