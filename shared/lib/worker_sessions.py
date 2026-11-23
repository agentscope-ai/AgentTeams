"""Common session descriptors; native adapters retain their own session format.

Remember only after final SDK serialization, using the checkpoint/config snapshot
represented by that session and the digest of persisted content.
"""
import time
from urllib.parse import urlencode

from task_recovery import Recovery, RecoveryConflict
from worker_matrix_state import matrix_event_key


class WorkerSessions:
    def __init__(self, runtime, *, recovery=None):
        self.runtime = runtime
        self.recovery = recovery or Recovery()

    @staticmethod
    def _path(work):
        if work["kind"] == "task":
            query = {"kind": "task", "team": work["team_id"], "task": work["task_id"]}
        elif work["kind"] == "message":
            query = {"kind": "message", "event_key": work["event_key"]}
        else:
            raise ValueError("Task or message session scope required")
        return "runtime/sessions?" + urlencode(query)

    def load(self, work):
        view = self.recovery.request("GET", self._path(work))
        if view["runtime"] != self.runtime:
            raise RecoveryConflict("Actual Worker Harness differs from this native adapter")
        return view

    @staticmethod
    def _matches_work(view, work):
        scope = view["work"]
        if scope["kind"] != work["kind"]:
            return False
        if work["kind"] == "task":
            return scope["team_id"] == work["team_id"] and scope["task_id"] == work["task_id"]
        event_key = matrix_event_key(scope["room_id"], scope["event_id"])
        return event_key == work["event_key"]

    def usable(self, view, native_ref, native_digest, *, now=None):
        """native_digest must come from reading the actual native file/row.

        A descriptor, read receipt or session ID alone cannot prove persistence.
        A reused native session still needs the current execution/tool admission.
        """
        cache = view.get("cache")
        return bool(view.get("compatible") and cache and native_ref and native_digest
                    and view.get("runtime") == cache.get("runtime") == self.runtime
                    and cache.get("native_ref") == native_ref and cache.get("native_digest") == native_digest
                    and cache.get("expires_at", 0) > (time.time() if now is None else now)
                    and all(cache.get(key) == view[key] for key in (
                        "checkpoint_version", "checkpoint_digest", "config_generation")))

    def remember(self, work, view, identity, native_ref, native_digest, expires_at):
        if (not self._matches_work(view, work) or view["runtime"] != self.runtime or identity["process_id"] != self.recovery.process
                or identity["instance_id"] != view["instance_id"] or identity["generation"] != view["generation"]):
            raise RecoveryConflict("Native persistence callback belongs to another execution")
        body = {key: view[key] for key in ("instance_id", "generation", "checkpoint_version",
                                         "checkpoint_digest", "config_generation")}
        body.update(process_id=self.recovery.process, native_ref=native_ref,
                    native_digest=native_digest, expires_at=expires_at)
        # Do not refresh and relabel old native bytes with a newer checkpoint.
        # A stale snapshot must fail at the Controller, then rebuild/reconcile.
        if view["etag"]:
            return self.recovery.request("PUT", self._path(work), body, view["etag"])
        return self.recovery.request("PUT", self._path(work), body, create=True)
