import json

import pytest

from copaw_worker.hooks.tools import _toolhelpers
from copaw_worker.hooks.tools import filesync as filesync_tool
from copaw_worker.hooks.tools import message as message_tool
from copaw_worker.hooks.tools import projectflow as projectflow_tool
from copaw_worker.hooks.tools import taskflow as taskflow_tool
from copaw_worker.task import TaskflowError


def _response_json(response):
    item = response.content[0]
    text = item.get("text") if isinstance(item, dict) else item.text
    return json.loads(text)


def test_ok_and_error_build_expected_payloads():
    ok_response = _toolhelpers._ok(action="noop", value=1)
    assert _response_json(ok_response) == {"ok": True, "action": "noop", "value": 1}

    error_response = _toolhelpers._error("boom", action="noop")
    assert _response_json(error_response) == {
        "ok": False,
        "error": "boom",
        "action": "noop",
    }


def test_required_str_and_optional_str():
    assert _toolhelpers._required_str({"key": " value "}, "key") == "value"
    with pytest.raises(TaskflowError):
        _toolhelpers._required_str({}, "key")
    with pytest.raises(TaskflowError):
        _toolhelpers._required_str({"key": "  "}, "key")

    assert _toolhelpers._optional_str({}, "key") is None
    assert _toolhelpers._optional_str({"key": "value"}, "key") == "value"
    with pytest.raises(TaskflowError):
        _toolhelpers._optional_str({"key": 5}, "key")


def test_coerce_payload_accepts_dict_str_and_none():
    assert _toolhelpers._coerce_payload(None) == {}
    assert _toolhelpers._coerce_payload({"a": 1}) == {"a": 1}
    assert _toolhelpers._coerce_payload('{"a": 1}') == {"a": 1}
    with pytest.raises(TaskflowError):
        _toolhelpers._coerce_payload("not json")
    with pytest.raises(TaskflowError):
        _toolhelpers._coerce_payload(123)


def test_workspace_dir_and_store_use_configured_working_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("COPAW_WORKING_DIR", str(tmp_path / "worker"))
    workspace = _toolhelpers._workspace_dir()
    assert workspace == tmp_path / "worker" / "workspaces" / "default"

    store = _toolhelpers._store()
    assert store.shared_dir == workspace / "shared"


@pytest.mark.parametrize(
    "module, names",
    [
        # taskflow/projectflow import the helpers they use; _response/_workspace_dir
        # stay private to _toolhelpers (called only via _ok/_error/_store).
        (taskflow_tool, ("_ok", "_error", "_store", "_required_str", "_optional_str")),
        (projectflow_tool, ("_ok", "_error", "_store", "_required_str", "_optional_str")),
        (message_tool, ("_ok", "_error")),
        (filesync_tool, ("_ok", "_error")),
    ],
)
def test_tool_modules_reuse_toolhelpers_functions(module, names):
    """Each tool module imports (rather than redefines) the shared helpers."""
    for name in names:
        assert getattr(module, name) is getattr(_toolhelpers, name), (
            f"{module.__name__}.{name} is not the shared _toolhelpers implementation"
        )


def test_taskflow_and_projectflow_share_coerce_payload():
    assert taskflow_tool._coerce_payload is _toolhelpers._coerce_payload
    assert projectflow_tool._coerce_payload is _toolhelpers._coerce_payload


def test_filesync_keeps_its_own_coerce_payload_variant():
    """filesync's _coerce_payload raises FilesyncToolError, not TaskflowError —
    it is intentionally NOT deduped into _toolhelpers (see filesync.py)."""
    assert filesync_tool._coerce_payload is not _toolhelpers._coerce_payload
    with pytest.raises(filesync_tool.FilesyncToolError):
        filesync_tool._coerce_payload("not json")
