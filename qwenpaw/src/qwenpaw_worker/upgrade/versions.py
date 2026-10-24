"""QwenPaw version discovery and current-version probing.

Feeds ``GET /upgrade/check``: the unversioned PyPI JSON endpoint carries a
``releases`` mapping (version -> file list) — there is NO top-level
``versions`` key (measured 2026-10-15, design doc §2.2). All network access
goes through the checker's ``_http_json`` (urllib, no new dependencies);
failures raise ``UpgradeIndexError`` so the endpoint can report them in
``errors[]`` instead of returning a 5xx.
"""
from __future__ import annotations

import re
from typing import Any

from qwenpaw_worker.upgrade.checker import _http_json

PYPI_JSON_URL_DEFAULT = "https://pypi.org/pypi/qwenpaw/json"

_PRE_RELEASE_TAIL = re.compile(r"(?:a|b|rc|alpha|beta|pre|dev)[.\-]?\d*$", re.IGNORECASE)


class UpgradeIndexError(Exception):
    """The version index (PyPI or mirror) could not be read or parsed."""


def _parse_version(value: str):
    """A PEP440-aware comparator when packaging is available, else a numeric
    key (pre-release suffix stripped) — same fallback discipline as the
    checker's ``_version_at_least``."""
    try:
        from packaging.version import Version

        return Version(value)
    except Exception:
        base = re.split(r"[a-zA-Z]+\d+", value, 1)[0]
        parts: list[int] = []
        for piece in base.split("."):
            m = re.match(r"(\d+)", piece)
            parts.append(int(m.group(1)) if m else 0)
        return parts


def is_valid_version(value: str) -> bool:
    """True for PEP440-parseable (or at least dotted-numeric) version strings."""
    if not value or not re.match(r"^\d+(\.\d+)*", value):
        return False
    try:
        from packaging.version import Version

        return Version(value).public == value or True  # parseability only
    except ImportError:
        return True
    except Exception:
        return False


def is_prerelease(value: str) -> bool:
    try:
        from packaging.version import Version

        return Version(value).is_prerelease
    except Exception:
        return bool(_PRE_RELEASE_TAIL.search(value))


def fetch_releases(index_json: str = PYPI_JSON_URL_DEFAULT,
                   timeout: float = 20.0) -> dict[str, list[dict]]:
    """GET the unversioned PyPI JSON endpoint; return the ``releases`` mapping.

    Raises ``UpgradeIndexError`` with a readable reason on network or payload
    problems (the /check endpoint converts it into ``errors[]``).
    """
    try:
        data = _http_json(index_json, timeout=timeout)
    except Exception as exc:  # noqa: BLE001 - translate to the typed error
        raise UpgradeIndexError(str(exc)) from exc
    releases = data.get("releases")
    if not isinstance(releases, dict):
        raise UpgradeIndexError("payload has no 'releases' mapping (unexpected index format)")
    return releases


def list_candidate_versions(current: str,
                            releases: dict[str, list[dict]],
                            include_prerelease: bool = False) -> list[str]:
    """Version names from ``releases`` that are > ``current``, PEP440-sorted
    ascending. Pre-releases (a/b/rc) are excluded unless
    ``include_prerelease``; postN releases count as stable. Unparseable names
    are dropped. ``current`` unparseable -> no candidates (nothing is 'above').
    """
    try:
        current_key = _parse_version(current)
    except Exception:
        return []
    candidates: list[tuple[Any, str]] = []
    for name in releases:
        if not is_valid_version(name):
            continue
        if not include_prerelease and is_prerelease(name):
            continue
        try:
            key = _parse_version(name)
        except Exception:
            continue
        try:
            if key <= current_key:
                continue
        except TypeError:
            continue
        candidates.append((key, name))
    candidates.sort(key=lambda item: item[0])
    return [name for _, name in candidates]


def release_published_at(files: list[dict]) -> str | None:
    """Earliest ``upload_time`` across a release's files (ISO8601), else None."""
    stamps = [item.get("upload_time") for item in (files or [])
              if isinstance(item, dict) and item.get("upload_time")]
    return min(stamps) if stamps else None


def current_version() -> tuple[str, str]:
    """The qwenpaw version this worker runs, as (version, source).

    Sources in order: the live app's ``GET /api/version`` (port from
    ``AGENTTEAMS_CONSOLE_PORT``, default 8088), then the venv metadata
    (``importlib.metadata``). Returns ``("unknown", "none")`` when neither is
    reachable — callers must still render the payload (the endpoint is 200).
    """
    import os

    try:
        from qwenpaw_worker.api import QwenPawApiClient

        port = int(os.environ.get("AGENTTEAMS_CONSOLE_PORT", "8088"))
        client = QwenPawApiClient(f"http://127.0.0.1:{port}", timeout=3.0)
        version = client.get_version()
        if version:
            return version, "api/version"
    except Exception:
        pass
    try:
        from importlib.metadata import version as _dist_version

        return _dist_version("qwenpaw"), "importlib.metadata"
    except Exception:
        pass
    return "unknown", "none"
