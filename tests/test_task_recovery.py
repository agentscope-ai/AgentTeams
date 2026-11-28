"""Contract tests, not native Harness or real-model continuation evidence."""
import copy
import io
import os
from pathlib import Path
import sys
import unittest
import urllib.error
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict, MetadataConflict, RecoveryUnavailable, enabled


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, {"AGENTTEAMS_PROCESS_ID": "actual-start"})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.task = {"task_id": "stable-task", "status": "in_progress", "execution": {
            "instance_id": "container/started-at", "process_id": "actual-start", "generation": 2,
            "checkpoint": {"version": 3, "completed_steps": ["one"], "remaining_steps": ["two", "three"]}}}
        self.snapshot = {"instance_id": "container/started-at", "tasks": [
            {"task": self.task, "team_id": "team", "etag": "read-version"}]}
        self.request = Mock(return_value=self.snapshot)
        self.recovery = Recovery(request=self.request)

    def test_completed_handoff_cannot_start_another_tool_but_can_settle_owned_return(self):
        self.task['status'] = 'completed'
        self.task['execution']['state'] = 'needs_verification'
        identity = {'task_id': 'stable-task', 'team_id': 'team', 'instance_id': 'container/started-at',
                    'process_id': 'actual-start', 'generation': 2}
        with self.assertRaises(RecoveryConflict):
            self.recovery.tools_current(identity)
        self.assertEqual(self.recovery.tools_current(identity, verification=True)['status'], 'completed')

    def test_managed_standalone_boot_still_discovers_later_team_assignments(self):
        with patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL": "http://controller", "AGENTTEAMS_AGENT_ROLE": "standalone"}):
            self.assertTrue(enabled())

    def test_scrubbed_token_file_environment_uses_the_scoped_worker_mount(self):
        with patch.dict(os.environ, {"AGENTTEAMS_PROCESS_ID": "actual-start", "AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid"}, clear=True), \
                patch("task_recovery.Path.read_text", autospec=True, return_value="scoped-worker-token\n") as read, \
                patch("task_recovery.urllib.request.urlopen", return_value=io.BytesIO(b'{"tasks":[]}')) as request:
            self.assertEqual(Recovery().discover(), {"tasks": []})
        read.assert_called_once_with(Path("/var/run/secrets/agentteams/token"))
        headers = dict((key.lower(), value) for key, value in request.call_args.args[0].header_items())
        self.assertEqual(headers["authorization"], "Bearer scoped-worker-token")
        self.assertEqual(headers["x-agentteams-process-id"], "actual-start")

    def test_temporary_controller_failure_is_a_controlled_tool_error(self):
        failures = [urllib.error.HTTPError("http://controller", 503, "Unavailable", {}, None),
            urllib.error.URLError("connection reset"), TimeoutError()]
        for failure in failures:
            with self.subTest(failure=type(failure).__name__), patch.dict(os.environ, {
                    "AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid", "AGENTTEAMS_AUTH_TOKEN_FILE": "/unused"}), \
                    patch("task_recovery.Path.read_text", return_value="private-test-token"), \
                    patch("task_recovery.urllib.request.urlopen", side_effect=failure):
                with self.assertRaises(RecoveryUnavailable):
                    Recovery()._request("GET", "tasks/recoverable")

    def test_read_only_recovery_retries_a_transient_timeout(self):
        with patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid"}), \
                patch("task_recovery.Path.read_text", return_value="private-test-token"), \
                patch("task_recovery.time.sleep"), \
                patch("task_recovery.urllib.request.urlopen", side_effect=[TimeoutError(), io.BytesIO(b'{"tasks":[]}')]) as request:
            self.assertEqual(Recovery()._request("GET", "tasks/recoverable"), {"tasks": []})
            self.assertEqual(request.call_count, 2)

    def test_write_recovery_timeout_is_not_replayed(self):
        with patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid"}), \
                patch("task_recovery.Path.read_text", return_value="private-test-token"), \
                patch("task_recovery.urllib.request.urlopen", side_effect=TimeoutError()) as request:
            with self.assertRaises(RecoveryUnavailable):
                Recovery()._request("POST", "tasks/task-one/execution", {"action": "checkpoint"})
            self.assertEqual(request.call_count, 1)

    def test_unavailable_admission_keeps_a_receipt_before_any_tool_can_be_dispatched(self):
        import tempfile
        from native_tool_returns import load
        from task_operation import begin
        from worker_matrix_state import WorkerMatrix
        calls = [{"id": "call-one", "name": "fixture", "arguments": {}}]
        identity = {"task_id": "stable-task", "team_id": "team", **{
            key: self.task['execution'][key] for key in ('process_id', 'instance_id', 'generation')}}
        for kind in ('task', 'message'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory, \
                    patch.dict(os.environ, {'TEAMHARNESS_RUNTIME_CONFIG': directory + '/runtime.yaml',
                                            'AGENTTEAMS_STORAGE_PREFIX': ''}):
                current = copy.deepcopy(self.task)
                owned = dict(identity)
                if kind == 'task':
                    owner = self.recovery
                    method, update = owner.begin_tools, 'checkpoint'
                    read = patch.object(owner, 'tools_current', return_value=current)
                    anchor = ['team', 'stable-task']
                else:
                    owner = WorkerMatrix('hermes', recovery=self.recovery)
                    owned['event_key'] = 'event-one'
                    method, update = owner.message_begin_tools, '_message_update'
                    read = patch.object(owner, 'message_current', return_value=current)
                    anchor = 'event-one'
                operation, _ = begin(current['execution']['checkpoint'], anchor, calls)
                with read, patch.object(owner, update, side_effect=RecoveryUnavailable('admission response and readback unavailable')):
                    with self.assertRaises(RecoveryUnavailable):
                        method(owned, calls)
                receipt = load(owned, operation)
                self.assertEqual(receipt['results'][0]['id'], 'call-one')
                self.assertTrue(receipt['results'][0]['isError'])
                self.assertEqual(receipt['results'][0]['status'], 'error')
                self.assertIn('not dispatched', receipt['results'][0]['content'][0]['text'])
                self.assertNotIn('effect', receipt['results'][0])

    def test_completed_call_is_not_relabelled_as_an_admission_failure(self):
        from task_operation import begin
        from worker_matrix_state import WorkerMatrix
        calls = [{"id": "call-one", "name": "fixture", "arguments": {}}]
        for kind in ('task', 'message'):
            with self.subTest(kind=kind):
                current = copy.deepcopy(self.task)
                identity = {"task_id": "stable-task", "team_id": "team", "event_key": "event-one"}
                anchor = ['team', 'stable-task'] if kind == 'task' else 'event-one'
                operation, _ = begin(current['execution']['checkpoint'], anchor, calls)
                current['execution']['checkpoint']['completed_steps'].append(operation)
                owner = self.recovery if kind == 'task' else WorkerMatrix('hermes', recovery=self.recovery)
                read = 'tools_current' if kind == 'task' else 'message_current'
                method = owner.begin_tools if kind == 'task' else owner.message_begin_tools
                with patch.object(owner, read, return_value=current), \
                        patch('native_tool_returns.not_dispatched') as record:
                    with self.assertRaises(RecoveryConflict):
                        method(identity, calls)
                    record.assert_not_called()

    def test_failed_inference_can_release_verified_progress_for_recovery(self):
        self.task['execution']['state'] = 'active'
        identity = {'task_id': 'stable-task', 'team_id': 'team', **{
            key: self.task['execution'][key] for key in ('process_id', 'instance_id', 'generation')}}
        self.assertTrue(self.recovery.release_failed_turn(identity))
        payload = self.request.call_args.args[2]
        self.assertEqual(payload['action'], 'release')
        self.assertEqual(payload['checkpoint']['completed_steps'], ['one'])
        self.assertEqual(payload['checkpoint']['version'], 4)

    def test_failed_inference_does_not_release_uncertain_tools_or_saved_replies(self):
        identity = {'task_id': 'stable-task', 'team_id': 'team', **{
            key: self.task['execution'][key] for key in ('process_id', 'instance_id', 'generation')}}
        self.task['execution']['state'] = 'active'
        for operation in [{'id': 'write', 'state': 'started'}, {'id': 'write', 'state': 'unknown'}]:
            self.task['execution']['checkpoint']['operation'] = operation
            self.assertFalse(self.recovery.release_failed_turn(identity))
        self.task['execution']['checkpoint'].pop('operation')
        self.task['execution']['checkpoint']['result'] = {'key': 'saved-result'}
        self.assertFalse(self.recovery.release_failed_turn(identity))
        self.assertTrue(all(call.args[0] == 'GET' for call in self.request.call_args_list))

    def test_session_cache_requires_harness_version_and_expiry(self):
        checkpoint = self.task["execution"]["checkpoint"]
        session = {"runtime": "codex", "checkpoint_digest": self.recovery.digest(checkpoint), "expires_at": 100}
        self.assertTrue(self.recovery.session_usable(session, self.task, "codex", now=99))
        self.assertFalse(self.recovery.session_usable(session, self.task, "hermes", now=99))
        self.assertFalse(self.recovery.session_usable(session, self.task, "codex", now=100))
        self.task["execution"]["checkpoint"]["version"] = 4
        self.assertFalse(self.recovery.session_usable(session, self.task, "codex", now=99))
        self.assertFalse(self.recovery.session_usable(None, self.task, "codex", now=99))

    def test_claim_uses_team_and_stable_task_not_project(self):
        self.request.return_value = {'task': self.task}
        self.recovery.claim(self.snapshot, self.snapshot["tasks"][0])
        args = self.request.call_args.args
        self.assertEqual(args[:2], ("POST", "tasks/stable-task/execution?team=team"))
        self.assertEqual(args[2]["process_id"], "actual-start")
        self.assertEqual(args[3], "read-version")

    def test_checkpoint_rereads_only_metadata_cas_conflicts(self):
        identity = {**self.task["execution"], "task_id": "stable-task", "team_id": "team"}
        digest = self.recovery.digest(self.task["execution"]["checkpoint"])
        posts = []
        def request(method, path, body=None, etag=None):
            if method == "GET":
                return copy.deepcopy(self.snapshot)
            posts.append((body, etag))
            if len(posts) == 1:
                self.snapshot["tasks"][0]["etag"] = "amended-version"
                self.task["spec"] = "Leader supplement, same execution"
                raise MetadataConflict("metadata race")
            return {"checkpoint": body["checkpoint"]}
        self.recovery.request = request
        result = self.recovery.checkpoint("stable-task", "team", {"version": 4},
            expected_identity=identity, expected_digest=digest)
        self.assertEqual(result["checkpoint"], {"version": 4})
        self.assertEqual([etag for _, etag in posts], ["read-version", "amended-version"])
        self.assertEqual(posts[0][0], posts[1][0])

    def test_checkpoint_lost_response_reads_back_exact_commit_without_repeating_write(self):
        for change in ("none", "checkpoint", "generation", "process", "missing"):
            with self.subTest(change=change):
                snapshot = copy.deepcopy(self.snapshot)
                expected = {"version": 4, "operation": {"id": "native-call", "state": "started"}}
                posts = []
                def request(method, path, body=None, etag=None):
                    if method == "GET":
                        return copy.deepcopy(snapshot)
                    posts.append(body)
                    execution = snapshot["tasks"][0]["task"]["execution"]
                    execution["checkpoint"] = copy.deepcopy(body["checkpoint"])
                    if change == "checkpoint":
                        execution["checkpoint"]["version"] += 1
                    if change == "generation":
                        execution["generation"] += 1
                    if change == "process":
                        execution["process_id"] = "successor"
                    if change == "missing":
                        snapshot["tasks"] = []
                    raise RecoveryUnavailable("response lost after commit")
                recovery = Recovery(request=request)
                if change == "none":
                    result = recovery.checkpoint("stable-task", "team", expected)
                    self.assertEqual(result["execution"]["checkpoint"], expected)
                    self.assertTrue(result["idempotent_hit"])
                else:
                    with self.assertRaises(RecoveryUnavailable):
                        recovery.checkpoint("stable-task", "team", expected)
                self.assertEqual(len(posts), 1, "read-back must not replay the write or native tool")

    def test_message_checkpoint_lost_response_uses_same_exact_readback_rule(self):
        from worker_matrix_state import WorkerMatrix
        for change in ("none", "checkpoint", "generation", "process", "runtime", "instance"):
            with self.subTest(change=change):
                entry = {"runtime": "hermes", "instance_id": "container/started-at", "etag": "old",
                         "execution": copy.deepcopy(self.task["execution"])}
                initial = copy.deepcopy(entry)
                expected = {"version": 4, "operation": {"id": "native-call", "state": "started"}}
                posts = []
                def request(method, path, body=None, etag=None):
                    if method == "GET":
                        return copy.deepcopy(entry)
                    posts.append(body)
                    entry["execution"]["checkpoint"] = copy.deepcopy(expected)
                    if change == "checkpoint":
                        entry["execution"]["checkpoint"]["version"] += 1
                    if change == "generation":
                        entry["execution"]["generation"] += 1
                    if change == "process":
                        entry["execution"]["process_id"] = "successor"
                    if change == "runtime":
                        entry["runtime"] = "codex"
                    if change == "instance":
                        entry["instance_id"] = "replacement"
                    raise RecoveryUnavailable("response lost after commit")
                matrix = WorkerMatrix("hermes", recovery=Recovery(request=request))
                if change == "none":
                    result = matrix._message_update("event", initial, "checkpoint", expected)
                    self.assertEqual(result["execution"]["checkpoint"], expected)
                else:
                    with self.assertRaises(RecoveryUnavailable):
                        matrix._message_update("event", initial, "checkpoint", expected)
                self.assertEqual(len(posts), 1)

    def test_commit_readback_ignores_only_server_owned_shape(self):
        proposed = {"version": 4, "operation": {"id": "one", "state": "started"}}
        committed = {**copy.deepcopy(proposed), "saved_at": "server time", "completed_steps": None,
                     "remaining_steps": None, "artifacts": None}
        self.assertTrue(Recovery.checkpoint_committed(committed, proposed))
        for key, value in (("version", 5), ("operation", {"id": "other", "state": "started"}),
                           ("completed_steps", ["unexpected"]), ("artifacts", [{"key": "other"}]),
                           ("result", {"key": "unexpected"})):
            with self.subTest(key=key):
                self.assertFalse(Recovery.checkpoint_committed({**committed, key: value}, proposed))

    def test_real_http_lost_checkpoint_response_continues_without_replaying_tool(self):
        import http.server
        import socket
        import threading
        import json
        snapshot = copy.deepcopy(self.snapshot)
        snapshot["tasks"][0]["task"]["execution"]["state"] = "active"
        posts = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                body = json.dumps(snapshot).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                posts.append(body)
                committed = body["checkpoint"]
                committed["saved_at"] = "2026-11-28T06:00:00Z"
                for field in ("completed_steps", "remaining_steps", "artifacts"):
                    committed.setdefault(field, None)
                snapshot["tasks"][0]["task"]["execution"]["checkpoint"] = committed
                self.connection.shutdown(socket.SHUT_RDWR)
                self.connection.close()
        with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL":
                        "http://127.0.0.1:" + str(server.server_port)}), \
                        patch("task_recovery.Path.read_text", return_value="fixture-token"):
                    recovery = Recovery()
                    identity = {**self.task["execution"], "task_id": "stable-task", "team_id": "team"}
                    operation = recovery.begin_tools(identity, [{"id": "call-one", "name": "fixture", "arguments": {}}])
                    self.assertEqual(operation, snapshot["tasks"][0]["task"]["execution"]["checkpoint"]["operation"]["id"])
                    self.assertEqual(len(posts), 1)
            finally:
                server.shutdown()
                thread.join()

    def test_metadata_retry_preserves_execution_fences_and_is_bounded(self):
        for change in ("checkpoint", "generation", "process", "repeat", "unavailable"):
            with self.subTest(change=change):
                snapshot = copy.deepcopy(self.snapshot)
                posts = []
                def request(method, path, body=None, etag=None):
                    if method == "GET":
                        return copy.deepcopy(snapshot)
                    posts.append(body)
                    execution = snapshot["tasks"][0]["task"]["execution"]
                    if change == "checkpoint":
                        execution["checkpoint"]["version"] += 1
                    if change == "generation":
                        execution["generation"] += 1
                    if change == "process":
                        execution["process_id"] = "successor"
                    if change == "unavailable":
                        raise RecoveryUnavailable("write outcome unknown")
                    raise MetadataConflict("metadata race")
                recovery = Recovery(request=request)
                with self.assertRaises(RecoveryUnavailable if change == "unavailable" else RecoveryConflict):
                    recovery.checkpoint("stable-task", "team", {"version": 4})
                self.assertEqual(len(posts), 2 if change == "repeat" else 1)

    def test_only_structured_metadata_conflicts_are_retryable(self):
        cases = [(409, b'{"code":"conflict"}', MetadataConflict),
                 (412, b'{"code":"conflict"}', MetadataConflict),
                 (409, b'{"error":"ownership changed"}', RecoveryConflict),
                 (403, b'{"code":"conflict"}', RecoveryConflict)]
        for status, body, expected in cases:
            with self.subTest(status=status, body=body), patch.dict(os.environ, {
                    "AGENTTEAMS_CONTROLLER_URL": "http://controller"}), \
                    patch("task_recovery.Path.read_text", return_value="worker-token"), \
                    patch("task_recovery.urllib.request.urlopen", side_effect=urllib.error.HTTPError(
                        "http://controller", status, "Conflict", {}, io.BytesIO(body))):
                with self.assertRaises(expected) as error:
                    Recovery()._request("POST", "tasks/task/execution", {})
                self.assertIs(type(error.exception), expected)

    def test_stale_process_cannot_commit_to_new_container(self):
        self.snapshot["instance_id"] = "replacement/started-at"
        with self.assertRaises(RecoveryConflict):
            self.recovery.checkpoint("stable-task", "team", {"version": 4})
        self.assertEqual(self.request.call_count, 1)

    def test_cancelled_or_reassigned_task_cannot_commit(self):
        self.snapshot["tasks"] = []
        with self.assertRaises(RecoveryConflict):
            self.recovery.checkpoint("stable-task", "team", {"version": 4})
        self.assertEqual(self.request.call_count, 1)

    def test_reply_upload_precedes_checkpoint_and_retains_step_progress(self):
        calls = []
        def request(method, path, *args):
            if method == "GET":
                return self.snapshot
            calls.append("checkpoint")
            return args[0]
        self.recovery.request = request
        with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket"}), patch("task_recovery.subprocess.run", side_effect=lambda *a, **k: calls.append("upload")) as upload:
            result = self.recovery.save_reply("stable-task", "team", "Saved answer")
        self.assertEqual(calls, ["upload", "checkpoint"])
        cp = result["checkpoint"]
        self.assertEqual(cp["completed_steps"], ["one"])
        self.assertEqual(cp["version"], 4)
        self.assertEqual(cp["delivery"], "pending")
        self.assertTrue(cp["reply_txn"].startswith("task-reply:"))
        self.assertEqual(upload.call_args.kwargs["input"], b"Saved answer")

    def test_delivery_only_reuses_saved_transaction_and_result(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "saved.txt", "sha256": "a"*64, "size": 12}, delivery="pending", reply_txn="stable-reply")
        original = copy.deepcopy(cp)
        self.recovery.delivered("stable-task", "team", "$sent")
        sent = self.request.call_args.args[2]["checkpoint"]
        self.assertEqual(sent["result"], original["result"])
        self.assertEqual(sent["reply_txn"], "stable-reply")
        self.assertEqual(sent["delivery"], "delivered")
        self.assertEqual(sent["reply_event"], "$sent")

    def test_delivered_progress_allows_next_reply_but_pending_and_submitted_do_not(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "progress.txt"}, delivery="delivered", reply_txn="progress", reply_event="$progress")
        with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket"}), patch("task_recovery.subprocess.run"):
            self.recovery.save_reply("stable-task", "team", "Step two done")
        saved = self.request.call_args.args[2]["checkpoint"]
        self.assertEqual(saved["remaining_steps"], ["two", "three"])
        self.assertNotIn("reply_event", saved)
        self.assertNotEqual(saved["reply_txn"], "progress")
        for status, delivery in (("in_progress", "pending"), ("submitted", "delivered")):
            self.task["status"] = status
            cp["delivery"] = delivery
            self.request.reset_mock()
            with patch("task_recovery.subprocess.run") as upload, self.assertRaises(RecoveryConflict):
                self.recovery.save_reply("stable-task", "team", "Do not replace")
            upload.assert_not_called()

    def test_interrupted_turn_leaves_saved_progress_available_to_the_next_start(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "progress.txt"}, delivery="delivered", reply_txn="progress", reply_event="$progress")
        before = copy.deepcopy(self.task)
        self.recovery.interrupted("stable-task", "team")
        self.assertEqual(self.task, before)
        self.request.assert_not_called()

    def test_native_callback_reclaims_only_its_process_and_delivers_saved_submission(self):
        self.task.update(status="submitted")
        self.task["execution"]["state"] = "released"
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Persisted report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as upload:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Local native final answer")
            self.assertEqual(saved, claimed)
            self.assertEqual(body, "Persisted report")
            claim.assert_called_once()
            upload.assert_not_called()
        self.snapshot["instance_id"] = "replacement-start"
        with patch.object(self.recovery, "claim") as claim, self.assertRaises(RecoveryConflict):
            self.recovery.prepare_reply("stable-task", "team", "Stale answer")
        claim.assert_not_called()

    def test_interruption_after_durable_submission_keeps_delivery_recoverable(self):
        self.task.update(status="submitted", result_path="shared/tasks/stable-task/result.md")
        before = copy.deepcopy(self.task)
        self.recovery.interrupted("stable-task", "team")
        self.assertEqual(self.task, before)
        self.request.assert_not_called()

    def test_active_submitted_callback_saves_its_answer_before_delivery_claim(self):
        self.task.update(status="submitted")
        self.task["execution"]["state"] = "active"
        with patch.object(self.recovery, "claim", side_effect=RecoveryConflict("no submitted report")) as claim, patch.object(self.recovery, "save_reply", return_value={"saved": True}) as save:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Native final answer")
        self.assertEqual((saved, body), ({"saved": True}, "Native final answer"))
        save.assert_called_once_with("stable-task", "team", "Native final answer")
        claim.assert_not_called()

    def test_active_submitted_callback_prefers_authoritative_report_over_native_text(self):
        self.task.update(status="submitted", result_path="shared/tasks/stable-task/result.md")
        self.task["execution"]["state"] = "active"
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Authoritative report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as save:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Stale native summary")
        self.assertEqual((saved, body), (claimed, "Authoritative report"))
        claim.assert_called_once()
        save.assert_not_called()

    def test_completed_callback_recovers_the_accepted_report(self):
        self.task.update(status="completed", result_path="shared/tasks/stable-task/result.md")
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Accepted report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as save:
            result = self.recovery.prepare_reply("stable-task", "team", "Stale native summary")
        self.assertEqual(result, (claimed, "Accepted report"))
        claim.assert_called_once()
        save.assert_not_called()



    def test_managed_resume_uses_common_protocol_without_changing_legacy_guidance(self):
        managed = {**self.task, 'task_plan_id': 'current-plan'}
        self.assertIn('common task protocol', Recovery.resume_text(managed))
        self.assertNotIn('taskflow.checkpoint_task', Recovery.resume_text(managed))
        self.assertIn('taskflow.checkpoint_task', Recovery.resume_text(self.task))

    def test_managed_resume_keeps_business_progress_without_repeating_control_snapshots(self):
        import json
        task = copy.deepcopy(self.task)
        task.update(task_plan_id='plan', team_id='team', spec='repeated whole requirement',
                    history=[{'summary': 'old whole result'}])
        checkpoint = task['execution']['checkpoint']
        checkpoint['completed_steps'].append('tools:internal-verified-call')
        checkpoint.update(operation={'state': 'verified', 'result': 'internal tool snapshot'},
                          artifacts={'report': {'content': 'whole report snapshot'}},
                          result='saved final result', reply_txn='internal delivery transaction',
                          delivery='delivered')
        before = copy.deepcopy(task)
        text = Recovery.resume_text(task)
        payload = json.loads(text.split('\n')[-1])
        self.assertEqual(payload, {'task_id': 'stable-task', 'team_id': 'team', 'status': 'in_progress',
            'progress': {'completed_steps': ['one'], 'remaining_steps': ['two', 'three']}})
        self.assertEqual(task, before, 'model projection must preserve authoritative recovery evidence')
        for duplicate in ['internal tool snapshot', 'whole report snapshot', 'saved final result',
                          'internal delivery transaction', 'repeated whole requirement', 'old whole result']:
            self.assertNotIn(duplicate, text)

    def test_managed_resume_accepts_controller_null_step_lists(self):
        import json
        task = copy.deepcopy(self.task)
        task['task_plan_id'] = 'plan'
        for completed, remaining in ((None, None), (['one'], None), (None, ['two'])):
            with self.subTest(completed=completed, remaining=remaining):
                task['execution']['checkpoint'].update(completed_steps=completed, remaining_steps=remaining)
                before = copy.deepcopy(task)
                payload = json.loads(Recovery.resume_text(task).split('\n')[-1])
                self.assertEqual(payload.get('progress', {}), {
                    key: value for key, value in {'completed_steps': completed, 'remaining_steps': remaining}.items()
                    if value})
                self.assertEqual(task, before)

    def test_waiting_task_is_not_automatically_resumed_but_current_turn_can_checkpoint(self):
        self.task['session_state'] = 'waiting_user'
        self.assertEqual(self.recovery.discover()['tasks'], [])
        self.assertEqual(self.recovery.current('stable-task', 'team'), self.task)
        self.recovery.checkpoint('stable-task', 'team', {'version': 4})
        self.assertEqual(self.request.call_args.args[0], 'POST')

    def test_resume_carries_the_durable_human_decision_in_both_task_protocols(self):
        import json
        decision = {'confirmation_id': 'attention:stable-task:one', 'question': 'Which region?',
                    'options': [{'id': 'east', 'label': 'East'}], 'resolved': True,
                    'answer': {'event_id': '$answer', 'sender': '@human:test',
                               'responses': [{'id': 'decision', 'selected': ['east']}]}}
        self.task['attention'] = [decision, {'question': 'Unanswered'}]
        for managed in (False, True):
            task = {**self.task, **({'task_plan_id': 'plan'} if managed else {})}
            payload = json.loads(Recovery.resume_text(task).split('\n')[-1])
            self.assertEqual(payload['human_decisions'], [decision])


if __name__ == "__main__":
    unittest.main()
