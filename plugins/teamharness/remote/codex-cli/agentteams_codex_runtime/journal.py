"""Atomic non-secret session journal shared by Codex execution roles."""

from __future__ import annotations

import json
import os
import tempfile
import threading
from pathlib import Path


class SessionJournal:
    def __init__(self, directory: Path) -> None:
        self.directory = directory.resolve()
        self.path = self.directory / "sessions.json"
        self.directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._threads = self._load()

    def _load(self) -> dict[str, str]:
        try:
            value = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {}
        return (
            {
                str(key): str(thread_id)
                for key, thread_id in value.items()
                if str(key) and str(thread_id)
            }
            if isinstance(value, dict)
            else {}
        )

    def thread_for(self, session_key: str) -> str:
        with self._lock:
            return self._threads.get(session_key, "")

    def set_thread(self, session_key: str, thread_id: str) -> None:
        with self._lock:
            self._threads[session_key] = thread_id
            payload = json.dumps(self._threads, indent=2, sort_keys=True) + "\n"
            with tempfile.NamedTemporaryFile(
                "w",
                encoding="utf-8",
                dir=self.directory,
                prefix="sessions-",
                suffix=".tmp",
                delete=False,
            ) as handle:
                handle.write(payload)
                temporary = Path(handle.name)
            os.replace(temporary, self.path)
