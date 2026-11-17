"""Shared Controller-backed Matrix input persistence; no native session format.

Adapters persist decoded accepted inputs before advancing their native cursor.
This ledger records pending work, not proof of inference or Task completion.
"""
import hashlib
import json
import os
import subprocess
import urllib.error

from task_recovery import Recovery, RecoveryConflict


def matrix_event_key(room_id, event_id):
    return hashlib.sha256(json.dumps([room_id, event_id], ensure_ascii=False,
                                     separators=(",", ":")).encode()).hexdigest()


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

    def accept(self, room, event_id, source, intent, *, task_id=None):
        """Retain one policy-accepted input; the caller serializes native admission.

        Source is the decoded Matrix event, never the rendered model prompt.
        Per-event callbacks have no sync-batch cursor to commit.
        """
        snapshot = self.start()
        key = matrix_event_key(room, event_id)
        previous = self.input(key)
        input_ = {"room_id": room, "event_id": event_id, "intent": intent, "input": source}
        if previous:
            # Reassignment/mention policy changes cannot reclassify an input
            # already accepted. The server still compares immutable bytes.
            input_["intent"] = previous["intent"]
            if previous.get("work"):
                input_["work"] = previous["work"]
        elif task_id is not None:
            tasks = self.recovery.discover()
            matches = [entry for entry in tasks["tasks"]
                if entry["task"]["task_id"] == task_id and entry["task"].get("room_id") == room]
            if len(matches) != 1:
                raise RecoveryConflict("Task input has no unambiguous Worker/team/room identity")
            input_["work"] = {"kind": "task", "team_id": matches[0]["team_id"], "task_id": task_id}
        elif intent == "message":
            input_["work"] = {"kind": "message"}
        self.ingest(snapshot, None, [input_])
        return key

    def consumer(self, snapshot, *, account, format, device=""):
        """Restore only this native account/device/format's shared cursor."""
        start = snapshot["state"]["active_start"]
        if (snapshot["runtime"] != self.runtime or start["runtime"] != self.runtime
                or start["instance_id"] != snapshot["instance_id"]
                or start["process_id"] != self.recovery.process):
            raise RecoveryConflict("Matrix consumer belongs to another native start")
        identity = {"runtime": self.runtime, "account": account, "device": device, "format": format}
        for saved in snapshot["state"]["consumers"].values():
            if all(saved.get(key, "") == value for key, value in identity.items()):
                return {**identity, "token": saved["token"]}
        return {**identity, "token": ""}

    def sync(self, snapshot, consumer, next_batch, timelines):
        """Commit decoded, policy-classified timelines before native dispatch.

        Adapters supply inputs in server order, including retained context.
        No accepted-event hash or timestamp sorting substitutes for that order.
        A new native consumer reconnects shared room coverage through backfill;
        it never imports another SDK's token or discards its initial timeline.
        """
        current = self.consumer(snapshot, account=consumer["account"],
                                device=consumer.get("device", ""), format=consumer["format"])
        if consumer["runtime"] != self.runtime or consumer["token"] != current["token"]:
            raise RecoveryConflict("Sync response started from a different native cursor")
        if not isinstance(next_batch, str) or not next_batch:
            raise ValueError("A complete native sync response needs its next_batch token")
        inputs, rooms = [], {}
        for timeline in timelines:
            room = timeline["room_id"]
            batch = timeline["inputs"]
            if room in rooms or any(item["room_id"] != room for item in batch):
                raise ValueError("One ordered timeline per Matrix room is required")
            if not batch:
                if timeline.get("limited"):
                    raise RecoveryConflict("Limited timeline has no retained backfill anchor")
                continue
            previous = snapshot["state"]["rooms"].get(room, {})
            gaps = list(previous.get("gaps", []))
            overlap = any(item["event_id"] == previous.get("through") for item in batch)
            if not overlap and (timeline.get("limited") or (not current["token"] and previous.get("through"))):
                gap = timeline.get("prev_batch")
                if not gap:
                    raise RecoveryConflict("Unconnected Matrix timeline needs a backfill token")
                if gap not in gaps:
                    gaps.append(gap)
            inputs.extend(batch)
            rooms[room] = {"through": batch[-1]["event_id"], "gaps": gaps}
        return self.ingest(snapshot, {**current, "from_token": current["token"], "token": next_batch}, inputs, rooms)

    def pending(self, snapshot):
        # The server resolves the Worker prefix from authenticated identity.
        # Native adapters cannot supply another Worker's storage path.
        return [self.recovery.request("GET", "runtime/matrix/events/" + key)
                for key in snapshot["state"]["pending"]]

    def input(self, key):
        try:
            return self.recovery.request("GET", "runtime/matrix/events/" + key)
        except RecoveryConflict as error:
            if isinstance(error.__cause__, urllib.error.HTTPError) and error.__cause__.code == 404:
                return None
            raise  # Unavailable/forbidden/changed state is not a missing input.

    def backfill(self, snapshot, page):
        start = snapshot["state"]["active_start"]
        if start["process_id"] != self.recovery.process or start["runtime"] != self.runtime:
            raise RecoveryConflict("History page belongs to another native start")
        saved = self.recovery.request("POST", "runtime/matrix", {
            "action": "backfill", "instance_id": start["instance_id"],
            "process_id": self.recovery.process, "backfill": page,
        }, snapshot["etag"])
        return {**snapshot, **saved}

    def native_input(self, key):
        return self.recovery.request("GET", "runtime/matrix/events/" + key + "/native")

    def native_committed(self, key, snapshot, native_ref, native_digest):
        input_ = snapshot["input"]
        if snapshot["runtime"] != self.runtime or matrix_event_key(input_["room_id"], input_["event_id"]) != key:
            raise RecoveryConflict("Native persistence facts belong to another input or Harness")
        return self.recovery.request("POST", "runtime/matrix/events/" + key + "/native", {
            "instance_id": snapshot["instance_id"], "process_id": self.recovery.process,
            "native_ref": native_ref, "native_digest": native_digest,
        }, snapshot["etag"])

    def native_current(self, snapshot, native_ref, native_digest):
        # Verify a persisted input row, not just a session ID or read ACK.
        receipt = snapshot["input"].get("native_commit") or {}
        start = receipt.get("start") or {}
        return bool(snapshot.get("current") and native_ref and native_digest
                    and snapshot["runtime"] == receipt.get("runtime") == start.get("runtime") == self.runtime
                    and start.get("instance_id") == snapshot["instance_id"]
                    and start.get("process_id") == self.recovery.process
                    and receipt.get("native_ref") == native_ref and receipt.get("native_digest") == native_digest)

    @staticmethod
    def _message_path(key):
        return "runtime/matrix/events/" + key + "/execution"

    def message(self, key):
        return self.recovery.request("GET", self._message_path(key))

    def _message_update(self, key, snapshot, action, checkpoint=None):
        if snapshot["runtime"] != self.runtime:
            raise RecoveryConflict("Actual Worker Harness differs from this native adapter")
        return self.recovery.request("POST", self._message_path(key), {
            "action": action, "instance_id": snapshot["instance_id"],
            "process_id": self.recovery.process,
            "generation": snapshot["execution"].get("generation", 0), "checkpoint": checkpoint,
        }, snapshot["etag"])

    def message_claim(self, key):
        claimed = self._message_update(key, self.message(key), "claim")
        return self._message_view(key, claimed)

    @staticmethod
    def _message_view(key, entry):
        execution = entry["execution"]
        checkpoint = execution.get("checkpoint")
        view = {**entry, "identity": {"event_key": key, "generation": execution["generation"],
            "instance_id": execution["instance_id"], "process_id": execution["process_id"]},
            "checkpoint_digest": Recovery.digest(checkpoint)}
        if entry["recovery"] == "resume":
            view["resume_text"] = (
                "Continue this existing Matrix work from the shared checkpoint. "
                "Do not repeat completed steps. Use verified artifacts and necessary room history. "
                "Before a side effect, persist its operation identity; unknown operations require "
                "verification before further execution. Save result bytes before publishing their reference.\n"
                + json.dumps({"room_id": entry["input"]["room_id"], "event_id": entry["input"]["event_id"],
                    "input": entry["input"].get("input"), "generation": execution["generation"],
                    "checkpoint": checkpoint}, ensure_ascii=False))
        return view

    def _message_owned(self, identity):
        entry = self.message(identity["event_key"])
        execution = entry["execution"]
        if (entry["runtime"] != self.runtime or entry["instance_id"] != execution.get("instance_id")
                or execution.get("runtime") != self.runtime
                or execution.get("process_id") != self.recovery.process
                or identity["process_id"] != self.recovery.process
                or execution.get("instance_id") != identity["instance_id"]
                or execution.get("generation") != identity["generation"]):
            raise RecoveryConflict("Message execution was replaced")
        return entry

    def message_current(self, identity):
        entry = self._message_owned(identity)
        if entry["execution"]["state"] != "active" or entry["recovery"] != "resume":
            raise RecoveryConflict("Message is not admitted for normal inference or tools")
        return self._message_view(identity["event_key"], entry)

    def message_checkpoint(self, identity, checkpoint, *, release=False):
        entry = self._message_owned(identity)
        return self._message_view(identity["event_key"], self._message_update(identity["event_key"], entry, "release" if release else "checkpoint", checkpoint))

    def message_save_reply(self, identity, text):
        entry = self._message_owned(identity)
        if entry["recovery"] == "deliver":
            # Claim verifies stored bytes; never replace them with late native text.
            return self.message_claim(identity["event_key"])
        if entry["recovery"] != "resume" or entry["execution"]["state"] != "active" or not text:
            raise RecoveryConflict("A verified active message and a result are required")
        data = text.encode()
        digest = hashlib.sha256(data).hexdigest()
        key = entry["input"]["member_prefix"] + "/runtime/matrix/results/" + digest + ".txt"
        subprocess.run(["mc", "pipe", os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/" + key],
                       input=data, capture_output=True, check=True, timeout=30)
        cp = dict(entry["execution"].get("checkpoint") or {})
        cp.update(version=cp.get("version", 0)+1, result={"key": key, "sha256": digest, "size": len(data)},
                  delivery="pending", reply_txn="matrix-reply:" + Recovery.digest([
                      entry["input"]["member_prefix"], entry["input"]["room_id"],
                      entry["input"]["event_id"], cp.get("version", 0)+1, digest]))
        cp.pop("reply_event", None)
        return self._message_view(identity["event_key"], self._message_update(identity["event_key"], entry, "checkpoint", cp))

    def message_delivered(self, identity, event_id):
        entry = self._message_owned(identity)
        cp = dict(entry["execution"].get("checkpoint") or {})
        if cp.get("delivery") == "delivered":
            return self._message_view(identity["event_key"], entry)
        if entry["recovery"] != "deliver" or entry["execution"]["state"] == "cancelled":
            raise RecoveryConflict("Message has no pending saved reply")
        cp.update(version=cp["version"]+1, delivery="delivered", reply_event=event_id)
        return self._message_view(identity["event_key"], self._message_update(identity["event_key"], entry, "checkpoint", cp))

    def message_cancel(self, identity):
        return self._message_view(identity["event_key"], self._message_update(identity["event_key"], self._message_owned(identity), "cancel"))
