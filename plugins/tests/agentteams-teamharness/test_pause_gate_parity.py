"""Parity twin of the #1402 versioned-read regressions against the bundle.

The versioned-read regressions added to
``plugins/tests/teamharness/test_pause_gate.py`` (spec 15: the
maintainer's snapshot -> pause -> ETag/commit overlap; spec 16: the
persistent version flip that exhausts the bounded read; spec 18: the
absent remote meta is the first push — the directory push including
meta.json is the commit; spec 19: the unreadable stat fails closed with
no half-write) must hold in the ``plugins/agentteams-teamharness``
compatibility copy as well — the bundle ships a byte-identical
``mcp/`` tree (pinned by the drift test), and a regression that survives
only in the live tree would ship to installations.

The SAME test functions as the legacy suite are executed here, with the
``server`` fixture pointed at the bundle's ``mcp/server.py``.  The
legacy test module is imported, not copied: the parity twin cannot drift
from the original.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_MCP_DIR = REPO_ROOT / "plugins" / "agentteams-teamharness" / "mcp"
LEGACY_MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
LEGACY_PAUSE_GATE = (
    REPO_ROOT / "plugins" / "tests" / "teamharness" / "test_pause_gate.py"
)


def _load_pause_gate_module() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_pause_gate_parity_source", LEGACY_PAUSE_GATE
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    try:
        spec.loader.exec_module(module)
    finally:
        del sys.modules[spec.name]
    return module


pause_gate = _load_pause_gate_module()


def _purge_legacy_mcp_modules() -> None:
    """Drop ``mcp*`` modules imported from the LEGACY tree so the bundle
    server binds to its own (byte-identical) copy: a cached legacy
    ``mcp`` package would otherwise shadow the bundle's through
    sys.modules and the parity run would silently test the wrong tree.
    Only modules whose files live under the legacy tree are dropped."""
    legacy_root = LEGACY_MCP_DIR.as_posix() + "/"
    for name in [
        name
        for name, module in sys.modules.items()
        if name == "mcp" or name.startswith("mcp.")
    ]:
        file = getattr(sys.modules[name], "__file__", None)
        if file is not None and file.startswith(legacy_root):
            del sys.modules[name]


def _load_bundle_server() -> Any:
    _purge_legacy_mcp_modules()
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_pause_gate_parity_test",
        BUNDLE_MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(BUNDLE_MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(BUNDLE_MCP_DIR))
    return module


@pytest.fixture()
def server() -> Any:
    return _load_bundle_server()


def test_pause_between_body_read_and_etag_is_caught_by_versioned_read_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 15 (parity): the maintainer's overlap, run against the
    bundle's compatibility copy."""
    pause_gate.test_pause_between_body_read_and_etag_is_caught_by_versioned_read(
        server, tmp_path, monkeypatch
    )


def test_versioned_read_exhausts_on_persistent_flip_no_stale_commit_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 16 (parity): the persistent version flip exhausts the bounded
    read in the bundle's copy and commits nothing."""
    pause_gate.test_versioned_read_exhausts_on_persistent_flip_no_stale_commit(
        server, tmp_path, monkeypatch
    )


def test_sync_project_first_push_when_remote_meta_absent_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 18 (parity): the absent remote meta is the first push in the
    bundle's copy — the directory push including meta.json is the
    commit, and no conditional commit is attempted."""
    pause_gate.test_sync_project_first_push_when_remote_meta_absent(
        server, tmp_path, monkeypatch
    )


def test_sync_project_fails_closed_on_etag_read_failure_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 19 (parity): the unreadable stat fails closed in the
    bundle's copy — no commit, and no directory push that omits
    meta.json."""
    pause_gate.test_sync_project_fails_closed_on_etag_read_failure(
        server, tmp_path, monkeypatch
    )


def test_sync_after_admin_resume_does_not_resurrect_stale_pause_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 20 (parity): a cache pulled before the admin resume must not
    write the released pause back in the bundle's copy either."""
    pause_gate.test_sync_after_admin_resume_does_not_resurrect_stale_pause(
        server, tmp_path, monkeypatch
    )


def test_sync_drops_stale_pause_when_remote_has_no_pause_key_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 21 (parity): the absent controller-owned key means "no pause" in
    the bundle's copy too — the stale local source is dropped."""
    pause_gate.test_sync_drops_stale_pause_when_remote_has_no_pause_key(
        server, tmp_path, monkeypatch
    )


def test_sync_drops_stale_pause_and_flag_when_remote_has_no_loop_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 22 (parity): with no remote loop both controller-owned keys are
    dropped in the bundle's copy while the worker-owned content lands."""
    pause_gate.test_sync_drops_stale_pause_and_flag_when_remote_has_no_loop(
        server, tmp_path, monkeypatch
    )


def test_sync_after_controller_resume_does_not_restore_stale_paused_status_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 23 (parity): the Controller's resume survives a stale progress
    sync in the bundle's copy too."""
    pause_gate.test_sync_after_controller_resume_does_not_restore_stale_paused_status(
        server, tmp_path, monkeypatch
    )


def test_sync_keeps_remote_completed_status_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 24 (parity): a stale local active cannot reopen a completed
    project in the bundle's copy."""
    pause_gate.test_sync_keeps_remote_completed_status(server, tmp_path, monkeypatch)


def test_mcp_pause_resume_control_actions_still_author_status_in_bundle(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 25 (parity): the deliberate MCP control actions keep authoring
    the status in the bundle's copy."""
    pause_gate.test_mcp_pause_resume_control_actions_still_author_status(
        server, tmp_path, monkeypatch
    )


# --- Round 16 (#1402): the pre-send decision refreshes the AUTHORITATIVE
# pause state. The source-tree regression now flips only the shared-storage
# copy, so a local re-read cannot see it; these wrappers run the same two
# cases against the bundle tree.


def test_parity_pause_during_membership_validation_suppresses_send(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    pause_gate.test_pause_during_membership_validation_suppresses_send(
        server, tmp_path, monkeypatch
    )


def test_parity_presend_refresh_failure_suppresses_send(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    pause_gate.test_presend_refresh_failure_suppresses_send(
        server, tmp_path, monkeypatch
    )
