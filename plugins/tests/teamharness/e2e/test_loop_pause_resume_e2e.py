"""Runtime e2e: the #1431 pause/resume closed loop through the real paths,
plus the #1402 follow-up slices (project writes must preserve the loop
pause veto: loop-only pause; project resume with the veto still held).

Drives the real Controller HTTP API (project create / replan / pause /
resume / workflow / task inspection) and the real TeamHarness taskflow in
separate ``loop_driver.py`` processes (only the two Matrix notifiers are
stubbed there).  Covers the maintainer's follow-up points on top of the
original closed loop:

1. pause blocks new dispatch — a fresh process delegating a ready task
   fails with "paused", records no notification, and the workflow shows
   the task unassigned with ``next == []`` while paused (no downstream /
   next-round release); after the resume, ``next`` lists the ready
   planned/assigned tasks again, so the empty list is pause-only;
2. in-flight work still saves its result after the pause — an assigned
   task's real submission lands in the object store and is read back
   through the Controller's task-inspection endpoint;
3. authorized resume + a fresh process continues exactly once — the
   ready task is delegated once (one send record), a second fresh
   process reuses the recorded event instead of re-sending, and the
   already-submitted task is not re-dispatched;
4. the Loop CR is created through the embedded kube-apiserver (the
   control-plane object the Loop endpoints resolve) — never through a
   controller create-loop path, which does not exist;
5. the durable state is the object store: ``loop.pause_sources`` is
   verified by reading the project meta with ``mc`` (the API response
   alone is not evidence);
6. the human-source boundary holds: a "project"-source resume cannot
   release the "user" veto (the loop stays paused on the remaining
   source), and only an admin "user"-source resume releases it;
7. the role boundary is executable: a worker token is rejected by the
   authorizer middleware before the handler runs, and a manager token is
   rejected by the handler when releasing the user source;
8. the later boundary of the conditional meta commit (#1402 B): a user
   pause that lands BETWEEN the commit's ETag read and its PUT (the
   driver fires the real loop pause right before the first conditional
   commit) forces a real 409 on the commit; the retry's re-snapshot +
   re-merge preserves the durable pause_sources, the in-flight
   submission still succeeds, and a fresh process stays blocked by the
   real worker gate until an admin user-source release.

Ordering note: the project endpoints used to round-trip the meta through a
typed struct whose ``loopMeta`` lacked ``pause_sources`` — a controller
clobber that made the pause/resume ORDER load-bearing (reversed, a project
write silently dropped the durable sources and turned the admin-only user
veto into a bypass). That clobber is fixed (#1402: the loop block now
round-trips byte-for-byte through the project writes, and the workflow
releases nothing while any pause source is held), so both orderings are
safe and are BOTH exercised: the closed loop is parametrised over
"classic" (project pause first, sources released before the project
resume) and "reversed" (loop pause first — the veto alone must hold
dispatch on an ACTIVE project — then the project is resumed by a Manager
while the user veto is still held), and the two independent tests cover
the order-robust slices (loop-only pause; project resume with the veto
present).

Skipped entirely unless ``E2E_CONTROLLER_URL`` is set AND
``E2E_MINIO=1`` AND the embedded-apiserver env (``E2E_K8S_API_URL`` /
``E2E_K8S_CA`` / ``E2E_K8S_NAMESPACE`` / ``E2E_K8S_TOKEN``) is present.
The CI ``loop-pause-e2e`` job provides all of it, plus the
``AGENTTEAMS_FS_*`` env the driver's real ``mc`` path needs (see the
README in this directory). The skip reason names exactly which vars are
missing, so a misconfigured run cannot silently drop the loop-endpoint
coverage. The worker/manager role-boundary tokens are asserted INSIDE the
test (not skipped): a run that cannot prove the role boundary is a broken
run.
"""

from __future__ import annotations

import json
import os
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

import pytest

E2E_DIR = Path(__file__).resolve().parent
DRIVER = E2E_DIR / "loop_driver.py"
BASE_URL = os.environ.get("E2E_CONTROLLER_URL", "").strip().rstrip("/")
TOKEN = os.environ.get("E2E_CONTROLLER_TOKEN", "").strip()

# Embedded kube-apiserver (a controller subprocess): the Loop CR is
# created through it, and the role-boundary tokens are its static token
# identities (see the loop-pause-e2e job's restart step).
K8S_API_URL = os.environ.get("E2E_K8S_API_URL", "").strip().rstrip("/")
K8S_CA = os.environ.get("E2E_K8S_CA", "").strip()
K8S_NAMESPACE = os.environ.get("E2E_K8S_NAMESPACE", "").strip()
K8S_TOKEN = os.environ.get("E2E_K8S_TOKEN", "").strip()
WORKER_TOKEN = os.environ.get("E2E_K8S_WORKER_TOKEN", "").strip()
MANAGER_TOKEN = os.environ.get("E2E_K8S_MANAGER_TOKEN", "").strip()

# The k8s helper may only be skipped when these are absent (and then the
# skip says exactly why). The worker/manager tokens are NOT here: the role
# boundary must be exercised, not skipped.
_K8S_SKIP_ENV = {
    "E2E_K8S_API_URL": K8S_API_URL,
    "E2E_K8S_CA": K8S_CA,
    "E2E_K8S_NAMESPACE": K8S_NAMESPACE,
    "E2E_K8S_TOKEN": K8S_TOKEN,
}

_skip_missing: list[str] = []
if not BASE_URL:
    _skip_missing.append("E2E_CONTROLLER_URL")
if os.environ.get("E2E_MINIO", "").strip() != "1":
    _skip_missing.append("E2E_MINIO=1")
_skip_missing += [name for name, value in _K8S_SKIP_ENV.items() if not value]

pytestmark = pytest.mark.skipif(
    bool(_skip_missing),
    reason="e2e needs the loop-pause-e2e job's env; missing: " + ", ".join(_skip_missing),
)


# --- Controller API (stdlib urllib only) ---------------------------------


def _api(
    method: str,
    path: str,
    body: dict[str, Any] | None = None,
    token: str | None = None,
) -> tuple[int, dict[str, Any]]:
    """Controller HTTP call. ``token`` overrides the module admin token —
    the role-boundary checks call with the worker/manager identities."""
    data = json.dumps(body).encode("utf-8") if body is not None else None
    request = urllib.request.Request(BASE_URL + path, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    bearer = TOKEN if token is None else token
    if bearer:
        request.add_header("Authorization", f"Bearer {bearer}")
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            status = response.status
            raw = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        raw = exc.read()
    except Exception as exc:  # URLError / timeout -> the controller is unreachable
        raise AssertionError(f"controller {method} {path} unreachable: {exc}") from exc
    try:
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except json.JSONDecodeError:
        payload = {"raw": raw.decode("utf-8", errors="replace")}
    return status, payload


# --- embedded kube-apiserver REST (Loop CR) --------------------------------


def _k8s_api(method: str, path: str, body: dict[str, Any], token: str) -> tuple[int, dict[str, Any]]:
    """Raw REST against the embedded kube-apiserver (TLS via the job CA).

    Returns (status, payload) for EVERY HTTP answer — including 401/403 —
    so the caller fails loudly with the exact command evidence. This path
    never skips: the loop-endpoint coverage is only real when the CR was
    created through the apiserver.
    """
    url = K8S_API_URL + path
    data = json.dumps(body).encode("utf-8")
    request = urllib.request.Request(url, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {token}")
    context = ssl.create_default_context(cafile=K8S_CA)
    try:
        with urllib.request.urlopen(request, timeout=30, context=context) as response:
            status, raw = response.status, response.read()
    except urllib.error.HTTPError as exc:
        status, raw = exc.code, exc.read()
    except Exception as exc:  # TLS failure, connection refused, timeout
        raise AssertionError(
            f"k8s apiserver {method} {url} unreachable (token {token[:12]}...): {exc}"
        ) from exc
    try:
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except json.JSONDecodeError:
        payload = {"raw": raw.decode("utf-8", errors="replace")}
    return status, payload


def _create_loop_cr(name: str, task_id: str) -> None:
    """Create the Loop CR through the embedded apiserver.

    spec.taskID is the anchor task: resolveLoopProject follows
    task meta -> project meta, and only a task with a published TaskMeta
    resolves. The anchor must be the task delegated before the first
    pause (B below); an anchor without a TaskMeta would make every
    pause/resume a legitimate no-op ("no task meta found"), which is
    exactly the gap these endpoints must close. teamName is omitted: the
    project is standalone (no team), and a non-empty teamName would make
    the resolution scan only the team-scoped prefix.
    """
    path = f"/apis/agentteams.io/v1beta1/namespaces/{K8S_NAMESPACE}/loops"
    body = {
        "apiVersion": "agentteams.io/v1beta1",
        "kind": "Loop",
        "metadata": {"name": name, "namespace": K8S_NAMESPACE},
        "spec": {"taskID": task_id, "loopType": "event"},
    }
    last = ""
    for _ in range(30):
        status, payload = _k8s_api("POST", path, body, K8S_TOKEN)
        if status == 201:
            return
        text = json.dumps(payload, ensure_ascii=False)
        # The embedded apiserver registers the loops CRD asynchronously at
        # startup (it only awaits workers/teams/humans/managers); until it
        # is established the POST 404s with "no matches for kind". Retry
        # that; fail loudly on anything else — a 401 here is exactly the
        # kind of blocker that must be reported, never skipped.
        if status == 404 and "no matches for kind" in text:
            last = f"CRD not established yet: {text[:300]}"
            time.sleep(1)
            continue
        raise AssertionError(
            "Loop CR create failed — a blocker, not a skip condition. "
            f"equivalent command: curl -sS -X POST --cacert {K8S_CA} "
            f"-H 'Authorization: Bearer <{K8S_TOKEN[:12]}...>' -d "
            f"'{json.dumps(body)}' {K8S_API_URL}{path} -> HTTP {status}: {text}"
        )
    raise AssertionError(f"Loop CRD never became established after 30s: {last}")


# --- durable state (object store via mc) -----------------------------------


def _mc_meta_object(project_id: str) -> dict[str, Any]:
    """Read the project meta straight from the object store with mc.

    The API response alone is not evidence that the pause state is
    durable: this is the authoritative read of ``loop.pause_sources``.
    """
    values = {
        "AGENTTEAMS_FS_ENDPOINT": os.environ.get("AGENTTEAMS_FS_ENDPOINT", "").strip(),
        "AGENTTEAMS_FS_ACCESS_KEY": os.environ.get("AGENTTEAMS_FS_ACCESS_KEY", "").strip(),
        "AGENTTEAMS_FS_SECRET_KEY": os.environ.get("AGENTTEAMS_FS_SECRET_KEY", "").strip(),
        "AGENTTEAMS_STORAGE_PREFIX": os.environ.get("AGENTTEAMS_STORAGE_PREFIX", "").strip(),
    }
    missing = [name for name, value in values.items() if not value]
    assert not missing, (
        f"durable-state check needs the object-store env; missing: {missing} "
        "(the loop-pause-e2e job exports it)"
    )
    bucket = os.environ.get("AGENTTEAMS_FS_BUCKET", "agentteams-storage").strip()
    # The object key layout depends on how the controller's storage client was
    # configured: with a storage prefix the key is
    # ``<prefix>/shared/projects/<id>/meta.json``, and without one (the
    # embedded loop-pause-e2e layout, measured on this head) the object lands
    # at ``shared/projects/<id>/meta.json``. Read whichever exists rather than
    # assuming one layout.
    alias = (
        f"mc -q alias set e2edurable {values['AGENTTEAMS_FS_ENDPOINT']} "
        f"{values['AGENTTEAMS_FS_ACCESS_KEY']} {values['AGENTTEAMS_FS_SECRET_KEY']} "
        f">/dev/null 2>&1"
    )
    prefix = values["AGENTTEAMS_STORAGE_PREFIX"].strip("/")
    candidates = []
    if prefix:
        candidates.append(f"{prefix}/shared/projects/{project_id}/meta.json")
    candidates.append(f"shared/projects/{project_id}/meta.json")
    last = None
    for key in candidates:
        command = f"{alias} && mc -q cat e2edurable/{bucket}/{key}"
        proc = subprocess.run(command, shell=True, capture_output=True, text=True, timeout=60)
        if proc.returncode == 0:
            return json.loads(proc.stdout)
        last = (command, proc.stderr)
    tried = ", ".join(f"{bucket}/{key}" for key in candidates)
    assert False, (
        f"mc read of the project meta failed for every candidate key ({tried}); "
        f"last error:\n{last[1] if last else ''}"
    )


# --- driver subprocess + sent-record helpers -----------------------------


def _run_driver(
    env: dict[str, str],
    sent_path: Path,
    action: str,
    project_id: str,
    task_id: str,
    worker: str,
    workspace: Path,
    result: dict[str, Any] | None = None,
    payload: dict[str, Any] | None = None,
    pause_loop_before_commit: str | None = None,
) -> dict[str, Any]:
    env = dict(env)
    env["E2E_WORKSPACE"] = str(workspace)
    command = [
        sys.executable,
        str(DRIVER),
        "--action",
        action,
        "--project",
        project_id,
        "--task",
        task_id,
        "--worker",
        worker,
    ]
    if result is not None:
        command += ["--result", json.dumps(result)]
    if payload is not None:
        command += ["--payload", json.dumps(payload)]
    if pause_loop_before_commit:
        command += ["--pause-loop-before-commit", pause_loop_before_commit]
    try:
        proc = subprocess.run(command, capture_output=True, text=True, timeout=300, env=env)
    except subprocess.TimeoutExpired as exc:
        raise AssertionError(
            f"driver {action} (task {task_id}) timed out\nsent: {_sent_tail(sent_path)}"
        ) from exc
    if proc.returncode != 0:
        raise AssertionError(
            f"driver {action} (task {task_id}) exited {proc.returncode}\n"
            f"stdout: {proc.stdout}\nstderr: {proc.stderr}\n"
            f"last sent records: {_sent_tail(sent_path)}"
        )
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError:
        raise AssertionError(
            f"driver {action} (task {task_id}) printed non-JSON stdout\n"
            f"stdout: {proc.stdout}\nstderr: {proc.stderr}\n"
            f"last sent records: {_sent_tail(sent_path)}"
        ) from None


def _sent_records(sent_path: Path) -> list[dict[str, Any]]:
    if not sent_path.exists():
        return []
    return [json.loads(line) for line in sent_path.read_text(encoding="utf-8").splitlines() if line.strip()]


def _sent_tail(sent_path: Path, limit: int = 5) -> str:
    return json.dumps(_sent_records(sent_path)[-limit:], ensure_ascii=False)


def _sent_counts(sent_path: Path, kind: str) -> dict[str, int]:
    counts: dict[str, int] = {}
    for record in _sent_records(sent_path):
        if record.get("kind") == kind:
            task_id = str(record.get("task_id") or "")
            counts[task_id] = counts.get(task_id, 0) + 1
    return counts


def _assert_sent_counts(sent_path: Path, kind: str, expected: dict[str, int]) -> None:
    counts = _sent_counts(sent_path, kind)
    for task_id, count in expected.items():
        assert counts.get(task_id, 0) == count, (
            f"{kind} send counts {counts} do not match expected {expected} "
            f"(task {task_id} sent {counts.get(task_id, 0)} times, want {count})\n"
            f"records: {json.dumps(_sent_records(sent_path), ensure_ascii=False)}"
        )


# --- workflow helpers ------------------------------------------------------


def _node_status(workflow: dict[str, Any]) -> dict[str, str]:
    return {
        node.get("id"): str(node.get("status") or "")
        for node in workflow.get("nodes", [])
        if isinstance(node, dict)
    }


def _assert_nodes(workflow: dict[str, Any], expected: dict[str, str]) -> None:
    nodes = _node_status(workflow)
    for task_id, status in expected.items():
        assert nodes.get(task_id) == status, (
            f"node {task_id} status {nodes.get(task_id)!r} != expected {status!r} "
            f"(nodes: {nodes}, workflow: {json.dumps(workflow, ensure_ascii=False)[:2000]})"
        )


def _find_interrupt(workflow: dict[str, Any], interrupt_id: str, value: str) -> dict[str, Any] | None:
    for interrupt in workflow.get("interrupts", []):
        if isinstance(interrupt, dict) and interrupt.get("id") == interrupt_id and interrupt.get("value") == value:
            return interrupt
    return None


# --- the scenario ----------------------------------------------------------


@pytest.fixture
def e2e_env(tmp_path: Path) -> tuple[dict[str, str], Path]:
    """Driver env: inherited object-store env + sent-log + dummy Matrix env
    (the stubs intercept before any HTTP call; the env only arms the send
    path inside the real taskflow code)."""
    sent_path = tmp_path / "sent.jsonl"
    env = dict(os.environ)
    env["E2E_SENT_JSONL"] = str(sent_path)
    # 127.0.0.1:9 (discard port, nothing listens): any code path that is NOT
    # intercepted by the stubs fails deterministically and instantly with
    # ECONNREFUSED instead of depending on DNS NXDOMAIN for
    # "matrix.e2e.invalid" (submit's artifact publish attempts a Matrix
    # upload before the send path, and a slow DNS failure would hang it).
    env.setdefault("AGENTTEAMS_MATRIX_URL", "http://127.0.0.1:9")
    env.setdefault("AGENTTEAMS_WORKER_MATRIX_TOKEN", "e2e-dummy-token")
    return env, sent_path


# --- shared setup: project + loop plan + anchor CR + pre-pause delegate ---


def _reset_task_objects(task_ids: list[str]) -> None:
    """Clear this scenario's task objects before it sets up.

    ``shared/tasks/<id>`` is a GLOBAL object key in the taskflow contract,
    not project-scoped. Two scenarios that reuse the ids A/B would see the
    previous assignment, the driver would "reuse" it instead of sending, and
    the cases would silently depend on execution order. Each scenario owns
    its ids and clears them up front; a missing object is a no-op.
    """
    endpoint = os.environ.get("AGENTTEAMS_FS_ENDPOINT", "").strip()
    access = os.environ.get("AGENTTEAMS_FS_ACCESS_KEY", "").strip()
    secret = os.environ.get("AGENTTEAMS_FS_SECRET_KEY", "").strip()
    missing = [
        name for name, value in (
            ("AGENTTEAMS_FS_ENDPOINT", endpoint),
            ("AGENTTEAMS_FS_ACCESS_KEY", access),
            ("AGENTTEAMS_FS_SECRET_KEY", secret),
        ) if not value
    ]
    assert not missing, (
        f"task reset needs the object-store env; missing: {missing} "
        "(the loop-pause-e2e job exports it)"
    )
    bucket = os.environ.get("AGENTTEAMS_FS_BUCKET", "agentteams-storage").strip()
    prefix = os.environ.get("AGENTTEAMS_STORAGE_PREFIX", "").strip("/")
    keys: list[str] = []
    for task_id in task_ids:
        relative = f"shared/tasks/{task_id}"
        if prefix:
            keys.append(f"{prefix}/{relative}")
        keys.append(relative)
    seen: set[str] = set()
    unique = [key for key in keys if not (key in seen or seen.add(key))]
    alias = (
        f"mc -q alias set e2ereset {endpoint} {access} {secret} >/dev/null 2>&1"
    )
    targets = " ".join(f"e2ereset/{bucket}/{key}" for key in unique)
    subprocess.run(
        f"{alias} && mc -q rm --recursive --force {targets}",
        shell=True, capture_output=True, text=True, timeout=120, check=False,
    )


def _setup_veto_project(
    env: dict[str, str],
    sent_path: Path,
    workdir: Path,
) -> tuple[str, str]:
    """Project + loop plan + Loop CR + pre-pause delegate, shared by all scenarios.

    Returns (project_id, loop_name). The graph is A (ready) -> B (waits on
    A). B is delegated FIRST, before any pause: the Loop CR anchors on a
    task whose TaskMeta must be published before the first pause, or every
    pause/resume would be a legitimate "no task meta found" no-op —
    exactly the gap these endpoints must close.
    """
    project_id = f"e2e-pause-{uuid.uuid4().hex[:8]}"
    loop_name = f"e2e-loop-{uuid.uuid4().hex[:8]}"
    worker = "@e2e-worker:matrix.e2e.invalid"
    # Scenarios share the fixed ids A/B; the objects behind them are global.
    _reset_task_objects(["A", "B"])

    # 1. project + task graph through the real Controller API
    status, project = _api("POST", "/api/v1/projects", {
        "project_id": project_id,
        "title": "loop e2e pause/resume",
        "source": "e2e",
        "requester": "e2e",
    })
    assert status == 201, f"create project -> {status}: {project}"
    status, replan = _api("POST", f"/api/v1/projects/{project_id}/replan", {
        "tasks": [
            {"taskId": "A", "title": "task A"},
            {"taskId": "B", "title": "task B", "dependsOn": ["A"]},
        ],
    })
    assert status == 200, f"replan -> {status}: {replan}"
    _assert_nodes(replan, {"A": "pending", "B": "pending"})
    assert replan.get("status") == "active", f"replan workflow status: {replan.get('status')}"
    assert replan.get("next") == ["A"], (
        f"expected next=['A'] (A ready, B blocked on A) before the pause, "
        f"got {replan.get('next')}: {json.dumps(replan, ensure_ascii=False)[:2000]}"
    )

    # 2. loop plan through the real projectflow (fresh driver process) —
    # BEFORE the delegate: _update_project_task only lands task statuses
    # into loop.tasks once the loop record exists, and buildWorkflow
    # renders loop.tasks (not project.tasks) for loop plans.
    result = _run_driver(
        env,
        sent_path,
        "plan_loop",
        project_id,
        "",
        "leader",
        workdir / "ws0",
        payload={
            "projectId": project_id,
            "goal": "e2e loop goal",
            "stopCondition": "e2e stop condition",
            "iterationTemplate": "e2e iteration {n}",
            "maxIterations": 2,
            "tasks": [
                {"taskId": "A", "title": "task A"},
                {"taskId": "B", "title": "task B", "dependsOn": ["A"]},
            ],
        },
    )
    assert result.get("ok") is True, f"plan_loop: {result}\nsent: {_sent_tail(sent_path)}"
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow after plan_loop -> {status}: {wf}"
    assert wf.get("plan_type") == "loop", (
        f"plan_type after plan_loop: {wf.get('plan_type')}: "
        f"{json.dumps(wf, ensure_ascii=False)[:2000]}"
    )
    _assert_nodes(wf, {"A": "pending", "B": "pending"})
    assert wf.get("next") == ["A"], (
        f"loop plan must release A before any dispatch, next={wf.get('next')}: "
        f"{json.dumps(wf, ensure_ascii=False)[:2000]}"
    )

    # 3. Loop CR through the embedded kube-apiserver (real control plane).
    # Anchor on B (the task delegated right after this): only a task with a
    # published TaskMeta resolves, and that is exactly the task whose pause
    # must actually gate the project.
    _create_loop_cr(loop_name, "B")

    # 4. delegate B BEFORE any pause (fresh driver process)
    result = _run_driver(env, sent_path, "delegate", project_id, "B", worker, workdir / "ws1")
    assert result.get("ok") is True, f"pre-pause delegate B: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("sent") is True, f"pre-pause delegate B: {result}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})
    return project_id, loop_name


def test_loop_only_pause_blocks_dispatch(e2e_env: tuple[dict[str, str], Path], tmp_path: Path) -> None:
    """#1402 slice (a): loop-only pause — the project stays ACTIVE, only
    ``loop.pause_sources=["user"]`` is held. The ready task must not be
    released (workflow ``next == []`` on an active project), a fresh
    process is blocked with "paused" and records no send, and dispatch
    proceeds exactly once only after the admin user-source release."""
    env, sent_path = e2e_env
    worker = "@e2e-worker:matrix.e2e.invalid"
    project_id, loop_name = _setup_veto_project(env, sent_path, tmp_path)

    # Loop pause only — the project is never paused.
    status, loop_paused = _api("POST", f"/api/v1/loops/{loop_name}/pause", {"source": "user"})
    assert status == 200, f"loop pause -> {status}: {loop_paused}"
    assert loop_paused.get("id") == loop_name, f"loop pause id: {loop_paused}"
    assert loop_paused.get("paused") is True, f"loop pause response: {loop_paused}"
    assert loop_paused.get("sources") == ["user"], f"loop pause sources: {loop_paused}"

    # Durable: the project is ACTIVE and the veto is stored.
    meta = _mc_meta_object(project_id)
    assert meta.get("status") == "active", (
        f"loop-only pause must not pause the project: {meta.get('status')}"
    )
    loop_record = meta.get("loop")
    assert isinstance(loop_record, dict) and loop_record.get("pause_sources") == ["user"], (
        f"durable loop.pause_sources must be ['user']: {json.dumps(loop_record, ensure_ascii=False)[:800]}"
    )

    # The ready task A is NOT released while the veto is held — even
    # though the project is active (the #1402 workflow gate).
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "active", f"workflow status: {wf.get('status')}"
    _assert_nodes(wf, {"A": "pending", "B": "delegated"})
    assert wf.get("next") == [], (
        f"active project with the loop veto held must release nothing: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )

    # A fresh process is blocked by the worker-side gate.
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws2")
    assert result.get("ok") is False, f"delegate A while the loop is paused must fail: {result}"
    assert "paused" in str(result.get("error")), f"delegate A error: {result.get('error')}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})  # no new delegate send

    # The admin user-source release unblocks dispatch; exactly one send.
    status, released = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"})
    assert status == 200, f"user-source resume -> {status}: {released}"
    assert released.get("paused") is False, f"loop must be unpaused after the release: {released}"
    assert released.get("sources") == [], f"sources after the release: {released}"
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws3")
    assert result.get("ok") is True, f"post-release delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "active", f"workflow status: {wf.get('status')}"
    assert wf.get("next") == ["A"], f"post-release next: {wf.get('next')}"


def test_project_resume_keeps_the_veto(e2e_env: tuple[dict[str, str], Path], tmp_path: Path) -> None:
    """#1402 slice (b): project resume WITH the veto present. A Manager
    resumes the project while ``loop.pause_sources`` holds "user": 200 +
    active, but the veto must survive the write (dropping it is exactly
    the #1402 bypass), the driver stays blocked, and dispatch proceeds
    exactly once only after the ADMIN user-source release."""
    env, sent_path = e2e_env
    worker = "@e2e-worker:matrix.e2e.invalid"
    assert MANAGER_TOKEN, (
        "E2E_K8S_MANAGER_TOKEN is required: append a "
        "system:serviceaccount:<ns>:agentteams-manager identity to the "
        "apiserver token file and restart the controller (see README)"
    )
    project_id, loop_name = _setup_veto_project(env, sent_path, tmp_path)

    status, loop_paused = _api("POST", f"/api/v1/loops/{loop_name}/pause", {"source": "user"})
    assert status == 200, f"loop pause -> {status}: {loop_paused}"
    assert loop_paused.get("sources") == ["user"], f"loop pause sources: {loop_paused}"
    status, paused_wf = _api("POST", f"/api/v1/projects/{project_id}/pause", {"reason": "e2e pause"})
    assert status == 200, f"project pause -> {status}: {paused_wf}"
    assert paused_wf.get("status") == "paused", f"pause response status: {paused_wf.get('status')}"

    # THE bypass scenario: a Manager resumes the project while the user
    # veto is still held.
    status, resumed_wf = _api("POST", f"/api/v1/projects/{project_id}/resume", {}, token=MANAGER_TOKEN)
    assert status == 200, f"manager project resume -> {status}: {resumed_wf}"
    assert resumed_wf.get("status") == "active", f"manager resume status: {resumed_wf.get('status')}"

    # Durable: active AND the veto intact through the manager's write.
    meta = _mc_meta_object(project_id)
    assert meta.get("status") == "active", (
        f"durable status after the manager resume: {meta.get('status')}"
    )
    loop_record = meta.get("loop")
    assert isinstance(loop_record, dict) and loop_record.get("pause_sources") == ["user"], (
        f"the user veto must survive the project resume write "
        f"(got {json.dumps(loop_record.get('pause_sources') if isinstance(loop_record, dict) else loop_record)}); "
        f"dropping it is exactly the #1402 bypass"
    )

    # Still blocked: the workflow releases nothing on the active project,
    # and a fresh process is refused by the worker-side gate.
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "active", f"workflow status: {wf.get('status')}"
    _assert_nodes(wf, {"A": "pending", "B": "delegated"})
    assert wf.get("next") == [], (
        f"the veto must hold the ready task on an active project: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws2")
    assert result.get("ok") is False, f"delegate A under the surviving veto must fail: {result}"
    assert "paused" in str(result.get("error")), f"delegate A error: {result.get('error')}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})  # no new delegate send

    # The user-source release is admin-only: manager 403, admin 200.
    status, denied = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"}, token=MANAGER_TOKEN)
    assert status == 403, f"manager user-source resume must be 403, got {status}: {denied}"
    assert "resuming the user pause source requires admin" in str(denied.get("message") or denied), (
        f"manager 403 must come from the handler: {denied}"
    )
    status, released = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"})
    assert status == 200, f"user-source resume -> {status}: {released}"
    assert released.get("paused") is False and released.get("sources") == [], f"release: {released}"

    # Dispatch proceeds exactly once (fresh process); a second fresh
    # process reuses the recorded event instead of re-sending.
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws3")
    assert result.get("ok") is True, f"post-release delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is not True, (
        f"post-release delegate A must be a fresh send: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws4")
    assert result.get("ok") is True, f"retry delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is True, (
        f"retry of an assigned task must reuse the recorded event: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})  # A still exactly once
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("next") == ["A"], f"post-release next: {wf.get('next')}"


@pytest.mark.parametrize("order", ["classic", "reversed"])
def test_pause_resume_closed_loop(
    e2e_env: tuple[dict[str, str], Path], tmp_path: Path, order: str
) -> None:
    env, sent_path = e2e_env
    worker = "@e2e-worker:matrix.e2e.invalid"
    # The role boundary is executable, not skippable: without the two
    # identities the boundary below is untested, which is a broken run.
    # (The loop-pause-e2e job appends them to the apiserver token file and
    # restarts the controller; a local run must do the same — see README.)
    assert WORKER_TOKEN, (
        "E2E_K8S_WORKER_TOKEN is required: append a "
        "system:serviceaccount:<ns>:agentteams-worker-e2e identity to the "
        "apiserver token file and restart the controller (see README)"
    )
    assert MANAGER_TOKEN, (
        "E2E_K8S_MANAGER_TOKEN is required: append a "
        "system:serviceaccount:<ns>:agentteams-manager identity to the "
        "apiserver token file and restart the controller (see README)"
    )

    # --- 1-4. shared setup: project + loop plan + anchor CR + delegate B ---
    project_id, loop_name = _setup_veto_project(env, sent_path, tmp_path)

    # --- 5-6. the pause phase, order-dependent -----------------------------
    # "classic": project pause first, loop pause second (the original
    # order). "reversed": loop pause first — the veto ALONE must hold the
    # dispatch on the still-active project (the #1402 workflow gate) — then
    # the project pause. Pre-#1402 the reversed order was broken (the typed
    # project write dropped pause_sources); both orders must pass now.
    if order == "classic":
        status, paused_wf = _api("POST", f"/api/v1/projects/{project_id}/pause", {"reason": "e2e pause"})
        assert status == 200, f"pause -> {status}: {paused_wf}"
        assert paused_wf.get("status") == "paused", f"pause response status: {paused_wf.get('status')}"
        assert paused_wf.get("next") == [], (
            f"paused workflow must release nothing, next={paused_wf.get('next')}: "
            f"{json.dumps(paused_wf, ensure_ascii=False)[:2000]}"
        )
        interrupt = _find_interrupt(paused_wf, "project", "paused")
        assert interrupt is not None, f"paused workflow missing project interrupt: {paused_wf.get('interrupts')}"
        assert interrupt.get("action_request", {}).get("action") == "resume", (
            f"pause interrupt action_request: {interrupt.get('action_request')}"
        )
        status, loop_paused = _api("POST", f"/api/v1/loops/{loop_name}/pause", {"source": "user"})
        assert status == 200, f"loop pause -> {status}: {loop_paused}"
        assert loop_paused.get("id") == loop_name, f"loop pause id: {loop_paused}"
        assert loop_paused.get("paused") is True, f"loop pause response: {loop_paused}"
        assert loop_paused.get("sources") == ["user"], f"loop pause sources: {loop_paused}"
    else:
        status, loop_paused = _api("POST", f"/api/v1/loops/{loop_name}/pause", {"source": "user"})
        assert status == 200, f"loop pause -> {status}: {loop_paused}"
        assert loop_paused.get("id") == loop_name, f"loop pause id: {loop_paused}"
        assert loop_paused.get("paused") is True, f"loop pause response: {loop_paused}"
        assert loop_paused.get("sources") == ["user"], f"loop pause sources: {loop_paused}"
        # The veto ALONE must hold the ready task on the still-ACTIVE
        # project (buildWorkflow must not release next while
        # loop.pause_sources is non-empty).
        status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
        assert status == 200, f"workflow -> {status}: {wf}"
        assert wf.get("status") == "active", (
            f"reversed order: the project is not paused yet: {wf.get('status')}"
        )
        _assert_nodes(wf, {"A": "pending", "B": "delegated"})
        assert wf.get("next") == [], (
            f"the loop veto alone must hold the ready task on an active project: "
            f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
        )
        status, paused_wf = _api("POST", f"/api/v1/projects/{project_id}/pause", {"reason": "e2e pause"})
        assert status == 200, f"pause -> {status}: {paused_wf}"
        assert paused_wf.get("status") == "paused", f"pause response status: {paused_wf.get('status')}"
        assert paused_wf.get("next") == [], (
            f"paused workflow must release nothing, next={paused_wf.get('next')}: "
            f"{json.dumps(paused_wf, ensure_ascii=False)[:2000]}"
        )
        interrupt = _find_interrupt(paused_wf, "project", "paused")
        assert interrupt is not None, f"paused workflow missing project interrupt: {paused_wf.get('interrupts')}"
        assert interrupt.get("action_request", {}).get("action") == "resume", (
            f"pause interrupt action_request: {interrupt.get('action_request')}"
        )

    # --- 7. role boundary: worker/manager tokens are rejected BEFORE any ---
    # state change (the durable check below proves nothing moved).
    status, denied = _api("POST", f"/api/v1/loops/{loop_name}/pause", {"source": "user"}, token=WORKER_TOKEN)
    assert status == 403, f"worker loop pause must be 403, got {status}: {denied}"
    assert 'worker "e2e" cannot update loop' in str(denied.get("message") or denied), (
        "worker 403 must come from the authorizer middleware (deny before the handler): "
        f"{denied}"
    )
    status, denied = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"}, token=MANAGER_TOKEN)
    assert status == 403, f"manager user-source resume must be 403, got {status}: {denied}"
    assert "resuming the user pause source requires admin" in str(denied.get("message") or denied), (
        "manager 403 must come from the handler (the manager passes the authorizer): "
        f"{denied}"
    )

    # --- 8. durable pause state: read the project meta from the object store ---
    meta = _mc_meta_object(project_id)
    loop_record = meta.get("loop")
    assert isinstance(loop_record, dict), (
        f"project meta has no loop record after plan_loop + loop pause: "
        f"{json.dumps(meta, ensure_ascii=False)[:800]}"
    )
    assert loop_record.get("pause_sources") == ["user"], (
        f"durable loop.pause_sources must be ['user'] after the loop pause "
        f"(got {json.dumps(loop_record.get('pause_sources'))}); this also proves the "
        f"two 403s above changed no state"
    )
    assert meta.get("status") == "paused", (
        f"durable project status after the pause phase: {meta.get('status')}"
    )
    project_status_now = "paused"

    # --- 8b. reversed order ONLY: the Manager resumes the project NOW,
    # while the user veto is still held (the order the pre-#1402 clobber
    # made unsafe — the project write dropped the veto, the bypass).
    # 200 + active, and the veto must survive the write.
    if order == "reversed":
        status, resumed_wf = _api(
            "POST", f"/api/v1/projects/{project_id}/resume", {}, token=MANAGER_TOKEN
        )
        assert status == 200, f"manager project resume -> {status}: {resumed_wf}"
        assert resumed_wf.get("status") == "active", (
            f"manager resume status: {resumed_wf.get('status')}"
        )
        meta = _mc_meta_object(project_id)
        assert meta.get("status") == "active", (
            f"durable status after the manager resume: {meta.get('status')}"
        )
        assert meta.get("loop", {}).get("pause_sources") == ["user"], (
            f"the user veto must survive the project resume write "
            f"(got {json.dumps(meta.get('loop', {}).get('pause_sources'))}); "
            f"dropping it is exactly the #1402 bypass"
        )
        status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
        assert status == 200, f"workflow -> {status}: {wf}"
        assert wf.get("status") == "active", f"workflow status: {wf.get('status')}"
        assert wf.get("next") == [], (
            f"the surviving veto must hold the ready task on an active project: "
            f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
        )
        project_status_now = "active"

    # --- 9. pause blocks NEW dispatch (fresh driver process) ---------------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws2")
    assert result.get("ok") is False, f"delegate A while paused must fail: {result}"
    assert "paused" in str(result.get("error")), f"delegate A while paused error: {result.get('error')}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})  # no new delegate send
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    _assert_nodes(wf, {"A": "pending", "B": "delegated"})
    assert wf.get("next") == [], (
        f"paused workflow must not release downstream / next-round work: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )

    # --- 10. in-flight work STILL saves its result under the pause ---------
    task_dir = tmp_path / "ws3" / "shared" / "tasks" / "B"
    task_dir.mkdir(parents=True)
    (task_dir / "result.md").write_text(
        "# Task B result\n\ne2e submission under pause.\n", encoding="utf-8"
    )
    (task_dir / "out.txt").write_text("e2e deliverable for B\n", encoding="utf-8")
    result = _run_driver(
        env,
        sent_path,
        "submit",
        project_id,
        "B",
        worker,
        tmp_path / "ws3",
        result={
            "summary": "task B finished under pause",
            "status": "SUCCESS",
            "deliverables": ["shared/tasks/B/out.txt"],
        },
    )
    assert result.get("ok") is True, f"submit B while paused: {result}\nsent: {_sent_tail(sent_path)}"
    # The result is persisted to the object store — read it back through the
    # Controller's task-inspection endpoint (stored TaskMeta).
    status, inspection = _api("GET", f"/api/v1/projects/{project_id}/tasks/B")
    assert status == 200, f"task B inspection -> {status}: {inspection}"
    assert inspection.get("status") == "submitted", f"task B status: {json.dumps(inspection, ensure_ascii=False)[:2000]}"
    assert inspection.get("result_status") == "SUCCESS", f"task B result_status: {inspection.get('result_status')}"
    assert inspection.get("summary") == "task B finished under pause", f"task B summary: {inspection.get('summary')}"
    assert inspection.get("result_path") == "shared/tasks/B/result.md", f"task B result_path: {inspection.get('result_path')}"
    assert inspection.get("submission_id"), f"task B submission_id missing: {inspection}"
    history_actions = [str(entry.get("action") or "") for entry in inspection.get("history", [])]
    assert "submit_task" in history_actions, f"task B history missing submit_task: {history_actions}"
    _assert_sent_counts(sent_path, "completion", {"B": 1})
    # The project-meta write-back must NOT clobber the pause state.
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == project_status_now, (
        f"submit must keep the project {project_status_now}: {wf.get('status')}"
    )
    _assert_nodes(wf, {"A": "pending", "B": "in-progress"})
    assert wf.get("next") == [], (
        f"the pause state must still hold the ready task after submit: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )

    # --- 11. human-source boundary ------------------------------------------
    # A "project"-source resume cannot release the human veto: the admin
    # passes the access check, and removing an absent source is a no-op —
    # the loop stays paused on the remaining "user" source.
    status, partial = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "project"})
    assert status == 200, f"project-source resume -> {status}: {partial}"
    assert partial.get("paused") is True, (
        f"the user veto must survive a project-source resume: {partial}"
    )
    assert partial.get("sources") == ["user"], f"remaining sources after project-source resume: {partial}"
    # Releasing the user veto is admin-only.
    status, released = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"})
    assert status == 200, f"user-source resume -> {status}: {released}"
    assert released.get("paused") is False, (
        f"the loop must be unpaused after the admin release: {released}"
    )
    assert released.get("sources") == [], f"sources after the admin release: {released}"

    # --- 12. authorized project resume (classic order: NOW; the reversed
    # order already resumed it in step 8b — while the veto was still held) --
    if order == "classic":
        status, resumed_wf = _api("POST", f"/api/v1/projects/{project_id}/resume", {})
        assert status == 200, f"resume -> {status}: {resumed_wf}"
        assert resumed_wf.get("status") == "active", f"resume response status: {resumed_wf.get('status')}"

    # --- 13. a FRESH process continues exactly once -------------------------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws4")
    assert result.get("ok") is True, f"post-resume delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is not True, (
        f"post-resume delegate A must be a fresh send: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "active", f"post-release workflow status: {wf.get('status')}"
    _assert_nodes(wf, {"A": "delegated", "B": "in-progress"})
    # `next` includes tasks whose raw status is planned *or* assigned
    # (buildWorkflow mirrors the upstream _ready_nodes semantics), so the
    # delegated A stays listed while its dependency-less sibling B must not
    # be released: B waits on A.
    assert wf.get("next") == ["A"], (
        f"post-resume only the delegated A stays ready (B waits on A): {wf.get('next')}"
    )

    # --- 14. a second fresh process REUSES the event (no second send) -------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws5")
    assert result.get("ok") is True, f"retry delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is True, (
        f"retry of an assigned task must reuse the recorded event: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})  # A still exactly once

    # --- 15. completed work is NOT re-dispatched (fresh process) ------------
    result = _run_driver(env, sent_path, "delegate", project_id, "B", worker, tmp_path / "ws6")
    assert result.get("ok") is False, f"delegate submitted B must fail: {result}"
    assert "submitted" in str(result.get("error")), (
        f"delegate submitted B error: {result.get('error')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    # Only A advanced (once); B is still the in-flight submission.
    _assert_nodes(wf, {"A": "delegated", "B": "in-progress"})

    # --- 16. check_task on the submitted task (fresh process, real path) ----
    result = _run_driver(env, sent_path, "status", project_id, "B", worker, tmp_path / "ws7")
    assert result.get("ok") is True, f"check B: {result}"
    assert result.get("effective") is True, f"check B effective: {result.get('effective')}"
    assert result.get("result", {}).get("status") == "SUCCESS", f"check B result: {result.get('result')}"


def test_pause_before_meta_commit_survives_conditional_commit(
    e2e_env: tuple[dict[str, str], Path], tmp_path: Path
) -> None:
    """#1402 B, the later boundary: a user pause that lands BETWEEN the
    project meta commit's ETag read and its PUT (the driver fires the
    real loop pause right before the first conditional commit) must
    force a REAL 409 on the commit, and the retry's re-snapshot +
    re-merge must preserve the durable ``loop.pause_sources`` — the
    in-flight submission still succeeds, a fresh process stays blocked
    by the real worker gate, and only the admin user-source release
    unblocks dispatch (exactly once)."""
    env, sent_path = e2e_env
    worker = "@e2e-worker:matrix.e2e.invalid"
    project_id, loop_name = _setup_veto_project(env, sent_path, tmp_path)

    # The in-flight B result, exactly as in the closed loop's step 10.
    task_dir = tmp_path / "ws3" / "shared" / "tasks" / "B"
    task_dir.mkdir(parents=True)
    (task_dir / "result.md").write_text(
        "# Task B result\n\ne2e submission across the commit boundary.\n", encoding="utf-8"
    )
    (task_dir / "out.txt").write_text("e2e deliverable for B\n", encoding="utf-8")

    # THE injection: the driver pauses the loop for real right before the
    # FIRST conditional meta commit (after the ETag read) — strictly later
    # than any pull-window race the unit tests cover.
    result = _run_driver(
        env,
        sent_path,
        "submit",
        project_id,
        "B",
        worker,
        tmp_path / "ws3",
        result={
            "summary": "task B finished across the commit boundary",
            "status": "SUCCESS",
            "deliverables": ["shared/tasks/B/out.txt"],
        },
        pause_loop_before_commit=loop_name,
    )
    assert result.get("ok") is True, (
        f"the in-flight submission must survive the 409 retry: {result}\n"
        f"sent: {_sent_tail(sent_path)}"
    )
    injection = result.get("pause_injection")
    assert isinstance(injection, dict), f"submit result missing pause_injection: {result}"
    assert injection.get("loop") == loop_name, f"injection loop: {injection}"
    assert injection.get("fired") is True, f"the injection never fired (no commit attempted): {injection}"
    assert injection.get("status") == 200, (
        f"the real loop pause must land with 200: {injection}"
    )
    commits = injection.get("commits")
    assert isinstance(commits, list) and len(commits) == 2, (
        f"expected exactly the 409 -> retry -> 200 chain, got: {json.dumps(commits, ensure_ascii=False)}"
    )
    assert commits[0].get("conflict") is True, (
        f"the first commit must hit the real 409 (the pause landed after the ETag read): {commits[0]}"
    )
    assert commits[1].get("ok") is True, f"the retry commit must succeed: {commits[1]}"
    assert commits[1].get("etag"), f"the retry commit must carry the new ETag: {commits[1]}"

    # Durable (object store read, not the API response): the project stays
    # ACTIVE (a loop pause never pauses the project) and the user veto is
    # preserved across the conditional-commit retry.
    meta = _mc_meta_object(project_id)
    assert meta.get("status") == "active", (
        f"the loop pause must not pause the project: {meta.get('status')}"
    )
    loop_record = meta.get("loop")
    assert isinstance(loop_record, dict) and loop_record.get("pause_sources") == ["user"], (
        "the pause that landed between the ETag read and the commit must survive "
        f"the retry (got {json.dumps(loop_record.get('pause_sources') if isinstance(loop_record, dict) else loop_record, ensure_ascii=False)})"
    )

    # The in-flight result still landed: submit has no pause gate by
    # design — read it back through the Controller's task inspection.
    status, inspection = _api("GET", f"/api/v1/projects/{project_id}/tasks/B")
    assert status == 200, f"task B inspection -> {status}: {inspection}"
    assert inspection.get("status") == "submitted", (
        f"task B status: {json.dumps(inspection, ensure_ascii=False)[:2000]}"
    )
    assert inspection.get("result_path") == "shared/tasks/B/result.md", (
        f"task B result_path: {inspection.get('result_path')}"
    )
    _assert_sent_counts(sent_path, "completion", {"B": 1})

    # The real worker-side gate now blocks new dispatch on the surviving
    # veto, and the workflow releases nothing on the active project.
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws4")
    assert result.get("ok") is False, f"delegate A under the surviving veto must fail: {result}"
    assert "paused" in str(result.get("error")), f"delegate A error: {result.get('error')}"
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "active", f"workflow status: {wf.get('status')}"
    _assert_nodes(wf, {"A": "pending", "B": "in-progress"})
    assert wf.get("next") == [], (
        f"the surviving veto must hold the ready task on an active project: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )
    _assert_sent_counts(sent_path, "delegate", {"B": 1})  # no new delegate send

    # The admin user-source release is the only unlock; dispatch proceeds
    # exactly once afterwards.
    status, released = _api("POST", f"/api/v1/loops/{loop_name}/resume", {"source": "user"})
    assert status == 200, f"user-source resume -> {status}: {released}"
    assert released.get("paused") is False, f"the loop must be unpaused after the release: {released}"
    assert released.get("sources") == [], f"sources after the release: {released}"
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws5")
    assert result.get("ok") is True, f"post-release delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
