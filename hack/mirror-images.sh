#!/bin/bash
# mirror-images.sh - Mirror upstream images to Higress registry (multi-arch)
#
# Uses skopeo to copy multi-arch manifests from upstream registries to
# the cn-hangzhou PRIMARY registry. Regional mirrors auto-sync from it:
#
#   Primary (push target): higress-registry.cn-hangzhou.cr.aliyuncs.com
#   Mirror (North America): higress-registry.us-west-1.cr.aliyuncs.com
#   Mirror (Southeast Asia): higress-registry.ap-southeast-7.cr.aliyuncs.com
#
# Prerequisites:
#   - skopeo installed (or use the container mode below)
#   - Logged in to the target registry
#
# Usage:
#   # Mirror all images (interactive login prompt)
#   ./hack/mirror-images.sh
#
#   # Mirror a single image by name
#   ./hack/mirror-images.sh tuwunel
#
#   # Dry-run (show commands without executing)
#   DRY_RUN=1 ./hack/mirror-images.sh
#
#   # Use skopeo container instead of local binary
#   USE_CONTAINER=1 ./hack/mirror-images.sh
#
#   # Override target registry / namespace
#   TARGET_REGISTRY=my-registry.example.com TARGET_NS=myns ./hack/mirror-images.sh

set -euo pipefail

# ============================================================
# Configuration
# ============================================================

TARGET_REGISTRY="${TARGET_REGISTRY:-higress-registry.cn-hangzhou.cr.aliyuncs.com}"
TARGET_NS="${TARGET_NS:-higress}"
TARGET_PREFIX="docker://${TARGET_REGISTRY}/${TARGET_NS}"

DATE_TAG="${DATE_TAG:-$(date +%Y%m%d)}"
DRY_RUN="${DRY_RUN:-}"
USE_CONTAINER="${USE_CONTAINER:-}"
SKOPEO_IMAGE="${SKOPEO_IMAGE:-quay.io/skopeo/stable:latest}"

# ============================================================
# Image mapping: SOURCE -> TARGET_NAME:TAG
#
# Format: "source_image|target_name|target_tag"
# ============================================================

IMAGES=(
    "ghcr.io/matrix-construct/tuwunel:main|tuwunel|${DATE_TAG}"
    "quay.io/minio/minio:latest|minio|${DATE_TAG}"
    "quay.io/minio/mc:latest|mc|${DATE_TAG}"
    "docker.io/vectorim/element-web:latest|element-web|${DATE_TAG}"
    "docker.io/library/node:20-slim|node|20-slim"
    "docker.io/library/ubuntu:24.04|ubuntu|24.04"
    "docker.io/library/golang:1.23-alpine|golang|1.23-alpine"
    "docker.io/library/alpine:3.20|alpine|3.20"
)

# ============================================================
# Helpers
# ============================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()   { echo -e "${CYAN}[mirror]${NC} $1"; }
ok()    { echo -e "${GREEN}[  OK  ]${NC} $1"; }
fail()  { echo -e "${RED}[ FAIL ]${NC} $1"; }
warn()  { echo -e "${YELLOW}[ WARN ]${NC} $1"; }

run_skopeo() {
    if [ -n "${USE_CONTAINER}" ]; then
        docker run --rm \
            -v "${HOME}/.config/containers:/.config/containers:ro" \
            "${SKOPEO_IMAGE}" \
            "$@"
    else
        skopeo "$@"
    fi
}

# ============================================================
# Login check / prompt
# ============================================================

check_login() {
    log "Checking authentication to ${TARGET_REGISTRY}..."

    if run_skopeo inspect "docker://${TARGET_REGISTRY}/${TARGET_NS}/all-in-one:latest" > /dev/null 2>&1; then
        ok "Already authenticated to ${TARGET_REGISTRY}"
        return 0
    fi

    warn "Not authenticated. Please login:"
    if [ -n "${USE_CONTAINER}" ]; then
        echo "  docker run -it --rm ${SKOPEO_IMAGE} login ${TARGET_REGISTRY}"
        echo ""
        echo "Or set USE_CONTAINER= and login locally:"
        echo "  skopeo login ${TARGET_REGISTRY}"
    else
        echo "  skopeo login ${TARGET_REGISTRY}"
    fi

    read -p "Attempt login now? (y/N) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        if [ -n "${USE_CONTAINER}" ]; then
            docker run -it --rm \
                -v "${HOME}/.config/containers:/.config/containers" \
                "${SKOPEO_IMAGE}" \
                login "${TARGET_REGISTRY}"
        else
            skopeo login "${TARGET_REGISTRY}"
        fi
    else
        fail "Login required. Exiting."
        exit 1
    fi
}

# ============================================================
# Copy a single image
# ============================================================

copy_image() {
    local entry="$1"
    local source_image target_name target_tag

    IFS='|' read -r source_image target_name target_tag <<< "${entry}"

    local target="${TARGET_PREFIX}/${target_name}:${target_tag}"

    log "Copying: docker://${source_image}"
    log "     -> ${target}"

    if [ -n "${DRY_RUN}" ]; then
        warn "[dry-run] skopeo copy --multi-arch all docker://${source_image} ${target}"
        return 0
    fi

    if run_skopeo copy --multi-arch all \
        "docker://${source_image}" \
        "${target}"; then
        ok "${target_name}:${target_tag} mirrored successfully"
    else
        fail "${target_name}:${target_tag} FAILED"
        return 1
    fi
}

# ============================================================
# Main
# ============================================================

main() {
    echo ""
    echo "============================================"
    echo "  HiClaw Image Mirror (skopeo multi-arch)"
    echo "============================================"
    echo "  Target:  ${TARGET_REGISTRY}/${TARGET_NS}/"
    echo "  Date tag: ${DATE_TAG}"
    [ -n "${DRY_RUN}" ] && echo "  Mode:    DRY RUN"
    [ -n "${USE_CONTAINER}" ] && echo "  Skopeo:  container (${SKOPEO_IMAGE})"
    echo "============================================"
    echo ""

    # Filter by name if argument given
    local filter="${1:-}"
    local to_copy=()

    if [ -n "${filter}" ]; then
        for entry in "${IMAGES[@]}"; do
            local name
            name=$(echo "${entry}" | cut -d'|' -f2)
            if [ "${name}" = "${filter}" ]; then
                to_copy+=("${entry}")
            fi
        done
        if [ ${#to_copy[@]} -eq 0 ]; then
            fail "No image matching '${filter}'. Available:"
            for entry in "${IMAGES[@]}"; do
                echo "  - $(echo "${entry}" | cut -d'|' -f2)"
            done
            exit 1
        fi
    else
        to_copy=("${IMAGES[@]}")
    fi

    # Show plan
    log "Images to mirror:"
    for entry in "${to_copy[@]}"; do
        local src tgt tag
        IFS='|' read -r src tgt tag <<< "${entry}"
        echo "  ${src}  ->  ${TARGET_NS}/${tgt}:${tag}"
    done
    echo ""

    if [ -z "${DRY_RUN}" ]; then
        check_login
    fi

    # Copy
    local total=${#to_copy[@]}
    local passed=0
    local failed=0

    for entry in "${to_copy[@]}"; do
        if copy_image "${entry}"; then
            passed=$((passed + 1))
        else
            failed=$((failed + 1))
        fi
        echo ""
    done

    # Summary
    echo "============================================"
    echo "  Mirror Summary"
    echo "============================================"
    echo -e "  Total:  ${total}"
    echo -e "  ${GREEN}Passed: ${passed}${NC}"
    [ "${failed}" -gt 0 ] && echo -e "  ${RED}Failed: ${failed}${NC}"
    echo "============================================"

    if [ "${failed}" -gt 0 ]; then
        echo ""
        warn "Some images failed. Re-run with the image name to retry:"
        echo "  ./hack/mirror-images.sh <image-name>"
        exit 1
    fi

    echo ""
    ok "All images mirrored. Update Dockerfiles if DATE_TAG changed from the current value."
}

main "$@"
