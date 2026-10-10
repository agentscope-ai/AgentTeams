import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))

from bridge_state import BridgeState, atomic_write  # noqa: E402


class LoadTest(unittest.TestCase):
    def test_missing_file_starts_empty_skeleton(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"
            state = BridgeState.load(path)

            self.assertEqual(state.next_batch, None)
            self.assertEqual(state.data["rooms"], {})
            self.assertEqual(state.data["events"], {})
            self.assertEqual(state.data["version"], 1)

    def test_unsupported_version_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            path.write_text('{"version": 2}', encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "unsupported Matrix bridge state"):
                BridgeState.load(path)

    def test_non_object_document_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            path.write_text("[1, 2]", encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "unsupported Matrix bridge state"):
                BridgeState.load(path)

    def test_missing_sections_are_backfilled(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            path.write_text('{"version": 1, "next_batch": "t_1"}', encoding="utf-8")
            state = BridgeState.load(path)

            self.assertEqual(state.next_batch, "t_1")
            self.assertEqual(state.data["rooms"], {})
            self.assertEqual(state.data["events"], {})

    def test_invalid_room_section_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            path.write_text('{"version": 1, "rooms": "not-a-dict"}', encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "invalid Matrix bridge state"):
                BridgeState.load(path)


class NextBatchTest(unittest.TestCase):
    def test_blank_cursor_reads_as_none(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")

            self.assertEqual(state.next_batch, None)
            state.data["next_batch"] = "   "
            self.assertEqual(state.next_batch, None)

    def test_cursor_round_trip(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")

            state.next_batch = "t_42"
            self.assertEqual(state.next_batch, "t_42")
            state.next_batch = None
            self.assertEqual(state.data["next_batch"], "")


class SessionTest(unittest.TestCase):
    def test_session_id_is_deterministic_per_room(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")

            first, first_resume = state.session_for("!room:matrix.local")
            second, _ = state.session_for("!room:matrix.local")

            self.assertFalse(first_resume)
            self.assertEqual(first, second)
            self.assertRegex(first, r"^session-agentteams-[0-9a-f]{32}$")

    def test_room_session_is_restored_after_bridge_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"

            first = BridgeState.load(path)
            first_session, first_resume = first.session_for("!team:matrix.local")
            self.assertFalse(first_resume)
            first.mark_session_ready("!team:matrix.local")
            first.save()

            restarted = BridgeState.load(path)
            restored_session, restored_resume = restarted.session_for("!team:matrix.local")

            self.assertEqual(restored_session, first_session)
            self.assertTrue(restored_resume)

    def test_room_without_session_id_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")
            state.data["rooms"]["!broken:matrix.local"] = {"session_id": "", "ready": True}

            with self.assertRaisesRegex(RuntimeError, "has no DSH session id"):
                state.session_for("!broken:matrix.local")


class EventLifecycleTest(unittest.TestCase):
    def test_new_event_starts_at_attempt_one(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")

            self.assertEqual(state.begin_event("$task", "!team:matrix.local"), 1)
            record = state.data["events"]["$task"]
            self.assertEqual(record["status"], "processing")
            self.assertEqual(record["room_id"], "!team:matrix.local")

    def test_processing_event_reuses_attempt_after_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            first = BridgeState.load(path)
            self.assertEqual(first.begin_event("$task", "!team:matrix.local"), 1)
            first.save()

            restarted = BridgeState.load(path)
            self.assertEqual(restarted.begin_event("$task", "!team:matrix.local"), 1)

    def test_failed_event_keeps_retry_count_across_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            first = BridgeState.load(path)
            self.assertEqual(first.begin_event("$task", "!team:matrix.local"), 1)
            first.mark_outbox_before("$task", {"existing.txt": "before-hash"})
            first.mark_failure("$task", "temporary gateway failure")
            first.save()

            second = BridgeState.load(path)
            self.assertEqual(second.outbox_before_for("$task"), {"existing.txt": "before-hash"})
            self.assertTrue(second.should_retry("$task", max_attempts=3))
            self.assertEqual(second.begin_event("$task", "!team:matrix.local"), 2)
            second.mark_failure("$task", "temporary gateway failure")
            self.assertTrue(second.should_retry("$task", max_attempts=3))
            self.assertEqual(second.begin_event("$task", "!team:matrix.local"), 3)
            second.mark_failure("$task", "permanent failure")
            self.assertFalse(second.should_retry("$task", max_attempts=3))

    def test_should_retry_without_record_is_true(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")
            self.assertTrue(state.should_retry("$unknown", max_attempts=1))

    def test_answer_round_trip_clears_outbox_before(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")
            state.mark_outbox_before("$task", {"existing.txt": "before-hash"})
            state.mark_answer("$task", "done", ["result.txt"])

            self.assertEqual(state.answer_for("$task"), ("done", ["result.txt"]))
            self.assertIsNone(state.outbox_before_for("$task"))
            self.assertEqual(state.data["events"]["$task"]["last_error"], "")

    def test_answer_for_missing_event_returns_none(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")
            self.assertIsNone(state.answer_for("$missing"))

    def test_completed_event_is_skipped_after_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"
            state = BridgeState.load(path)
            state.begin_event("$task", "!team:matrix.local")
            state.mark_answer("$task", "done", ["result.txt"])
            state.mark_completed("$task", "$reply")
            state.save()

            restarted = BridgeState.load(path)

            self.assertTrue(restarted.is_completed("$task"))
            self.assertEqual(restarted.answer_for("$task"), ("done", ["result.txt"]))
            self.assertFalse(restarted.is_completed("$other"))

    def test_completed_events_are_trimmed_to_two_thousand(self):
        with tempfile.TemporaryDirectory() as directory:
            state = BridgeState.load(Path(directory) / "state.json")
            for index in range(2002):
                state.mark_completed(f"$e{index}", f"$r{index}")
                state.data["events"][f"$e{index}"]["updated_at"] = index

            state.mark_completed("$final", "$rfinal")
            state.data["events"]["$final"]["updated_at"] = 10**6

            events = state.data["events"]
            self.assertEqual(len(events), 2000)
            self.assertIn("$final", events)
            self.assertIn("$e2001", events)
            self.assertNotIn("$e0", events)


class PersistenceTest(unittest.TestCase):
    def test_save_writes_sorted_json_and_leaves_no_tmp_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            state = BridgeState.load(path)
            state.next_batch = "t_9"
            state.begin_event("$task", "!room")
            state.save()

            text = path.read_text(encoding="utf-8")
            self.assertEqual(text, json.dumps(state.data, ensure_ascii=False, sort_keys=True) + "\n")
            self.assertEqual(json.loads(text)["next_batch"], "t_9")
            self.assertEqual([name for name in Path(directory).iterdir() if name.suffix == ".tmp"], [])

    def test_atomic_write_creates_parent_directories(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "nested" / "dir" / "state.json"
            atomic_write(target, '{"version": 1}\n')

            self.assertEqual(target.read_text(encoding="utf-8"), '{"version": 1}\n')
            self.assertFalse(target.with_suffix(".json.tmp").exists())


if __name__ == "__main__":
    unittest.main()
