#!/usr/bin/env bash
set -euo pipefail

# Verify a downloaded module artifact before installing it.
#
# Usage:
#   verify-artifact.sh --artifact FILE --sha256 HEX
#   verify-artifact.sh --artifact FILE --manifest release-manifest.json
#   verify-artifact.sh --artifact FILE --sha256sums sha256sums.txt
#
# The checksum comes from the release metadata (manifest/sha256sums), never
# from the artifact itself. Exits 1 on any mismatch or missing entry; prints
# one "ok:" line on success.

ARTIFACT=""
SHA256=""
MANIFEST=""
SUMS=""

usage() {
  cat <<'EOF'
Usage: verify-artifact.sh --artifact FILE (--sha256 HEX | --manifest FILE | --sha256sums FILE)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --artifact) ARTIFACT="${2:?--artifact requires a value}"; shift 2 ;;
    --sha256) SHA256="${2:?--sha256 requires a value}"; shift 2 ;;
    --manifest) MANIFEST="${2:?--manifest requires a value}"; shift 2 ;;
    --sha256sums) SUMS="${2:?--sha256sums requires a value}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$ARTIFACT" ] || { echo "ERROR: --artifact is required" >&2; exit 2; }
[ -f "$ARTIFACT" ] || { echo "ERROR: artifact not found: $ARTIFACT" >&2; exit 1; }
base="$(basename "$ARTIFACT")"

if [ -n "$MANIFEST" ]; then
  [ -f "$MANIFEST" ] || { echo "ERROR: manifest not found: $MANIFEST" >&2; exit 1; }
  SHA256="$(python3 - "$MANIFEST" "$base" <<'PY'
import json, sys
manifest, name = sys.argv[1], sys.argv[2]
data = json.load(open(manifest))
for art in data.get("artifacts", []):
    if art.get("name") == name:
        print(art.get("sha256", ""))
        break
PY
)"
  [ -n "$SHA256" ] || { echo "ERROR: artifact not listed in manifest: $base" >&2; exit 1; }
fi

if [ -n "$SUMS" ]; then
  [ -f "$SUMS" ] || { echo "ERROR: sha256sums not found: $SUMS" >&2; exit 1; }
  SHA256="$(awk -v name="$base" '$2 == name {print $1; exit}' "$SUMS")"
  [ -n "$SHA256" ] || { echo "ERROR: artifact not listed in sha256sums: $base" >&2; exit 1; }
fi

[ -n "$SHA256" ] || { echo "ERROR: --sha256, --manifest or --sha256sums is required" >&2; exit 2; }

actual="$(python3 - "$ARTIFACT" <<'PY'
import hashlib, sys
h = hashlib.sha256()
with open(sys.argv[1], "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
print(h.hexdigest())
PY
)"

if [ "$(printf '%s' "$actual" | tr 'A-F' 'a-f')" != "$(printf '%s' "$SHA256" | tr 'A-F' 'a-f')" ]; then
  echo "ERROR: sha256 mismatch for $base (want $SHA256, got $actual)" >&2
  exit 1
fi

echo "ok: $base $actual"
