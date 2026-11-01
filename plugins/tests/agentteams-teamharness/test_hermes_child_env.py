"""Hermes MCP child-env projection + default task lifecycle (maintainer
P1 on 1143b604).

The supported hermes ``v2026.4.16`` SDK builds the MCP child environment
in two stages (``tools/mcp_tool.py``): at config load,
``_load_mcp_config()`` makes ``~/.hermes/.env`` available and resolves
each ``${VAR}`` in the server config from the environment (an
unresolvable reference stays the literal placeholder); at spawn,
``_build_safe_env(user_env)`` gives the child a small safe baseline
(PATH, HOME, TMPDIR, LANG, TERM, LC_ALL, SHELL, USER plus ``XDG_*``)
from the parent process and the entry's env map — nothing else crosses
the boundary (no role overrides, no secrets, no whole-env
inheritance). These tests verify the adapter's projected entry against
that contract:

1. a faithful local reproduction of the SDK filtering yields a child env
   with exactly the allowlisted storage/Matrix values — no role
   overrides, no unrelated junk;
2. when the real SDK (``HERMES_SDK_ROOT`` / ``HERMES_416_ROOT`` /
   ``/tmp/hermes-416`` / importable ``tools.mcp_tool``) is available, the
   real ``_build_safe_env`` satisfies the same contract (skipped with an
   explicit reason otherwise — CI installs hermes ``v2026.4.16``);
3. under that child env the default ``delegate_task`` -> ``ack_task`` ->
   ``submit_task`` lifecycle succeeds with the existing harness backends,
   and the persisted submission identity survives a restart of the MCP
   server process (idempotent re-submit);
4. negative control: a child env carrying only
   ``TEAMHARNESS_RUNTIME_CONFIG`` reproduces the documented pull failure
   ("cannot pull authoritative project state from shared storage; retry").
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import os
import re
import subprocess
import sys
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE = REPO_ROOT / "plugins" / "agentteams-teamharness"
HERMES_ADAPTER = BUNDLE / "adapters" / "hermes"
MCP_SERVER_PY = BUNDLE / "mcp" / "server.py"

ENV_VAR = "TEAMHARNESS_RUNTIME_CONFIG"
ALLOWLIST = (
    # state/runtime
    "TEAMHARNESS_RUNTIME_CONFIG",
    "TEAMHARNESS_SHARED_DIR",
    "AGENTTEAMS_SHARED_DIR",
    "AGENTTEAMS_STORAGE_PREFIX",
    "AGENTTEAMS_SHARED_STORAGE_PREFIX",
    "TEAMHARNESS_MATRIX_CONTEXT_FILE",
    # Matrix
    "AGENTTEAMS_MATRIX_URL",
    "AGENTTEAMS_WORKER_MATRIX_TOKEN",
    "AGENTTEAMS_WORKER_NAME",
    "AGENTTEAMS_MATRIX_USER_ID",
    "AGENTTEAMS_MATRIX_DOMAIN",
    # storage credentials
    "AGENTTEAMS_FS_ENDPOINT",
    "AGENTTEAMS_FS_ACCESS_KEY",
    "AGENTTEAMS_FS_SECRET_KEY",
)
ROLE_VARS = ("AGENTTEAMS_AGENT_ROLE", "AGENTTEAMS_WORKER_ROLE")
JUNK_VARS = ("TH_TEST_UNRELATED_JUNK", "ANOTHER_JUNK_VAR")
PULL_FAILED = "cannot pull authoritative project state from shared storage; retry"
# The SDK's spawn-time safe baseline (tools/mcp_tool.py::_SAFE_ENV_KEYS),
# plus the XDG_* prefix filter.
SAFE_BASE_KEYS = frozenset(
    {"HOME", "TMPDIR", "LANG", "PATH", "TERM", "LC_ALL", "SHELL", "USER"}
)


def fake_parent_env(tmp_path: Path) -> dict:
    """The parent (worker) environment the hermes runtime would carry:
    every allowlisted variable set to a synthetic value, plus unrelated
    junk and the role overrides the child must never see."""
    env = {
        ENV_VAR: str(tmp_path / "runtime.yaml"),
        "TEAMHARNESS_SHARED_DIR": str(tmp_path / "shared"),
        "AGENTTEAMS_SHARED_DIR": str(tmp_path / "shared2"),
        "AGENTTEAMS_STORAGE_PREFIX": "teams/team-a",
        "AGENTTEAMS_SHARED_STORAGE_PREFIX": "teams/team-a/shared",
        "TEAMHARNESS_MATRIX_CONTEXT_FILE": str(tmp_path / "matrix-context.json"),
        "AGENTTEAMS_MATRIX_URL": "http://127.0.0.1:1",
        "AGENTTEAMS_WORKER_MATRIX_TOKEN": "synthetic-worker-token-123",
        "AGENTTEAMS_WORKER_NAME": "worker-a",
        "AGENTTEAMS_MATRIX_USER_ID": "@worker-a:m.org",
        "AGENTTEAMS_MATRIX_DOMAIN": "m.org",
        "AGENTTEAMS_FS_ENDPOINT": "http://127.0.0.1:9000",
        "AGENTTEAMS_FS_ACCESS_KEY": "synthetic-minio-access",
        "AGENTTEAMS_FS_SECRET_KEY": "synthetic-minio-secret",
    }
    env.update(
        {
            JUNK_VARS[0]: "junk-value-1",
            JUNK_VARS[1]: "junk-value-2",
            "AGENTTEAMS_AGENT_ROLE": "override-leader",
            "AGENTTEAMS_WORKER_ROLE": "override-worker",
        }
    )
    return env


def install_hermes_entry(tmp_path: Path, parent_env: dict) -> dict:
    """Run the real installer into an isolated HOME under the fake parent
    env (which doubles as the installer env) and return the generated
    ``mcp_servers.teamharness`` entry."""
    home = tmp_path / "home"
    (home / ".hermes").mkdir(parents=True)
    env = dict(os.environ)
    env["HOME"] = str(home)
    env["HERMES_HOME"] = str(home / ".hermes")
    for key in list(env):
        if key in ALLOWLIST or key in ROLE_VARS or key in JUNK_VARS:
            del env[key]
    env.update(parent_env)
    config = home / ".hermes" / "config.yaml"
    plugin_home = home / ".hermes" / "plugins" / "agentteams-teamharness"
    result = subprocess.run(
        [
            "bash",
            str(HERMES_ADAPTER / "install.sh"),
            "--config",
            str(config),
            "--plugin-home",
            str(plugin_home),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    data = yaml.safe_load(config.read_text())
    return data["mcp_servers"]["teamharness"]


def _interpolate_env_vars(value: Any, parent_env: dict) -> Any:
    """Mirror of ``tools/mcp_tool.py::_interpolate_env_vars`` (the
    config-load stage): resolve ``${VAR}`` from the parent environment,
    keeping the literal placeholder when unresolvable."""
    if isinstance(value, str):
        return re.sub(
            r"\$\{([^}]+)\}",
            lambda m: parent_env.get(m.group(1), m.group(0)),
            value,
        )
    if isinstance(value, dict):
        return {k: _interpolate_env_vars(v, parent_env) for k, v in value.items()}
    if isinstance(value, list):
        return [_interpolate_env_vars(v, parent_env) for v in value]
    return value


def build_safe_env_repro(entry: dict, parent_env: dict) -> dict:
    """Faithful local reproduction of the hermes ``v2026.4.16`` SDK child
    env (``tools/mcp_tool.py``): the config-load stage interpolates
    ``${VAR}`` in the entry from the parent environment, then the
    spawn-time ``_build_safe_env`` keeps only the safe baseline
    (SAFE_BASE_KEYS + ``XDG_*``) from the parent process and adds the
    entry's ``env`` map. Nothing else from the parent crosses over."""
    user_env = _interpolate_env_vars(entry.get("env"), parent_env)
    env = {
        key: value
        for key, value in parent_env.items()
        if key in SAFE_BASE_KEYS or key.startswith("XDG_")
    }
    if user_env:
        env.update(user_env)
    return env


@contextmanager
def _environ(replacement: dict):
    """Replace os.environ wholesale (the in-process MCP server sees
    exactly this env, like the spawned child would) and restore it."""
    saved = dict(os.environ)
    os.environ.clear()
    os.environ.update(replacement)
    try:
        yield
    finally:
        os.environ.clear()
        os.environ.update(saved)


def _load_mcp_server(module_name: str):
    """Load a fresh copy of mcp/server.py (each test drives its own
    instance; refs.bind points call-time lookups at it)."""
    # Evict a foreign top-level `mcp` package (e.g. the MCP-protocol SDK
    # that the real-SDK test's tools.mcp_tool import pulls in) so
    # `from mcp.artifact import ...` inside server.py resolves to this
    # bundle's package.
    bound = sys.modules.get("mcp")
    bound_file = str(getattr(bound, "__file__", "") or "")
    if bound is not None and not bound_file.startswith(str(BUNDLE / "mcp")):
        for name in [n for n in sys.modules if n == "mcp" or n.startswith("mcp.")]:
            del sys.modules[name]
    spec = importlib.util.spec_from_file_location(module_name, MCP_SERVER_PY)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _stub_harness_backends(server: Any) -> None:
    """The existing harness backends (test_trace_integration pattern):
    local state is authoritative; the shared-storage sync and Matrix
    notification paths are stubbed — no real MinIO/Matrix in unit tests."""
    server._sync_task = lambda *a, **kw: True
    server._pull_task = lambda *a, **kw: True
    server._sync_project = lambda *a, **kw: True
    server._pull_project = lambda *a, **kw: True
    server._validate_assignee_membership = lambda *a, **kw: {"ok": True}
    server._send_delegate_notification = lambda *a, **kw: {
        "sent": True,
        "eventId": "$fake-delegate",
        "roomId": "!worker-room:m.org",
        "assignee": "worker-a",
    }


def _mcp_call(server: Any, name: str, arguments: dict) -> dict:
    result = server.call_tool(name, arguments)
    return json.loads(result["content"][0]["text"])


def _seed_project(workspace: Path, project_id: str, task_id: str) -> None:
    project_dir = workspace / "shared" / "projects" / project_id
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / "meta.json").write_text(
        json.dumps(
            {
                "project_id": project_id,
                "source_room_id": "!team:m.org",
                "tasks": [
                    {"task_id": task_id, "status": "ready", "assigned_to": "worker-a"}
                ],
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )


# ---------------------------------------------------------------------------
# 1. faithful _build_safe_env behaviour (local reproduction of the SDK)
# ---------------------------------------------------------------------------


def test_faithful_build_safe_env_keeps_only_allowlisted_values(tmp_path):
    parent_env = fake_parent_env(tmp_path)
    entry = install_hermes_entry(tmp_path, parent_env)
    # the fake parent env carries no safe-baseline variable, so the
    # faithful child env is exactly the projected entry env
    child = build_safe_env_repro(entry, parent_env)

    # exactly the allowlisted variables, resolved from the parent env
    assert set(child) == set(ALLOWLIST)
    for name in ALLOWLIST:
        assert child[name] == parent_env[name]
    # role overrides and unrelated junk stay out of the child
    for name in ROLE_VARS + JUNK_VARS:
        assert name not in child
    # the entry itself carries references only — no literal values
    for name in ALLOWLIST:
        assert parent_env[name] not in entry["env"].values()


# ---------------------------------------------------------------------------
# 2. real SDK path (hermes v2026.4.16 checkout, when available)
# ---------------------------------------------------------------------------


def _import_hermes_mcp_tool():
    """The real ``tools.mcp_tool`` from a pinned hermes checkout, or None.

    Lookup order: ``HERMES_SDK_ROOT``, ``HERMES_416_ROOT`` (the repo's
    existing convention, see test_hermes_plugin_sdk.py), ``/tmp/hermes-416``,
    then an already-importable ``tools.mcp_tool``."""
    roots = []
    for var in ("HERMES_SDK_ROOT", "HERMES_416_ROOT"):
        root = os.environ.get(var, "").strip()
        if root:
            roots.append(Path(root))
    roots.append(Path("/tmp/hermes-416"))
    for root in roots:
        if (root / "tools" / "mcp_tool.py").is_file():
            if str(root) not in sys.path:
                sys.path.insert(0, str(root))
            try:
                return importlib.import_module("tools.mcp_tool")
            except Exception:
                return None
    try:
        return importlib.import_module("tools.mcp_tool")
    except ImportError:
        return None


REQUIRE_NATIVE_ENV = "AGENTTEAMS_TEST_REQUIRE_NATIVE_SDK"


def _require_native_sdk() -> bool:
    """CI sets this on the job that installs a pinned SDK: a configured
    native runtime that cannot run must FAIL that job instead of skipping."""
    return os.environ.get(REQUIRE_NATIVE_ENV, "").strip() == "1"


def _skip_or_fail(reason: str) -> None:
    if _require_native_sdk():
        pytest.fail(f"{reason} (required by {REQUIRE_NATIVE_ENV}=1)")
    pytest.skip(reason)


def test_real_sdk_build_safe_env_keeps_only_allowlisted_values(tmp_path, monkeypatch):
    mcp_tool = _import_hermes_mcp_tool()
    if mcp_tool is None:
        _skip_or_fail(
            "hermes v2026.4.16 SDK not available for the real-SDK child-env "
            "regression (set HERMES_SDK_ROOT or HERMES_416_ROOT to a "
            "checkout with tools/mcp_tool.py; CI installs hermes v2026.4.16)"
        )
    fn = getattr(mcp_tool, "_build_safe_env", None)
    if fn is None:
        _skip_or_fail(
            "hermes SDK's tools.mcp_tool has no _build_safe_env (signature "
            "drift from the v2026.4.16 pin)"
        )
    parent_env = fake_parent_env(tmp_path)
    entry = install_hermes_entry(tmp_path, parent_env)

    # The SDK resolves ${VAR} from the process environment at config
    # load; both stages read os.environ, so point it at the fake parent
    # env.
    monkeypatch.setattr(os, "environ", dict(parent_env))
    interpolate = getattr(mcp_tool, "_interpolate_env_vars", None)
    if callable(interpolate):
        # the real config-load stage (keeps unresolvable references
        # verbatim)
        cfg = interpolate(entry)
        user_env = cfg.get("env") if isinstance(cfg, dict) else None
    else:
        user_env = _interpolate_env_vars(entry.get("env"), parent_env)
    try:
        child = fn(user_env)
    except TypeError:
        _skip_or_fail(
            "hermes SDK _build_safe_env no longer takes the entry's env "
            "map (signature drift from the v2026.4.16 pin); update the "
            "call site"
        )
    assert isinstance(child, dict)
    # the same contract as the local reproduction: the allowlisted
    # storage/Matrix context is present and resolved from the parent env;
    # role overrides and unrelated junk are absent (the SDK may add its
    # own base variables, so set equality is asserted on the reproduction)
    for name in ALLOWLIST:
        assert child.get(name) == parent_env[name], (
            f"{name} missing or wrong in the real SDK child env: "
            f"{child.get(name)!r}"
        )
    for name in ROLE_VARS + JUNK_VARS:
        assert name not in child


# ---------------------------------------------------------------------------
# 3. default task lifecycle under the projected child env, with restart
#    idempotency of the persisted submission identity
# ---------------------------------------------------------------------------


def test_delegate_ack_submit_under_projected_child_env(tmp_path):
    parent_env = fake_parent_env(tmp_path)
    entry = install_hermes_entry(tmp_path, parent_env)
    # the child process environment the SDK would spawn (the fake parent
    # env carries no safe-baseline variable, so this is exactly the
    # projected entry env)
    child_env = build_safe_env_repro(entry, parent_env)

    workspace = tmp_path / "ws" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    project_id, task_id = "proj-child", "task-child"
    _seed_project(workspace, project_id, task_id)
    base_args = {"workspaceDir": str(workspace)}
    submit_payload = {
        "taskId": task_id,
        "summary": "feature built",
        "status": "SUCCESS",
        "deliverables": [f"shared/tasks/{task_id}/output.md"],
    }

    server = _load_mcp_server("th_child_env_server")
    _stub_harness_backends(server)
    with _environ(child_env):
        delegate = _mcp_call(
            server,
            "taskflow",
            {
                **base_args,
                "action": "delegate_task",
                "role": "leader",
                "payload": {
                    "projectId": project_id,
                    "taskId": task_id,
                    "roomId": "!worker-room:m.org",
                    "assignedTo": "worker-a",
                    "spec": "build the feature",
                },
            },
        )
        assert delegate["ok"] is True, delegate
        assert delegate["task"]["status"] == "assigned"
        assert delegate["notification"]["sent"] is True

        ack = _mcp_call(
            server,
            "taskflow",
            {
                **base_args,
                "action": "ack_task",
                "role": "worker",
                "payload": {"taskId": task_id},
            },
        )
        assert ack["ok"] is True, ack
        assert ack["task"]["status"] == "in_progress"

        submit = _mcp_call(
            server,
            "taskflow",
            {**base_args, "action": "submit_task", "role": "worker", "payload": submit_payload},
        )
        assert submit["ok"] is True, submit
        assert submit["task"]["status"] == "submitted"
        submission_id = submit["task"]["submission_id"]
        assert submission_id
        assert submit["task"]["submitted_by_role"] == "worker"

    # Restart the MCP server process: a fresh module instance over the same
    # workspace — the persisted state (incl. the submission identity) is on
    # disk, and the new process runs under the same projected child env.
    server2 = _load_mcp_server("th_child_env_server_restart")
    _stub_harness_backends(server2)
    with _environ(child_env):
        resubmit = _mcp_call(
            server2,
            "taskflow",
            {**base_args, "action": "submit_task", "role": "worker", "payload": submit_payload},
        )
    assert resubmit["ok"] is True, resubmit
    assert resubmit.get("reused") is True
    # the persisted submission identity survives the restart
    assert resubmit["task"]["submission_id"] == submission_id
    assert resubmit["task"]["submitted_by_role"] == "worker"
    assert resubmit["task"]["status"] == "submitted"


# ---------------------------------------------------------------------------
# 4. negative control: only TEAMHARNESS_RUNTIME_CONFIG in the child env
# ---------------------------------------------------------------------------


def test_child_env_without_storage_context_reproduces_pull_failure(tmp_path):
    """The pre-fix shape of the entry (a single
    ``TEAMHARNESS_RUNTIME_CONFIG`` reference): under the SDK filtering the
    child receives none of the storage/Matrix context, so the first
    mutating taskflow action hits the documented retryable pull failure —
    no stubs: the real ``_pull_project`` runs against the real
    ``_filesync`` (no credentials, no resolvable storage alias)."""
    child_env = {ENV_VAR: str(tmp_path / "runtime.yaml")}

    workspace = tmp_path / "ws" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    project_id, task_id = "proj-neg", "task-neg"

    server = _load_mcp_server("th_child_env_server_negative")
    with _environ(child_env):
        result = _mcp_call(
            server,
            "taskflow",
            {
                "workspaceDir": str(workspace),
                "action": "delegate_task",
                "role": "leader",
                "payload": {
                    "projectId": project_id,
                    "taskId": task_id,
                    "roomId": "!worker-room:m.org",
                    "assignedTo": "worker-a",
                    "spec": "build the feature",
                },
            },
        )
    assert result["ok"] is False
    assert result["error"] == PULL_FAILED
    assert result["retryable"] is True
    assert result["action"] == "delegate_task"
