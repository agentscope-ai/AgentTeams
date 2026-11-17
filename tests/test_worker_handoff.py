"""Supervisor callbacks around real Worker step boundaries and process stops."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict, RecoveryUnavailable
from worker_handoff import NativeProcesses, SupervisorHandoff, recovery_scanned


def setup_monitor(tmp_path, monkeypatch):
    marker = tmp_path / "native-scanned"
    monkeypatch.setenv("AGENTTEAMS_SUPERVISOR_READY_FILE", str(marker))
    monkeypatch.setenv("AGENTTEAMS_PROCESS_ID", "this-start")
    monitor = SupervisorHandoff("hermes", SimpleNamespace(pid=100), marker)
    monitor.native = Mock(held={})
    monitor.native.freeze.side_effect = lambda: monitor.native.held.update({100: 9})
    return monitor


def test_drain_requires_real_adapter_readiness_probe_and_quiescence(tmp_path, monkeypatch):
    monitor = setup_monitor(tmp_path, monkeypatch)
    op = {"id":"A", "phase":"draining", "source":{"instance_id":"old", "runtime":"hermes", "process_id":"this-start"}}
    steps = []
    def request(method, path, *args, **kwargs):
        steps.append(path)
        if method == "GET":
            return {"handoff": op}, '"etag"'
        assert args == ({"request_id":"A","instance_id":"old","process_id":"this-start"}, '"etag"')
        if path.endswith("probe=true") and len(steps) == 2:
            raise RecoveryConflict("source operation is still running")
        if not path.endswith("probe=true"):
            assert monitor.native.held
            raise RecoveryUnavailable("ack response lost")
    monitor.recovery.request = request
    monitor.check()
    assert not steps
    recovery_scanned()
    with pytest.raises(RecoveryConflict):
        monitor.check()
    monitor.native.freeze.assert_not_called()
    with pytest.raises(RecoveryUnavailable):
        monitor.check()
    assert steps[-2:] == ["runtime/handoff/checkpoint?probe=true", "runtime/handoff/checkpoint"]
    monitor.native.resume.assert_not_called()
    op["phase"] = "checkpointed"
    monitor.check()
    monitor.native.resume.assert_not_called()


def test_recovery_ack_only_from_scanned_target_start(tmp_path, monkeypatch):
    monitor = setup_monitor(tmp_path, monkeypatch)
    op = {"id":"A", "phase":"recovering","source":{},
          "target":{"runtime":"hermes","instance_id":"new","process_id":"foreign-start"}}
    monitor.recovery.request = Mock(return_value=({"handoff":op},'"e"'))
    recovery_scanned()
    monitor.check()
    assert monitor.recovery.request.call_count == 1
    op["target"]["process_id"] = "this-start"
    monitor.check()
    monitor.recovery.request.assert_called_with("POST","runtime/handoff/recovered",
        {"request_id":"A","instance_id":"new","process_id":"this-start"}, '"e"')
    monitor.native.freeze.assert_not_called()


@pytest.mark.skipif(not sys.platform.startswith("linux") or not hasattr(os, "pidfd_open"), reason="Linux Worker process namespace")
def test_freeze_native_and_separate_session_tool_before_ack(tmp_path):
    marker = "test-" + str(os.getpid())
    pidfile = tmp_path / "pid"
    child = subprocess.Popen([sys.executable, "-c",
        "import pathlib,subprocess,sys; t=subprocess.Popen(['sleep','60'],start_new_session=True); "
        "pathlib.Path(sys.argv[1]).write_text(str(t.pid)); t.wait()", str(pidfile)],
        start_new_session=True, env={**os.environ, "AGENTTEAMS_PROCESS_ID":marker})
    tree = NativeProcesses(child.pid, marker)
    tool_pid = None
    try:
        deadline = time.monotonic()+3
        while not pidfile.exists() and time.monotonic()<deadline:
            time.sleep(.01)
        tool_pid = int(pidfile.read_text())
        assert os.getpgid(tool_pid) != os.getpgid(child.pid)
        tree.freeze()
        assert {child.pid, tool_pid} <= tree.held.keys()
        for pid in (child.pid, tool_pid):
            assert Path(f"/proc/{pid}/stat").read_text().rsplit(")",1)[1].split()[0] == "T"
        tree.resume()
        assert not tree.held
    finally:
        tree.resume()
        if tool_pid is not None:
            os.kill(tool_pid, signal.SIGKILL)
        child.wait(timeout=3)
