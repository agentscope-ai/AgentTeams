#!/usr/bin/env bash
set -euo pipefail
runtime=${1:?runtime required}
version=${2:?version required}
platform=${3:?platform required}
controller=${4:?controller image required}
registry=${REGISTRY:-higress-registry.cn-hangzhou.cr.aliyuncs.com}
image="${registry}/agentteams/agentteams-harness-${runtime}:${version}"
common=(--label "org.opencontainers.image.revision=$(git rev-parse HEAD)" --label "org.opencontainers.image.version=${RELEASE_VERSION:?}" --build-arg APT_MIRROR= --build-arg PIP_INDEX_URL=https://pypi.org/simple/)
case "$runtime" in
  openclaw) target=worker; output=LOCAL_WORKER ;;
  qwenpaw) target=qwenpaw-worker; output=LOCAL_QWENPAW_WORKER ;;
  hermes) target=hermes-worker; output=LOCAL_HERMES_WORKER ;;
  deepseek) target=deepseek-harness-worker; output=LOCAL_DEEPSEEK_HARNESS_WORKER ;;
  openhuman) target=openhuman-worker; output=LOCAL_OPENHUMAN_WORKER ;;
  codex)
    docker build --platform "$platform" --build-context at=. "${common[@]}" -t "${image}-source" codex-harness
    docker build --platform "$platform" "${common[@]}" --build-arg "CODEX_IMAGE=${image}-source" --build-arg "CONTROLLER_IMAGE=$controller" -f scripts/release/Dockerfile.codex -t "$image" .
    ;;
  *) echo "Unknown runtime: $runtime" >&2; exit 1 ;;
esac
if [[ "$runtime" != codex ]]; then
  make -o build-agentteams-controller "build-${target}" \
    "$output=$image" "LOCAL_CONTROLLER_BUILD_IMAGE=$controller" \
    "DOCKER_PLATFORM=$platform" HIGRESS_REGISTRY=higress-registry.us-west-1.cr.aliyuncs.com \
    "DOCKER_BUILD_ARGS=${common[*]}"
fi
# These containers have no credentials, mounts, or connection to the demo network.
docker run --rm --platform "$platform" --entrypoint sh "$image" -ec 'command -v agt; agt tracing --help >/dev/null; test -d /opt/agentteams/plugins'
case "$runtime" in
  openclaw) docker run --rm --platform "$platform" --entrypoint bash -v "$PWD/worker/tests/image-smoke.sh:/tmp/smoke.sh:ro" "$image" /tmp/smoke.sh ;;
  qwenpaw) docker run --rm --platform "$platform" --entrypoint /opt/venv/qwenpaw/bin/python "$image" -c 'import qwenpaw_worker' ;;
  hermes) docker run --rm --platform "$platform" --entrypoint python3 "$image" -c 'import hermes_worker' ;;
  deepseek) docker run --rm --platform "$platform" --entrypoint python3 -v "$PWD/deepseek-harness/tests/headless_smoke.py:/tmp/smoke.py:ro" "$image" /tmp/smoke.py ;;
  openhuman) docker run --rm --platform "$platform" --entrypoint openhuman-core "$image" --version ;;
  codex)
    docker run --rm --platform "$platform" --entrypoint codex "$image" --version
    docker run --rm --platform "$platform" --entrypoint /opt/venv/bin/python "$image" -c 'import agentteams_codex.bridge'
    ;;
esac
docker push "$image"
