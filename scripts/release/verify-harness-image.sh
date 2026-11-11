#!/usr/bin/env bash
# Read the remote manifest JSON from stdin; verify both image configurations.
set -euo pipefail
image=${1:?image required}
revision=${2:?revision required}
version=${3:?version required}
jq -e '[.manifests[] | select(.platform.os == "linux") | .platform.architecture] | sort == ["amd64", "arm64"]' >/dev/null
# Docker manifest lists cannot carry OCI index annotations. Image config labels
# provide the source identity for both Docker and OCI indexes.
docker buildx imagetools inspect "$image" --format '{{json .Image}}' |
    jq -e --arg revision "$revision" --arg version "$version" '
      . as $images
      | all(["linux/amd64", "linux/arm64"][]; . as $platform
          | $images[$platform].os == "linux"
          and $images[$platform].architecture == ($platform | split("/")[1])
          and $images[$platform].config.Labels["org.opencontainers.image.revision"] == $revision
          and $images[$platform].config.Labels["org.opencontainers.image.version"] == $version)
    ' >/dev/null
printf 'Verified %s from %s (%s): linux/amd64 + linux/arm64\n' "$image" "$revision" "$version"
