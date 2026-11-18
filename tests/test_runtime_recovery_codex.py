import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import uuid

from fixtures.runtime_recovery_codex import rollout_facts
from runtime_recovery_e2e import assert_codex_native


class CodexRolloutTests(unittest.TestCase):
    def test_only_activity_in_the_actual_selected_turn_counts(self):
        thread, turn = str(uuid.uuid4()), str(uuid.uuid4())
        marker, room = "recovery:recovery-team/T:2", "!room:test"
        def item(kind, **fields):
            return {"type": "response_item", "payload": {"type": kind, **fields}}
        with tempfile.TemporaryDirectory() as folder:
            home = Path(folder)
            path = home / "sessions" / ("rollout-example-" + thread + ".jsonl")
            path.parent.mkdir()
            rows = [{"type": "session_meta", "payload": {"id": thread}},
                    {"type": "turn_context", "payload": {"turn_id": "old"}},
                    item("function_call"), item("function_call_output"),
                    {"type": "event_msg", "payload": {"type": "task_started", "turn_id": turn}},
                    item("message", role="user", content=[{"type": "input_text", "text":
                        f"[AgentTeams event {marker}]\nCurrent Matrix session: matrix:{room}\nTask description"}]),
                    item("function_call"), item("function_call_output")]
            path.write_text("".join(json.dumps(r)+"\n" for r in rows))
            native = rollout_facts(home, thread, turn, marker, room)
            self.assertEqual(native["native_digest"], hashlib.sha256(path.read_bytes()).hexdigest())
            self.assertEqual((native["source_inputs"], native["tool_calls"], native["tool_outputs"]), (1, 1, 1))
            self.assertEqual(rollout_facts(home, thread, "old", marker, room)["source_inputs"], 0)
            self.assertEqual(rollout_facts(home, thread, turn, marker, "!other:test")["source_inputs"], 0)
            rows[0]["payload"]["id"] = str(uuid.uuid4())
            path.write_text("".join(json.dumps(r)+"\n" for r in rows))
            with self.assertRaisesRegex(ValueError, "metadata"):
                rollout_facts(home, thread, turn, marker, room)

    def test_journal_identity_or_a_stale_descriptor_does_not_prove_reuse(self):
        execution = {"runtime": "codex", "instance_id": "container/start", "process_id": "process",
                     "generation": 2, "checkpoint": {"version": 4}}
        work = {"kind": "task", "team_id": "recovery-team", "task_id": "T"}
        native = {"thread_id": "ref", "metadata_id": "ref", "turn_id": "turn", "native_digest": "d"*64,
                  "native_bytes": 100, "source_inputs": 1, "tool_calls": 1, "tool_outputs": 1}
        cache = {"schema_version": 1, "kind": "WorkerSessionCache", "member_prefix": "agents/agent-a", "runtime": "codex",
                 "work": work, "start": {k:execution[k] for k in ("instance_id","process_id","runtime")}, "generation": 2,
                 "checkpoint_version": 4, "checkpoint_digest": hashlib.sha256(b'{"version":4}').hexdigest(),
                 "config_generation": 4, "expires_at": 4102444800, "native_ref": "ref", "native_digest": "d"*64}
        proof = {"route": {**work, **execution}, "native": native, "cache": cache, "config_generation": 4}
        task = {"task_id": "T", "execution": execution}
        self.assertEqual(assert_codex_native(proof, task, require_cache=True), native)
        for field, key, value in [("route", "generation", 1), ("route", "process_id", "retired"), ("native", "source_inputs", 0),
                                  ("native", "metadata_id", "other"), ("native", "tool_outputs", 0), ("cache", "checkpoint_digest", "old"),
                                  ("cache", "native_digest", "old"), ("cache", "expires_at", 0)]:
            bad = copy.deepcopy(proof); bad[field][key] = value
            with self.subTest(field=field, key=key), self.assertRaises(AssertionError):
                assert_codex_native(bad, task, require_cache=True)
