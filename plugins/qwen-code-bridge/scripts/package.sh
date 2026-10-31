#!/bin/sh
# Build the distributable plugin zip (v0.2.0 B6).
#
# Usage: sh scripts/package.sh [output_dir]   (default: /tmp)
# Produces: <output_dir>/qwen-code-bridge-<version>.zip
#
# Excludes: __pycache__/, tests/, state/, web-shell/dist/, *.pyc, *.log
# (the zip is rooted at qwen-code-bridge/ so `upload` sees a single plugin).
set -eu

HERE="$(cd "$(dirname "$0")/.." && pwd)"
NAME="qwen-code-bridge"
OUT="${1:-/tmp}"
mkdir -p "$OUT"

VERSION="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$HERE/plugin.json" | head -1)"
if [ -z "$VERSION" ]; then
    echo "package.sh: cannot read version from $HERE/plugin.json" >&2
    exit 1
fi

PY="$(command -v python || command -v python3 || true)"
if [ -z "$PY" ]; then
    echo "package.sh: no python interpreter found" >&2
    exit 1
fi

ZIP="$OUT/$NAME-$VERSION.zip"

PYHERE="$HERE" PYNAME="$NAME" PYZIP="$ZIP" "$PY" - <<'PYEOF'
import os
import sys
import zipfile

here = os.environ["PYHERE"]
name = os.environ["PYNAME"]
out = os.environ["PYZIP"]

EXCLUDE_DIRS = {"__pycache__", "tests", "state"}
EXCLUDE_FILE_SUFFIXES = (".pyc", ".log")

count = 0
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
    for root, dirs, files in os.walk(here):
        dirs[:] = [d for d in dirs if d not in EXCLUDE_DIRS]
        # web-shell/dist: exclude the build artifact subtree
        if os.path.relpath(root, here) == os.path.join("web-shell", "dist"):
            dirs[:] = []
        for fname in sorted(files):
            if fname.endswith(EXCLUDE_FILE_SUFFIXES):
                continue
            full = os.path.join(root, fname)
            arcname = os.path.join(name, os.path.relpath(full, here))
            zf.write(full, arcname)
            count += 1

print(f"packaged {count} files")
PYEOF

ls -l "$ZIP"
