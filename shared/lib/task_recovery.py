"""Worker execution recovery shared by native Harness adapters and Task tools.

Native sessions are caches. Only a claimed authoritative Task can start work;
delivery-only recovery never enters the model. No Matrix cursor is discarded.
"""
from __future__ import annotations

import hashlib
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


class RecoveryUnavailable(ValueError):
    """A failed request is not proof that execution ownership was lost."""


class Recovery:
    def __init__(self, *, request=None):
        self.request = request or self._request
        self.process = os.environ["AGENTTEAMS_PROCESS_ID"]

    def _request(self, method, path, body=None, etag=None):
        token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/secrets/agentteams/token").read_text().strip()
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json",
                   "X-AgentTeams-Process-ID": self.process}
        if etag:
            headers["If-Match"] = etag
        request = urllib.request.Request(os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/" + path,
            data=json.dumps(body).encode() if body is not None else None, method=method, headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=15) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code in {403, 404, 409, 412}:
                raise RecoveryConflict(error.read().decode()[:1024]) from error
            raise RecoveryUnavailable(f"Task recovery request failed (HTTP {error.code}); read the current Task before retrying") from error
        except (urllib.error.URLError, TimeoutError) as error:
            raise RecoveryUnavailable("Task recovery connection failed; read the current Task before retrying") from error

    def discover(self):
        return self.request("GET", "tasks/recoverable")

    @staticmethod
    def task_path(task_id, team):
        return "tasks/" + urllib.parse.quote(task_id, safe="") + "/execution?team=" + urllib.parse.quote(team, safe="")

    def claim(self, snapshot, entry):
        return self.request("POST", self.task_path(entry["task"]["task_id"], entry["team_id"]),
            {"action": "claim", "instance_id": snapshot["instance_id"], "process_id": self.process}, entry["etag"])

    def checkpoint(self, task_id, team, checkpoint, *, release=False):
        snapshot = self.discover()
        entry = next((e for e in snapshot["tasks"] if e["task"]["task_id"] == task_id and e["team_id"] == team), None)
        if entry is None:
            raise RecoveryConflict("Task was cancelled, reassigned or is no longer recoverable")
        execution = entry["task"].get("execution", {})
        if execution.get("process_id") != self.process or execution.get("instance_id") != snapshot["instance_id"]:
            raise RecoveryConflict("Task is not owned by this process")
        return self.request("POST", self.task_path(task_id, team),
            {"action": "release" if release else "checkpoint", "instance_id": snapshot["instance_id"],
             "process_id": self.process, "checkpoint": checkpoint}, entry["etag"])

    def current(self, task_id, team):
        snapshot = self.discover()
        return next((e["task"] for e in snapshot["tasks"]
                     if e["task"]["task_id"] == task_id and e["team_id"] == team), None)

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
        cp.pop("reply_event", None)
        cp.update(version=version, result={"key": key, "sha256": digest, "size": len(data)},
                  delivery="pending", reply_txn="task-reply:" + hashlib.sha256(f"{team}/{task_id}/{version}/{digest}".encode()).hexdigest())
        return self.checkpoint(task_id, team, cp)

    def prepare_reply(self, task_id, team, text):
        """Fence a native final callback and prefer an already saved report.

        A Task tool can release execution after submission. The same verified
        process may reclaim it for delivery; a replaced process cannot.
        """
        snapshot = self.discover()
        entry = next((e for e in snapshot["tasks"] if e["task"]["task_id"] == task_id and e["team_id"] == team), None)
        execution = entry["task"].get("execution", {}) if entry else {}
        if execution.get("process_id") != self.process or execution.get("instance_id") != snapshot["instance_id"]:
            raise RecoveryConflict("Native reply execution was replaced or is no longer recoverable")
        # submit_task permits a summary without a report file. While this
        # process still owns active execution, persist its final answer before
        # asking claim to recover an already persisted submitted report.
        submitted_report = entry["task"]["status"] == "submitted" and entry["task"].get("result_path")
        if execution.get("state") == "active" and not (execution.get("checkpoint") or {}).get("result") and not submitted_report:
            return self.save_reply(task_id, team, text), text
        claimed = self.claim(snapshot, entry)
        cp = claimed["execution"].get("checkpoint") or {}
        if cp.get("delivery") == "pending":
            return claimed, claimed["saved_result"]
        if cp.get("delivery") == "delivered" and claimed["task"]["status"] == "submitted":
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
        task = self.current(task_id, team)
        if task is None:
            return
        cp = dict(task["execution"].get("checkpoint") or {})
        if cp.get("result") and cp.get("delivery") != "delivered":
            return
        operation = cp.get("operation") or {}
        if operation.get("state") in {"started", "unknown"}:
            return  # Already fenced for verification; never replace its identity.
        for field in ("result", "delivery", "reply_txn", "reply_event"):
            cp.pop(field, None)
        operation = {"id": "native-turn:" + self.process}
        cp.update(version=cp.get("version", 0)+1, operation={**operation, "state": "unknown"})
        return self.checkpoint(task_id, team, cp)

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
        return ("Recover your existing Task from the authoritative task system. Do not create a new task.\n"
            "Before any new side effect, record its operation identity with checkpoint_task. If an operation is "
            "started or unknown, verify its external outcome and save evidence before continuing; do not rerun it. "
            "Completed steps must not run again. Pull and verify referenced artifacts before using them. "
            "Save artifacts before committing their checkpoint. A read receipt or ACK is not a checkpoint.\n"
            "Use taskflow.checkpoint_task at step boundaries. Use the same task ID for submission.\n"
            + json.dumps({"task_id": task["task_id"], "team_id": task.get("team_id"),
                "status": task["status"], "title": task.get("title"), "spec": task.get("spec"),
                "project_id": task.get("project_id"), "main_task_id": task.get("main_task_id"),
                "generation": execution["generation"], "checkpoint": checkpoint,
                "history": task.get("history", [])[-10:]}, ensure_ascii=False))


def enabled():
    return bool(os.getenv("AGENTTEAMS_CONTROLLER_URL") and os.getenv("AGENTTEAMS_PROCESS_ID")
                and os.getenv("AGENTTEAMS_AGENT_ROLE", "worker") in {"standalone", "worker", "remote-member", "leader", "team_leader"})
