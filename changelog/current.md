# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/` here before the next release.

---

**Bug Fixes**

- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.
