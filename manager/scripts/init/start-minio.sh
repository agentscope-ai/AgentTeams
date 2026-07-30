#!/bin/bash
# start-minio.sh - Start MinIO object storage (single node, single disk)

export MINIO_ROOT_USER="${AGENTTEAMS_MINIO_USER:-${AGENTTEAMS_ADMIN_USER:-admin}}"
export MINIO_ROOT_PASSWORD="${AGENTTEAMS_MINIO_PASSWORD:-${AGENTTEAMS_ADMIN_PASSWORD:-admin}}"

mkdir -p /data/minio

exec minio server /data/minio --console-address ":9001" --address ":9000"
