"""Session descriptors are common metadata, never native sessions or checkpoints."""
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_sessions import WorkerSessions
from task_recovery_rpc import execute


def snapshot():
    return {"work": {"kind": "task", "team_id": "team/中文", "task_id": "T &1"}, "instance_id": "actual/start", "runtime": "hermes", "generation": 2,
            "checkpoint_version": 4, "checkpoint_digest": "c" * 64, "config_generation": 7,
            "compatible": True, "etag": "cache-etag", "cache": {
                "runtime": "hermes", "native_ref": "native-session", "native_digest": "a" * 64,
                "checkpoint_version": 4, "checkpoint_digest": "c" * 64, "config_generation": 7,
                "expires_at": 2000}}


def identity():
    return {"instance_id": "actual/start", "process_id": "current-process", "generation": 2}


def test_native_content_must_be_verified_in_addition_to_current_common_metadata():
    view = snapshot()
    sessions = WorkerSessions("hermes", recovery=Mock())
    assert sessions.usable(view, "native-session", "a" * 64, now=1000)
    for ref, digest in [(None, "a" * 64), ("native-session", None), ("old-session", "a" * 64), ("native-session", "b" * 64)]:
        assert not sessions.usable(view, ref, digest, now=1000)
    assert not sessions.usable(view, "native-session", "a" * 64, now=2000)
    for key, value in [("checkpoint_version", 5), ("checkpoint_digest", "d" * 64), ("config_generation", 8), ("runtime", "qwenpaw"), ("compatible", False), ("cache", None)]:
        changed = {**view, key: value}
        assert not sessions.usable(changed, "native-session", "a" * 64, now=1000)


def test_remember_binds_native_persistence_to_the_original_execution_snapshot():
    view = snapshot()
    recovery = Mock(process="current-process")
    sessions = WorkerSessions("hermes", recovery=recovery)
    work = {"kind": "task", "team_id": "team/中文", "task_id": "T &1"}
    sessions.remember(work, view, identity(), "native-session", "a" * 64, 2000)
    method, path, body, etag = recovery.request.call_args.args
    assert method == "PUT" and "team=team%2F%E4%B8%AD%E6%96%87" in path and "task=T+%261" in path
    assert body == {"instance_id": "actual/start", "process_id": "current-process", "generation": 2,
                    "checkpoint_version": 4, "checkpoint_digest": "c" * 64, "config_generation": 7,
                    "native_ref": "native-session", "native_digest": "a" * 64, "expires_at": 2000}
    assert etag == "cache-etag"
    assert view == snapshot()
    # No re-read that could label stale native content with a newer checkpoint.
    assert recovery.request.call_count == 1


def test_first_native_descriptor_is_create_only():
    import hashlib
    import json
    key = hashlib.sha256(json.dumps(["!room:test", "$input"], separators=(",", ":")).encode()).hexdigest()
    view = {**snapshot(), "etag": "", "cache": None, "work": {"kind": "message", "room_id": "!room:test", "event_id": "$input"}}
    recovery = Mock(process="current-process")
    sessions = WorkerSessions("hermes", recovery=recovery)
    sessions.remember({"kind": "message", "event_key": key}, view, identity(), "native", "a" * 64, 2000)
    assert recovery.request.call_args.kwargs == {"create": True}
    assert recovery.request.call_args.args[1] == "runtime/sessions?kind=message&event_key=" + key


@pytest.mark.parametrize("change", ["instance_id", "process_id", "generation"])
def test_stale_native_callback_cannot_write_a_descriptor(change):
    recovery = Mock(process="current-process")
    stale = {**identity(), change: "old"}
    with pytest.raises(RecoveryConflict):
        WorkerSessions("hermes", recovery=recovery).remember(snapshot()["work"], snapshot(), stale, "native", "a" * 64, 2000)
    recovery.request.assert_not_called()


def test_native_content_cannot_be_relabelled_as_another_work_item():
    recovery = Mock(process="current-process")
    with pytest.raises(RecoveryConflict):
        WorkerSessions("hermes", recovery=recovery).remember(
            {"kind": "task", "team_id": "team/中文", "task_id": "other"}, snapshot(), identity(), "native", "a" * 64, 2000)
    recovery.request.assert_not_called()


def test_native_rpc_uses_the_same_session_facade():
    with patch("task_recovery_rpc.enabled", return_value=True), patch("worker_sessions.WorkerSessions") as sessions, patch("task_recovery_rpc.Recovery"):
        work = {"kind": "task", "team_id": "team", "task_id": "T"}
        execute({"action": "session.load", "runtime": "hermes", "work": work})
        sessions.return_value.load.assert_called_once_with(work)
        execute({"action": "session.remember", "runtime": "hermes", "work": work,
                 "snapshot": snapshot(), "identity": identity(), "native_ref": "native",
                 "native_digest": "a" * 64, "expires_at": 2000})
        sessions.return_value.remember.assert_called_once_with(work, snapshot(), identity(), "native", "a" * 64, 2000)
