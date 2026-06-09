"""Remote (local-environment) harness worker.

``RemoteWorker`` runs the harness agent in a developer's local environment
(laptop/workstation) instead of as a controller-managed Kubernetes pod. It is a
thin subclass of :class:`harness_worker.worker.Worker`: the entire bootstrap,
Matrix relay, and ``claude -p`` invocation flow is inherited unchanged — the
harness worker is not modified.

The only remote-specific behaviour added here is a **scoped push of agent-setup
files under ``.harness/``** so a developer does not lose their local setup when
switching the machine running the remote worker. The standard ``push_loop``
excludes the whole runtime-home dir (``.harness/``); cluster workers keep it
local, but remote workers persist a curated subset to MinIO so it follows the
developer across environments. Pull is already handled by ``FileSync.mirror_all``
at startup (it mirrors the full ``agents/<name>/`` prefix, including ``.harness/``).

Security boundary: ``.env`` (Matrix token, gateway key, MinIO creds) is never
pushed. A developer can opt individual files out of the push via
``.harness/.harnessignore`` (gitignore-style patterns).
"""
from __future__ import annotations

import asyncio
import fnmatch
import logging
import time
from pathlib import Path

from hiclaw_common.sync import _mc

from harness_worker.worker import Worker, console

logger = logging.getLogger(__name__)

# Non-secret agent-setup files persisted to MinIO so they follow the developer.
_HARNESS_SETUP_FILES = ("mcp-local.json", "claude.settings.json", "claudeignore")
# Directories under .harness/ whose contents are persisted (recursively).
_HARNESS_SETUP_DIRS = ("sessions",)
# Never pushed regardless of .harnessignore (secrets / local-only control file).
_NEVER_PUSH = {".env", ".harnessignore"}


class RemoteWorker(Worker):
    """Harness worker for a local developer environment.

    Inherits the full bootstrap/relay/invoke flow from :class:`Worker` and only
    layers a scoped ``.harness/`` setup push on top.
    """

    async def start(self) -> bool:
        ok = await super().start()
        if ok:
            asyncio.create_task(self._harness_push_loop(check_interval=5))
            console.print(
                "[dim]Remote mode: persisting .harness/ setup to MinIO "
                "(opt out via .harness/.harnessignore).[/dim]"
            )
        return ok

    # ------------------------------------------------------------------ #
    # Scoped .harness/ setup push                                        #
    # ------------------------------------------------------------------ #
    async def _harness_push_loop(self, check_interval: int = 5) -> None:
        last_push_time: float = time.time()
        while True:
            await asyncio.sleep(check_interval)
            try:
                now = time.time()
                pushed = await asyncio.get_event_loop().run_in_executor(
                    None, self._push_harness_setup, last_push_time
                )
                last_push_time = now
                if pushed:
                    logger.info("harness setup push: uploaded %s", pushed)
            except asyncio.CancelledError:
                break
            except Exception as exc:  # noqa: BLE001 - non-fatal background loop
                logger.warning("harness setup push error: %s", exc)

    def _push_harness_setup(self, since: float = 0) -> list[str]:
        """Upload changed, non-secret ``.harness/`` setup files to MinIO.

        Mirrors ``FileSync.push_local``'s cat-compare semantics (skip when the
        remote copy is byte-identical) so a static workspace produces no churn.
        """
        if self.sync is None:
            return []
        home = self._harness_home
        if not home.exists():
            return []

        patterns = self._harnessignore_patterns()
        self.sync._ensure_alias()

        candidates: list[Path] = []
        for name in _HARNESS_SETUP_FILES:
            path = home / name
            if path.is_file():
                candidates.append(path)
        for dirname in _HARNESS_SETUP_DIRS:
            subdir = home / dirname
            if subdir.is_dir():
                candidates.extend(p for p in subdir.rglob("*") if p.is_file())

        pushed: list[str] = []
        for path in candidates:
            rel_home = path.relative_to(home).as_posix()
            if path.name in _NEVER_PUSH or rel_home in _NEVER_PUSH:
                continue
            if self._is_ignored(rel_home, patterns):
                continue
            try:
                if path.stat().st_mtime <= since:
                    continue
            except OSError:
                continue

            rel = path.relative_to(self.config.workspace_dir).as_posix()  # ".harness/..."
            key = f"{self.sync._prefix}/{rel}"
            try:
                remote = self.sync._cat(key)
                local_content = path.read_text(errors="replace")
                if remote == local_content:
                    continue
                _mc("cp", str(path), self.sync._object_path(key), check=True)
                pushed.append(rel)
            except Exception as exc:  # noqa: BLE001 - per-file best effort
                logger.debug("harness setup push failed for %s: %s", rel, exc)

        return pushed

    def _harnessignore_patterns(self) -> list[str]:
        ignore_file = self._harness_home / ".harnessignore"
        if not ignore_file.exists():
            return []
        patterns: list[str] = []
        try:
            for raw in ignore_file.read_text(errors="replace").splitlines():
                line = raw.strip()
                if line and not line.startswith("#"):
                    patterns.append(line)
        except OSError as exc:
            logger.warning("could not read .harnessignore: %s", exc)
        return patterns

    @staticmethod
    def _is_ignored(rel_to_home: str, patterns: list[str]) -> bool:
        """gitignore-style match of a ``.harness/``-relative path against patterns.

        A trailing ``/`` makes a directory prefix match; otherwise the pattern is
        fnmatch-ed against the full relative path, its first path component, and
        its basename.
        """
        first = rel_to_home.split("/", 1)[0]
        base = rel_to_home.rsplit("/", 1)[-1]
        for pat in patterns:
            if pat.endswith("/"):
                head = pat[:-1]
                if rel_to_home == head or rel_to_home.startswith(pat):
                    return True
            elif (
                fnmatch.fnmatch(rel_to_home, pat)
                or fnmatch.fnmatch(first, pat)
                or fnmatch.fnmatch(base, pat)
            ):
                return True
        return False
