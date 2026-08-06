import hashlib
import unittest

from deepagents_agentteams.threads import checkpoint_thread_id


class CheckpointThreadIDTests(unittest.TestCase):
    def test_scopes_checkpoint_to_worker_room_and_thread_root(self) -> None:
        expected = hashlib.sha256(
            b"agentteams-deepagents-v1\x00worker-uid-1\x00!room:example.org\x00$root-event"
        ).hexdigest()

        actual = checkpoint_thread_id(
            worker_uid="worker-uid-1",
            room_id="!room:example.org",
            thread_root_event_id="$root-event",
        )

        self.assertEqual(actual, f"atd-{expected}")

    def test_uses_room_id_as_root_for_unthreaded_messages(self) -> None:
        expected = checkpoint_thread_id(
            worker_uid="worker-uid-1",
            room_id="!room:example.org",
            thread_root_event_id="!room:example.org",
        )

        actual = checkpoint_thread_id(
            worker_uid="worker-uid-1",
            room_id="!room:example.org",
            thread_root_event_id=None,
        )

        self.assertEqual(actual, expected)


if __name__ == "__main__":
    unittest.main()
