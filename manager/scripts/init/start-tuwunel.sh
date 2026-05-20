#!/bin/bash
# start-tuwunel.sh - Start Tuwunel Matrix Homeserver
# NOTE: Tuwunel is a conduwuit fork. Environment variables use CONDUWUIT_ prefix.

mkdir -p /data/tuwunel

export CONDUWUIT_SERVER_NAME="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
export CONDUWUIT_DATABASE_PATH="/data/tuwunel"
export CONDUWUIT_ADDRESS="0.0.0.0"
export CONDUWUIT_PORT=6167
export CONDUWUIT_ALLOW_REGISTRATION=true
export CONDUWUIT_REGISTRATION_TOKEN="${HICLAW_REGISTRATION_TOKEN}"
export CONDUWUIT_ALLOW_LEGACY_MEDIA=true
export CONDUWUIT_ALLOW_UNSTABLE_ROOM_VERSIONS=true
export CONDUWUIT_DB_POOL_WORKERS_LIMIT=32
# Disable Tuwunel's default new-user displayname suffix ("💕") so Matrix
# profiles match the HiClaw worker/human/manager names exactly.
export CONDUWUIT_NEW_USER_DISPLAYNAME_SUFFIX="${CONDUWUIT_NEW_USER_DISPLAYNAME_SUFFIX:-}"
# Increase default cache capacity to prevent RocksDB thrashing (tuwunel#123)
export CONDUWUIT_CACHE_CAPACITY_MODIFIER="${CONDUWUIT_CACHE_CAPACITY_MODIFIER:-2.0}"

# Agent lifecycle cleanup: collapse rooms once their last local member
# leaves and force a /forget on leave so a recreated same-named
# worker/manager/human starts from a clean room state. See
# hiclaw-controller LeaveAll*Rooms / DeleteRoom flows.
export CONDUWUIT_DELETE_ROOMS_AFTER_LEAVE="${CONDUWUIT_DELETE_ROOMS_AFTER_LEAVE:-true}"
export CONDUWUIT_FORGET_FORCED_UPON_LEAVE="${CONDUWUIT_FORGET_FORCED_UPON_LEAVE:-true}"

# User creation is handled by start-manager-agent.sh via Registration API
# (single-step registration, no UIAA flow needed)

exec tuwunel
