#!/bin/bash
set -euo pipefail

# ARM64 compatibility POC only; no changes to the published Worker image.
if [ ! -f /.dockerenv ] || [ "$(id -u)" != 0 ] || [ "$(uname -m)" != aarch64 ]; then
    echo "This headless POC requires a root ARM64 Docker Worker container." >&2
    exit 1
fi
exec 9>/tmp/agentteams-browser-skill-install.lock
flock -n 9 || { echo "Another browser skill installation is running." >&2; exit 1; }
INSTALL_DIR=/opt/agentteams/optional/browser-automation
WORK_DIR=$(mktemp -d)
trap 'rm -rf "${WORK_DIR}"' EXIT
mkdir -p "${INSTALL_DIR}"
npm install --prefix "${INSTALL_DIR}" --save-exact --ignore-scripts \
    agent-browser@0.41.2 @sparticuz/chromium-min@153.0.0

if [ -n "${BROWSER_SLIM_ARCHIVE:-}" ]; then
    cp "${BROWSER_SLIM_ARCHIVE}" "${WORK_DIR}/browser.tar"
else
    curl -fL --retry 2 --connect-timeout 10 \
        https://github.com/Sparticuz/chromium/releases/download/v153.0.0/chromium-v153.0.0-pack.arm64.tar \
        -o "${WORK_DIR}/browser.tar"
fi
printf '%s  %s\n' b6874fc0d7af15e16a15ef1b73177731f698c296c73d26aa4a90840c0777a338 \
    "${WORK_DIR}/browser.tar" | sha256sum -c -
tar -xf "${WORK_DIR}/browser.tar" -C "${WORK_DIR}"
cd "${INSTALL_DIR}"
BROWSER_SLIM_WORK_DIR="${WORK_DIR}" node --input-type=module <<'JS'
import path from 'node:path';
import {inflate} from '@sparticuz/chromium-min';
for (const name of ['chromium.br', 'fonts.tar.br', 'al2023.tar.br', 'swiftshader.tar.br']) {
    await inflate(path.join(process.env.BROWSER_SLIM_WORK_DIR, name));
}
JS

if [ -n "${BROWSER_SLIM_FONT:-}" ]; then
    cp "${BROWSER_SLIM_FONT}" "${WORK_DIR}/font.otf"
else
    curl -fL --retry 2 --connect-timeout 10 \
        https://raw.githubusercontent.com/notofonts/noto-cjk/f8d157532fbfaeda587e826d4cd5b21a49186f7c/Sans/OTF/SimplifiedChinese/NotoSansCJKsc-Regular.otf \
        -o "${WORK_DIR}/font.otf"
fi
printf '%s  %s\n' 2c76254f6fc379fddfce0a7e84fb5385bb135d3e399294f6eeb6680d0365b74b \
    "${WORK_DIR}/font.otf" | sha256sum -c -
cp "${WORK_DIR}/font.otf" /tmp/fonts/NotoSansCJKsc-Regular.otf
curl -fsSL --retry 2 --connect-timeout 10 \
    https://raw.githubusercontent.com/notofonts/noto-cjk/f8d157532fbfaeda587e826d4cd5b21a49186f7c/Sans/LICENSE \
    -o /tmp/fonts/NotoSansCJK-LICENSE.txt

# Keep the installed CLI reference but drop binaries for other OS/architectures.
for file in "${INSTALL_DIR}"/node_modules/agent-browser/bin/agent-browser-*; do
    case "${file}" in
        */agent-browser-linux-arm64) chmod +x "${file}" ;;
        *) rm -f "${file}" ;;
    esac
done
export LD_LIBRARY_PATH="/tmp/al2023/lib:/tmp${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
export FONTCONFIG_PATH=/tmp/fonts
/tmp/chromium --version
"${INSTALL_DIR}/node_modules/.bin/agent-browser" --version
