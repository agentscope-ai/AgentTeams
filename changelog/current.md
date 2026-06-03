# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/` here before the next release.

---

- **OpenHuman runtime**: OpenHuman added as the fourth Worker runtime with native Matrix support via `channel-matrix` feature flag; includes controller routing (K8s + Docker backends), Dockerfile, entrypoint script, agent template, Helm chart integration, and Makefile build targets.

**Bug Fixes**

- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.
- **Team migration safety**: Team auto-migration now rejects unrelated pre-existing Worker CRs with the same member name unless they are explicitly marked for adoption and match the projected spec; `workerMembers` CRD validation also rejects duplicate members and invalid leader counts.
