#!/usr/bin/env bash
# Capture the five-route G1 surface from a running deployment.
# Usage: BASE_URL=http://host:port TOKEN=<bearer> ./capture.sh <out-dir>
# Never commit the output: it contains environment-specific ids and timestamps.
set -euo pipefail
BASE_URL="${BASE_URL:?set BASE_URL}"
TOKEN="${TOKEN:?set TOKEN}"
OUT="${1:?usage: capture.sh <out-dir>}"
mkdir -p "$OUT"
AUTH=(-H "Authorization: Bearer $TOKEN")

curl -sS -D "$OUT/01-create.hdr" -o "$OUT/01-create.json" -X POST \
  "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"name":"g1-probe","runtime":"qwenpaw"}' "$BASE_URL/api/v1/workers"

curl -sS -D "$OUT/02-list.hdr" -o "$OUT/02-list.json" "${AUTH[@]}" \
  "$BASE_URL/api/v1/workers"

curl -sS -D "$OUT/03-get.hdr" -o "$OUT/03-get.json" "${AUTH[@]}" \
  "$BASE_URL/api/v1/workers/g1-probe"

curl -sS -D "$OUT/04-update.hdr" -o "$OUT/04-update.json" -X PUT \
  "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"soul":"probe"}' "$BASE_URL/api/v1/workers/g1-probe"

curl -sS -D "$OUT/05-delete.hdr" -o "$OUT/05-delete.json" -X DELETE \
  "${AUTH[@]}" "$BASE_URL/api/v1/workers/g1-probe"

echo "captured 5 routes into $OUT (do not commit)"
