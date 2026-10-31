"""Unit tests for file-backed session state and the single-writer lock
(P9, PR1 M1).
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

import core.state as state_mod
from core.state import (
    CoreLockedError,
    FileSessionStore,
    SingleWriterLock,
    StateLayout,
)


# ---------------------------------------------------------------------------
# StateLayout
# ---------------------------------------------------------------------------


def test_state_layout_paths(tmp_path):
    layout = StateLayout(tmp_path / "state")
    assert layout.sync_token == tmp_path / "state" / "sync_token"
    assert layout.e2ee == tmp_path / "state" / "e2ee"
    assert layout.session_keys == tmp_path / "state" / "session_keys.json"
    assert layout.media_cache == tmp_path / "state" / "media_cache"
    assert layout.history == tmp_path / "state" / "history.json"
    assert layout.lock == tmp_path / "state" / "core.lock"


def test_state_layout_ensure_creates_tree(tmp_path):
    layout = StateLayout(tmp_path / "deep" / "state")
    layout.ensure()
    assert layout.root.is_dir()
    assert layout.e2ee.is_dir()
    assert layout.media_cache.is_dir()
    # Idempotent.
    layout.ensure()
    assert layout.root.is_dir()


# ---------------------------------------------------------------------------
# FileSessionStore: sync token
# ---------------------------------------------------------------------------


def test_sync_token_roundtrip(tmp_path):
    store = FileSessionStore(tmp_path / "state")
    assert store.load_sync_token() is None  # no file yet
    store.save_sync_token("tBAB456789")
    assert store.load_sync_token() == "tBAB456789"
    store.save_sync_token("tNEW000001")
    assert store.load_sync_token() == "tNEW000001"


def test_sync_token_empty_file_reads_none(tmp_path):
    store = FileSessionStore(tmp_path / "state")
    store.layout.ensure()
    store.layout.sync_token.write_text("   \n", encoding="utf-8")
    assert store.load_sync_token() is None


# ---------------------------------------------------------------------------
# FileSessionStore: session keys
# ---------------------------------------------------------------------------


def test_session_key_roundtrip(tmp_path):
    store = FileSessionStore(tmp_path / "state")
    assert store.load_session_key("!room", "@alice") is None
    store.save_session_key("!room", "@alice", "sess-alice")
    assert store.load_session_key("!room", "@alice") == "sess-alice"
    # Other (room, sender) pairs are independent.
    store.save_session_key("!room", "@bob", "sess-bob")
    store.save_session_key("!other", "@alice", "sess-other-room")
    assert store.load_session_key("!room", "@bob") == "sess-bob"
    assert store.load_session_key("!other", "@alice") == "sess-other-room"
    assert store.load_session_key("!room", "@alice") == "sess-alice"
    assert store.load_session_key("!missing", "@alice") is None
    # Overwrite.
    store.save_session_key("!room", "@alice", "sess-alice-2")
    assert store.load_session_key("!room", "@alice") == "sess-alice-2"


def test_session_keys_json_is_stable(tmp_path):
    store = FileSessionStore(tmp_path / "state")
    store.save_session_key("!b", "@x", "1")
    store.save_session_key("!a", "@y", "2")
    first = store.layout.session_keys.read_text(encoding="utf-8")
    store.save_session_key("!c", "@z", "3")
    second = store.layout.session_keys.read_text(encoding="utf-8")
    # Sorted keys + indent=2 => deterministic formatting.
    assert '"!a"' in first and first.index('"!a"') < first.index('"!b"')
    assert second.index('"!a"') < second.index('"!b"') < second.index('"!c"')
    assert '\n  "' in second  # indent=2


# ---------------------------------------------------------------------------
# Atomic writes (crash mid-write must not corrupt the previous value)
# ---------------------------------------------------------------------------


def test_atomic_write_uses_temp_file_in_same_dir_and_crash_keeps_old(
    tmp_path, monkeypatch
):
    store = FileSessionStore(tmp_path / "state")
    store.save_sync_token("tOLD")

    seen = {}

    def fake_replace(src, dst):
        seen["src"] = src
        seen["dst"] = dst
        raise OSError("simulated crash before the replace")

    monkeypatch.setattr(state_mod.os, "replace", fake_replace)
    with pytest.raises(OSError, match="simulated crash"):
        store.save_sync_token("tNEW")

    # The temp file lived next to the target (atomic rename on one fs).
    assert os.path.dirname(seen["src"]) == str(tmp_path / "state")
    assert Path(seen["dst"]) == tmp_path / "state" / "sync_token"
    # The previous value is intact after the simulated crash.
    assert store.load_sync_token() == "tOLD"
    # The temp file was cleaned up.
    assert not os.path.exists(seen["src"])


def test_atomic_write_success_leaves_no_temp_files(tmp_path):
    store = FileSessionStore(tmp_path / "state")
    store.save_sync_token("t1")
    store.save_session_key("!room", "@a", "k")
    leftovers = [p for p in (tmp_path / "state").iterdir() if p.name.endswith(".tmp")]
    assert leftovers == []


# ---------------------------------------------------------------------------
# SingleWriterLock
# ---------------------------------------------------------------------------


def test_lock_acquire_release_roundtrip(tmp_path):
    path = tmp_path / "state" / "core.lock"
    lock = SingleWriterLock(path)
    assert not lock.held
    lock.acquire()
    assert lock.held
    assert path.exists()
    assert oct(os.stat(path).st_mode & 0o777) == "0o600"
    lock.release()
    assert not lock.held
    # Re-acquire after release works.
    lock.acquire()
    lock.release()


def test_lock_mutual_exclusion_fail_closed(tmp_path):
    path = tmp_path / "state" / "core.lock"
    first = SingleWriterLock(path)
    first.acquire()
    try:
        second = SingleWriterLock(path)
        with pytest.raises(CoreLockedError):
            second.acquire()
        assert not second.held
        # A third instance sees the same fail-closed behaviour.
        third = SingleWriterLock(path)
        with pytest.raises(CoreLockedError):
            third.acquire()
    finally:
        first.release()
    # Once released, the waiting writer can proceed.
    second = SingleWriterLock(path)
    second.acquire()
    assert second.held
    second.release()


def test_lock_double_acquire_same_instance_raises(tmp_path):
    lock = SingleWriterLock(tmp_path / "core.lock")
    lock.acquire()
    try:
        with pytest.raises(RuntimeError):
            lock.acquire()
    finally:
        lock.release()


def test_lock_context_manager(tmp_path):
    lock = SingleWriterLock(tmp_path / "core.lock")
    with lock as entered:
        assert entered is lock
        assert lock.held
        with pytest.raises(CoreLockedError):
            SingleWriterLock(tmp_path / "core.lock").acquire()
    assert not lock.held
