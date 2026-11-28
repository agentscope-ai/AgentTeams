"""Worker execution recovery shared by native Harness adapters and Task tools.

Native sessions are caches. Only a claimed authoritative Task can start work;
delivery-only recovery never enters the model. No Matrix cursor is discarded.
"""
from __future__ import annotations

import hashlib
import http.client
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request


class RecoveryConflict(ValueError):
    pass


class MetadataConflict(RecoveryConflict):
    """The conditional metadata write was rejected without committing it."""


class RecoveryUnavailable(ValueError):
    """A failed request is not proof that execution ownership was lost."""


class Recovery:
    def __init__(self, *, request=None, device_delivery=False):
        self.device_delivery = device_delivery
        self.request = request or self._request
        self.process = os.environ["AGENTTEAMS_PROCESS_ID"]

    def _request(self, method, path, body=None, etag=None, *, create=False, response_etag=False):
        token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/secrets/agentteams/token").read_text().strip()
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json",
                   "X-AgentTeams-Process-ID": self.process}
        if self.device_delivery:
            headers["X-AgentTeams-Reply-Scope"] = "device-v1"
        if create and etag:
            raise ValueError("Use one conditional write precondition")
        if create:
            headers["If-None-Match"] = "*"
        elif etag:
            headers["If-Match"] = etag
        request = urllib.request.Request(os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/" + path,
            data=json.dumps(body).encode() if body is not None else None, method=method, headers=headers)
        for attempt in range(2):
            try:
                with urllib.request.urlopen(request, timeout=15) as response:
                    payload = json.load(response)
                    return (payload, response.headers.get("ETag")) if response_etag else payload
            except urllib.error.HTTPError as error:
                if error.code in {403, 404, 409, 412}:
                    message = error.read().decode()[:1024]
                    try:
                        conflict = json.loads(message).get("code") == "conflict"
                    except (ValueError, AttributeError):
                        conflict = False
                    kind = MetadataConflict if error.code in {409, 412} and conflict else RecoveryConflict
                    raise kind(message) from error
                raise RecoveryUnavailable(f"Task recovery request failed (HTTP {error.code}); read the current Task before retrying") from error
            except (urllib.error.URLError, TimeoutError, ConnectionError, http.client.HTTPException) as error:
                if method == "GET" and attempt == 0:
                    time.sleep(0.25)
                    continue
                raise RecoveryUnavailable("Task recovery connection failed; read the current Task before retrying") from error

    def discover(self, *, include_waiting=False):
        snapshot = self.request("GET", "tasks/recoverable")
        if include_waiting:
            return snapshot
        return {**snapshot, "tasks": [entry for entry in snapshot["tasks"]
            if entry["task"].get("session_state") != "waiting_user"]}

    @staticmethod
    def task_path(task_id, team):
        return "tasks/" + urllib.parse.quote(task_id, safe="") + "/execution?team=" + urllib.parse.quote(team, safe="")

    def claim(self, snapshot, entry):
        claimed = self.request("POST", self.task_path(entry["task"]["task_id"], entry["team_id"]),
            {"action": "claim", "instance_id": snapshot["instance_id"], "process_id": self.process}, entry["etag"])
        task = claimed['task']
        execution = task.get('execution') or {}
        operation = (execution.get('checkpoint') or {}).get('operation') or {}
        if execution.get('state') == 'needs_verification' and operation.get('state') == 'started':
            from native_tool_returns import load
            identity = {**execution, 'task_id': task['task_id'], 'team_id': entry['team_id']}
            stored = load(identity, operation['id'])
            if stored:
                self.finish_tools(identity, operation['id'], stored['results'], artifacts=stored.get('artifacts', []))
                claimed = {**claimed, 'task': self.current(task['task_id'], entry['team_id'])}
        return claimed

    def checkpoint(self, task_id, team, checkpoint, *, release=False, expected_identity=None, expected_digest=None):
        for attempt in range(2):
            snapshot = self.discover(include_waiting=True)
            entry = next((e for e in snapshot["tasks"] if e["task"]["task_id"] == task_id and e["team_id"] == team), None)
            if entry is None:
                raise RecoveryConflict("Task was cancelled, reassigned or is no longer recoverable")
            execution = entry["task"].get("execution", {})
            if execution.get("process_id") != self.process or execution.get("instance_id") != snapshot["instance_id"]:
                raise RecoveryConflict("Task is not owned by this process")
            if expected_identity is None:
                expected_identity = {key: execution.get(key) for key in ('generation', 'instance_id', 'process_id')}
            if expected_digest is None:
                expected_digest = self.digest(execution.get('checkpoint'))
            if expected_identity is not None and any(execution.get(key) != expected_identity.get(key)
                                                    for key in ('generation', 'instance_id', 'process_id')):
                raise RecoveryConflict('Native tool execution ownership changed')
            if expected_digest is not None and self.digest(execution.get('checkpoint')) != expected_digest:
                raise RecoveryConflict('Native tool checkpoint changed before commit')
            try:
                return self.request("POST", self.task_path(task_id, team),
                    {"action": "release" if release else "checkpoint", "instance_id": snapshot["instance_id"],
                     "process_id": self.process, "checkpoint": checkpoint}, entry["etag"])
            except MetadataConflict:
                if attempt:
                    raise
                # No tool is repeated. Merge only after rechecking the same
                # generation, owner and unchanged original checkpoint.
            except RecoveryUnavailable:
                # The conditional write may have committed before its response
                # was lost. Confirm that exact checkpoint without replaying it.
                if not release:
                    confirmed = self.discover(include_waiting=True)
                    saved = next((e for e in confirmed["tasks"]
                        if e["task"]["task_id"] == task_id and e["team_id"] == team), None)
                    actual = saved["task"].get("execution", {}) if saved else {}
                    if (saved and confirmed["instance_id"] == expected_identity.get("instance_id")
                            and all(actual.get(key) == expected_identity.get(key)
                                    for key in ("generation", "instance_id", "process_id"))
                            and self.checkpoint_committed(actual.get("checkpoint"), checkpoint)):
                        return {**saved, "execution": actual, "idempotent_hit": True}
                raise

    @staticmethod
    def checkpoint_committed(actual, proposed):
        """Compare the submitted fields, excluding Core's commit timestamp."""
        def fields(checkpoint):
            value = dict(checkpoint or {})
            value.pop("saved_at", None)
            for key in ("completed_steps", "remaining_steps", "artifacts"):
                value.setdefault(key, None)
            return value
        return fields(actual) == fields(proposed)

    def current(self, task_id, team):
        snapshot = self.discover(include_waiting=True)
        return next((e["task"] for e in snapshot["tasks"]
                     if e["task"]["task_id"] == task_id and e["team_id"] == team), None)

    def tools_current(self, identity, *, verification=False):
        task = self.current(identity['task_id'], identity['team_id'])
        execution = task.get('execution', {}) if task else {}
        if (not task or execution.get('process_id') != self.process
                or any(execution.get(key) != identity.get(key) for key in ('generation', 'instance_id', 'process_id'))):
            raise RecoveryConflict('Native tool execution ownership was replaced')
        if not verification and task.get('status') != 'in_progress':
            raise RecoveryConflict('Task execution is already submitted or completed; only its owned tool return may settle')
        if execution.get('state') not in ({'active', 'needs_verification'} if verification else {'active'}):
            raise RecoveryConflict('Native unconfirmed operation requires verification before normal admission')
        if not verification and task.get('session_state') == 'waiting_user':
            raise RecoveryConflict('Task is waiting for a human decision')
        return task

    def begin_tools(self, identity, calls):
        from task_operation import begin
        task = self.tools_current(identity)
        original = task['execution'].get('checkpoint')
        operation, cp = begin(original, [identity['team_id'], identity['task_id']], calls)
        try:
            self.checkpoint(identity['task_id'], identity['team_id'], cp,
                            expected_identity=identity, expected_digest=self.digest(original))
        except RecoveryUnavailable:
            from native_tool_returns import not_dispatched
            not_dispatched(identity, operation, calls)
            raise
        return operation

    def finish_tools(self, identity, operation, results, *, artifacts=()):
        from native_tool_returns import stage
        stage(identity, operation, results, artifacts=list(artifacts))
        from task_operation import result_bytes, finish, verified_return
        task = self.tools_current(identity, verification=True)
        original = task['execution'].get('checkpoint')
        if verified_return(original, operation, results):
            return {**task, 'idempotent_hit': True}
        data = result_bytes(original, operation, results)
        digest = hashlib.sha256(data).hexdigest()
        key = f"teams/{identity['team_id']}/shared/tasks/{identity['task_id']}/checkpoints/tools-{digest}.txt"
        subprocess.run(['mc', 'pipe', os.environ['AGENTTEAMS_STORAGE_PREFIX'].rstrip('/') + '/' + key],
                       input=data, check=True, capture_output=True, timeout=30)
        cp = finish(original, operation, {'key': key, 'sha256': digest, 'size': len(data)})
        for artifact in artifacts:
            if artifact not in cp['artifacts']:
                cp['artifacts'].append(artifact)
        return self.checkpoint(identity['task_id'], identity['team_id'], cp,
                               expected_identity=identity, expected_digest=self.digest(original))

    def save_reply(self, task_id, team, text):
        task = self.current(task_id, team)
        if task is None:
            raise RecoveryConflict("Task is no longer assigned to this Worker")
        cp = dict(task["execution"].get("checkpoint") or {})
        if cp.get("result") and (cp.get("delivery") != "delivered" or task["status"] == "submitted"):
            raise RecoveryConflict("A saved reply already exists; recover its delivery instead")
        data = text.encode()
        digest = hashlib.sha256(data).hexdigest()
        key = f"teams/{team}/shared/tasks/{task_id}/checkpoints/reply-{digest}.txt"
        # mc receives bytes on stdin; no shell or native-session format is used.
        subprocess.run(["mc", "pipe", os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/" + key],
                       input=data, check=True, capture_output=True, timeout=30)
        version = cp.get("version", 0) + 1
        if self.device_delivery:
            cp["reply_attempt"] = {"state": "ready"}
        else:
            cp.pop("reply_attempt", None)
        cp.pop("reply_event", None)
        cp.update(version=version, result={"key": key, "sha256": digest, "size": len(data)},
                  delivery="pending", reply_txn="task-reply:" + hashlib.sha256(f"{team}/{task_id}/{version}/{digest}".encode()).hexdigest())
        return self.checkpoint(task_id, team, cp)

    def prepare_reply(self, task_id, team, text):
        """Fence a native final callback and prefer an already saved report.

        A Task tool can release execution after submission. The same verified
        process may reclaim it for delivery; a replaced process cannot.
        """
        snapshot = self.discover(include_waiting=True)
        entry = next((e for e in snapshot["tasks"] if e["task"]["task_id"] == task_id and e["team_id"] == team), None)
        execution = entry["task"].get("execution", {}) if entry else {}
        if execution.get("process_id") != self.process or execution.get("instance_id") != snapshot["instance_id"]:
            raise RecoveryConflict("Native reply execution was replaced or is no longer recoverable")
        # submit_task permits a summary without a report file. While this
        # process still owns active execution, persist its final answer before
        # asking claim to recover an already persisted submitted report.
        final_report = entry["task"]["status"] in {"submitted", "completed"} and entry["task"].get("result_path")
        if execution.get("state") == "active" and not (execution.get("checkpoint") or {}).get("result") and not final_report:
            return self.save_reply(task_id, team, text), text
        claimed = self.claim(snapshot, entry)
        cp = claimed["execution"].get("checkpoint") or {}
        if cp.get("delivery") == "pending":
            return claimed, claimed["saved_result"]
        if cp.get("delivery") == "delivered" and claimed["task"]["status"] in {"submitted", "completed"}:
            return claimed, ""
        return self.save_reply(task_id, team, text), text

    def delivered(self, task_id, team, event_id):
        task = self.current(task_id, team)
        if task is None:
            raise RecoveryConflict("Task delivery ownership changed")
        cp = dict(task["execution"]["checkpoint"])
        cp.update(version=cp["version"] + 1, delivery="delivered", reply_event=event_id)
        return self.checkpoint(task_id, team, cp)

    def interrupted(self, task_id, team):
        """Keep the last durable checkpoint; registered operations retain their own state."""

    def release_failed_turn(self, identity):
        """Release a failed inference only after every registered tool is settled."""
        task = self.current(identity['task_id'], identity['team_id'])
        execution = task.get('execution', {}) if task else {}
        checkpoint = dict(execution.get('checkpoint') or {})
        operation = checkpoint.get('operation') or {}
        if (not task or task['status'] not in {'assigned', 'in_progress'}
                or execution.get('state') != 'active' or checkpoint.get('result')
                or operation and operation.get('state') != 'verified'):
            return False
        checkpoint.update(version=checkpoint.get('version', 0) + 1)
        self.checkpoint(identity['task_id'], identity['team_id'], checkpoint,
                        release=True, expected_identity=identity)
        return True

    @staticmethod
    def session_usable(session, task, runtime, *, now=None):
        """The native adapter must additionally verify the session exists."""
        cp = task.get("execution", {}).get("checkpoint")
        return bool(session and session.get("runtime") == runtime
            and session.get("expires_at", 0) > (time.time() if now is None else now)
            and session.get("checkpoint_digest") == Recovery.digest(cp))

    @staticmethod
    def digest(checkpoint):
        return hashlib.sha256(json.dumps(checkpoint, sort_keys=True, separators=(",", ":")).encode()).hexdigest()

    @staticmethod
    def resume_text(task):
        execution = task["execution"]
        checkpoint = execution.get("checkpoint")
        decisions = [record for record in task.get('attention', []) if record.get('answer')]
        decision_context = {'human_decisions': decisions} if decisions else {}
        if task.get('task_plan_id'):
            progress = {key: [step for step in ((checkpoint or {}).get(key) or []) if not step.startswith('tools:')]
                        for key in ('completed_steps', 'remaining_steps')}
            progress = {key: steps for key, steps in progress.items() if steps}
            return ("Continue your existing Task through the common task protocol. Use its prepared inputs and output "
                "directory; read missing details on demand. Preserve verified work and submit this Task's outputs.\n"
                + json.dumps({'task_id': task['task_id'], 'team_id': task.get('team_id'), 'status': task['status'],
                    **({'progress': progress} if progress else {}), **decision_context}, ensure_ascii=False))
        return ("Recover your existing Task from the authoritative task system. Do not create a new task.\n"
            "Before a new external side effect, record its operation identity with checkpoint_task. If an operation is "
            "started or unknown, verify its external outcome and save evidence before continuing; do not rerun it. "
            "Continue from the last saved progress. Repeat unsaved reasoning or replayable work when needed. "
            "Pull and verify referenced artifacts before using them. "
            "Save artifacts before committing their checkpoint. A read receipt or ACK is not a checkpoint.\n"
            "Use taskflow.checkpoint_task at step boundaries. If your assigned executor already persists these "
            "checkpoints, invoke it unchanged; do not pre-register its operation IDs or commit a duplicate checkpoint "
            "outside its protocol. Call submit_task directly with the same Task ID once prior operations are verified. "
            "It saves its own canonical result; do not register submission itself as a started operation.\n"
            + json.dumps({"task_id": task["task_id"], "team_id": task.get("team_id"),
                "status": task["status"], "title": task.get("title"), "spec": task.get("spec"),
                "project_id": task.get("project_id"), "main_task_id": task.get("main_task_id"),
                "generation": execution["generation"], "checkpoint": checkpoint,
                "history": task.get("history", [])[-10:], **decision_context}, ensure_ascii=False))


def enabled():
    return bool(os.getenv("AGENTTEAMS_CONTROLLER_URL") and os.getenv("AGENTTEAMS_PROCESS_ID")
                and os.getenv("AGENTTEAMS_AGENT_ROLE", "worker") in {"standalone", "worker", "remote-member", "leader", "team_leader"})
