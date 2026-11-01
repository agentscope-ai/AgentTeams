#!/usr/bin/env bash
# Real-engine regression: a default `docker run` container reports
# .HostConfig.OomKillDisable as <nil>, while `docker create` stores false.
# boundary_verify must treat those as equal (but keep explicit true distinct).
#
# Requires a running Docker daemon and a cached image. Skips when either is
# unavailable; does not pull, tag, push or deploy anything.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$root/install/agentteams-upgrade.sh"
image="${AGENTTEAMS_TEST_IMAGE:-}"

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "SKIP test-upgrade-boundary-real (no docker daemon)"
  exit 0
fi
if [ -z "$image" ]; then
  image="$(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -v '<none>' | head -n1 || true)"
fi
if [ -z "$image" ] || ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "SKIP test-upgrade-boundary-real (no cached image; set AGENTTEAMS_TEST_IMAGE)"
  exit 0
fi

suffix="$$-$(date +%s)"
old_name="at1412-oom-old-${suffix}"
new_name="at1412-oom-new-${suffix}"
true_name="at1412-oom-true-${suffix}"
cleanup() { docker container rm --force "$old_name" "$new_name" "$true_name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Default docker run: OomKillDisable is unset (nil).
docker run -d --name "$old_name" "$image" sleep 60 >/dev/null
# Replacement docker create: Docker stores the default as false.
docker create --name "$new_name" "$image" sleep 60 >/dev/null

AGENTTEAMS_UPGRADE_FUNCTIONS_ONLY=1 . "$script"
DOCKER_CMD="docker"

# 1. Default docker run (nil) vs replacement docker create (false): same boundary.
# Keep this assertion before the optional explicit-true case.
if ! boundary_verify "$old_name" "$new_name" >/dev/null 2>&1; then
  echo "FAIL: default OomKillDisable <nil> vs false was rejected" >&2
  exit 1
fi
echo "ok: default OomKillDisable <nil> vs false accepted"

# 2. Explicit true must remain a real difference. Some cgroup-v2 daemons accept
# the flag, warn that it was discarded, and inspect reports false; in that case
# there is no true container to compare, so skip only this optional case.
if ! docker create --name "$true_name" --oom-kill-disable "$image" sleep 60 >/dev/null 2>&1; then
  echo "PASS test-upgrade-boundary-real ($image: nil/false accepted; explicit true create failed, skipped)"
  exit 0
fi
actual="$(docker inspect --format '{{.HostConfig.OomKillDisable}}' "$true_name" 2>/dev/null || true)"
if [ "$actual" != "true" ]; then
  echo "PASS test-upgrade-boundary-real ($image: nil/false accepted; explicit true discarded by cgroup-v2 daemon, actual=${actual:-unknown}, skipped)"
  exit 0
fi
if boundary_verify "$true_name" "$new_name" >/dev/null 2>&1; then
  echo "FAIL: explicit OomKillDisable true vs false was accepted" >&2
  exit 1
fi

echo "PASS test-upgrade-boundary-real ($image: nil/false accepted, true/false rejected)"
