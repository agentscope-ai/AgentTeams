#!/usr/bin/env bash
set -euo pipefail
mode=${1:?}
kind=${2:?}
arch=${3:?}
stem="stage-v1.2.8-${GITHUB_SHA:0:12}"
registry=${REGISTRY:?}/agentteams
if [[ "$mode" == dependency ]]; then
  if [[ "$kind" == controller ]]; then name=agentteams-controller; else name=openclaw-base; fi
  input="agentteams/$name:latest"
  test "$(docker image inspect "$input" --format '{{.Os}}/{{.Architecture}}')" = "linux/$arch"
  image="$registry/$name:$stem-$arch"
  docker build --build-arg "INPUT_IMAGE=$input" --label "org.opencontainers.image.revision=$SOURCE" \
    --label org.opencontainers.image.version=v1.2.8 --label "io.agentteams.packaging-revision=$GITHUB_SHA" \
    -f scripts/release/128-metadata.Dockerfile -t "$image" .
  if [[ "$kind" == controller ]]; then
    docker run --rm --entrypoint agt "$image" tracing --help
    docker run --rm --entrypoint sha256sum "$image" /usr/local/bin/agt /usr/local/bin/agentteams-controller
  else
    docker run --rm --entrypoint bash "$image" -ec '
      node -e "const v=process.versions.node.split(\".\").map(Number); if(v[0]!==24 || v[1]<22 || (v[1]===22 && v[2]<1)) process.exit(1)"
      node --version
      openclaw --version
      grep -rFl "com.agentteams.read_audience" /opt/openclaw/dist/extensions/matrix >/dev/null
    '
  fi
else
  git fetch --no-tags origin 3b2d53f0c4daeaa975bf2ca4c80fd8a21c0d9f9f "$SOURCE"
  git diff --exit-code 3b2d53f0c4daeaa975bf2ca4c80fd8a21c0d9f9f "$SOURCE" -- manager plugins shared worker qwenpaw hermes deepseek-harness openhuman hack/versions.env
  if [[ "$kind" == embedded ]]; then name=agentteams-embedded; else name=agentteams-harness-$kind; fi
  image="$registry/$name:$stem-$arch"
  if [[ "$kind" == openclaw ]]; then
    make -o build-agentteams-controller build-worker LOCAL_WORKER="$image" \
      LOCAL_CONTROLLER_BUILD_IMAGE="$CONTROLLER" DOCKER_PLATFORM="linux/$arch" \
      OPENCLAW_BASE_BUILD_ARG="--build-arg OPENCLAW_BASE_IMAGE=$BASE" \
      HIGRESS_REGISTRY=higress-registry.us-west-1.cr.aliyuncs.com \
      DOCKER_BUILD_ARGS="--build-arg APT_MIRROR= --label org.opencontainers.image.revision=$SOURCE --label org.opencontainers.image.version=v1.2.8 --label io.agentteams.packaging-revision=$GITHUB_SHA"
    docker run --rm --entrypoint bash -v "$PWD/worker/tests/image-smoke.sh:/tmp/smoke.sh:ro" "$image" /tmp/smoke.sh
    docker run --rm --entrypoint bash "$image" -ec 'grep -rFl "com.agentteams.read_audience" /opt/openclaw/dist/extensions/matrix >/dev/null'
  else
    input="$registry/$name:stage-v1.2.8-3b2d53f0c4da-$arch"
    digest="$(docker buildx imagetools inspect "$input" --format '{{.Manifest.Digest}}')"
    input="${input%:*}@$digest"
    if [[ "$kind" == openhuman ]]; then path=/usr/local/bin/agt.bin; else path=/usr/local/bin/agt; fi
    docker build --build-arg "INPUT_IMAGE=$input" --build-arg "CONTROLLER_IMAGE=$CONTROLLER" \
      --build-arg "AGT_PATH=$path" --label "org.opencontainers.image.revision=$SOURCE" \
      --label org.opencontainers.image.version=v1.2.8 --label "io.agentteams.packaging-revision=$GITHUB_SHA" \
      --label "io.agentteams.foundation=$input" --target "$([[ "$kind" == embedded ]] && echo embedded || echo harness)" \
      -f scripts/release/128-cli-overlay.Dockerfile -t "$image" .
    case "$kind" in
      qwenpaw) docker run --rm --entrypoint /opt/venv/qwenpaw/bin/python "$image" -c 'import qwenpaw_worker' ;;
      hermes) docker run --rm --entrypoint /opt/venv/hermes/bin/python "$image" -c 'import hermes_worker' ;;
      deepseek) docker run --rm --entrypoint python3 -v "$PWD/deepseek-harness/tests/headless_smoke.py:/tmp/smoke.py:ro" "$image" /tmp/smoke.py ;;
      openhuman) docker run --rm --entrypoint openhuman-core "$image" --help ;;
      codex) docker run --rm --entrypoint codex "$image" --version; docker run --rm --entrypoint /opt/venv/bin/python "$image" -c 'import agentteams_codex.bridge' ;;
      embedded) docker run --rm --entrypoint bash "$image" -ec 'command -v agentteams-controller; command -v tuwunel; command -v minio; test -d /opt/agentteams/config/crd' ;;
    esac
  fi
  docker run --rm --entrypoint agt "$image" tracing --help
fi
docker push "$image"
digest="$(docker buildx imagetools inspect "$image" --format '{{.Manifest.Digest}}')"
ref="${image%:*}@$digest"
docker pull --platform "linux/$arch" "$ref"
if [[ "$kind" != openclaw-base ]]; then docker run --rm --entrypoint agt "$ref" tracing --help; fi
echo "$kind $arch $ref" >> "$GITHUB_STEP_SUMMARY"
