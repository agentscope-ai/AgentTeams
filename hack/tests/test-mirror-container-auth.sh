#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIRROR_SCRIPT="${MIRROR_SCRIPT:-${SCRIPT_DIR}/../mirror-images.sh}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

HOME_DIR="${TMP_DIR}/home"
BIN_DIR="${TMP_DIR}/bin"
DOCKER_LOG="${TMP_DIR}/docker.log"
mkdir -p "${HOME_DIR}" "${BIN_DIR}"

cat > "${BIN_DIR}/docker" <<'STUB'
#!/bin/bash
set -euo pipefail

printf '%s\n' "$*" >> "${DOCKER_LOG}"

case " $* " in
    *" inspect "*)
        exit 1
        ;;
    *" login "*)
        mkdir -p "${HOME}/.config/containers"
        printf '%s\n' '{"auths":{"registry.example.com":{}}}' > "${HOME}/.config/containers/auth.json"
        ;;
esac
STUB
chmod +x "${BIN_DIR}/docker"

HOME="${HOME_DIR}" \
PATH="${BIN_DIR}:${PATH}" \
DOCKER_LOG="${DOCKER_LOG}" \
USE_CONTAINER=1 \
SKOPEO_IMAGE="quay.io/skopeo/stable:test" \
TARGET_REGISTRY="registry.example.com" \
TARGET_NS="example" \
DATE_TAG="20260716" \
bash "${MIRROR_SCRIPT}" tuwunel <<< "y" >/dev/null

AUTH_DIR="${HOME_DIR}/.config/containers"
AUTH_FILE="${AUTH_DIR}/auth.json"

if [ ! -f "${AUTH_FILE}" ]; then
    echo "FAIL: container login credentials did not persist on the host" >&2
    exit 1
fi

if [ "$(grep -c 'REGISTRY_AUTH_FILE=/auth/auth.json' "${DOCKER_LOG}")" -ne 3 ]; then
    echo "FAIL: inspect, login, and copy did not share the container auth file" >&2
    cat "${DOCKER_LOG}" >&2
    exit 1
fi

if ! grep -Fq -- "-v ${AUTH_DIR}:/auth:ro" "${DOCKER_LOG}"; then
    echo "FAIL: non-interactive skopeo calls did not mount auth read-only" >&2
    cat "${DOCKER_LOG}" >&2
    exit 1
fi

if ! grep -Fq -- "-v ${AUTH_DIR}:/auth quay.io/skopeo/stable:test login registry.example.com" "${DOCKER_LOG}"; then
    echo "FAIL: skopeo login did not mount persistent auth read-write" >&2
    cat "${DOCKER_LOG}" >&2
    exit 1
fi

echo "PASS: containerized skopeo reuses persistent login credentials"
