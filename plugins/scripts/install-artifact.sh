#!/usr/bin/env bash
set -euo pipefail

# Verify a release artifact and install it into the local QwenPaw runtime.
#
# Usage: install-artifact.sh --artifact FILE (--sha256 HEX | --manifest FILE | --sha256sums FILE)
#                            [--dry-run]
#
# The checksum is read from the release metadata; a mismatch stops before
# anything is installed. Installation is delegated to `qwenpaw plugin install
# --force`, which hot-loads when QwenPaw is running.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACT=""
SHA256=""
MANIFEST=""
SUMS=""
DRY_RUN=0

usage() {
  cat <<'EOF'
Usage: install-artifact.sh --artifact FILE (--sha256 HEX | --manifest FILE | --sha256sums FILE) [--dry-run]
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --artifact) ARTIFACT="${2:?--artifact requires a value}"; shift 2 ;;
    --sha256) SHA256="${2:?--sha256 requires a value}"; shift 2 ;;
    --manifest) MANIFEST="${2:?--manifest requires a value}"; shift 2 ;;
    --sha256sums) SUMS="${2:?--sha256sums requires a value}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$ARTIFACT" ] || { echo "ERROR: --artifact is required" >&2; exit 2; }
[ -f "$ARTIFACT" ] || { echo "ERROR: artifact not found: $ARTIFACT" >&2; exit 1; }

verify_args=(--artifact "$ARTIFACT")
[ -n "$SHA256" ] && verify_args+=(--sha256 "$SHA256")
[ -n "$MANIFEST" ] && verify_args+=(--manifest "$MANIFEST")
[ -n "$SUMS" ] && verify_args+=(--sha256sums "$SUMS")
bash "${SCRIPT_DIR}/verify-artifact.sh" "${verify_args[@]}"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "dry-run: qwenpaw plugin install --force $ARTIFACT"
  exit 0
fi

if ! command -v qwenpaw >/dev/null 2>&1; then
  echo "ERROR: qwenpaw command not found" >&2
  exit 1
fi

qwenpaw plugin install --force "$ARTIFACT"
echo "ok: installed $ARTIFACT"
