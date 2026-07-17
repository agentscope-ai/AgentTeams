# Legacy Manager Docker Layout (Archived)

This directory holds **pre–embedded-stack** Manager Dockerfiles. They are kept for compatibility with releases before v1.1.0 and for reference only.

## What lives here

| Dockerfile | Description |
|------------|-------------|
| `Dockerfile.copaw-all-in-one` | Single container: Higress + Tuwunel + MinIO + Element Web + CoPaw Manager, orchestrated by `manager/supervisord.conf` |
| `Dockerfile.aliyun` | Cloud (SAE) Manager image with `AGENTTEAMS_RUNTIME=aliyun` — no supervisord |

## What to use instead

**New local installs:** `install/hiclaw-install.sh` with the embedded controller image (`agentteams-embedded`) plus a slim Manager container. See [`docs/embedded-docker-layout.md`](../../docs/embedded-docker-layout.md).

**Kubernetes:** Helm chart + slim `manager/Dockerfile` / `manager/Dockerfile.copaw` — not these legacy files.

## Related files (not in this directory)

- `manager/supervisord.conf` — supervisord programs for **legacy all-in-one Manager** only
- `hiclaw-controller/supervisord.embedded.conf` — supervisord for the **current embedded** infrastructure container

Do not confuse the two supervisord configs when debugging local Docker installs.
