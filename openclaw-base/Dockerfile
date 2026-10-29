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
#   OPENCLAW_REPO_URL - OpenClaw git repository URL (default: https://github.com/higress-group/openclaw.git)
#   OPENCLAW_TAG      - OpenClaw git tag to check out (default: v2026.10.6;
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
ARG OPENCLAW_REPO_URL=https://github.com/higress-group/openclaw.git
# Pinned checkout tag. Docker cannot source env files, so this default must
# stay in sync with OPENCLAW_TAG in hack/versions.env.
ARG OPENCLAW_TAG=v2026.10.6

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

# Install the latest Node.js 24 LTS (floor: 24.22.1 for the 2026-10-21 security release).
# By default the build resolves the latest 24.x LTS from nodejs.org and verifies its SHA-256.
# For a byte-reproducible build, pass --build-arg NODE_VERSION=24.22.1 (or any 24.x >= floor).
# NODE_MAJOR is fixed to 24: a 25.x/26.x override is rejected even if NODE_MAJOR=24 is passed.
# Docker may reuse this layer: use --no-cache for a forced refresh to the latest 24 LTS.
ARG NODE_MAJOR=24
ARG NODE_MIN_VERSION=24.22.1
ARG NODE_VERSION=
ARG NODE_SHA256=
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl xz-utils jq; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) node_arch="x64" ;; \
      arm64) node_arch="arm64" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    required_major="24"; \
    if [ "${NODE_MAJOR}" != "${required_major}" ]; then \
      echo "NODE_MAJOR=${NODE_MAJOR} is not Node ${required_major} LTS; refusing to build" >&2; exit 1; \
    fi; \
    case "${NODE_MIN_VERSION}" in 24.*) ;; *) echo "NODE_MIN_VERSION=${NODE_MIN_VERSION} is not Node 24.x" >&2; exit 1 ;; esac; \
    ver="${NODE_VERSION}"; \
    if [ -z "$ver" ]; then \
      ver="$(curl -fsSL https://nodejs.org/dist/index.json | jq -r --arg p "v${NODE_MAJOR}." '[.[] | select(.lts != false) | select(.version | startswith($p))][0].version' | sed 's/^v//')"; \
    fi; \
    ver="${ver#v}"; \
    test -n "$ver"; \
    case "$ver" in \
      *[!0-9.]*|*..*|.*|*.) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    case "$ver" in \
      *.*.*) ;; \
      *) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    case "$ver" in \
      *.*.*.*) echo "invalid Node version: $ver" >&2; exit 1 ;; \
    esac; \
    major="${ver%%.*}"; \
    if [ "$major" != "$required_major" ]; then \
      echo "Node $ver is not a Node ${required_major}.x release; refusing a non-${required_major} major" >&2; exit 1; \
    fi; \
    if [ "$(printf '%s\n%s\n' "$NODE_MIN_VERSION" "$ver" | sort -V | head -n1)" != "$NODE_MIN_VERSION" ]; then \
      echo "Node $ver is older than required floor $NODE_MIN_VERSION" >&2; exit 1; \
    fi; \
    tarball="node-v${ver}-linux-${node_arch}.tar.xz"; \
    sha="${NODE_SHA256}"; \
    if [ -z "$sha" ]; then \
      sha="$(curl -fsSL "https://nodejs.org/dist/v${ver}/SHASUMS256.txt" | awk -v f="$tarball" '$2 == f {print $1}' | head -n1)"; \
    fi; \
    test -n "$sha" || { echo "no SHASUMS256.txt entry for ${tarball}" >&2; exit 1; }; \
    curl -fsSLO "https://nodejs.org/dist/v${ver}/${tarball}"; \
    echo "${sha}  ${tarball}" | sha256sum -c -; \
    tar -xJf "$tarball" -C /usr/local --strip-components=1 --no-same-owner; \
    rm -f "$tarball"; \
    node -v; npm -v; \
    test "$(node -v)" = "v${ver}"; \
    rm -rf /var/lib/apt/lists/*

# Match OpenClaw's packageManager to avoid switching to a standalone pnpm
# binary, which its build scripts incorrectly invoke through Node.js.
RUN npm install -g pnpm@10.32.1

# Clone, patch, build, and optimize OpenClaw
# 1. Fetch only the pinned tag to minimize .git size
# 2. Apply the AgentTeams patch series (any conflict fails the build) and
#    commit it — .git is kept so downstream image builds can rebase against
#    the patched tree (see hack/patches/openclaw/README.md)
# 3. Install dependencies and build
# 4. Remove unnecessary files (tests, docs, etc.) - keep all node_modules
#    for extensions; .git is intentionally kept (see step 2)
RUN git init /opt/openclaw && \
    cd /opt/openclaw && \
    git remote add origin ${OPENCLAW_REPO_URL} && \
    git fetch --depth 1 origin "${OPENCLAW_TAG}" && \
    git checkout --detach FETCH_HEAD

COPY --from=openclaw-patches . /tmp/agentteams-patches/
RUN cd /opt/openclaw && \
    for p in /tmp/agentteams-patches/0*.patch; do \
        echo "==> Applying $(basename "$p")" && \
        git apply --check "$p" && \
        git apply "$p" || exit 1; \
    done && \
    git add -A && \
    git -c user.email=build@local -c user.name=build \
        commit -m "agentteams patch series" && \
    rm -rf /tmp/agentteams-patches

RUN cd /opt/openclaw && \
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
