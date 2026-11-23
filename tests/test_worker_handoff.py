"""Replacement startup readiness is independent of source progress."""
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import Mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from worker_handoff import SupervisorHandoff, recovery_scanned


def setup_monitor(tmp_path, monkeypatch):
    marker = tmp_path / "native-scanned"
    monkeypatch.setenv("AGENTTEAMS_SUPERVISOR_READY_FILE", str(marker))
    monkeypatch.setenv("AGENTTEAMS_PROCESS_ID", "this-start")
    return SupervisorHandoff("hermes", SimpleNamespace(), marker)


def test_source_does_not_acknowledge_or_freeze(tmp_path, monkeypatch):
    monitor = setup_monitor(tmp_path, monkeypatch)
    recovery_scanned()
    for phase in ("draining", "stopping"):
        source = {"runtime": "hermes", "instance_id": "old", "process_id": "this-start"}
        monitor.recovery.request = Mock(return_value=({"handoff": {"id": "A", "phase": phase, "source": source}}, '"etag"'))
        monitor.check()
        monitor.recovery.request.assert_called_once_with("GET", "runtime/handoff", response_etag=True)


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
