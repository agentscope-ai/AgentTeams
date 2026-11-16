"""Test-only side effects outside Worker storage; never a product Task database.

Run in a separate container with a private ledger and no Docker socket. Worker
requests use their own Controller credential; only the driver can read proof.
"""
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

STEPS = ["one", "two", "three"]


class Ledger:
    def __init__(self, path):
        self.path = Path(path)
        self.lock = threading.Lock()
        self.data = json.loads(self.path.read_text()) if self.path.exists() else {"effects": [], "calls": [], "intents": []}

    def save(self):
        temporary = self.path.with_suffix(".tmp")
        with temporary.open("w") as stream:
            json.dump(self.data, stream, sort_keys=True)
            stream.flush()
            os.fsync(stream.fileno())
        temporary.replace(self.path)
        descriptor = os.open(self.path.parent, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def proof(self, team, task_id):
        with self.lock:
            return {kind: [dict(row) for row in rows if row["team_id"] == team and row["task_id"] == task_id]
                    for kind, rows in self.data.items()}

    def effect(self, snapshot, team, task_id, step, process, method):
        with self.lock:
            call = {"team_id": team, "task_id": task_id, "step": step, "process_id": process,
                    "method": method, "accepted": False, "at_ns": time.time_ns()}
            self.data["calls"].append(call)
            try:
                task = next((entry["task"] for entry in snapshot["tasks"]
                             if entry["team_id"] == team and entry["task"]["task_id"] == task_id), None)
                execution = (task or {}).get("execution", {})
                cp = execution.get("checkpoint") or {}
                operation = cp.get("operation") or {}
                if (not task or task["status"] != "in_progress" or not process
                        or execution.get("process_id") != process
                        or execution.get("instance_id") != snapshot["instance_id"]
                        or execution.get("state") not in {"active", "needs_verification"}):
                    raise ValueError("Task is not owned by this current process")
                done = cp.get("completed_steps", [])
                if step not in STEPS or done != STEPS[:STEPS.index(step)]:
                    raise ValueError("Task checkpoint is not at this step boundary")
                effects = [row for row in self.data["effects"] if row["team_id"] == team and row["task_id"] == task_id]
                existing = next((row for row in effects if row["step"] == step), None)
                identity = {"team_id": team, "task_id": task_id, "step": step,
                            "process_id": process, "instance_id": execution["instance_id"],
                            "generation": execution["generation"]}
                if method == "REGISTER":
                    if execution["state"] != "active" or operation.get("state") not in {None, "verified"} or existing:
                        raise ValueError("Register intent before the durable operation start")
                    if [row["step"] for row in effects] != done:
                        raise ValueError("Task disagrees with independent completed prefix")
                    result = {**identity, "checkpoint_version": cp.get("version", 0) + 1}
                    self.data["intents"].append(result)
                elif operation.get("id") != f"fixture:{task_id}:{step}" or operation.get("state") not in {"started", "unknown"}:
                    raise ValueError("Register this step operation before executing or verifying it")
                elif method == "GET":
                    if existing is None:
                        raise ValueError("Needs verification: no verified external result; no rerun")
                    result = existing
                elif method == "POST":
                    if existing:
                        raise ValueError("Side effect already executed; verify its result instead")
                    if operation["state"] != "started":
                        raise ValueError("Unknown side effects require verification, not execution")
                    intent = {**identity, "checkpoint_version": cp["version"]}
                    if intent not in self.data["intents"]:
                        raise ValueError("No pre-start intent from this process; verify instead")
                    if [row["step"] for row in effects] != done:
                        raise ValueError("Task disagrees with independent completed prefix")
                    result = {"team_id": team, "task_id": task_id, "step": step,
                              "generation": execution["generation"], "process_id": process,
                              "instance_id": execution["instance_id"], "checkpoint_version": cp["version"],
                              "executed_at_ns": time.time_ns()}
                    self.data["effects"].append(result)
                else:
                    raise ValueError("Unsupported effect method")
                call["accepted"] = True
                return dict(result)
            finally:
                self.save()


def server(address, controller, ledger, proof_key):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass  # Never log bearer credentials or model-controlled request data.

        def respond(self, status, payload):
            data = json.dumps(payload, sort_keys=True).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self):
            self.do_GET()

        def do_GET(self):
            path = urllib.parse.urlsplit(self.path)
            params = urllib.parse.parse_qs(path.query)
            if path.path == "/healthz":
                return self.respond(200, {"ready": True})
            token = self.headers.get("Authorization", "")
            team, task_id = params.get("team", [""])[0], params.get("task", [""])[0]
            if path.path == "/proof" and self.command == "GET":
                if not hmac.compare_digest(token, "Bearer " + proof_key):
                    return self.respond(403, {"error": "Driver proof credential required"})
                return self.respond(200, ledger.proof(team, task_id))
            if path.path not in {"/effect", "/intent"} or (path.path == "/intent" and self.command != "POST"):
                return self.respond(404, {"error": "Unknown endpoint"})
            if not token.startswith("Bearer ") or token == "Bearer " + proof_key:
                return self.respond(403, {"error": "Worker credential required"})
            process = self.headers.get("X-AgentTeams-Process-ID", "")
            request = urllib.request.Request(controller.rstrip("/") + "/api/v1/tasks/recoverable",
                headers={"Authorization": token, "X-AgentTeams-Process-ID": process})
            try:
                with urllib.request.urlopen(request, timeout=15) as response:
                    snapshot = json.load(response)
                result = ledger.effect(snapshot, team, task_id, params.get("step", [""])[0], process,
                    "REGISTER" if path.path == "/intent" else self.command)
            except ValueError as error:
                return self.respond(409, {"error": str(error)})
            except (OSError, urllib.error.URLError):
                return self.respond(503, {"error": "Controller verification unavailable; no execution"})
            return self.respond(200, result)

    return ThreadingHTTPServer(address, Handler)


if __name__ == "__main__":
    config = json.loads(Path("/private/config.json").read_text())
    server(("0.0.0.0", 8091), config["controller"], Ledger("/ledger/effects.json"), config["proof_key"]).serve_forever()
