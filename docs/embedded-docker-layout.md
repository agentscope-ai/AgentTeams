# Embedded Docker vs Legacy All-in-One Layout

AgentTeams supports several local deployment shapes. This document clarifies which Docker/supervisord artifacts apply to each, so operators and contributors do not accidentally follow the wrong path (especially in K8s docs or CI).

## Current supported local install (embedded stack)

**Installer:** `install/hiclaw-install.sh` / `install/hiclaw-install.ps1`

**Architecture since v1.1.0:**

| Component | Image | Process model |
|-----------|-------|---------------|
| Infrastructure + controller | `agentteams-embedded` (`hiclaw/hiclaw-embedded`) | `supervisord` via `hiclaw-controller/Dockerfile.embedded` + `hiclaw-controller/supervisord.embedded.conf` |
| Manager agent | `agentteams-manager` or `agentteams-manager-copaw` | Single process (`start-manager-agent.sh`); **no** manager supervisord |
| Workers | `agentteams-worker`, `agentteams-copaw-worker`, etc. | Created by controller via Docker socket |

The embedded controller container runs Higress, Tuwunel, MinIO, Element Web, and `hiclaw-controller` under **one** supervisord config (`supervisord.embedded.conf`). The Manager is a **separate slim container** that talks to infrastructure over the Docker network.

Shared install defaults (ports, image names, version gates) live in [`install/defaults.env`](../install/defaults.env).

## Legacy all-in-one (archived)

**Location:** [`manager/docker-legacy/`](../manager/docker-legacy/)

These Dockerfiles bundle infrastructure **and** the Manager agent in one container, orchestrated by [`manager/supervisord.conf`](../manager/supervisord.conf):

| File | Purpose |
|------|---------|
| `Dockerfile.copaw-all-in-one` | CoPaw Manager + Higress + Tuwunel + MinIO in one image |
| `Dockerfile.aliyun` | SAE/cloud Manager variant (`AGENTTEAMS_RUNTIME=aliyun`; no supervisord) |

The bash installer may still activate legacy mode when installing a release **before v1.1.0** (no embedded image). That path is compatibility-only; new installs should use embedded mode.

**Do not use `manager/supervisord.conf` or `manager/docker-legacy/` for:**

- Kubernetes / Helm deployments (controller + separate Pods own infrastructure)
- Documenting the default quick-start install flow
- CI smoke tests unless explicitly testing legacy compatibility

## Kubernetes / Helm

In cluster mode there is **no** embedded supervisord stack inside the Manager Pod. Higress, Matrix, and storage are separate chart components; the controller reconciles CRs. Manager/Worker images are the slim Dockerfiles under `manager/Dockerfile` and `manager/Dockerfile.copaw`.

See [`docs/architecture.md`](architecture.md) and [`docs/quickstart.md`](quickstart.md) for the supported paths.
