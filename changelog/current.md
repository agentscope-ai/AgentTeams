# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/` here before the next release.

---

- fix(agent): update file-sharing path guidance for CoPaw and Team Leader agents to use `/root/hiclaw-fs/agents/...` instead of the retired `/root/.hiclaw-worker/...` path.

- **OpenHuman runtime**: OpenHuman added as the fourth Worker runtime with native Matrix support via `channel-matrix` feature flag; includes controller routing (K8s + Docker backends), Dockerfile, entrypoint script, agent template, Helm chart integration, and Makefile build targets.

**Bug Fixes**

- **CoPaw Worker heartbeat**: CoPaw worker templates now seed heartbeat at a 10-minute interval so Team Leader agents created from the worker template can run heartbeat turns without requiring an explicit Team CR heartbeat spec.
- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.
