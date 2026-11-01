#!/usr/bin/env bash
# Resolve a plugin release for build/package/upload.
#
# Usage:
#   resolve-plugin-release.sh --tag <metadata.name>-v<semver>   # tag trigger
#   resolve-plugin-release.sh --plugin <plugin dir>             # manual selection
#
# Version policy (kept consistent with plugins/scripts/validate-plugin.rb):
#   * plain semver and pre-release semver are accepted (e.g. 0.1.0-rc.1);
#   * build metadata (+xxx) is rejected on both sides;
#   * the tag version suffix must be exactly equal (string equality, no
#     semver equivalence) to metadata.version.
#
# On success prints manifest=, name= and version= lines to stdout and
# exits 0. On failure prints a single ERROR line to stderr and exits 1.
# Read-only: never modifies any file. Run from the repository root.

set -euo pipefail

SEMVER_RE='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
TAG_RE='^(.*)-v([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$'

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

meta() {
  ruby -ryaml -e '
    doc = YAML.load_file(ARGV[0])
    value = (doc.dig("metadata", ARGV[1]) || "").to_s
    abort("missing metadata.#{ARGV[1]} in #{ARGV[0]}") if value.strip.empty?
    puts value
  ' "$1" "$2"
}

list_manifests() {
  for m in plugins/*/plugin.yaml; do
    [ -f "$m" ] || continue
    printf '%s\n' "$m"
  done
}

emit() {
  printf 'manifest=%s\n' "$1"
  printf 'name=%s\n' "$2"
  printf 'version=%s\n' "$3"
}

tag=""
plugin=""
while [ $# -gt 0 ]; do
  case "$1" in
    --tag)
      [ $# -ge 2 ] || fail "--tag requires a value"
      tag="$2"
      shift 2
      ;;
    --plugin)
      [ $# -ge 2 ] || fail "--plugin requires a value"
      plugin="$2"
      shift 2
      ;;
    *)
      fail "unknown argument: $1 (usage: --tag <name>-v<semver> | --plugin <dir>)"
      ;;
  esac
done

if [ -n "$tag" ] && [ -n "$plugin" ]; then
  fail "use exactly one of --tag or --plugin"
fi
if [ -z "$tag" ] && [ -z "$plugin" ]; then
  fail "usage: resolve-plugin-release.sh --tag <name>-v<semver> | --plugin <dir>"
fi

if [ -n "$plugin" ]; then
  dir="${plugin%/}"
  [ -n "$dir" ] || dir="."
  manifest="${dir}/plugin.yaml"
  [ -f "$manifest" ] || fail "no plugin manifest at ${manifest}"
  name="$(meta "$manifest" name)"
  version="$(meta "$manifest" version)"
  case "$version" in
    *"+") fail "build metadata is not supported for plugin releases (manifest: ${manifest})" ;;
  esac
  if [[ ! "$version" =~ $SEMVER_RE ]]; then
    fail "manifest version ${version} in ${manifest} is not a valid semver (pre-release allowed, build metadata is not supported for plugin releases)"
  fi
  emit "$manifest" "$name" "$version"
  exit 0
fi

# --tag mode: <metadata.name>-v<semver>. The name is the longest prefix
# before the trailing -v<version>, so prefix-related names (foo vs foo-bar)
# resolve to the exact metadata.name instead of silently picking one.
if [[ ! "$tag" =~ $TAG_RE ]]; then
  if [[ "$tag" =~ ^.*-v[0-9]+\.[0-9]+\.[0-9]+.*\+$ ]]; then
    fail "build metadata is not supported for plugin releases (tag: ${tag})"
  fi
  fail "tag ${tag} is not in the form <metadata.name>-v<semver> (pre-release allowed, build metadata is not supported for plugin releases)"
fi
name="${BASH_REMATCH[1]}"
tag_version="${BASH_REMATCH[2]}"
[ -n "$name" ] || fail "tag ${tag} has an empty plugin name"

first_match=""
matches=""
match_count=0
while IFS= read -r manifest; do
  [ -f "$manifest" ] || continue
  manifest_name="$(meta "$manifest" name)" || continue
  if [ "$manifest_name" = "$name" ]; then
    [ -n "$first_match" ] || first_match="$manifest"
    matches="${matches}${manifest} "
    match_count=$((match_count + 1))
  fi
done < <(list_manifests)

if [ "$match_count" -eq 0 ]; then
  fail "no plugin manifest named ${name} matches tag ${tag}; available manifests: $(list_manifests | tr '\n' ' ')"
fi
if [ "$match_count" -gt 1 ]; then
  fail "tag ${tag} matches multiple plugin manifests named ${name}: ${matches}"
fi

manifest="$first_match"
manifest_version="$(meta "$manifest" version)"
case "$manifest_version" in
  *"+") fail "build metadata is not supported for plugin releases (manifest: ${manifest})" ;;
esac
if [[ ! "$manifest_version" =~ $SEMVER_RE ]]; then
  fail "manifest version ${manifest_version} in ${manifest} is not a valid semver (pre-release allowed, build metadata is not supported for plugin releases)"
fi
if [ "$tag_version" != "$manifest_version" ]; then
  fail "tag version ${tag_version} does not match manifest version ${manifest_version}"
fi
emit "$manifest" "$name" "$manifest_version"
