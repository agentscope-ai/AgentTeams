"""#1402 refactor B: the Python-side pause gate (#1431 acceptance).

Once a pause takes effect: no new tasks are delegated, no downstream is
released, no next round opens; in-flight tasks can still ack/submit and
land their result. ``resume_project`` (agent-callable) only clears the
legacy ``status`` — ``loop.pause_sources`` is cleared by the Go API admin
action alone, an agent cannot self-resolve.

Authoritative state is ``loop.pause_sources`` in the project meta (written
by the Go side); the legacy ``project.status == "paused"`` stays
compatible and contributes the ``"project"`` source.

Covers (spec 3):
1. ``pause_sources=["user"]`` -> delegate_task rejected
2. ``pause_sources=["user","project"]`` -> delegate rejected, ack/submit succeed
3. ready_loop_nodes -> readyLoopNodes=[], paused=true, pauseSources=["user"]
4. plan_loop -> rejected
5. record_loop_iteration -> recorded but paused=true, readyLoopNodes=[]
6. legacy ``status="paused"`` (no sources) -> same effect as 1/3
7. resume_project (agent) -> the authoritative merge re-pauses against the
   durable remote state, sources remain, delegate still rejected; the
   Go-side clear of the sources unblocks delegation
8. pause persisted during membership validation (after the initial read,
   before the send) -> send suppressed, task stays prepared

Covers (#1402 conditional commit, the overlap between the entry pull and
the submit write-back):
9. pause lands on the remote between the worker's pull and its submit
   write-back -> the conditional commit (If-Match on the observed ETag)
   merges the pause state and the committed remote meta keeps the full
   pause state (status, pause_sources, paused_by_project, extra loop
   keys) while the worker's task update lands
10. the conditional commit conflicts once (a second pause source drifts
    in between the read and the commit) -> the bounded retry
    re-snapshots, re-merges and converges with every source preserved
11. storage drifts the meta during every commit window -> the bounded
    budget (3 attempts) is exhausted and _sync_project returns False
12. no remote pause -> the commit is verified against the committed ETag
    (the empty expectation must not early-return over the re-read) and
    the worker-authored fields land in the final remote meta

Covers (#1402 conditional commit, the later boundary and fail-closed):
13. no pause in the snapshot, but the remote is paused before the commit
    (the 409 path) -> the retry re-snapshots and the committed meta
    keeps the pause
14. no pause in the merged meta, but a pause lands between the commit
    and the verify re-read (the removed early-return pin) -> the
    committed-ETag re-check catches the new version and the retry keeps
    the pause in both the committed meta and the local copy
15. the maintainer's #1402 P1 overlap (snapshot -> human pause ->
    ETag/commit): the remote serves the unpaused body first, the pause
    lands immediately after that read (new body, new ETag) -> the
    versioned read (stat -> cat -> stat) detects the version change,
    discards the stale read, re-reads, merges the pause and commits the
    paused body — the final remote meta keeps pause_sources=["user"] and
    status="paused", the worker's task update is present, and a fresh
    pull reports paused
16. the remote version flips between the bracketing stats on every read
    (a writer in flight on every attempt) -> the bounded versioned read
    discards every body and exhausts its attempts, and _sync_project
    returns False without ever attempting a commit (no stale commit)
17. the controller rejects the conditional commit (404/405 = the
    controller predates the endpoint, 428, 503) -> fail closed:
    _sync_project returns False and the meta is never written through an
    unconditional filesync push
18. the remote meta object is absent (no ETag to match) -> the normal
    first push: the directory push INCLUDING meta.json (no conditional
    commit, no exclusion) is the commit, and the sync returns its result
19. the ETag probe fails (a storage error, not an absent object) ->
    fail closed: no commit attempted, and no directory push that omits
    meta.json either (an unknown remote version is never half-written)
"""

from __future__ import annotations

import hashlib
import importlib
import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"

WORKER = "@worker-a:example.test"
ROOM = "!pause-gate:example.test"


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_pause_gate_test",
        MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


@pytest.fixture()
def server() -> Any:
    return _load_server()


def _arguments(workspace: Path, action: str, **payload: Any) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": action, "payload": payload}


def _write_project(workspace: Path, project_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _read_project(workspace: Path, project_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _meta_etag(meta: dict[str, Any]) -> str:
    """A deterministic per-content etag: models the object store's
    content-derived ETag, so a version is addressed by what it contains
    — the same property the controller's strict CAS (If-Match) relies on."""
    return hashlib.sha256(
        json.dumps(meta, sort_keys=True, ensure_ascii=False).encode("utf-8")
    ).hexdigest()


class FakeProjectStorage:
    """The filesync + conditional-commit fake behind one remote meta store.

    Models the #1402 conflict windows under the conditional commit flow:

    - ``pull`` writes the current remote object to the local file, then
      applies ``pull_mutation`` once (the concurrent Controller write
      that lands right after the worker's pull);
    - ``cat`` reads the remote object without touching local state, then
      applies ``cat_mutation`` once (the concurrent Controller write that
      lands right after the worker's body read — the maintainer's
      snapshot -> pause -> ETag window);
    - ``etag`` returns the store's content-derived etag (absent object
      -> the does-not-exist error ``mc stat --json`` produces;
      ``etag_read_error`` forces a non-absent probe failure) and, when
      ``stat_mutation`` is set, applies it after every etag read (a
      writer in flight between the bracketing stats);
    - a single-file push of a project meta is a hard test error
      (tripwire): the #1402 flow must commit the meta through the
      controller's conditional endpoint, never through an unconditional
      filesync push;
    - ``commit(project_id, meta, etag)`` (installed over
      ``server.sync._commit_project_meta``) is the strict CAS: it
      applies ``window_mutation`` first (drift landing between the
      worker's read and its commit), a mismatch with the store's current
      etag returns the 409 shape, a match writes the meta and returns
      its new etag, then ``commit_mutation`` is applied (drift landing
      between the commit and the verify re-read); ``fail_closed``
      returns a scripted non-409 endpoint failure (428/503/404/405)
      instead of touching the store;
    - a directory push WITHOUT an exclusion mirrors the local meta.json
      into the store (the first push: the directory push including
      meta.json is the commit); one that excludes meta.json (the
      conditional-commit path) never touches the meta object; both are
      recorded and succeed;
    - task pushes are recorded and succeed.

    In the real system the Controller's CreateProject writes the meta to
    object storage, so a live project normally has a remote meta (and
    therefore an ETag); tests of the conditional-commit path seed the
    store accordingly. The store is left empty only for the first-push
    case (spec 18): the remote object is absent, and the directory push
    including meta.json is the commit.
    """

    def __init__(
        self,
        remote: dict[str, dict[str, Any]] | None = None,
        *,
        pull_mutation: Any = None,
        cat_mutation: Any = None,
        stat_mutation: Any = None,
        window_mutation: Any = None,
        commit_mutation: Any = None,
        drift_always: bool = False,
        fail_closed: dict[str, Any] | None = None,
        etag_read_error: str | None = None,
    ) -> None:
        self.remote: dict[str, dict[str, Any]] = {
            key: dict(value) for key, value in (remote or {}).items()
        }
        self.pull_mutation = pull_mutation
        self.cat_mutation = cat_mutation
        self.stat_mutation = stat_mutation
        self.window_mutation = window_mutation
        self.commit_mutation = commit_mutation
        self.drift_always = drift_always
        self.fail_closed = fail_closed
        self.etag_read_error = etag_read_error
        self.calls: list[dict[str, Any]] = []
        self.commit_calls = 0
        self.commits: list[tuple[str, dict[str, Any]]] = []

    @staticmethod
    def _is_directory(path: str) -> bool:
        return "." not in path.rsplit("/", 1)[-1]

    def _etag(self, path: str) -> str | None:
        meta = self.remote.get(path)
        return _meta_etag(meta) if meta is not None else None

    def __call__(self, arguments: dict[str, Any]) -> dict[str, Any]:
        action = str(arguments.get("action") or "")
        path = str(arguments.get("path") or "")
        local = Path(arguments["workspaceDir"]) / Path(*path.split("/"))
        self.calls.append(dict(arguments))
        if action == "cat":
            meta = self.remote.get(path)
            if meta is None:
                return {
                    "ok": False,
                    "error": f"<ERROR> The object {path} does not exist.",
                }
            result = {"ok": True, "content": json.dumps(meta)}
            if self.cat_mutation is not None:
                # The concurrent Controller write lands immediately after
                # this body read: the body is served, THEN the store
                # changes (new body, new etag).
                self.remote[path] = self.cat_mutation(self.remote[path])
                if not self.drift_always:
                    self.cat_mutation = None
            return result
        if action == "etag":
            if self.etag_read_error is not None:
                return {"ok": False, "error": self.etag_read_error}
            etag = self._etag(path)
            if etag is None:
                return {
                    "ok": False,
                    "error": f"<ERROR> The object {path} does not exist.",
                }
            result = {"ok": True, "etag": etag}
            if self.stat_mutation is not None:
                # A writer in flight between the bracketing stats: the
                # version the caller just read is already stale.
                self.remote[path] = self.stat_mutation(self.remote[path])
                if not self.drift_always:
                    self.stat_mutation = None
            return result
        if action == "pull":
            meta = self.remote.get(path)
            if meta is None:
                return {
                    "ok": False,
                    "error": f"<ERROR> The object {path} does not exist.",
                }
            local.parent.mkdir(parents=True, exist_ok=True)
            local.write_text(json.dumps(meta), encoding="utf-8")
            if self.pull_mutation is not None:
                self.remote[path] = self.pull_mutation(self.remote[path])
                if not self.drift_always:
                    self.pull_mutation = None
            return {"ok": True}
        if action == "push":
            if self._is_directory(path):
                if "exclude" not in arguments:
                    # The first push: the directory mirror carries no
                    # exclusion, so the local meta.json lands in the
                    # store — the directory push IS the commit.
                    meta_local = local / "meta.json"
                    if meta_local.is_file():
                        self.remote[f"{path}/meta.json"] = json.loads(
                            meta_local.read_text(encoding="utf-8")
                        )
                return {"ok": True}
            if path.startswith("shared/projects/") and path.endswith("meta.json"):
                raise AssertionError(
                    "tripwire: the project meta was pushed through filesync; the #1402 "
                    "flow must commit it through the controller's conditional endpoint"
                )
            self.remote[path] = json.loads(local.read_text(encoding="utf-8"))
            return {"ok": True}
        if action == "stat":
            return {"ok": True, "exists": path in self.remote}
        return {"ok": True}

    def commit(self, project_id: str, meta: dict[str, Any], etag: str) -> dict[str, Any]:
        """The fake ``_commit_project_meta``: a strict CAS against the
        store, in the read -> commit -> verify window order."""
        path = f"shared/projects/{project_id}/meta.json"
        self.commit_calls += 1
        self.commits.append((etag, json.loads(json.dumps(meta))))
        if self.fail_closed is not None:
            return dict(self.fail_closed)
        if self.window_mutation is not None:
            self.remote[path] = self.window_mutation(self.remote[path])
            if not self.drift_always:
                self.window_mutation = None
        if etag != self._etag(path):
            return {
                "ok": False,
                "conflict": True,
                "error": "the project meta changed after the read (409); re-snapshot and re-commit",
            }
        self.remote[path] = json.loads(json.dumps(meta))
        committed_etag = self._etag(path)
        if self.commit_mutation is not None:
            self.remote[path] = self.commit_mutation(self.remote[path])
            if not self.drift_always:
                self.commit_mutation = None
        return {"ok": True, "etag": committed_etag}


def _install_sync_fakes(
    monkeypatch: Any, server: Any, storage: FakeProjectStorage
) -> None:
    """Install the storage fake as both halves of the #1402 write path:
    the filesync actions (refs proxy on the server module) and the
    controller's conditional commit (module global on server.sync)."""
    monkeypatch.setattr(server, "_filesync", storage)
    monkeypatch.setattr(server.sync, "_commit_project_meta", storage.commit)


def _loop(
    pause_sources: list[str] | None = None,
    *,
    tasks: list[dict[str, Any]] | None = None,
    current_iteration: int = 0,
) -> dict[str, Any]:
    if tasks is None:
        tasks = [
            {
                "task_id": "pg-task-01",
                "title": "In-flight pass",
                "assigned_to": WORKER,
                "depends_on": [],
                "status": "in_progress",
            },
        ]
    loop: dict[str, Any] = {
        "goal": "iterate until done",
        "stop_condition": "all checks green",
        "iteration_template": "run the checks",
        "max_iterations": 3,
        "current_iteration": current_iteration,
        "status": "running",
        "tasks": tasks,
    }
    if pause_sources is not None:
        loop["pause_sources"] = pause_sources
    return loop


def _project(
    project_id: str, *, status: str, loop: dict[str, Any] | None = None
) -> dict[str, Any]:
    meta: dict[str, Any] = {
        "project_id": project_id,
        "title": f"Pause gate {project_id}",
        "status": status,
        "plan_type": "loop" if loop is not None else "dag",
        "tasks": [],
    }
    if loop is not None:
        meta["loop"] = loop
    return meta


def _delegate_args(workspace: Path, project_id: str, task_id: str) -> dict[str, Any]:
    return _arguments(
        workspace,
        "delegate_task",
        projectId=project_id,
        taskId=task_id,
        roomId=ROOM,
        assignedTo=WORKER,
    )


def _taskflow_module() -> Any:
    return importlib.import_module("mcp.lifecycle.taskflow")


def test_delegate_rejected_when_paused_via_sources(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 1: loop.pause_sources=["user"] blocks a new delegation."""
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    meta = _project("pg-1", status="active", loop=_loop(pause_sources=["user"]))
    _write_project(workspace, "pg-1", meta)
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-1/meta.json": meta})
    )

    result = server._taskflow(_delegate_args(workspace, "pg-1", "pg-1-next"))

    assert result["ok"] is False
    assert result["action"] == "delegate_task"
    assert result["error"] == "project is paused (sources: user)"


def test_inflight_ack_submit_succeed_while_delegate_blocked(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 2: ["user","project"] blocks delegation; the in-flight task
    still acks and submits and its result lands in the project node."""
    monkeypatch.setattr(
        _taskflow_module(),
        "_task_completion_notification",
        lambda *args, **kwargs: {"sent": True, "eventId": "$pg-completion"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    task_id = "pg-2-inflight"
    loop_tasks = [
        {
            "task_id": task_id,
            "title": "In-flight pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "assigned",
        },
    ]
    meta = _project(
        "pg-2",
        status="active",
        # Canonical Go-side order: lowercased, deduped, sorted.
        loop=_loop(pause_sources=["project", "user"], tasks=loop_tasks),
    )
    _write_project(workspace, "pg-2", meta)
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "project_id": "pg-2",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
        },
    )
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-2/meta.json": meta})
    )

    # New dispatch is blocked; both sources are reported (sorted).
    delegate = server._taskflow(_delegate_args(workspace, "pg-2", "pg-2-next"))
    assert delegate["ok"] is False
    assert delegate["error"] == "project is paused (sources: project, user)"

    # The in-flight task still acks (assigned -> in_progress).
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    ack = server._taskflow(_arguments(workspace, "ack_task", taskId=task_id))
    assert ack["ok"] is True
    assert ack["task"]["status"] == "in_progress"

    # ...and submits (in_progress -> submitted); the result lands.
    submit = server._taskflow(
        _arguments(workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done")
    )
    assert submit["ok"] is True
    assert submit["task"]["status"] == "submitted"
    meta = _read_project(workspace, "pg-2")
    assert meta["loop"]["tasks"][0]["status"] == "submitted"
    assert meta["loop"]["pause_sources"] == ["project", "user"]


def test_ready_loop_nodes_reports_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 3: paused -> readyLoopNodes=[] with paused/pauseSources exposed.
    The second task is dependency-ready, so the empty list proves the gate."""
    workspace = tmp_path / "agent"
    loop_tasks = [
        {
            "task_id": "pg-3-a",
            "title": "Done pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "completed",
        },
        {
            "task_id": "pg-3-b",
            "title": "Next pass",
            "assigned_to": WORKER,
            "depends_on": ["pg-3-a"],
            "status": "planned",
        },
    ]
    meta = _project(
        "pg-3", status="active", loop=_loop(pause_sources=["user"], tasks=loop_tasks)
    )
    _write_project(workspace, "pg-3", meta)
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-3/meta.json": meta})
    )

    result = server._projectflow(_arguments(workspace, "ready_loop_nodes", projectId="pg-3"))

    assert result["ok"] is True
    assert result["readyLoopNodes"] == []
    assert result["paused"] is True
    assert result["pauseSources"] == ["user"]


def test_plan_loop_rejected_when_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 4: plan_loop is refused while paused and writes nothing."""
    workspace = tmp_path / "agent"
    meta = _project("pg-4", status="active", loop=_loop(pause_sources=["user"]))
    _write_project(workspace, "pg-4", meta)
    meta_before = _read_project(workspace, "pg-4")
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-4/meta.json": meta})
    )

    result = server._projectflow(
        _arguments(
            workspace,
            "plan_loop",
            projectId="pg-4",
            goal="iterate",
            stopCondition="done",
            iterationTemplate="run",
            maxIterations=3,
            tasks=[
                {
                    "taskId": "pg-4-x",
                    "title": "x",
                    "assignedTo": WORKER,
                    "dependsOn": [],
                    "status": "planned",
                },
            ],
        )
    )

    assert result["ok"] is False
    assert result["error"] == "project is paused (sources: user)"
    assert _read_project(workspace, "pg-4") == meta_before


def test_record_iteration_lands_but_releases_nothing_when_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 5: the in-flight iteration is recorded, but nothing is released
    downstream and no next round opens (readyLoopNodes=[])."""
    workspace = tmp_path / "agent"
    meta = _project(
        "pg-5", status="active", loop=_loop(pause_sources=["user"], current_iteration=0)
    )
    _write_project(workspace, "pg-5", meta)
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-5/meta.json": meta})
    )

    result = server._projectflow(
        _arguments(
            workspace,
            "record_loop_iteration",
            projectId="pg-5",
            iteration=1,
            decision="continue",
            summary="iteration one",
        )
    )

    assert result["ok"] is True
    assert result["paused"] is True
    assert result["pauseSources"] == ["user"]
    assert result["readyLoopNodes"] == []
    # The in-flight iteration still landed in the project state.
    meta = _read_project(workspace, "pg-5")
    assert meta["loop"]["current_iteration"] == 1
    # The recorded entry may carry newer additive fields (e.g. revision);
    # assert the fields this contract owns instead of exact-dict equality.
    entry = meta["loop"]["history"][0]
    for key, value in {
        "iteration": 1,
        "decision": "continue",
        "summary": "iteration one",
        "next_action": "",
    }.items():
        assert entry[key] == value
    assert meta["loop"]["pause_sources"] == ["user"]


def test_legacy_paused_status_gates_like_sources(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 6: legacy status="paused" (no pause_sources) gates delegation and
    ready nodes the same way, reporting the "project" source."""
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    meta = _project("pg-6", status="paused", loop=_loop())
    _write_project(workspace, "pg-6", meta)
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-6/meta.json": meta})
    )

    delegate = server._taskflow(_delegate_args(workspace, "pg-6", "pg-6-next"))
    assert delegate["ok"] is False
    assert delegate["error"] == "project is paused (sources: project)"

    ready = server._projectflow(_arguments(workspace, "ready_loop_nodes", projectId="pg-6"))
    assert ready["ok"] is True
    assert ready["readyLoopNodes"] == []
    assert ready["paused"] is True
    assert ready["pauseSources"] == ["project"]


def test_resume_keeps_sources_and_admin_clear_unblocks(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 7: the agent's resume_project clears only the legacy status
    locally, but the authoritative merge re-pauses it against the durable
    remote state (a remote paused status is never written over with
    active) — pause_sources remain, delegation stays blocked, and only
    the Go API admin action (a durable clear of status + sources)
    unblocks it. An agent cannot self-resolve."""
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *args, **kwargs: {
            "sent": True,
            "eventId": "$pg-fresh",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        },
    )
    monkeypatch.setattr(server, "_validate_assignee_membership", lambda *args, **kwargs: {"ok": True})
    workspace = tmp_path / "agent"
    meta_path = "shared/projects/pg-7/meta.json"
    meta = _project("pg-7", status="paused", loop=_loop(pause_sources=["user"]))
    _write_project(workspace, "pg-7", meta)
    storage = FakeProjectStorage({meta_path: meta})
    _install_sync_fakes(monkeypatch, server, storage)

    # The agent resume clears the legacy status locally; the write-back's
    # authoritative merge re-pauses it against the durable remote state.
    resumed = server._projectflow(_arguments(workspace, "resume_project", projectId="pg-7"))
    assert resumed["ok"] is True
    assert resumed["synced"] is True
    meta = _read_project(workspace, "pg-7")
    assert meta["status"] == "paused"
    assert meta["loop"]["pause_sources"] == ["user"]

    # ...so the project is still paused: delegation stays blocked. The
    # legacy status contributes the "project" source alongside the
    # explicit one (spec 6 rule).
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    still = server._taskflow(_delegate_args(workspace, "pg-7", "pg-7-next"))
    assert still["ok"] is False
    assert still["error"] == "project is paused (sources: project, user)"

    # The Go API admin action clears the pause durably (status + sources)
    # -> dispatch unblocks.
    meta = _read_project(workspace, "pg-7")
    cleared = {**meta, "status": "active", "loop": {**meta["loop"], "pause_sources": []}}
    _write_project(workspace, "pg-7", cleared)
    storage.remote[meta_path] = cleared
    freed = server._taskflow(
        _arguments(
            workspace,
            "delegate_task",
            projectId="pg-7",
            taskId="pg-7-next",
            roomId=ROOM,
            assignedTo=WORKER,
            title="Next round",
            spec="do it",
        )
    )
    assert freed["ok"] is True
    assert freed["task"]["status"] == "assigned"


def test_pause_during_membership_validation_suppresses_send(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 8: a pause persisted during membership validation - after
    delegate_task's initial pause read, before the send - is already
    effective at the pre-send re-check: the assignment is suppressed,
    the task stays prepared, and nothing is committed to assigned."""
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    workspace = tmp_path / "agent"
    meta = _project("pg-8", status="active", loop=_loop())
    _write_project(workspace, "pg-8", meta)
    _install_sync_fakes(
        monkeypatch, server, FakeProjectStorage({"shared/projects/pg-8/meta.json": meta})
    )
    sent: list[dict[str, Any]] = []

    def _membership_with_pause(*args: Any, **kwargs: Any) -> dict[str, Any]:
        # Storage interleaving: the user pause lands while room membership
        # is being validated - after the initial pause read.
        meta = _read_project(workspace, "pg-8")
        meta["loop"]["pause_sources"] = ["user"]
        _write_project(workspace, "pg-8", meta)
        return {"ok": True}

    monkeypatch.setattr(server, "_validate_assignee_membership", _membership_with_pause)
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *args, **kwargs: sent.append(dict(kwargs))
        or {
            "sent": True,
            "eventId": "$pg-never-sent",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        },
    )

    result = server._taskflow(_delegate_args(workspace, "pg-8", "pg-8-next"))

    # The send was suppressed and the failure matches the pre-check shape.
    assert sent == []
    assert result["ok"] is False
    assert result["action"] == "delegate_task"
    assert "paused" in result["error"]
    # The task stays prepared - it was never committed to assigned.
    task = _read_task(workspace, "pg-8-next")
    assert task["status"] == "prepared"
    # The project task state was not marked assigned either.
    meta = _read_project(workspace, "pg-8")
    nodes = meta["tasks"] + meta["loop"]["tasks"]
    assert not any(
        isinstance(item, dict) and item.get("task_id") == "pg-8-next" for item in nodes
    )
    assert all(item.get("status") != "assigned" for item in nodes)


def test_pause_between_pull_and_submit_survives_write_back(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 9 (the maintainer's #1402 overlap): the worker pulls an active
    meta, a human pause (status=paused, loop.pause_sources=["user"]) lands
    on the remote between that pull and the submit write-back.  The
    conditional commit must merge the pause state into the write and the
    committed remote meta must keep the full pause state while the
    worker's task update lands."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    monkeypatch.setattr(
        _taskflow_module(),
        "_task_completion_notification",
        lambda *args, **kwargs: {"sent": True, "eventId": "$pg9-completion"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "pg-9-inflight"
    loop_tasks = [
        {
            "task_id": task_id,
            "title": "In-flight pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "assigned",
        },
    ]
    base_meta = _project("pg-9", status="active", loop=_loop(tasks=loop_tasks))
    storage = FakeProjectStorage(
        {"shared/projects/pg-9/meta.json": base_meta},
        pull_mutation=lambda meta: {
            **meta,
            "status": "paused",
            "loop": {
                **meta["loop"],
                "pause_sources": ["user"],
                "paused_by_project": False,
                "last_paused_at": "2026-10-31T00:00:00Z",
            },
        },
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, "pg-9", base_meta)
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "project_id": "pg-9",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
        },
    )

    submit = server._taskflow(
        _arguments(workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done")
    )

    assert submit["ok"] is True
    assert submit["task"]["status"] == "submitted"
    assert storage.commit_calls == 1
    final = storage.remote["shared/projects/pg-9/meta.json"]
    assert final["status"] == "paused"
    loop = final["loop"]
    assert loop["pause_sources"] == ["user"]
    assert loop["paused_by_project"] is False
    assert loop["last_paused_at"] == "2026-10-31T00:00:00Z"
    # The worker's update landed alongside the preserved pause state.
    assert loop["tasks"][0]["task_id"] == task_id
    assert loop["tasks"][0]["status"] == "submitted"
    # The directory mirror must never have written meta.json.
    dir_pushes = [
        call
        for call in storage.calls
        if call.get("action") == "push" and call.get("path") == "shared/projects/pg-9"
    ]
    assert dir_pushes
    assert all(call.get("exclude") == ["meta.json"] for call in dir_pushes)
    # A fresh pull reports the pause to the reader: the explicit source plus
    # the "project" source contributed by the legacy status (spec 6 rule).
    assert server.sync._pull_project(_arguments(workspace, "noop"), "pg-9") is True
    state_module = importlib.import_module("mcp.project.state")
    paused, sources = state_module._loop_pause_state(_read_project(workspace, "pg-9"))
    assert paused is True
    assert sources == ["project", "user"]


def test_sync_project_retries_conflict_and_converges(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 10: the conditional commit returns 409 once because a second
    pause source drifts in between the worker's read and its commit; the
    bounded retry re-snapshots, re-merges and converges with every source
    preserved."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-10"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop(pause_sources=["user"]))
    storage = FakeProjectStorage(
        {meta_path: base_meta},
        window_mutation=lambda meta: {
            **meta,
            "loop": {**meta["loop"], "pause_sources": ["audit", "user"]},
        },
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is True

    assert storage.commit_calls == 2
    final = storage.remote[meta_path]
    assert final["status"] == "active"
    assert final["loop"]["pause_sources"] == ["audit", "user"]


def test_sync_project_exhausted_retries_return_false(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 11: storage drifts the meta during every commit window, so the
    conditional commit conflicts on all attempts and the bounded budget
    (3 attempts) is exhausted; _sync_project returns False — an exhausted
    retry is an error, never a silent success."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-11"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    drifted = iter((["audit"], ["audit", "cron"], ["audit", "cron", "system"]))

    def _drift(meta: dict[str, Any]) -> dict[str, Any]:
        return {**meta, "loop": {**meta["loop"], "pause_sources": next(drifted)}}

    storage = FakeProjectStorage(
        {meta_path: base_meta},
        window_mutation=_drift,
        drift_always=True,
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is False
    assert storage.commit_calls == 3


def test_submit_without_pause_keeps_worker_fields(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 12: with no remote pause the commit is verified against the
    committed ETag (the empty expectation must still re-read, not
    early-return) and the worker-authored fields (assignment, loop task
    update) land in the final remote meta."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    monkeypatch.setattr(
        _taskflow_module(),
        "_task_completion_notification",
        lambda *args, **kwargs: {"sent": True, "eventId": "$pg12-completion"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "pg-12-inflight"
    loop_tasks = [
        {
            "task_id": task_id,
            "title": "In-flight pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "assigned",
        },
    ]
    base_meta = _project("pg-12", status="active", loop=_loop(tasks=loop_tasks))
    storage = FakeProjectStorage({"shared/projects/pg-12/meta.json": base_meta})
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, "pg-12", base_meta)
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "project_id": "pg-12",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
        },
    )

    submit = server._taskflow(
        _arguments(workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done")
    )

    assert submit["ok"] is True
    final = storage.remote["shared/projects/pg-12/meta.json"]
    assert final["status"] == "active"
    loop = final["loop"]
    assert "pause_sources" not in loop
    assert loop["tasks"][0]["status"] == "submitted"
    assert loop["tasks"][0]["assigned_to"] == WORKER
    assert final["title"] == "Pause gate pg-12"
    assert storage.commit_calls == 1
    # The verify step re-read the live ETag after the commit (the removed
    # early-return pin): at least the commit's If-Match read plus the
    # verify re-read, never just the commit's.
    etag_reads = [call for call in storage.calls if call.get("action") == "etag"]
    assert len(etag_reads) >= 2


def test_pause_landing_before_commit_is_kept_on_retry(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 13: the snapshot carries no pause, but the remote is paused
    between the worker's read and its commit (the 409 path) -> the bounded
    retry re-snapshots, re-merges and the committed meta keeps the
    pause that the first snapshot did not see."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-13"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    storage = FakeProjectStorage(
        {meta_path: base_meta},
        window_mutation=lambda meta: {
            **meta,
            "status": "paused",
            "loop": {**meta["loop"], "pause_sources": ["user"]},
        },
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is True

    assert storage.commit_calls == 2
    final = storage.remote[meta_path]
    assert final["status"] == "paused"
    assert final["loop"]["pause_sources"] == ["user"]


def test_pause_between_commit_and_verify_is_caught_by_etag_reread(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 14 (the removed early-return pin): the merged meta carries no
    pause state, but a pause lands on the remote between the commit and
    the verify re-read.  The empty expectation must not early-return
    over the re-read: the committed-ETag re-check sees the new version
    and the retry re-merges the pause, which the committed meta and the
    local copy keep.  (The pre-#1402 verify would have returned True
    after one commit with the local copy still active.)"""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-14"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    storage = FakeProjectStorage(
        {meta_path: base_meta},
        commit_mutation=lambda meta: {
            **meta,
            "status": "paused",
            "loop": {**meta["loop"], "pause_sources": ["user"]},
        },
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is True

    assert storage.commit_calls == 2
    final = storage.remote[meta_path]
    assert final["status"] == "paused"
    assert final["loop"]["pause_sources"] == ["user"]
    local = _read_project(workspace, project_id)
    assert local["status"] == "paused"
    assert local["loop"]["pause_sources"] == ["user"]


def test_pause_between_body_read_and_etag_is_caught_by_versioned_read(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 15 (the maintainer's #1402 P1 overlap): the remote serves the
    unpaused body first; the human pause (status=paused,
    loop.pause_sources=["user"]) lands immediately after that body read
    (new body, new ETag).  The versioned read (stat -> cat -> stat) must
    detect the version change, discard the stale read, re-read, merge
    the pause and commit the paused body under the version it read with:
    the final remote meta keeps the full pause state while the worker's
    task update lands, and a fresh pull reports paused."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    monkeypatch.setattr(
        _taskflow_module(),
        "_task_completion_notification",
        lambda *args, **kwargs: {"sent": True, "eventId": "$pg15-completion"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "pg-15-inflight"
    loop_tasks = [
        {
            "task_id": task_id,
            "title": "In-flight pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "assigned",
        },
    ]
    base_meta = _project("pg-15", status="active", loop=_loop(tasks=loop_tasks))
    storage = FakeProjectStorage(
        {"shared/projects/pg-15/meta.json": base_meta},
        cat_mutation=lambda meta: {
            **meta,
            "status": "paused",
            "loop": {**meta["loop"], "pause_sources": ["user"]},
        },
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, "pg-15", base_meta)
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "project_id": "pg-15",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
        },
    )

    submit = server._taskflow(
        _arguments(workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done")
    )

    assert submit["ok"] is True
    assert submit["task"]["status"] == "submitted"
    # One commit: the pause was merged into the FIRST conditional commit
    # (via the re-read), not lost to a stale body committed under a
    # fresh ETag — and not papered over by a 409 retry.
    assert storage.commit_calls == 1
    final = storage.remote["shared/projects/pg-15/meta.json"]
    assert final["status"] == "paused"
    loop = final["loop"]
    assert loop["pause_sources"] == ["user"]
    # The worker's update landed alongside the preserved pause state.
    assert loop["tasks"][0]["task_id"] == task_id
    assert loop["tasks"][0]["status"] == "submitted"
    # The versioned read bracketed the body with two stats, and the flip
    # forced the re-read (a single cat / a single bracket would mean the
    # pause landed after the read the commit was built from).
    etag_reads = [call for call in storage.calls if call.get("action") == "etag"]
    cat_reads = [call for call in storage.calls if call.get("action") == "cat"]
    assert len(etag_reads) >= 4
    assert len(cat_reads) >= 2
    # The directory mirror must never have written meta.json.
    dir_pushes = [
        call
        for call in storage.calls
        if call.get("action") == "push" and call.get("path") == "shared/projects/pg-15"
    ]
    assert dir_pushes
    assert all(call.get("exclude") == ["meta.json"] for call in dir_pushes)
    # A fresh pull reports the pause to the reader: the explicit source
    # plus the "project" source contributed by the legacy status (spec 6
    # rule).
    assert server.sync._pull_project(_arguments(workspace, "noop"), "pg-15") is True
    state_module = importlib.import_module("mcp.project.state")
    paused, sources = state_module._loop_pause_state(_read_project(workspace, "pg-15"))
    assert paused is True
    assert sources == ["project", "user"]


def test_versioned_read_exhausts_on_persistent_flip_no_stale_commit(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 16: the remote version flips between the bracketing stats on
    every read (a writer is in flight on every attempt) -> the first
    versioned read must discard the body and retry, the bounded read
    exhausts its attempts, and _sync_project returns False without ever
    attempting a commit — a body unbound from its version is never
    committed (no stale commit)."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-16"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    flips = 0

    def _flip(meta: dict[str, Any]) -> dict[str, Any]:
        nonlocal flips
        flips += 1
        return {**meta, "loop": {**meta["loop"], "flip": flips}}

    storage = FakeProjectStorage(
        {meta_path: base_meta},
        stat_mutation=_flip,
        drift_always=True,
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is False
    # The version really flipped on every stat (the test is not vacuous):
    # at least one full read was discarded for it.
    assert flips >= 2
    assert storage.remote[meta_path] != base_meta
    # ...yet no commit was ever attempted, and no discarded body reached
    # the local state.
    assert storage.commit_calls == 0
    assert _read_project(workspace, project_id) == base_meta


@pytest.mark.parametrize("status", [404, 405, 428, 503])
def test_sync_project_fails_closed_on_endpoint_rejection(
    server: Any, tmp_path: Path, monkeypatch: Any, status: int
) -> None:
    """Spec 17: the controller rejects the conditional commit (404/405 =
    the controller predates the endpoint, 428 = If-Match rejected, 503 =
    the backend cannot strict-CAS) -> _sync_project fails closed; the
    remote store is untouched and the meta is never written through an
    unconditional filesync push (the tripwire would fire on a
    regression)."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = f"pg-15-{status}"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    storage = FakeProjectStorage(
        {meta_path: base_meta},
        fail_closed={"ok": False, "error": f"simulated controller rejection ({status})"},
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is False
    assert storage.commit_calls == 1
    # No commit landed and no unconditional push replaced it.
    assert storage.remote[meta_path] == base_meta


def test_sync_project_first_push_when_remote_meta_absent(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 18: the remote meta object is absent (no ETag to match) ->
    the normal first push: there is nothing to match against, so the
    directory push INCLUDING meta.json (no conditional commit, no
    exclusion) is the commit, and _sync_project returns its result."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-16"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    storage = FakeProjectStorage()  # the remote object does not exist
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is True
    # The first push is the commit: no conditional commit was attempted.
    assert storage.commit_calls == 0
    # ...and exactly one directory push went out, WITHOUT the
    # conditional path's exclusion — meta.json is part of the push.
    dir_pushes = [
        call
        for call in storage.calls
        if call.get("action") == "push" and call.get("path") == f"shared/projects/{project_id}"
    ]
    assert len(dir_pushes) == 1
    assert "exclude" not in dir_pushes[0]
    # The meta landed in the store under the first push.
    assert storage.remote[meta_path] == base_meta


def test_sync_project_fails_closed_on_etag_read_failure(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 19: the ETag probe fails with a storage error (not an absent
    object) -> still fail closed: an unmatchable commit must not degrade
    to an unconditional push, and the remote version is unknown, so the
    conditional path's directory push (the one that omits meta.json)
    must not go out either — the project is never half-written."""
    monkeypatch.setattr(server.sync, "_SYNC_PROJECT_RETRY_BACKOFFS", (0.0, 0.0))
    workspace = tmp_path / "agent"
    project_id = "pg-17"
    meta_path = f"shared/projects/{project_id}/meta.json"
    base_meta = _project(project_id, status="active", loop=_loop())
    storage = FakeProjectStorage(
        {meta_path: base_meta},
        etag_read_error="remote stat probe failed: connection refused",
    )
    _install_sync_fakes(monkeypatch, server, storage)
    _write_project(workspace, project_id, base_meta)

    assert server.sync._sync_project(_arguments(workspace, "noop"), project_id) is False
    assert storage.commit_calls == 0
    assert storage.remote[meta_path] == base_meta
    # No directory push at all — neither the conditional path's push
    # (excluding meta.json) nor a first-push-style push including it:
    # the remote state could not even be probed.
    dir_pushes = [
        call
        for call in storage.calls
        if call.get("action") == "push" and call.get("path") == f"shared/projects/{project_id}"
    ]
    assert dir_pushes == []
