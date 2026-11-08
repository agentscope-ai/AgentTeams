#!/usr/bin/env bash
# Validate a release tag against both manifests of a plugin that ships a
# plugin.yaml (agentteams manifest) and a plugin.json (runtime manifest).
#
# The generic `build-plugin` job runs plugins/scripts/resolve-plugin-release.sh
# --tag, which pins tag<->metadata.version for yaml-manifest plugins. Plugins
# packaged by their own packager (plugins/qwen-code-bridge/scripts/package.sh
# reads plugin.json) bypass that check, so a mismatched tag still produced a
# successful ZIP: this script is the missing gate.
#
# Usage:
#   check-release-tag.sh --tag <name>-v<semver> --yaml <plugin.yaml> --json <plugin.json> [--expect-name <name>]
#
# Policy (same as resolve-plugin-release.sh):
#   * plain and pre-release semver accepted, build metadata (+xxx) rejected;
#   * the tag prefix must equal the manifest name;
#   * tag version, metadata.version and plugin.json version must be equal as
#     strings (no semver equivalence).
#
# Read-only. On success prints one "ok:" line and exits 0; on failure prints a
# single "ERROR:" line to stderr and exits 1. An empty --tag is a no-op so a
# manual dispatch (which has no release tag) can run the same step.

set -euo pipefail

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

tag=""
yaml_manifest=""
json_manifest=""
expect_name=""

while [ $# -gt 0 ]; do
  case "$1" in
    --tag)
      [ $# -ge 2 ] || fail "--tag requires a value"
      tag="$2"
      shift 2
      ;;
    --yaml)
      [ $# -ge 2 ] || fail "--yaml requires a value"
      yaml_manifest="$2"
      shift 2
      ;;
    --json)
      [ $# -ge 2 ] || fail "--json requires a value"
      json_manifest="$2"
      shift 2
      ;;
    --expect-name)
      [ $# -ge 2 ] || fail "--expect-name requires a value"
      expect_name="$2"
      shift 2
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[ -n "$yaml_manifest" ] || fail "--yaml is required"
[ -n "$json_manifest" ] || fail "--json is required"
[ -f "$yaml_manifest" ] || fail "missing yaml manifest: $yaml_manifest"
[ -f "$json_manifest" ] || fail "missing json manifest: $json_manifest"

if [ -z "$tag" ]; then
  echo "skip: no release tag supplied (manual dispatch)"
  exit 0
fi

read_yaml() {
  ruby -ryaml -e '
    doc = YAML.load_file(ARGV[0])
    value = (doc.dig("metadata", ARGV[1]) || "").to_s
    abort("missing metadata.#{ARGV[1]} in #{ARGV[0]}") if value.strip.empty?
    puts value
  ' "$1" "$2"
}

read_json() {
  ruby -rjson -e '
    doc = JSON.parse(File.read(ARGV[0]))
    value = (doc[ARGV[1]] || "").to_s
    abort("missing #{ARGV[1]} in #{ARGV[0]}") if value.strip.empty?
    puts value
  ' "$1" "$2"
}

case "$tag" in
  *+*) fail "tag ${tag} carries build metadata (+...), which releases reject" ;;
esac

if [[ ! "$tag" =~ ^(.*)-v([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]]; then
  fail "tag ${tag} is not <name>-v<semver>"
fi
tag_name="${BASH_REMATCH[1]}"
tag_version="${BASH_REMATCH[2]}"

yaml_name="$(read_yaml "$yaml_manifest" name || fail "cannot read metadata.name from ${yaml_manifest}")"
yaml_version="$(read_yaml "$yaml_manifest" version || fail "cannot read metadata.version from ${yaml_manifest}")"
json_name="$(read_json "$json_manifest" name || fail "cannot read name from ${json_manifest}")"
json_version="$(read_json "$json_manifest" version || fail "cannot read version from ${json_manifest}")"

if [ -n "$expect_name" ] && [ "$expect_name" != "$yaml_name" ]; then
  fail "expected manifest name ${expect_name}, ${yaml_manifest} says ${yaml_name}"
fi
if [ "$tag_name" != "$yaml_name" ]; then
  fail "tag prefix ${tag_name} does not match manifest name ${yaml_name}"
fi
if [ "$json_name" != "$yaml_name" ]; then
  fail "${json_manifest} name ${json_name} does not match ${yaml_manifest} name ${yaml_name}"
fi
if [ "$tag_version" != "$yaml_version" ]; then
  fail "tag version ${tag_version} does not match ${yaml_manifest} metadata.version ${yaml_version}"
fi
if [ "$tag_version" != "$json_version" ]; then
  fail "tag version ${tag_version} does not match ${json_manifest} version ${json_version}"
fi

printf 'ok: %s tag=%s version=%s (%s, %s)\n' \
  "$yaml_name" "$tag" "$tag_version" "$yaml_manifest" "$json_manifest"
