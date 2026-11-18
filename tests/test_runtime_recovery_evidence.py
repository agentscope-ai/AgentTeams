"""Reject the forged step-boundary pattern observed in real case 17."""
import unittest
from unittest.mock import patch

with patch.dict("os.environ", {"RECOVERY_E2E_FROM": "codex", "RECOVERY_E2E_TO": "hermes", "RECOVERY_E2E_MODE": "replace", "RECOVERY_E2E_CASE": "01"}):
    from runtime_recovery_e2e import assert_effect_authorization, assert_step_boundaries


class ReceiptBoundaryTests(unittest.TestCase):
    def test_independent_proof_requires_single_effect_after_same_start_registration(self):
        effect = {"team_id": "team", "task_id": "T", "step": "two", "instance_id": "target",
                  "process_id": "process", "generation": 2, "checkpoint_version": 3, "executed_at_ns": 30}
        register = {**effect, "method": "REGISTER", "at_ns": 10, "accepted": True}
        post = {**effect, "method": "POST", "at_ns": 20, "accepted": True}
        assert_effect_authorization({"calls": [register, post], "intents": [effect]}, effect)
        for calls, intents in [
            ([post], [effect]), ([register, post, post], [effect]),
            ([{**register, "at_ns": 25}, post], [effect]),
            ([register, post], [{**effect, "generation": 1}]),
        ]:
            with self.subTest(calls=calls, intents=intents), self.assertRaises(AssertionError):
                assert_effect_authorization({"calls": calls, "intents": intents}, effect)

    def test_registered_start_and_verified_boundary_before_the_next_step(self):
        assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 7]], 4)
        assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 7]], 2)

    def test_case17_manual_receipts_without_a_step_two_boundary_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "step boundary"):
            assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 6]], 4)


class ImageSelectionTests(unittest.TestCase):
    def test_explicit_current_images_allow_the_qwenpaw_lane(self):
        import json
        import runpy
        from pathlib import Path
        with patch.dict("os.environ", {
            "RECOVERY_E2E_FROM": "qwenpaw", "RECOVERY_E2E_TO": "hermes",
            "RECOVERY_E2E_MODE": "replace", "RECOVERY_E2E_CASE": "23",
            "RECOVERY_E2E_CORE_IMAGE": "recovery-1631/controller:local-current",
            "RECOVERY_E2E_HARNESS_IMAGES": json.dumps({"qwenpaw": "recovery-1631/qwenpaw:local-current"}),
        }):
            config = runpy.run_path(str(Path(__file__).with_name("runtime_recovery_e2e.py")))
        self.assertEqual(config["CORE_IMAGE"], "recovery-1631/controller:local-current")
        self.assertEqual(config["HARNESS_IMAGES"]["qwenpaw"], "recovery-1631/qwenpaw:local-current")
        self.assertEqual(config["TASK_ID"], "live-qwenpaw-hermes-23")


class SubmissionHistoryTests(unittest.TestCase):
    def test_acceptance_preserves_the_native_worker_submission(self):
        from runtime_recovery_e2e import assert_native_submission
        submit = {"action": "submit_task", "actor": "worker:agent-a", "from": "in_progress", "to": "submitted", "seq": 2}
        accept = {"action": "accept_task_result", "actor": "leader:leader", "from": "submitted", "to": "completed", "seq": 3}
        assert_native_submission({"status": "submitted", "history": [submit]})
        assert_native_submission({"status": "completed", "history": [submit, accept]})

    def test_final_status_alone_does_not_prove_submission_or_review(self):
        from runtime_recovery_e2e import assert_native_submission
        submit = {"action": "submit_task", "actor": "worker:agent-a", "from": "in_progress", "to": "submitted", "seq": 2}
        accept = {"action": "accept_task_result", "actor": "leader:leader", "from": "submitted", "to": "completed", "seq": 3}
        invalid = [
            {"status": "completed", "history": []},
            {"status": "completed", "history": [submit]},
            {"status": "completed", "history": [submit, {**accept, "actor": "worker:agent-a"}]},
            {"status": "completed", "history": [submit, {**accept, "seq": 1}]},
            {"status": "submitted", "history": [submit, submit]},
            {"status": "cancelled", "history": [submit, accept]},
        ]
        for task in invalid:
            with self.subTest(task=task), self.assertRaises(ValueError):
                assert_native_submission(task)


class DeepSeekBoundaryTests(unittest.TestCase):
    def test_active_source_turn_still_needs_the_matching_native_tool_results(self):
        import hashlib
        from runtime_recovery_e2e import assert_deepseek_native

        task = {"task_id": "task-a", "execution": {"generation": 1}}
        input_id = "matrix-" + hashlib.sha256(b"recovery:recovery-team/task-a:1").hexdigest()
        native = {"input_id": input_id, "task_mentioned": True, "completed": False,
                  "tool_calls": 7, "tool_results": 7}
        self.assertIs(assert_deepseek_native({"native": native}, task, allow_active_turn=True), native)
        with self.assertRaises(AssertionError):
            assert_deepseek_native({"native": native}, task)
        for change in ({"tool_results": 0}, {"input_id": "matrix-other-task"}):
            with self.subTest(change=change), self.assertRaises(AssertionError):
                assert_deepseek_native({"native": {**native, **change}}, task, allow_active_turn=True)


class ReplacementLifecycleTests(unittest.TestCase):
    def test_unstarted_target_retry_must_be_removed_before_the_only_started_target(self):
        from runtime_recovery_e2e import replacement_lifecycle

        def event(action, actor, timestamp):
            return {"Action": action, "timeNano": timestamp,
                    "Actor": {"ID": actor, "Attributes": {"image": "target-image"}}}

        died = event("die", "old", 1)
        removed = event("destroy", "old", 2)
        first = event("create", "unstarted", 3)
        cleanup = event("destroy", "unstarted", 4)
        target = event("create", "target", 5)
        started = event("start", "target", 6)
        rows = [died, removed, first, cleanup, target, started]
        order, selected = replacement_lifecycle(rows, died, "old")
        self.assertEqual(selected, target)
        self.assertEqual(order["unstarted_target_attempts"], [{"id": "unstarted", "create": 3, "destroy": 4}])
        for invalid in ([died, removed, first, target, started], rows + [event("start", "unstarted", 4)]):
            with self.subTest(invalid=invalid), self.assertRaises(AssertionError):
                replacement_lifecycle(invalid, died, "old")


class OpenHumanCacheTests(unittest.TestCase):
    def test_native_tool_history_must_match_task_worker_and_latest_checkpoint(self):
        import copy
        import hashlib
        import json
        import time
        from runtime_recovery_e2e import assert_openhuman_native_cache
        cp = {"version": 4, "completed_steps": ["one"]}
        history = [{"role": "user", "content": "继续 task-a"},
                   {"role": "assistant", "content": "执行步骤一"},
                   {"role": "tool", "content": "产物已持久化"}]
        cache = {"format_version": 1, "runtime": "openhuman", "scope": ["recovery-team", "task-a"],
                 "authority": "worker-a", "session_id": "native-thread", "expires_at": time.time() + 60,
                 "checkpoint_digest": hashlib.sha256(json.dumps(cp, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
                 "history": history,
                 "native_digest": hashlib.sha256(json.dumps(history, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()}
        start = {"instance_id": "backend/one", "process_id": "process", "runtime": "openhuman"}
        execution = {**start, "checkpoint": cp, "generation": 1}
        shared = {"schema_version": 1, "kind": "WorkerSessionCache", "runtime": "openhuman",
                  "member_prefix": "agents/agent-a", "work": {"kind": "task", "team_id": "recovery-team", "task_id": "task-a"},
                  "config_generation": 4, "generation": 1, "start": start, "expires_at": cache["expires_at"],
                  "checkpoint_version": 4, "checkpoint_digest": cache["checkpoint_digest"], "native_ref": cache["session_id"],
                  "native_digest": cache["native_digest"]}
        proof = {"cache": cache, "authority": "worker-a", "descriptor": shared, "config_generation": 4}
        assert_openhuman_native_cache(proof, execution, "task-a")
        final = {**execution, "checkpoint": {"version": 10, "delivery": "delivered"}}
        assert_openhuman_native_cache(proof, final, "task-a", terminal=True)
        with self.assertRaises(AssertionError):
            assert_openhuman_native_cache(proof, final, "task-a")
        for change in [{"generation": 2}, {"config_generation": 5}, {"native_ref": "other"}, {"checkpoint_version": 11}]:
            with self.subTest(change=change), self.assertRaises(AssertionError):
                assert_openhuman_native_cache({**proof, "descriptor": {**shared, **change}}, final, "task-a", terminal=True)
        for change in [{"runtime": "hermes"}, {"scope": ["foreign-team", "task-a"]},
                       {"scope": ["recovery-team", "task-b"]}, {"authority": "worker-b"},
                       {"checkpoint_digest": "old"}, {"expires_at": 1}, {"session_id": ""},
                       {"native_digest": "changed"}, {"history": []}]:
            with self.subTest(change=change), self.assertRaises(AssertionError):
                assert_openhuman_native_cache({**proof, "cache": {**cache, **change}}, execution, "task-a")
        for history in [[{"role": "user", "content": "task-a but no tools"}],
                        [{"role": "tool", "content": "a different task"}]]:
            changed = copy.deepcopy(cache)
            changed["history"] = history
            changed["native_digest"] = hashlib.sha256(json.dumps(history, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()
            with self.subTest(history=history), self.assertRaises(AssertionError):
                assert_openhuman_native_cache({**proof, "cache": changed, "descriptor": {**shared, "native_digest": changed["native_digest"]}}, execution, "task-a")

        fallback = [cache["history"][0], {"role": "assistant", "content": '<tool_call>{"name":"shell","arguments":{}}</tool_call>'},
                    {"role":"user", "content":'[Tool results]\n<tool_result name="shell">\nstep one for task-a\n</tool_result>\n'}]
        for name, valid in [("shell", True), ("another", False)]:
            rows = [*fallback[:2], {"role":"user", "content":fallback[-1]["content"].replace('name="shell"', f'name="{name}"')}]
            digest = hashlib.sha256(json.dumps(rows, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()
            candidate = {**proof, "cache": {**cache, "history": rows, "native_digest": digest},
                         "descriptor": {**shared, "native_digest": digest}}
            if valid:
                assert_openhuman_native_cache(candidate, execution, "task-a")
            else:
                with self.assertRaises(AssertionError):
                    assert_openhuman_native_cache(candidate, execution, "task-a")


class OpenClawCacheTests(unittest.TestCase):
    def test_actual_native_session_and_latest_checkpoint_are_required(self):
        import hashlib
        import json
        import time
        from runtime_recovery_e2e import assert_openclaw_native_cache
        cp = {"version": 10, "completed_steps": ["one", "two", "three"]}
        cache = {"runtime": "openclaw", "expires_at": time.time() + 60, "session_id": "native",
                 "checkpoint_digest": hashlib.sha256(json.dumps(cp, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
                 "native_digest": "actual-sdk-transcript"}
        native = {"session_id": "native", "native_digest": "actual-sdk-transcript", "message_count": 7, "task_mentioned": True}
        assert_openclaw_native_cache({"cache": cache, "native": native}, cp)
        for changed in [{"session_id": "old"}, {"native_digest": "uncommitted"}, {"message_count": 0}, {"task_mentioned": False}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_openclaw_native_cache({"cache": cache, "native": {**native, **changed}}, cp)
        with self.assertRaises(AssertionError):
            assert_openclaw_native_cache({"cache": cache, "native": native}, {**cp, "version": 11})


class HermesSharedCacheTests(unittest.TestCase):
    def test_common_descriptor_requires_matching_native_and_authoritative_state(self):
        import hashlib
        import json
        import time
        from runtime_recovery_e2e import assert_hermes_native_cache
        cp = {"version": 4, "completed_steps": ["one"]}
        cache = {"schema_version": 1, "kind": "WorkerSessionCache", "member_prefix": "agents/agent-a", "runtime": "hermes",
                 "work": {"kind": "task", "team_id": "recovery-team", "task_id": "T"}, "native_ref": "native-session",
                 "native_digest": "a"*64, "expires_at": time.time()+60, "checkpoint_version": 4, "config_generation": 3,
                 "checkpoint_digest": hashlib.sha256(json.dumps(cp,sort_keys=True,separators=(",", ":")).encode()).hexdigest()}
        proof = {"cache": cache, "native_digest": "a"*64, "message_count": 4, "tool_count": 1, "task_mentioned": True, "config_generation": 3}
        assert_hermes_native_cache(proof, cp, "T")
        for changed in [{"native_digest": "other"}, {"tool_count": 0}, {"task_mentioned": False}, {"config_generation": 4}, {"cache": None}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_hermes_native_cache({**proof, **changed}, cp, "T")
        for changed in [{"checkpoint_version": 3}, {"checkpoint_digest": "old"}, {"member_prefix": "agents/other"}, {"expires_at": 1}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_hermes_native_cache({**proof, "cache": {**cache, **changed}}, cp, "T")


class QwenPawSharedCacheTests(unittest.TestCase):
    def test_current_source_cache_and_final_native_route_are_distinct_proofs(self):
        import copy
        import hashlib
        import json
        import time
        from runtime_recovery_e2e import assert_qwenpaw_native_cache, assert_qwenpaw_native_route
        task_id = "T"
        session = "matrix:task:"+hashlib.sha256(json.dumps(["recovery-team", task_id],
            sort_keys=True, separators=(",", ":")).encode()).hexdigest()+":native"
        cp = {"version": 4, "completed_steps": ["one"]}
        cache = {"schema_version": 1, "kind": "WorkerSessionCache", "member_prefix": "agents/agent-a",
            "runtime": "qwenpaw", "work": {"kind": "task", "team_id": "recovery-team", "task_id": task_id},
            "native_ref": session, "native_digest": "a"*64, "config_generation": 3,
            "checkpoint_version": 4, "checkpoint_digest": hashlib.sha256(json.dumps(cp,
                sort_keys=True, separators=(",", ":")).encode()).hexdigest(), "expires_at": time.time()+60}
        native = {"session_id": session, "native_digest": "a"*64, "agent_state_present": True,
            "task_mentioned": True, "message_count": 8, "tool_count": 2}
        route = {"trace_id": "actual-trace", "session_id": session, "task_id": task_id,
            "runtime": "qwenpaw", "generation": 2}
        proof = {"cache": cache, "native": [native], "routes": [route], "config_generation": 3}
        assert_qwenpaw_native_cache(proof, cp, task_id)
        assert_qwenpaw_native_route({**proof, "cache": None}, task_id, 2)
        for changed in [{"checkpoint_version": 3}, {"config_generation": 2}, {"native_digest": "b"*64},
                        {"expires_at": 0}, {"member_prefix": "agents/other"}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_qwenpaw_native_cache({**proof, "cache": {**cache, **changed}}, cp, task_id)
        for changed in [{"tool_count": 0}, {"task_mentioned": False}, {"agent_state_present": False}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_qwenpaw_native_route({**proof, "native": [{**native, **changed}]}, task_id, 2)
        for changed in [{"task_id": "other"}, {"generation": 1}, {"runtime": "hermes"}, {"trace_id": ""}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_qwenpaw_native_route({**proof, "routes": [{**route, **changed}]}, task_id, 2)
        for changed in [{"native": []}, {"routes": []}, {"native": [native, copy.deepcopy(native)]},
                        {"routes": [route, {**route, "session_id": session+"other"}]}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_qwenpaw_native_route({**proof, **changed}, task_id, 2)


class CutoverBoundaryTests(unittest.TestCase):
    def test_source_snapshot_is_persisted_before_any_lifecycle_mutation(self):
        import json
        from pathlib import Path
        import uuid
        import runtime_recovery_e2e as driver
        task = {"status": "in_progress", "execution": {"checkpoint": {
            "completed_steps": ["one"], "remaining_steps": ["two", "three"]}}}
        before = {"task": task, "instances": [{}, {"id": "old"}], "native_sessions": {"source": "actual-native"}}
        case = "boundary-test-" + uuid.uuid4().hex
        path = Path("/tmp/1631-live-" + case + "-before-cutover.json")
        self.addCleanup(lambda: path.unlink(missing_ok=True))
        class MutationObserved(Exception):
            pass
        def api(method, *args):
            if method == "GET":
                return task
            self.assertEqual(json.loads(path.read_text())["native_sessions"], before["native_sessions"])
            raise MutationObserved()
        with patch.object(driver, "CASE", case), patch.object(driver, "FAULT", ""), \
                patch.object(driver, "api", side_effect=api), \
                patch.object(driver, "docker", return_value=json.dumps([{"Id": "old", "Config": {"Image": driver.HARNESS_IMAGES[driver.SOURCE]}}])), \
                patch.object(driver, "evidence", return_value=before), \
                patch.object(driver, "source_read_receipt", return_value=None) as read, \
                patch.object(driver, "fixture_tool_processes", return_value=[]):
            with self.assertRaisesRegex(RuntimeError, "source native Matrix read receipt"):
                driver.cutover()
            self.assertFalse(path.exists())
            read.return_value = {"source_event": "$assignment", "read_event": "$receipt"}
            with self.assertRaises(MutationObserved):
                driver.cutover()
            self.assertEqual(json.loads(path.read_text())["source_read_receipt"], read.return_value)
            with self.assertRaisesRegex(RuntimeError, "already has a boundary"):
                driver.cutover()


class ReturnCursorTests(unittest.TestCase):
    def test_real_initial_sync_uses_only_the_same_native_consumer(self):
        import json
        import runtime_recovery_e2e as driver

        def cursor(device, value):
            return {"runtime": "qwenpaw", "account": "@worker:test", "device": device,
                    "format": "matrix-nio/sync-v3", "token_sha256": value}

        middle = {"matrix": {"cursors": {"old": cursor("old", "old-cursor")}}}
        container = {"State": {"StartedAt": "1970-01-01T00:00:10Z"},
                     "NetworkSettings": {"Networks": {driver.NETWORK: {"IPAddress": "10.0.0.5"}}}}
        for device, since, valid in [("old", "old-cursor", True), ("new", None, True),
                                     ("old", None, False), ("new", "old-cursor", False)]:
            req = {"epoch": 11, "since_sha256": since, "full_state": True, "ip": "10.0.0.5"}
            lines = [dict(req, epoch=9, since_sha256="predecessor"), dict(req, ip="10.0.0.6"), req]
            with self.subTest(device=device, since=since), patch.object(driver, "SOURCE", "qwenpaw"), \
                    patch.object(driver, "docker", return_value=json.dumps(["@worker:test", device])), \
                    patch.object(driver.Path, "read_text", return_value="\n".join(map(json.dumps, lines))):
                final = {"cursors": {"actual": cursor(device, "current")}}
                if valid:
                    self.assertEqual(driver.verify_return_cursor(middle, final, container)["initial_request"], req)
                else:
                    with self.assertRaisesRegex(AssertionError, "different consumer"):
                        driver.verify_return_cursor(middle, final, container)
