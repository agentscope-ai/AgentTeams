# install/dev — developer-only build helpers

These tools are **not** part of the production installer. They are for PR testing and for producing arm64 images when the Pi must not compile.

## build-main.sh

Builds an image from a git ref in a temporary worktree.

```bash
# Controller / embedded (Pi flavor; run on a Docker host, not the Pi)
install/dev/build-main.sh \
  --ref origin/main \
  --component controller \
  --flavor embedded \
  --platform linux/arm64 \
  --tag agentteams-embedded:pr-1428-<sha> \
  --save

# Standalone controller
install/dev/build-main.sh --ref origin/main --component controller \
  --platform linux/amd64 --tag agentteams-controller:dev

# Manager / worker / QwenPaw worker
install/dev/build-main.sh --ref origin/main --component manager-qwenpaw --tag ...
install/dev/build-main.sh --ref origin/main --component qwenpaw-worker --tag ...
```

Load on the Pi:

```bash
cat agentteams-embedded_pr-1428-<sha>.tar | ssh swrp51@192.168.31.8 'docker load'
```

Then run the upgrade script with `--controller-image <tag>` (and `--higress-backfill-dir` if the gateway plugin set is missing).

## Cross-platform builds need QEMU binfmt

On an x86 host, `--platform linux/arm64` fails with `exec format error` until arm64 emulation is registered:

```bash
docker run --privileged --rm tonistiigi/binfmt --install arm64
docker run --rm --platform linux/arm64 alpine uname -m   # aarch64
```

The build script checks this before the long build and prints the commands if it is missing.

## Constraints

- **Pi: no build.** The Pi is a deployment/test node; build off-Pi.
- The QwenPaw container used for development has no Docker; run this on a host with Docker.
- `main` includes #1387 (merge `e2d62b0e`); use `--ref origin/main`. The Pi image `pi-jt-s3` already contains the same external-S3 feature; a fresh `origin/main` build is optional for final verification.
- Runtime version upgrades are an image field / online upgrade, not an AgentTeams build.
- Manager/worker/dashboard are separate images; build only when their code changes.
