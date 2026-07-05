#!/usr/bin/env bash
set -euo pipefail

if command -v qwenpaw >/dev/null 2>&1; then
  bash "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/adapters/qwenpaw/uninstall.sh"
fi
