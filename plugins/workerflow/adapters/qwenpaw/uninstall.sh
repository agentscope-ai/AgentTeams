#!/usr/bin/env bash
set -euo pipefail

if command -v qwenpaw >/dev/null 2>&1; then
  printf 'y\n' | qwenpaw plugin uninstall workerflow >/dev/null 2>&1 || true
fi
