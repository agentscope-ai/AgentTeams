#!/bin/sh
# Build the distributable plugin zip (v0.2.0 B6; release metadata v0.3.0).
#
# Usage: sh scripts/package.sh [output_dir]   (default: /tmp)
# Produces: <output_dir>/qwen-code-bridge-<version>.zip
#           <output_dir>/sha256sums.txt
#           <output_dir>/release-manifest.json
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

EXCLUDE_DIRS = {"__pycache__", "tests", "state", ".pytest_cache", ".mypy_cache", ".git"}
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

SUMS="$OUT/sha256sums.txt"
MANIFEST="$OUT/release-manifest.json"

COMMIT="$(git rev-parse HEAD 2>/dev/null || true)"
if [ -z "$COMMIT" ]; then
    echo "package.sh: warning: git rev-parse HEAD failed; release-manifest.json commit left empty" >&2
fi

PYZIP="$ZIP" PYSUMS="$SUMS" PYMANIFEST="$MANIFEST" PYNAME="$NAME" \
PYVERSION="$VERSION" PYCOMMIT="$COMMIT" "$PY" - <<'PYEOF'
import datetime
import hashlib
import json
import os
import zipfile

zip_path = os.environ["PYZIP"]
sha = hashlib.sha256()
with open(zip_path, "rb") as fh:
    for chunk in iter(lambda: fh.read(65536), b""):
        sha.update(chunk)
sha = sha.hexdigest()

with open(os.environ["PYSUMS"], "w", encoding="utf-8") as fh:
    fh.write(f"{sha}  {os.path.basename(zip_path)}\n")

with zipfile.ZipFile(zip_path) as zf:
    files = sorted(zf.namelist())

manifest = {
    "module": os.environ["PYNAME"],
    "version": os.environ["PYVERSION"],
    "commit": os.environ.get("PYCOMMIT", ""),
    "files": files,
    "sha256": sha,
    "built_at": datetime.datetime.now(datetime.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    ),
}
with open(os.environ["PYMANIFEST"], "w", encoding="utf-8") as fh:
    json.dump(manifest, fh, indent=2)
    fh.write("\n")

print(f"release metadata -> {os.path.basename(os.environ['PYSUMS'])}, {os.path.basename(os.environ['PYMANIFEST'])}")
PYEOF

ls -l "$ZIP" "$SUMS" "$MANIFEST"
