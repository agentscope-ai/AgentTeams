"""Shared Controller-backed Matrix input persistence; no native session format.

Adapters persist decoded accepted inputs before advancing their native cursor.
This ledger records pending work, not proof of inference or Task completion.
"""
from task_recovery import Recovery, RecoveryConflict


class WorkerMatrix:
    def __init__(self, runtime, *, recovery=None):
        self.runtime = runtime
        self.recovery = recovery or Recovery()

    def load(self):
        return self.recovery.request("GET", "runtime/matrix")

    def start(self):
        snapshot = self.load()
        if snapshot["runtime"] != self.runtime:
            raise RecoveryConflict("Actual Worker Harness differs from this native adapter")
        body = {"action": "start", "instance_id": snapshot["instance_id"], "process_id": self.recovery.process}
        if snapshot["etag"]:
            saved = self.recovery.request("POST", "runtime/matrix", body, snapshot["etag"])
        else:
            saved = self.recovery.request("POST", "runtime/matrix", body, create=True)
        return {**snapshot, **saved}

    def ingest(self, snapshot, consumer, inputs, rooms=None):
        start = snapshot["state"]["active_start"]
        if (start["process_id"] != self.recovery.process or start["runtime"] != self.runtime
                or (consumer is not None and consumer["runtime"] != self.runtime)):
            raise RecoveryConflict("Matrix input snapshot is not owned by this native start")
        saved = self.recovery.request("POST", "runtime/matrix", {
            "action": "ingest", "instance_id": start["instance_id"], "process_id": self.recovery.process,
            "consumer": consumer, "inputs": inputs, "rooms": rooms or {},
        }, snapshot["etag"])
        return {**snapshot, **saved}

    def pending(self, snapshot):
        # The server resolves the Worker prefix from authenticated identity.
        # Native adapters cannot supply another Worker's storage path.
        return [self.recovery.request("GET", "runtime/matrix/events/" + key)
                for key in snapshot["state"]["pending"]]
