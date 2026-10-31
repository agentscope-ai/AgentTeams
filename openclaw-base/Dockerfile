# ============================================================
# AgentTeams OpenClaw Base Image - Shared base for Manager and Worker
# ============================================================
# Based on a plain Ubuntu 24.04 image (mirrored from the higress
# registry for in-China pull speed). The previous all-in-one base
# carried Higress + Envoy + Pilot + Console (~1.7 GB) which the
# modern Manager and Worker do not run — Higress lives in the
# embedded controller image, not here.
#
# Includes: Ubuntu 24.04 + Node.js 24 + OpenClaw + mcporter
#           + common tools (git, jq, etc.)
#
# Build args:
#   HIGRESS_REGISTRY  - base image registry (default: cn-hangzhou)
#   APT_MIRROR        - APT mirror host (default: empty; set to mirrors.aliyun.com for China)
#   OPENCLAW_REPO_URL - OpenClaw git repository URL (default: https://github.com/openclaw/openclaw.git)
#   OPENCLAW_TAG      - OpenClaw git tag to check out (default: v2026.10.9;
#                       keep in sync with OPENCLAW_TAG in hack/versions.env)
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
ARG OPENCLAW_REPO_URL=https://github.com/openclaw/openclaw.git
# Pinned checkout tag. Docker cannot source env files, so this default must
# stay in sync with OPENCLAW_TAG in hack/versions.env.
ARG OPENCLAW_TAG=v2026.10.9

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

# Install Node.js 24 via NodeSource (the base image does not ship Node.js).
# setup_24.x auto-installs gnupg / apt-transport-https if missing.
# OpenClaw requires Node >= 22.14 (no upper cap); Node 24 is the current LTS
# (Node 22 EOLs 2027-04) and matches the coding-CLI "target 24" guidance.
# Match OpenClaw's packageManager to avoid switching to a standalone pnpm
# binary, which its build scripts incorrectly invoke through Node.js.
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - && \
    apt-get install -y nodejs && \
    npm install -g pnpm@12.15.0 && \
    node -e 'const [a,b,c]=process.versions.node.split(".").map(Number); if (a!==24 || a<24 || (a===24&&(b<22 || (b===22&&c<1)))) { console.error(`Node ${process.versions.node} is not Node 24.x >= 24.22.1`); process.exit(1) }' && \
    rm -rf /var/lib/apt/lists/*

# Clone, build, and optimize OpenClaw
# 1. Fetch only the pinned upstream tag (no AgentTeams native Matrix patches;
#    the AgentTeams Matrix bundle is a separate plugin/config projection)
# 2. Install dependencies and build
# 3. Remove unnecessary files (tests, docs) but keep .git for the online-upgrade
#    work (P7 rebase path)
RUN git init /opt/openclaw && \
    cd /opt/openclaw && \
    git remote add origin ${OPENCLAW_REPO_URL} && \
    git fetch --depth 1 origin "${OPENCLAW_TAG}" && \
    git checkout --detach FETCH_HEAD && \
    pnpm config set registry ${NPM_REGISTRY} && \
    pnpm install --frozen-lockfile && \
    pnpm build && \
    pnpm ui:build && \
    echo "==> Verifying critical native addons..." && \
    MSC_PKG=$(find /opt/openclaw/node_modules -path '*/matrix-sdk-crypto-nodejs/package.json' -print -quit) && \
    if [ -z "${MSC_PKG}" ]; then echo "ERROR: matrix-sdk-crypto-nodejs package not found in node_modules"; exit 1; fi && \
    MSC_DIR=$(dirname "${MSC_PKG}") && \
    MSC_VER=$(jq -r .version "${MSC_DIR}/package.json") && \
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
    node -e "require('${MSC_DIR}/${MSC_FILE}'); console.log('==> matrix-sdk-crypto LOAD OK (NAPI/ABI verified) on Node ' + process.version)" && \
    chmod +x /opt/openclaw/openclaw.mjs && \
    ln -sf /opt/openclaw/openclaw.mjs /usr/local/bin/openclaw && \
    echo "==> Linked openclaw CLI to /usr/local/bin/openclaw" && \
    rm -rf \
        .github \
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
