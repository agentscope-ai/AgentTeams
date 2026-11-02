"""File-backed session state and the single-writer lock (P9, PR1 M1).

Layout under the matrix state directory (``$AGENTTEAMS_MATRIX_STATE_DIR``,
default ``<agent workspace>/.agentteams/matrix/``)::

    <state_dir>/
      sync_token        last successful sync batch token (plain text)
      e2ee/             matrix-nio E2EE store (opaque to the core)
      session_keys.json per-(room, sender) session keys (stable JSON)
      media_cache/      downloaded media cache
      history.json      persisted room history (populated from M2 on)
      core.lock         single-writer lock (flock)

Writes go through a temp file + :func:`os.replace` so a crash mid-write
can never corrupt the previous value.  Only one core process per agent:
the lock is acquired non-blocking and failing to acquire it is a
fail-closed error.
"""

from __future__ import annotations

import fcntl
import json
import os
import tempfile
from pathlib import Path
from typing import Union


class CoreLockedError(RuntimeError):
    """Another process already holds the single-writer lock (fail-closed)."""


class StateLayout:
    """Canonical file layout under the matrix state directory."""

    SYNC_TOKEN = "sync_token"
    E2EE_DIR = "e2ee"
    SESSION_KEYS = "session_keys.json"
    MEDIA_CACHE_DIR = "media_cache"
    HISTORY = "history.json"
    LOCK = "core.lock"

    def __init__(self, state_dir: Union[str, Path]) -> None:
        self.root = Path(state_dir)
        self.sync_token = self.root / self.SYNC_TOKEN
        self.e2ee = self.root / self.E2EE_DIR
        self.session_keys = self.root / self.SESSION_KEYS
        self.media_cache = self.root / self.MEDIA_CACHE_DIR
        self.history = self.root / self.HISTORY
        self.lock = self.root / self.LOCK

    def ensure(self) -> None:
        """Create the state directory tree (idempotent)."""
        self.root.mkdir(parents=True, exist_ok=True)
        self.e2ee.mkdir(parents=True, exist_ok=True)
        self.media_cache.mkdir(parents=True, exist_ok=True)


def _atomic_write(path: Union[str, Path], data: Union[str, bytes]) -> None:
    """Atomically write *data* to *path* (temp file + ``os.replace``).

    The temp file is created in the same directory as *path* so the
    replacement is atomic on POSIX.  On any failure the temp file is
    removed and the previous content of *path* is left untouched.
    """
    path = Path(path)
    if isinstance(data, str):
        data = data.encode("utf-8")
    fd, tmp_path = tempfile.mkstemp(
        prefix=path.name + ".", suffix=".tmp", dir=str(path.parent)
    )
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_path, path)
    except BaseException:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass
        raise


class FileSessionStore:
    """A :class:`~core.interfaces.SessionStore` backed by :class:`StateLayout`.

    ``sync_token`` is stored as plain text (trailing newline).
    ``session_keys.json`` is a nested mapping
    ``{"<room_id>": {"<sender>": "<key>"}}`` written with stable JSON
    formatting (``indent=2``, ``sort_keys=True``) so diffs stay clean.
    """

    def __init__(self, state_dir: Union[str, Path, StateLayout]) -> None:
        if isinstance(state_dir, StateLayout):
            self._layout = state_dir
        else:
            self._layout = StateLayout(state_dir)

    @property
    def layout(self) -> StateLayout:
        return self._layout

    # -- sync token ------------------------------------------------------

    def load_sync_token(self) -> str | None:
        path = self._layout.sync_token
        try:
            text = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            return None
        text = text.strip()
        return text or None

    def save_sync_token(self, token: str) -> None:
        self._layout.ensure()
        _atomic_write(self._layout.sync_token, token + "\n")

    # -- session keys ----------------------------------------------------

    def _load_session_key_map(self) -> dict:
        try:
            with self._layout.session_keys.open(encoding="utf-8") as fh:
                data = json.load(fh)
        except FileNotFoundError:
            return {}
        if not isinstance(data, dict):
            raise ValueError("session_keys.json is malformed (expected an object)")
        return data

    def _save_session_key_map(self, data: dict) -> None:
        payload = json.dumps(data, indent=2, sort_keys=True, ensure_ascii=False)
        _atomic_write(self._layout.session_keys, payload + "\n")

    def load_session_key(self, room_id: str, sender: str) -> str | None:
        return self._load_session_key_map().get(room_id, {}).get(sender)

    def save_session_key(self, room_id: str, sender: str, key: str) -> None:
        self._layout.ensure()
        data = self._load_session_key_map()
        data.setdefault(room_id, {})[sender] = key
        self._save_session_key_map(data)


class SingleWriterLock:
    """Non-blocking ``flock``-based single-writer lock (fail-closed).

    The lock is held on an open file descriptor for the lifetime of the
    process holding it; the lock *file* is not removed on release so
    concurrent acquirers always race on the same path.
    """

    def __init__(self, path: Union[str, Path]) -> None:
        self._path = Path(path)
        self._fh: int | None = None

    @property
    def path(self) -> Path:
        return self._path

    @property
    def held(self) -> bool:
        return self._fh is not None

    def acquire(self) -> None:
        """Acquire the lock or raise :class:`CoreLockedError` (fail-closed)."""
        if self._fh is not None:
            raise RuntimeError(f"lock {self._path} already held by this instance")
        self._path.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(str(self._path), os.O_RDWR | os.O_CREAT, 0o600)
        try:
            os.fchmod(fd, 0o600)
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            os.close(fd)
            raise CoreLockedError(
                f"matrix core lock {self._path} is held by another writer; "
                "only one core process per agent is allowed (fail-closed)"
            ) from None
        self._fh = fd

    def release(self) -> None:
        """Release the lock (idempotent)."""
        if self._fh is None:
            return
        try:
            fcntl.flock(self._fh, fcntl.LOCK_UN)
        finally:
            os.close(self._fh)
            self._fh = None

    def __enter__(self) -> "SingleWriterLock":
        self.acquire()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.release()
