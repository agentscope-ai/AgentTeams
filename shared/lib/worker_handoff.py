"""Quiesce a registered Linux Worker start at a Controller-verified boundary."""
import os
from pathlib import Path
import signal
import time

from task_recovery import Recovery, RecoveryConflict


def recovery_scanned():
    """Called by the adapter after Task discovery and its first Matrix recovery scan."""
    marker = os.getenv("AGENTTEAMS_SUPERVISOR_READY_FILE")
    if marker:
        Path(marker).write_text(os.environ["AGENTTEAMS_PROCESS_ID"])


def _processes():
    # Inspect one Worker PID namespace. Collect ancestry as well as the boot
    # fence: tools may start separate sessions and become reparented.
    records = {}
    for path in Path("/proc").glob("[0-9]*/stat"):
        try:
            # comm may contain spaces and parentheses.
            fields = path.read_text().rsplit(")", 1)[1].split()
            records[int(path.parent.name)] = (int(fields[1]), fields[0])
        except (FileNotFoundError, ProcessLookupError):
            continue
    return records


class NativeProcesses:
    def __init__(self, child_pid, process):
        self.child_pid = child_pid
        self.marker = b"AGENTTEAMS_PROCESS_ID=" + process.encode()
        self.held = {}

    def members(self):
        records = _processes()
        cgroup = Path(f"/proc/{self.child_pid}/cgroup").read_bytes()
        members = {self.child_pid}
        for pid, (_, state) in records.items():
            if pid == os.getpid() or state in {"Z", "X"}:
                continue
            root = pid
            while root in records and root not in (0, 1, os.getpid()) and root not in members:
                root = records[root][0]
            if root in members:
                members.add(pid)
                continue
            try:
                if (records[pid][0] in (1, os.getpid()) and Path(f"/proc/{pid}/cgroup").read_bytes() == cgroup
                        and self.marker in Path(f"/proc/{pid}/environ").read_bytes().split(b"\0")):
                    members.add(pid)
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                continue
        return {pid for pid in members if pid in records and records[pid][1] not in {"Z", "X"}}

    @staticmethod
    def is_stopped(pid):
        tasks = Path(f"/proc/{pid}/task").glob("*/stat")
        return all(t.read_bytes().rsplit(b")", 1)[1].split()[0] in {b"T", b"t", b"Z", b"X"} for t in tasks)

    def freeze(self):
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            members = self.members()
            if self.child_pid not in members:
                raise RecoveryConflict("Native Worker process exited before handoff acknowledgement")
            stopped = True
            for pid in members:
                try:
                    if pid not in self.held:
                        fd = os.pidfd_open(pid)
                        try:
                            if pid not in self.members():
                                continue
                            already_stopped = self.is_stopped(pid)
                            if not already_stopped:
                                signal.pidfd_send_signal(fd, signal.SIGSTOP)
                            self.held[pid] = (fd, already_stopped)
                        finally:
                            if pid not in self.held:
                                os.close(fd)
                    if not self.is_stopped(pid):
                        stopped = False
                except (FileNotFoundError, ProcessLookupError):
                    stopped = False
            if stopped and self.members() <= self.held.keys():
                return
            time.sleep(.02)
        raise RecoveryConflict("Native Worker threads are still running")

    def resume(self):
        for fd, already_stopped in self.held.values():
            try:
                if not already_stopped:
                    signal.pidfd_send_signal(fd, signal.SIGCONT)
            except ProcessLookupError:
                pass
            finally:
                os.close(fd)
        self.held.clear()


class SupervisorHandoff:
    def __init__(self, runtime, child, ready_path):
        self.recovery = Recovery()
        self.runtime, self.child, self.ready_path = runtime, child, ready_path
        self.native = NativeProcesses(child.pid, self.recovery.process)

    def check(self):
        if not self.ready_path.is_file() or self.ready_path.read_text() != self.recovery.process:
            return
        state, etag = self.recovery.request("GET", "runtime/handoff", response_etag=True)
        op = state.get("handoff")
        if not op or not etag:
            return
        phase = op["phase"]
        start = (op.get("target") if phase == "recovering" else op["source"]) or {}
        if start.get("process_id") != self.recovery.process or start.get("runtime") != self.runtime:
            return
        body = {"request_id": op["id"], "instance_id": start["instance_id"], "process_id": self.recovery.process}
        if phase == "recovering":
            self.recovery.request("POST", "runtime/handoff/recovered", body, etag)
        elif phase == "draining":
            endpoint = "runtime/handoff/checkpoint"
            # The source keeps working until every active execution reaches a
            # verifiable step boundary. The probe performs no state change.
            try:
                self.recovery.request("POST", endpoint + "?probe=true", body, etag)
            except RecoveryConflict as error:
                # After a lost acknowledgement, thaw only on a fresh, explicit
                # conflict for the same still-draining operation.
                if self.native.held and getattr(error.__cause__, "code", None) == 409:
                    fresh = self.recovery.request("GET", "runtime/handoff").get("handoff") or {}
                    if fresh.get("id") == op["id"] and fresh.get("phase") == "draining" and fresh.get("source") == start:
                        self.native.resume()
                raise
            if not self.native.held:
                try:
                    self.native.freeze()
                except Exception:
                    self.native.resume()
                    raise
            self.recovery.request("POST", endpoint, body, etag)

    def monitor(self):
        from runtime_observability import event
        previous = None
        while self.child.poll() is None:
            try:
                self.check()
                previous = None
            except Exception as error:
                # A read or ACK can be lost after the Controller committed it.
                # Preserve the frozen source until the authoritative phase is known.
                failure = type(error).__name__
                if failure != previous:
                    event("handoff.pending", error_type=failure)
                previous = failure
            time.sleep(2 if self.native.held else 5)
