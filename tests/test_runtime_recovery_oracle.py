"""Independent effect ledger tests. Controller discovery is a fixture here."""
import copy
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import threading
import urllib.error
import urllib.request

from fixtures.runtime_recovery_oracle import Ledger, server


class IndependentLedgerTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "ledger.json"
        self.ledger = Ledger(self.path)
        self.task = {"task_id": "stable", "status": "in_progress", "execution": {
            "instance_id": "docker/first", "process_id": "first-process", "generation": 1,
            "state": "active", "checkpoint": {"version": 1, "completed_steps": [],
                "operation": {"id": "fixture:stable:one", "state": "started"}}}}
        self.snapshot = {"instance_id": "docker/first", "tasks": [{"team_id": "team", "task": self.task}]}

    def call(self, step="one", method="POST", process="first-process"):
        return self.ledger.effect(self.snapshot, "team", "stable", step, process, method)

    def test_restart_verifies_unknown_result_without_another_effect(self):
        first = self.call()
        self.ledger = Ledger(self.path)
        self.snapshot["instance_id"] = "docker/replacement"
        self.task["execution"].update(instance_id="docker/replacement", process_id="new-process", generation=2,
                                      state="needs_verification")
        self.task["execution"]["checkpoint"]["operation"]["state"] = "unknown"
        self.assertEqual(self.call(method="GET", process="new-process"), first)
        proof = self.ledger.proof("team", "stable")
        self.assertEqual(len(proof["effects"]), 1)
        self.assertEqual([row["method"] for row in proof["calls"]], ["POST", "GET"])

    def test_duplicate_execution_is_rejected_and_recorded(self):
        self.call()
        with self.assertRaisesRegex(ValueError, "already executed"):
            self.call()
        self.assertEqual(len(self.ledger.proof("team", "stable")["effects"]), 1)
        self.assertFalse(self.ledger.proof("team", "stable")["calls"][-1]["accepted"])

    def test_worker_uploaded_receipts_cannot_create_an_effect(self):
        self.task["execution"]["checkpoint"]["artifacts"] = [{"key": "forged/receipt.json", "sha256": "test"}]
        with self.assertRaisesRegex(ValueError, "no verified external result"):
            self.call(method="GET")
        self.assertEqual(self.ledger.proof("team", "stable")["effects"], [])

    def test_ownership_and_step_boundaries_fail_closed(self):
        original = copy.deepcopy(self.snapshot)
        mutations = [lambda: self.snapshot.update(instance_id="docker/other"),
                     lambda: self.task.update(status="cancelled"),
                     lambda: self.snapshot.update(tasks=[]),
                     lambda: self.task["execution"].update(process_id="other-process"),
                     lambda: self.task["execution"]["checkpoint"]["operation"].update(state="verified")]
        for mutate in mutations:
            self.snapshot = copy.deepcopy(original)
            self.task = self.snapshot["tasks"][0]["task"]
            mutate()
            with self.assertRaises(ValueError):
                self.call()
            self.assertEqual(self.ledger.proof("team", "stable")["effects"], [])

    def test_completed_prefix_must_match_independent_ledger(self):
        self.task["execution"]["checkpoint"].update(version=3, completed_steps=["one"],
            operation={"id": "fixture:stable:two", "state": "started"})
        with self.assertRaisesRegex(ValueError, "independent completed prefix"):
            self.call(step="two")
        self.assertEqual(json.loads(self.path.read_text())["effects"], [])

    def test_http_uses_worker_discovery_and_keeps_proof_private(self):
        service = server(("127.0.0.1", 0), "http://controller", self.ledger, "private-driver-key")
        thread = threading.Thread(target=lambda: service.serve_forever(poll_interval=0.01), daemon=True)
        thread.start()
        self.addCleanup(service.server_close)
        self.addCleanup(thread.join)
        self.addCleanup(service.shutdown)
        endpoint = "http://127.0.0.1:" + str(service.server_port)
        real_urlopen = urllib.request.urlopen
        forwarded = []

        def discovery(request, **kwargs):
            if request.full_url.startswith("http://controller"):
                forwarded.append(dict(request.header_items()))
                return contextlib.closing(io.BytesIO(json.dumps(self.snapshot).encode()))
            return real_urlopen(request, **kwargs)

        def request(path, token, method="GET"):
            return urllib.request.Request(endpoint + path, method=method,
                headers={"Authorization": "Bearer " + token, "X-AgentTeams-Process-ID": "first-process"})

        with patch("urllib.request.urlopen", side_effect=discovery):
            with self.assertRaises(urllib.error.HTTPError) as error:
                urllib.request.urlopen(request("/proof?team=team&task=stable", "worker-token"))
            self.assertEqual(error.exception.code, 403)
            with urllib.request.urlopen(request("/effect?team=team&task=stable&step=one", "worker-token", "POST")) as response:
                self.assertEqual(json.load(response)["checkpoint_version"], 1)
            with urllib.request.urlopen(request("/proof?team=team&task=stable", "private-driver-key")) as response:
                self.assertEqual(len(json.load(response)["effects"]), 1)
        self.assertEqual(forwarded, [{"Authorization": "Bearer worker-token", "X-agentteams-process-id": "first-process"}])


if __name__ == "__main__":
    unittest.main()
