#!/bin/sh
# Rebuild + install the base-prefixed Qwen Code web shell (see README.md).
set -e
SRC=${1:?usage: build.sh <qwen-code source dir> [qwenpaw working dir]}
WORK=${QWEN_CODE_BRIDGE_WORK:-/app/working}
BUILD=/root/qc-build
rm -rf "$BUILD"; mkdir -p "$BUILD"
tar -C "$SRC" --exclude=.git --exclude=node_modules -cf - . | tar -C "$BUILD" -xf -
cd "$BUILD/packages/web-shell"
# patch
python3 - "$BUILD" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
p = root/'packages/web-shell/vite.config.ts'
s = p.read_text()
if 'WEB_SHELL_BASE' not in s:
    s = s.replace("root: 'client',", "base: process.env['WEB_SHELL_BASE'] ?? '/',\n  root: 'client',", 1)
    p.write_text(s)
PY
pnpm install --ignore-scripts
pnpm --filter "@qwen-code/sdk..." run build
WEB_SHELL_BASE=/api/qwen-code-bridge/web/ pnpm exec vite build
DEST="$WORK/.qwenpaw/qwen-code-bridge/web-shell"
mkdir -p "$DEST"; rm -rf "$DEST/dist"; cp -a dist "$DEST/"
echo "installed -> $DEST/dist"
