"""Report readiness for a registered replacement Worker."""
import os
from pathlib import Path
import time

from task_recovery import Recovery


def recovery_scanned():
    """Called by the adapter after Task discovery and its first Matrix recovery scan."""
    marker = os.getenv("AGENTTEAMS_SUPERVISOR_READY_FILE")
    if marker:
        Path(marker).write_text(os.environ["AGENTTEAMS_PROCESS_ID"])


class SupervisorHandoff:
    def __init__(self, runtime, child, ready_path):
        self.recovery = Recovery()
        self.runtime, self.child, self.ready_path = runtime, child, ready_path

    def check(self):
        if not self.ready_path.is_file() or self.ready_path.read_text() != self.recovery.process:
            return
        state, etag = self.recovery.request("GET", "runtime/handoff", response_etag=True)
        op = state.get("handoff")
        if not op or op["phase"] == "completed":
            return True
        if not etag or op["phase"] != "recovering":
            return False
        start = op.get("target") or {}
        if start.get("process_id") != self.recovery.process or start.get("runtime") != self.runtime:
            return
        body = {"request_id": op["id"], "instance_id": start["instance_id"], "process_id": self.recovery.process}
        self.recovery.request("POST", "runtime/handoff/recovered", body, etag)
        return True

    def monitor(self):
        from runtime_observability import event
        previous = None
        while self.child.poll() is None:
            try:
                if self.check():
                    return
                previous = None
            except Exception as error:
                failure = type(error).__name__
                if failure != previous:
                    event("handoff.pending", error_type=failure)
                previous = failure
            time.sleep(5)
