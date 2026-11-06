#!/usr/bin/env bash
# Controls for plugins/scripts/check-release-tag.sh.
#
# The bridge release job packages with plugins/qwen-code-bridge/scripts/package.sh,
# which reads plugin.json and never compares the requested tag with the
# manifests. These cases pin both sides: a matching tag passes, while a tag that
# disagrees with metadata.version (plugin.yaml) or version (plugin.json) fails
# before packaging, so a mismatched release tag cannot produce artifacts.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="${ROOT_DIR}/plugins/scripts/check-release-tag.sh"
[[ -x "$CHECK" ]] || {
  echo "FAIL: missing or non-executable ${CHECK}" >&2
  exit 1
}

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

write_manifests() {
  local dir="$1" name="$2" yaml_version="$3" json_version="$4" json_name="${5:-}"
  [ -n "$json_name" ] || json_name="$name"
  mkdir -p "${dir}"
  cat >"${dir}/plugin.yaml" <<YAML
apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  name: ${name}
  version: ${yaml_version}
YAML
  cat >"${dir}/plugin.json" <<JSON
{
  "id": "${json_name}",
  "name": "${json_name}",
  "version": "${json_version}"
}
JSON
}

expect_skip() {
  local label="$1"
  shift
  if ! "$CHECK" "$@" >"${workdir}/out" 2>"${workdir}/err"; then
    echo "FAIL: ${label} should be a no-op success" >&2
    cat "${workdir}/out" "${workdir}/err" >&2
    exit 1
  fi
  grep -q '^skip:' "${workdir}/out" || {
    echo "FAIL: ${label} did not report skip" >&2
    exit 1
  }
}

expect_ok() {
  local label="$1"
  shift
  if ! "$CHECK" "$@" >"${workdir}/out" 2>"${workdir}/err"; then
    echo "FAIL: ${label} should pass" >&2
    cat "${workdir}/out" "${workdir}/err" >&2
    exit 1
  fi
  grep -q '^ok:' "${workdir}/out" || {
    echo "FAIL: ${label} did not report ok" >&2
    exit 1
  }
}

expect_fail() {
  local label="$1" needle="$2"
  shift 2
  if "$CHECK" "$@" >"${workdir}/out" 2>"${workdir}/err"; then
    echo "FAIL: ${label} should fail" >&2
    exit 1
  fi
  grep -q "${needle}" "${workdir}/err" || {
    echo "FAIL: ${label} failed without '${needle}'" >&2
    cat "${workdir}/err" >&2
    exit 1
  }
}

# 1. matching tag (the #1411 manifests carry 0.3.0)
write_manifests "${workdir}/match" qwen-code-bridge 0.3.0 0.3.0
expect_ok "matching tag" \
  --tag qwen-code-bridge-v0.3.0 \
  --yaml "${workdir}/match/plugin.yaml" \
  --json "${workdir}/match/plugin.json"

# 2. pre-release accepted on both sides
write_manifests "${workdir}/pre" qwen-code-bridge 0.3.0-rc.1 0.3.0-rc.1
expect_ok "matching pre-release tag" \
  --tag qwen-code-bridge-v0.3.0-rc.1 \
  --yaml "${workdir}/pre/plugin.yaml" \
  --json "${workdir}/pre/plugin.json"

# 3. tag version disagrees with metadata.version
write_manifests "${workdir}/yaml-drift" qwen-code-bridge 0.3.0 0.3.0
expect_fail "tag/yaml version mismatch" "metadata.version" \
  --tag qwen-code-bridge-v9.9.9 \
  --yaml "${workdir}/yaml-drift/plugin.yaml" \
  --json "${workdir}/yaml-drift/plugin.json"

# 4. tag version agrees with plugin.yaml but not with plugin.json
write_manifests "${workdir}/json-drift" qwen-code-bridge 0.3.0 0.2.0
expect_fail "tag/json version mismatch" "plugin.json" \
  --tag qwen-code-bridge-v0.3.0 \
  --yaml "${workdir}/json-drift/plugin.yaml" \
  --json "${workdir}/json-drift/plugin.json"

# 5. tag prefix disagrees with the manifest name
write_manifests "${workdir}/prefix" qwen-code-bridge 0.3.0 0.3.0
expect_fail "tag prefix mismatch" "does not match manifest name" \
  --tag qwenpaw-v0.3.0 \
  --yaml "${workdir}/prefix/plugin.yaml" \
  --json "${workdir}/prefix/plugin.json"

# 6. build metadata is rejected like the generic resolver does
expect_fail "build metadata rejected" "build metadata" \
  --tag qwen-code-bridge-v0.3.0+build.7 \
  --yaml "${workdir}/match/plugin.yaml" \
  --json "${workdir}/match/plugin.json"

# 7. the two manifests disagree on the name
write_manifests "${workdir}/name-drift" qwen-code-bridge 0.3.0 0.3.0 other-bridge
expect_fail "manifest names disagree" "does not match" \
  --tag qwen-code-bridge-v0.3.0 \
  --yaml "${workdir}/name-drift/plugin.yaml" \
  --json "${workdir}/name-drift/plugin.json"

# 8. manual dispatch has no tag: the step is a no-op, not a failure
expect_skip "manual dispatch (no tag)" \
  --tag "" \
  --yaml "${workdir}/match/plugin.yaml" \
  --json "${workdir}/match/plugin.json"

# 9. --expect-name guards against renaming the manifest without the tag prefix
expect_fail "expect-name mismatch" "expected manifest name" \
  --tag qwen-code-bridge-v0.3.0 \
  --yaml "${workdir}/match/plugin.yaml" \
  --json "${workdir}/match/plugin.json" \
  --expect-name other-bridge

echo "ok: bridge release tag controls"
