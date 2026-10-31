"""OpenClaw MCP child-env projection + default task lifecycle (mirrors the
accepted Hermes child-env suite).

OpenClaw's native MCP runner filters the child environment: the entry's
``env`` map is expanded from the environment over the whole parsed config
(``src/config/env-substitution.ts``) and the spawned server receives that
map — nothing else from the parent crosses the boundary (no baseline
inheritance, no role overrides, no whole-env pass-through). These tests
verify the adapter's projected entry against that contract:

1. the entry env is exactly the resolvable allowlist — all values
   ``${NAME}``, no literal secret, no role vars (a clean installer env
   yields only the runtime-config entry);
2. a faithful local reproduction of the OpenClaw MCP child-env filter
   (entry env expanded from a fake parent env; nothing else inherited)
   keeps only the allowlisted values;
3. when the real OpenClaw SDK (``OPENCLAW_SDK_ROOT`` / ``OPENCLAW_BIN`` /
   ``openclaw`` on PATH) is available, the real config readback satisfies
   the same contract (skipped with an explicit reason otherwise — CI runs
   it with ``openclaw@2026.4.14``);
4. under that child env the default ``delegate_task`` -> ``ack_task`` ->
   ``submit_task`` lifecycle succeeds with the existing harness backends,
   and the persisted submission identity survives a restart of the MCP
   server process (idempotent re-submit);
5. negative control: a child env carrying only
   ``TEAMHARNESS_RUNTIME_CONFIG`` reproduces the documented pull failure
   ("storage alias agentteams is not configured; missing ...").
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE = REPO_ROOT / "plugins" / "agentteams-teamharness"
OPENCLAW_ADAPTER = BUNDLE / "adapters" / "openclaw"
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
ALIAS_NOT_CONFIGURED = (
    "storage alias agentteams is not configured; missing "
    "AGENTTEAMS_FS_ENDPOINT/AGENTTEAMS_FS_ACCESS_KEY/AGENTTEAMS_FS_SECRET_KEY"
)


def fake_parent_env(tmp_path: Path) -> dict:
    """The parent (worker) environment the OpenClaw runtime would carry:
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


def _isolated_installer_env(home: Path, parent_env: dict) -> dict:
    """Deterministic installer env: HOME pinned, the OPENCLAW_* resolution
    variables and every allowlisted/role/junk variable scrubbed, then
    ``parent_env`` applied (OpenClaw has no documented dotenv file, so the
    installer env is the only resolvable source)."""
    env = dict(os.environ)
    env["HOME"] = str(home)
    for key in ("OPENCLAW_CONFIG", "OPENCLAW_CONFIG_PATH", "OPENCLAW_STATE_DIR", "OPENCLAW_HOME"):
        env.pop(key, None)
    for key in list(env):
        if key in ALLOWLIST or key in ROLE_VARS or key in JUNK_VARS:
            del env[key]
    env.update(parent_env)
    return env


def install_openclaw_entry(tmp_path: Path, parent_env: dict) -> dict:
    """Run the real installer into an isolated HOME under the fake parent
    env (which doubles as the installer env) and return the generated
    ``mcp.servers.teamharness`` entry."""
    home = tmp_path / "home"
    (home / ".openclaw").mkdir(parents=True, exist_ok=True)
    env = _isolated_installer_env(home, parent_env)
    config = home / ".openclaw" / "openclaw.json"
    result = subprocess.run(
        [
            "bash",
            str(OPENCLAW_ADAPTER / "install.sh"),
            "--config",
            str(config),
        ],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    data = json.loads(config.read_text())
    return data["mcp"]["servers"]["teamharness"]


def _expand_openclaw_ref(value: Any, parent_env: dict) -> Any:
    """Mirror of ``src/config/env-substitution.ts`` (the pinned runtime's
    config-load stage): expand ``${VAR}`` from the parent environment,
    keeping the literal placeholder when the variable is unset (uppercase
    names, as in the upstream implementation)."""
    if isinstance(value, str):
        return re.sub(
            r"\$\{([A-Z][A-Z0-9_]*)\}",
            lambda m: parent_env.get(m.group(1), m.group(0)),
            value,
        )
    return value


def build_openclaw_child_env_repro(entry: dict, parent_env: dict) -> dict:
    """Faithful local reproduction of the OpenClaw MCP child env: the
    entry's ``env`` map is expanded from the parent environment and the
    spawned server receives exactly that map — nothing else from the
    parent crosses the boundary (no safe-baseline inheritance, no role
    overrides, no whole-env pass-through)."""
    child = {}
    for key, value in (entry.get("env") or {}).items():
        expanded = _expand_openclaw_ref(value, parent_env)
        if isinstance(expanded, str):
            child[key] = expanded
    return child


def _find_openclaw_cli() -> tuple[list[str], str] | None:
    """The real OpenClaw CLI (its config/env-substitution stage), or None.

    Lookup order: ``OPENCLAW_BIN``, ``openclaw`` on PATH, then a runnable
    CLI entry under ``OPENCLAW_SDK_ROOT`` (a checkout or npm package, run
    through node). There is no importable Python module — the SDK is the
    TypeScript runtime, so the CLI is the unit under test."""
    binary = os.environ.get("OPENCLAW_BIN", "").strip()
    if binary and Path(binary).is_file():
        return [binary], "OPENCLAW_BIN"
    on_path = shutil.which("openclaw")
    if on_path:
        return [on_path], "openclaw on PATH"
    root = os.environ.get("OPENCLAW_SDK_ROOT", "").strip()
    if root:
        node = shutil.which("node")
        if node:
            for candidate in (
                "bin/openclaw.js",
                "dist/cli.js",
                "openclaw.js",
                "openclaw.mjs",
                "cli.js",
            ):
                entry = Path(root) / candidate
                if entry.is_file():
                    return [node, str(entry)], f"OPENCLAW_SDK_ROOT ({candidate})"
    return None


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
    # that another test's imports may have pulled in) so
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
# 1. entry env is exactly the resolvable allowlist, by reference only
# ---------------------------------------------------------------------------


def test_entry_env_is_exactly_the_resolvable_allowlist(tmp_path):
    """Entry shape: a clean installer env carries only the runtime-config
    reference; an installer env with every allowlisted var set carries
    exactly those ${NAME} references — no literal values, no role
    overrides, no unrelated variables."""
    # 1. clean installer env (every allowlisted/role/junk var scrubbed)
    entry = install_openclaw_entry(tmp_path, {})
    assert entry["env"] == {ENV_VAR: "${" + ENV_VAR + "}"}

    # 2. every allowlisted var set (plus junk and role overrides that
    #    must stay out)
    parent_env = fake_parent_env(tmp_path)
    entry = install_openclaw_entry(tmp_path, parent_env)
    assert set(entry["env"]) == set(ALLOWLIST)
    for name in ALLOWLIST:
        assert entry["env"][name] == "${" + name + "}"
    raw = (tmp_path / "home" / ".openclaw" / "openclaw.json").read_text()
    for name in ALLOWLIST:
        assert parent_env[name] not in raw, f"literal value of {name} written into config"
    for name in ROLE_VARS + JUNK_VARS:
        assert name not in raw, f"excluded variable {name} projected into the entry"


# ---------------------------------------------------------------------------
# 2. faithful child-env filter behaviour (local reproduction of the SDK)
# ---------------------------------------------------------------------------


def test_faithful_openclaw_child_env_keeps_only_allowlisted_values(tmp_path):
    parent_env = fake_parent_env(tmp_path)
    entry = install_openclaw_entry(tmp_path, parent_env)
    child = build_openclaw_child_env_repro(entry, parent_env)

    # exactly the allowlisted variables, resolved from the parent env —
    # nothing else from the parent crosses the boundary
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
# 3. real SDK path (openclaw CLI, when available)
# ---------------------------------------------------------------------------


def test_real_openclaw_config_readback_keeps_only_allowlisted_values(tmp_path):
    cli = _find_openclaw_cli()
    if cli is None:
        pytest.skip(
            "no OpenClaw SDK available for the real-SDK child-env regression "
            "(set OPENCLAW_BIN or OPENCLAW_SDK_ROOT, or put openclaw on PATH; "
            "CI installs openclaw@2026.4.14)"
        )
    argv_prefix, source = cli
    parent_env = fake_parent_env(tmp_path)
    install_openclaw_entry(tmp_path, parent_env)

    # The runtime expands ${VAR} from the process environment at config
    # load (src/config/env-substitution.ts); point the CLI at the
    # isolated HOME and run it under the fake parent env.
    home = tmp_path / "home"
    config = home / ".openclaw" / "openclaw.json"
    env = _isolated_installer_env(home, parent_env)
    env["OPENCLAW_CONFIG_PATH"] = str(config)
    result = subprocess.run(
        [*argv_prefix, "config", "get", "mcp.servers.teamharness", "--json"],
        capture_output=True,
        text=True,
        env=env,
        timeout=120,
    )
    if result.returncode != 0:
        pytest.skip(
            f"openclaw CLI ({source}) `config get` failed "
            f"(signature drift from the pinned runtime?): "
            f"{result.stderr.strip()[:400]}"
        )
    text = result.stdout
    try:
        payload = json.loads(text)
    except json.JSONDecodeError:
        start, end = text.find("{"), text.rfind("}")
        assert start != -1 and end > start, f"no JSON object in openclaw output:\n{text}"
        payload = json.loads(text[start : end + 1])
    if isinstance(payload, dict) and isinstance(payload.get("mcp"), dict):
        server = payload["mcp"].get("servers", {}).get("teamharness")
    else:
        server = payload
    resolved = server.get("env") if isinstance(server, dict) else None
    assert isinstance(resolved, dict), (
        f"no env map in the real-SDK readback:\n{json.dumps(payload, indent=2)}"
    )
    # the same contract as the local reproduction: the allowlisted
    # storage/Matrix context is present and resolved from the parent env;
    # role overrides and unrelated junk are absent
    for name in ALLOWLIST:
        assert resolved.get(name) == parent_env[name], (
            f"{name} missing or wrong in the real-SDK readback: {resolved.get(name)!r}"
        )
    for name in ROLE_VARS + JUNK_VARS:
        assert name not in resolved


# ---------------------------------------------------------------------------
# 4. default task lifecycle under the projected child env, with restart
#    idempotency of the persisted submission identity
# ---------------------------------------------------------------------------


def test_delegate_ack_submit_under_projected_child_env(tmp_path):
    parent_env = fake_parent_env(tmp_path)
    entry = install_openclaw_entry(tmp_path, parent_env)
    # the child process environment the runtime would spawn (exactly the
    # expanded entry env — nothing else crosses the boundary)
    child_env = build_openclaw_child_env_repro(entry, parent_env)

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

    server = _load_mcp_server("th_oc_child_env_server")
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
    server2 = _load_mcp_server("th_oc_child_env_server_restart")
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
# 5. negative control: only TEAMHARNESS_RUNTIME_CONFIG in the child env
# ---------------------------------------------------------------------------


def test_child_env_without_storage_context_reproduces_alias_failure(tmp_path):
    """The pre-fix shape of the entry (a single
    ``TEAMHARNESS_RUNTIME_CONFIG`` reference): under the OpenClaw
    child-env filter the child receives none of the storage/Matrix
    context, so the documented pull failure reproduces — no stubs: the
    real ``_filesync`` runs with no credentials and no resolvable storage
    alias (the shared prefix from the runtime config rides the
    ``agentteams`` mc alias), and the default delegate lifecycle hits the
    retryable pull failure on top."""
    # a Worker-style runtime config: the shared prefix rides the
    # "agentteams" mc alias, which the child cannot resolve
    runtime = tmp_path / "runtime.yaml"
    runtime.write_text(
        "storage:\n  sharedPrefix: agentteams/teams/team-a/shared\n",
        encoding="utf-8",
    )
    child_env = {ENV_VAR: str(runtime)}

    workspace = tmp_path / "ws" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    project_id, task_id = "proj-neg", "task-neg"

    server = _load_mcp_server("th_oc_child_env_server_negative")
    with _environ(child_env):
        listing = _mcp_call(
            server,
            "filesync",
            {
                "workspaceDir": str(workspace),
                "action": "list",
                "path": f"shared/projects/{project_id}/",
            },
        )
        assert listing["ok"] is False
        assert listing["error"] == ALIAS_NOT_CONFIGURED

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
