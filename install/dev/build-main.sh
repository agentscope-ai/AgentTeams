#!/usr/bin/env bash
# ============================================================================
# build-main.sh — dev-only builder for AgentTeams images from a git ref.
#
# This is NOT part of the production installer. Run it on a host with Docker
# (the Pi is compile-forbidden; the QwenPaw container has no Docker).
#
# Examples:
#   install/dev/build-main.sh --ref origin/main --component controller --flavor embedded \
#     --platform linux/arm64 --tag agentteams-embedded:pr-1428-$(git rev-parse --short HEAD) --save
#
#   install/dev/build-main.sh --ref origin/main --component controller \
#     --platform linux/amd64 --tag agentteams-controller:dev --push
# ============================================================================
set -u

REF="origin/main"
COMPONENT="controller"
FLAVOR="standalone"
PLATFORM="linux/arm64"
TAG=""
SAVE=0
PUSH=0
KEEP_WORKTREE=0
NO_CACHE=""
DASHBOARD_CONTEXT="${DASHBOARD_CONTEXT:-}"

usage() {
    cat <<EOF
Usage: build-main.sh [options]

  --ref REF              git ref to build (default: origin/main)
  --component C          controller|manager|manager-qwenpaw|worker|qwenpaw-worker|hermes-worker|dashboard|all
  --flavor F             standalone|embedded (controller only; default: standalone)
  --platform P           target platform (default: linux/arm64)
  --tag TAG              image tag (default: agentteams-dev:<short-sha>)
  --save                 docker save the built image to <tag>.tar
  --push                 docker push the built image
  --no-cache             pass --no-cache to docker build
  --keep-worktree        keep the temporary git worktree
  -h, --help

Requires: git, make, docker. The Pi must not run this script.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --ref) REF="$2"; shift 2 ;;
        --component) COMPONENT="$2"; shift 2 ;;
        --flavor) FLAVOR="$2"; shift 2 ;;
        --platform) PLATFORM="$2"; shift 2 ;;
        --tag) TAG="$2"; shift 2 ;;
        --save) SAVE=1; shift ;;
        --push) PUSH=1; shift ;;
        --no-cache) NO_CACHE="--no-cache"; shift ;;
        --keep-worktree) KEEP_WORKTREE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "ERROR: unknown option: $1" >&2; usage; exit 2 ;;
    esac
done

command -v git >/dev/null 2>&1 || { echo "ERROR: git not found" >&2; exit 3; }
command -v make >/dev/null 2>&1 || { echo "ERROR: make not found" >&2; exit 3; }
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found (run this on a Docker host, not the Pi/container)" >&2; exit 3; }
docker info >/dev/null 2>&1 || { echo "ERROR: docker daemon is not running" >&2; exit 3; }

HOST_ARCH="$(uname -m)"
HOST_PLATFORM=""
case "$HOST_ARCH" in
    x86_64|amd64) HOST_PLATFORM="linux/amd64" ;;
    aarch64|arm64) HOST_PLATFORM="linux/arm64" ;;
esac
if [ -n "$HOST_PLATFORM" ] && [ "$PLATFORM" != "$HOST_PLATFORM" ]; then
    echo "NOTE: cross-building $PLATFORM on $HOST_ARCH; checking QEMU binfmt..."
    if ! docker run --rm --platform "$PLATFORM" alpine uname -m >/dev/null 2>&1; then
        echo "ERROR: cannot run $PLATFORM binaries on this host (exec format error)." >&2
        echo "Install QEMU binfmt:" >&2
        echo "  docker run --privileged --rm tonistiigi/binfmt --install arm64" >&2
        echo "Then verify:" >&2
        echo "  docker run --rm --platform $PLATFORM alpine uname -m" >&2
        exit 3
    fi
fi

if ! git rev-parse --verify "$REF" >/dev/null 2>&1; then
    git fetch origin >/dev/null 2>&1 || true
fi
if ! git rev-parse --verify "$REF" >/dev/null 2>&1; then
    echo "ERROR: cannot resolve ref: $REF" >&2
    exit 3
fi

SHA="$(git rev-parse --short "$REF")"
[ -n "$TAG" ] || TAG="agentteams-dev:${SHA}"
WT="$(mktemp -d)/agentteams-build-${SHA}-$$"
CTRL_TAG="${TAG}-controller"
MANIFEST="build-manifest.json"

cleanup() {
    if [ "$KEEP_WORKTREE" = "1" ]; then
        echo "worktree kept: $WT" >&2
    else
        git worktree remove --force "$WT" >/dev/null 2>&1 || rm -rf "$WT"
    fi
}
trap cleanup EXIT

git worktree add --detach "$WT" "$REF" >/dev/null || { echo "ERROR: git worktree add failed" >&2; exit 3; }
echo "==> building $COMPONENT ($FLAVOR) from $REF @ $SHA for $PLATFORM"
echo "    worktree: $WT"
echo "    tag:      $TAG"

build_controller() {
    make -C "$WT" build-agentteams-controller \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$TAG"
}

build_embedded() {
    make -C "$WT" build-embedded \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_EMBEDDED="$TAG"
}

build_manager() {
    make -C "$WT" build-manager \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_CONTROLLER_BUILD_IMAGE="$CTRL_TAG" \
        LOCAL_MANAGER="$TAG"
}

build_manager_qwenpaw() {
    make -C "$WT" build-manager-qwenpaw \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_CONTROLLER_BUILD_IMAGE="$CTRL_TAG" \
        LOCAL_MANAGER_QWENPAW="$TAG"
}

build_worker() {
    make -C "$WT" build-worker \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_CONTROLLER_BUILD_IMAGE="$CTRL_TAG" \
        LOCAL_WORKER="$TAG"
}

build_qwenpaw_worker() {
    make -C "$WT" build-qwenpaw-worker \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_CONTROLLER_BUILD_IMAGE="$CTRL_TAG" \
        LOCAL_QWENPAW_WORKER="$TAG"
}

build_hermes_worker() {
    make -C "$WT" build-hermes-worker \
        DOCKER_PLATFORM="$PLATFORM" \
        DOCKER_BUILD_ARGS="$NO_CACHE" \
        LOCAL_CONTROLLER="$CTRL_TAG" \
        LOCAL_CONTROLLER_BUILD_IMAGE="$CTRL_TAG" \
        LOCAL_HERMES_WORKER="$TAG"
}

case "$COMPONENT" in
    controller)
        if [ "$FLAVOR" = "embedded" ]; then build_embedded; else build_controller; fi
        ;;
    manager) build_manager ;;
    manager-qwenpaw) build_manager_qwenpaw ;;
    worker) build_worker ;;
    qwenpaw-worker) build_qwenpaw_worker ;;
    hermes-worker) build_hermes_worker ;;
    dashboard)
        [ -n "$DASHBOARD_CONTEXT" ] || { echo "ERROR: set DASHBOARD_CONTEXT to the agentteams-dashboard source tree" >&2; exit 3; }
        make -C "$WT" build-dashboard DASHBOARD_CONTEXT="$DASHBOARD_CONTEXT" DASHBOARD_IMAGE="$TAG"
        ;;
    all)
        build_controller
        build_manager
        build_worker
        build_qwenpaw_worker
        ;;
    *) echo "ERROR: unknown component: $COMPONENT" >&2; exit 2 ;;
esac

if ! docker image inspect "$TAG" >/dev/null 2>&1; then
    echo "ERROR: image not found after build: $TAG" >&2
    exit 4
fi

{
    echo "{"
    echo "  \"ref\": \"$(printf '%s' "$REF" | sed 's/"/\\"/g')\","
    echo "  \"sha\": \"$(git -C "$WT" rev-parse HEAD)\","
    echo "  \"dirty\": $(git -C "$WT" status --porcelain | wc -l | tr -d ' '),"
    echo "  \"component\": \"$COMPONENT\","
    echo "  \"flavor\": \"$FLAVOR\","
    echo "  \"platform\": \"$PLATFORM\","
    echo "  \"tag\": \"$(printf '%s' "$TAG" | sed 's/"/\\"/g')\","
    echo "  \"image_id\": \"$(docker image inspect --format '{{.Id}}' "$TAG")\","
    echo "  \"built_at\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\""
    echo "}"
} > "$MANIFEST"
echo "==> manifest: $MANIFEST"

if [ "$SAVE" = "1" ]; then
    OUT="${TAG//[:\/]/_}.tar"
    docker save "$TAG" -o "$OUT"
    echo "==> saved: $OUT"
    echo "    load on the Pi:"
    echo "    cat $OUT | ssh swrp51@192.168.31.8 'docker load'"
fi
if [ "$PUSH" = "1" ]; then
    docker push "$TAG"
    echo "==> pushed: $TAG"
fi
echo "==> done: $TAG"
