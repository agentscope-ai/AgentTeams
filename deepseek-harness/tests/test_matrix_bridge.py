import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))

from matrix_bridge import (  # noqa: E402
    BridgeState,
    MatrixClient,
    dsh_error_detail,
    materialize_attachment,
    matrix_events,
    matrix_transaction_id,
    restore_output_paths,
    reply_mention_user_ids,
    run_dsh,
    runtime_agent_user_ids,
    runtime_matrix_context,
    send_workspace_outputs,
    snapshot_outbox,
    sync_output_paths,
    text_events,
    visible_matrix_user_ids,
)


class TextEventsTest(unittest.TestCase):
    def test_keeps_new_text_from_other_users_in_watched_rooms(self):
        payload = {
            "rooms": {
                "join": {
                    "!personal:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$task",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "unsigned": None,
                                    "content": {"msgtype": "m.text", "body": "do the task"},
                                },
                                {
                                    "event_id": "$self",
                                    "sender": "@dsh:matrix.local",
                                    "type": "m.room.message",
                                    "content": {"msgtype": "m.text", "body": "old answer"},
                                },
                                {
                                    "event_id": "$file",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {"msgtype": "m.file", "body": "input.txt"},
                                },
                            ]
                        }
                    },
                    "!unwatched:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$ignored",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {"msgtype": "m.text", "body": "ignore"},
                                }
                            ]
                        }
                    },
                }
            }
        }

        self.assertEqual(
            text_events(payload, {"!personal:matrix.local": "personal"}, "@dsh:matrix.local"),
            [
                {
                    "room_id": "!personal:matrix.local",
                    "event_id": "$task",
                    "sender": "@admin:matrix.local",
                    "body": "do the task",
                }
            ],
        )

    def test_ignores_blank_messages_and_redacted_events(self):
        payload = {
            "rooms": {
                "join": {
                    "!room:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$blank",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {"msgtype": "m.text", "body": "  "},
                                },
                                {
                                    "event_id": "$redacted",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "unsigned": {"redacted_because": {}},
                                    "content": {"msgtype": "m.text", "body": "removed"},
                                },
                            ]
                        }
                    }
                }
            }
        }

        self.assertEqual(text_events(payload, {"!room:matrix.local": "personal"}, "@dsh:matrix.local"), [])

    def test_dsh_error_prefers_actionable_message_over_runtime_footer(self):
        stderr = """file:///dsh/lib/index.js:1
Error: dsh: HTTP_405: DeepSeek API error (HTTP 405)
    at boot (file:///dsh/lib/index.js:2)
Node.js v22.22.3
"""
        self.assertEqual(
            dsh_error_detail(stderr, 1),
            "Error: dsh: HTTP_405: DeepSeek API error (HTTP 405)",
        )

    def test_team_agent_reply_is_accepted_only_when_structurally_targeted(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$agent-reply",
                                    "sender": "@leader:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "completed",
                                        "m.relates_to": {"m.in_reply_to": {"event_id": "$task"}},
                                        "m.mentions": {"user_ids": ["@worker:matrix.local"]},
                                    },
                                },
                                {
                                    "event_id": "$human-reply",
                                    "sender": "@coordinator:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "please continue",
                                        "m.relates_to": {"m.in_reply_to": {"event_id": "$agent-reply"}},
                                    },
                                },
                            ]
                        }
                    }
                }
            }
        }

        events = matrix_events(
            payload,
            {"!team:matrix.local": "team"},
            "@worker:matrix.local",
            {"@leader:matrix.local", "@worker:matrix.local"},
        )

        self.assertEqual([event["event_id"] for event in events], ["$agent-reply"])

    def test_team_agent_message_requires_a_structured_mention(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$untargeted",
                                    "sender": "@leader:matrix.local",
                                    "type": "m.room.message",
                                    "content": {"msgtype": "m.text", "body": "for someone else"},
                                },
                                {
                                    "event_id": "$targeted",
                                    "sender": "@leader:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "please handle this",
                                        "m.mentions": {"user_ids": ["@worker:matrix.local"]},
                                    },
                                },
                            ]
                        }
                    }
                }
            }
        }

        events = matrix_events(
            payload,
            {"!team:matrix.local": "team"},
            "@worker:matrix.local",
            {"@leader:matrix.local", "@worker:matrix.local"},
        )

        self.assertEqual([event["event_id"] for event in events], ["$targeted"])

    def test_unmentioned_team_human_is_quiet_but_personal_room_remains_conversational(self):
        event = {
            "event_id": "$human",
            "sender": "@admin:matrix.local",
            "type": "m.room.message",
            "content": {"msgtype": "m.text", "body": "ordinary chatter"},
        }
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {"timeline": {"events": [event]}},
                    "!personal:matrix.local": {"timeline": {"events": [event]}},
                }
            }
        }

        events = matrix_events(
            payload,
            {"!team:matrix.local": "team", "!personal:matrix.local": "personal"},
            "@worker:matrix.local",
            {"@worker:matrix.local"},
        )

        self.assertEqual([item["room_id"] for item in events], ["!personal:matrix.local"])

    def test_team_room_mention_targets_one_worker_or_the_whole_room(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$other",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "for the other worker",
                                        "m.mentions": {"user_ids": ["@other:matrix.local"]},
                                    },
                                },
                                {
                                    "event_id": "$self",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "for this worker",
                                        "m.mentions": {"user_ids": ["@worker:matrix.local"]},
                                    },
                                },
                                {
                                    "event_id": "$room",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "for the room",
                                        "m.mentions": {"room": True},
                                    },
                                },
                            ]
                        }
                    }
                }
            }
        }

        events = matrix_events(
            payload,
            {"!team:matrix.local": "team"},
            "@worker:matrix.local",
            {"@worker:matrix.local", "@other:matrix.local"},
        )

        self.assertEqual([item["event_id"] for item in events], ["$self", "$room"])

    def test_visible_known_agent_ids_become_matrix_mentions(self):
        allowed = {"@leader:matrix.local", "@worker:matrix.local"}

        self.assertEqual(
            visible_matrix_user_ids("@leader:matrix.local，任务完成", allowed),
            ["@leader:matrix.local"],
        )
        self.assertEqual(visible_matrix_user_ids("email@leader:matrix.local", allowed), [])

        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "send_content", return_value="$reply") as send_content:
            result = client.send_text(
                "!team:matrix.local",
                "@leader:matrix.local，任务完成",
                "$task",
                "$task:reply",
                ["@leader:matrix.local"],
            )

        self.assertEqual(result, "$reply")
        sent = send_content.call_args.args[1]
        self.assertEqual(sent["m.mentions"], {"user_ids": ["@leader:matrix.local"]})
        self.assertEqual(sent["m.relates_to"], {"m.in_reply_to": {"event_id": "$task"}})

    def test_visible_known_agent_ids_accept_sentence_punctuation_without_matching_longer_ids(self):
        allowed = {"@leader:matrix.local"}

        self.assertEqual(
            {
                "colon": visible_matrix_user_ids("@leader:matrix.local: task complete", allowed),
                "period": visible_matrix_user_ids("@leader:matrix.local. Task complete", allowed),
                "port": visible_matrix_user_ids("@leader:matrix.local:8448", allowed),
                "longer-domain": visible_matrix_user_ids("@leader:matrix.local.example", allowed),
            },
            {
                "colon": ["@leader:matrix.local"],
                "period": ["@leader:matrix.local"],
                "port": [],
                "longer-domain": [],
            },
        )

    def test_text_reply_marks_an_automatically_targeted_source_agent(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "send_content", return_value="$reply") as send_content:
            client.send_text(
                "!team:matrix.local",
                "TASK_COMPLETED",
                "$delegation",
                "$delegation:reply",
                ["@leader:matrix.local"],
                True,
            )

        content = send_content.call_args.args[1]
        self.assertTrue(content["com.agentteams.auto_source_mention"])

    def test_agent_reply_mentions_source_even_when_answer_omits_its_id(self):
        event = {
            "event_id": "$delegation",
            "sender": "@leader:matrix.local",
            "body": "complete the task",
        }

        self.assertEqual(
            reply_mention_user_ids(
                event,
                "TASK_COMPLETED",
                {"@leader:matrix.local", "@worker:matrix.local"},
                "@worker:matrix.local",
            ),
            ["@leader:matrix.local"],
        )

    def test_auto_source_mention_is_not_bounced_back(self):
        event = {
            "event_id": "$completion",
            "sender": "@worker:matrix.local",
            "body": "TASK_COMPLETED",
            "auto_source_mention": True,
        }

        self.assertEqual(
            reply_mention_user_ids(
                event,
                "Acknowledged",
                {"@leader:matrix.local", "@worker:matrix.local"},
                "@leader:matrix.local",
            ),
            [],
        )

    def test_matrix_event_preserves_auto_source_mention_marker(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$completion",
                                    "sender": "@worker:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.text",
                                        "body": "TASK_COMPLETED",
                                        "m.mentions": {"user_ids": ["@leader:matrix.local"]},
                                        "com.agentteams.auto_source_mention": True,
                                    },
                                }
                            ]
                        }
                    }
                }
            }
        }

        events = matrix_events(
            payload,
            {"!team:matrix.local": "team"},
            "@leader:matrix.local",
            {"@leader:matrix.local", "@worker:matrix.local"},
        )

        self.assertTrue(events[0]["auto_source_mention"])


class RoomSessionContinuationTest(unittest.TestCase):
    def test_room_session_is_restored_after_bridge_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"

            first_process = BridgeState.load(path)
            first_session, first_is_resume = first_process.session_for("!team:matrix.local")
            self.assertFalse(first_is_resume)
            first_process.mark_session_ready("!team:matrix.local")
            first_process.save()

            restarted_process = BridgeState.load(path)
            restored_session, restored_is_resume = restarted_process.session_for("!team:matrix.local")

            self.assertEqual(restored_session, first_session)
            self.assertTrue(restored_is_resume)

    @patch("matrix_bridge.subprocess.run")
    def test_dsh_receives_room_session_and_resume_mode(self, mocked_run):
        mocked_run.return_value.returncode = 0
        mocked_run.return_value.stdout = "continued answer\n"
        mocked_run.return_value.stderr = ""

        answer = run_dsh(
            "what did I say before?",
            Path("/workspace"),
            30,
            session_id="session-agentteams-room-123",
            resume=True,
            event_id="$second-message",
            attempt=2,
        )

        self.assertEqual(answer, "continued answer")
        environment = mocked_run.call_args.kwargs["env"]
        self.assertEqual(environment["TEAMHARNESS_DSH_SESSION_ID"], "session-agentteams-room-123")
        self.assertEqual(environment["TEAMHARNESS_DSH_RESUME"], "true")
        self.assertEqual(environment["TEAMHARNESS_MATRIX_EVENT_ID"], "$second-message")
        self.assertEqual(environment["TEAMHARNESS_DSH_ATTEMPT"], "2")

    def test_runtime_room_set_changes_when_worker_joins_a_team(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text(
                "member:\n"
                "  runtimeName: dsh-worker\n"
                "  matrixUserId: '@dsh-worker:matrix.local'\n"
                "  personalRoomId: '!personal:matrix.local'\n",
                encoding="utf-8",
            )
            _user_id, standalone_rooms = runtime_matrix_context(runtime, "dsh-worker", "matrix.local")

            runtime.write_text(
                "team:\n"
                "  teamRoomId: '!team:matrix.local'\n"
                "member:\n"
                "  runtimeName: dsh-worker\n"
                "  matrixUserId: '@dsh-worker:matrix.local'\n"
                "  personalRoomId: '!personal:matrix.local'\n",
                encoding="utf-8",
            )
            _user_id, team_rooms = runtime_matrix_context(runtime, "dsh-worker", "matrix.local")

            self.assertEqual(standalone_rooms, {"!personal:matrix.local": "personal"})
            self.assertEqual(
                team_rooms,
                {"!personal:matrix.local": "personal", "!team:matrix.local": "team"},
            )

    def test_runtime_agent_user_ids_excludes_human_members(self):
        runtime = Path(__file__).resolve().parents[2] / "plugins" / "teamharness" / "adapters" / "deepseek-harness" / "fixtures" / "runtime.yaml"

        self.assertEqual(
            runtime_agent_user_ids(runtime),
            {"@leader-a:matrix.local", "@dsh-worker-a:matrix.local"},
        )

    def test_runtime_agent_user_ids_accepts_controller_indentless_sequence(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text(
                "team:\n"
                "  members:\n"
                "  - matrixUserId: '@leader:matrix.local'\n"
                "    role: team_leader\n"
                "  - matrixUserId: '@coordinator:matrix.local'\n"
                "    role: coordinator\n"
                "member:\n"
                "  matrixUserId: '@leader:matrix.local'\n",
                encoding="utf-8",
            )

            self.assertEqual(runtime_agent_user_ids(runtime), {"@leader:matrix.local"})


class AttachmentReceiveTest(unittest.TestCase):
    def test_image_and_file_events_are_extracted_from_matrix(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$image",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.image",
                                        "body": "diagram.png",
                                        "url": "mxc://matrix.local/image-id",
                                        "info": {"mimetype": "image/png", "size": 8},
                                        "m.mentions": {"user_ids": ["@dsh:matrix.local"]},
                                    },
                                },
                                {
                                    "event_id": "$file",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.file",
                                        "body": "../../budget.csv",
                                        "url": "mxc://matrix.local/file-id",
                                        "info": {"mimetype": "text/csv", "size": 12},
                                        "m.mentions": {"user_ids": ["@dsh:matrix.local"]},
                                    },
                                },
                            ]
                        }
                    }
                }
            }
        }

        events = matrix_events(payload, {"!team:matrix.local": "team"}, "@dsh:matrix.local")

        self.assertEqual([event["kind"] for event in events], ["image", "file"])
        self.assertEqual(events[1]["mxc_url"], "mxc://matrix.local/file-id")
        self.assertEqual(events[1]["mimetype"], "text/csv")

    def test_attachment_is_written_under_workspace_inbox_with_safe_name(self):
        class FakeMatrixClient:
            def download_media(self, mxc_url, max_bytes):
                self.call = (mxc_url, max_bytes)
                return b"a,b\n1,2\n", "text/csv"

        event = {
            "room_id": "!team:matrix.local",
            "event_id": "$file",
            "sender": "@admin:matrix.local",
            "kind": "file",
            "body": "../../budget.csv",
            "filename": "../../budget.csv",
            "mxc_url": "mxc://matrix.local/file-id",
            "mimetype": "text/csv",
        }

        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            prompt, saved_path = materialize_attachment(event, FakeMatrixClient(), workspace, 1024)

            self.assertTrue(saved_path.is_relative_to((workspace / "inbox").resolve()))
            self.assertEqual(saved_path.name, "budget.csv")
            self.assertEqual(saved_path.read_bytes(), b"a,b\n1,2\n")
            self.assertIn("inbox/", prompt.replace("\\", "/"))
            self.assertIn("budget.csv", prompt)

    def test_attachment_rejects_inbox_symlink_outside_workspace(self):
        class FakeMatrixClient:
            def download_media(self, _mxc_url, _max_bytes):
                return b"unsafe", "application/octet-stream"

        event = {
            "room_id": "!team:matrix.local",
            "event_id": "$file",
            "sender": "@admin:matrix.local",
            "kind": "file",
            "body": "payload.bin",
            "filename": "payload.bin",
            "mxc_url": "mxc://matrix.local/file-id",
            "mimetype": "application/octet-stream",
        }

        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as outside:
            workspace = Path(directory)
            try:
                (workspace / "inbox").symlink_to(Path(outside), target_is_directory=True)
            except OSError as error:
                self.skipTest(f"directory symlinks are unavailable: {error}")

            with self.assertRaisesRegex(RuntimeError, "escapes the Workspace"):
                materialize_attachment(event, FakeMatrixClient(), workspace, 1024)


class AttachmentSendTest(unittest.TestCase):
    def test_returned_file_mentions_the_source_agent(self):
        client = MatrixClient("http://matrix.local", "token")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.txt"
            path.write_text("done", encoding="utf-8")
            with (
                patch.object(client, "upload_media", return_value=("mxc://matrix.local/result", "text/plain", 4)),
                patch.object(client, "send_content", return_value="$file") as send_content,
            ):
                event_id = client.send_file(
                    "!team:matrix.local",
                    path,
                    "$delegation",
                    "$delegation:file:result.txt",
                    ["@leader:matrix.local"],
                    True,
                )

        self.assertEqual(event_id, "$file")
        content = send_content.call_args.args[1]
        self.assertEqual(content["m.mentions"], {"user_ids": ["@leader:matrix.local"]})
        self.assertTrue(content["com.agentteams.auto_source_mention"])

    def test_only_new_or_changed_outbox_files_are_sent(self):
        class FakeMatrixClient:
            def __init__(self):
                self.sent = []

            def send_file(
                self,
                room_id,
                path,
                reply_to,
                transaction_key,
                mentioned_user_ids=None,
                auto_source_mention=False,
            ):
                self.sent.append(
                    (room_id, path.name, reply_to, transaction_key, mentioned_user_ids, auto_source_mention)
                )
                return "$uploaded"

        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            outbox = workspace / "outbox"
            outbox.mkdir()
            (outbox / "old.txt").write_text("unchanged", encoding="utf-8")
            (outbox / "changed.txt").write_text("before", encoding="utf-8")
            before = snapshot_outbox(workspace)

            (outbox / "changed.txt").write_text("after", encoding="utf-8")
            (outbox / "result.png").write_bytes(b"png-result")
            client = FakeMatrixClient()
            sent = send_workspace_outputs(
                client,
                "!team:matrix.local",
                "$task",
                workspace,
                before,
                ["@leader:matrix.local"],
                True,
            )

            self.assertEqual([path.name for path in sent], ["changed.txt", "result.png"])
            self.assertEqual([call[1] for call in client.sent], ["changed.txt", "result.png"])
            self.assertTrue(all(call[2] == "$task" for call in client.sent))
            self.assertTrue(all(call[4] == ["@leader:matrix.local"] for call in client.sent))
            self.assertTrue(all(call[5] is True for call in client.sent))

    def test_output_rejects_outbox_symlink_outside_workspace(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as outside:
            workspace = Path(directory)
            outside_file = Path(outside) / "secret.txt"
            outside_file.write_text("secret", encoding="utf-8")
            try:
                (workspace / "outbox").symlink_to(Path(outside), target_is_directory=True)
            except OSError as error:
                self.skipTest(f"directory symlinks are unavailable: {error}")

            with self.assertRaisesRegex(RuntimeError, "escapes the Workspace"):
                send_workspace_outputs(object(), "!team:matrix.local", "$task", workspace, {})


class DeliveryReliabilityTest(unittest.TestCase):
    def test_matrix_transaction_is_stable_for_the_same_source_event(self):
        first = matrix_transaction_id("$task:reply")
        after_restart = matrix_transaction_id("$task:reply")

        self.assertEqual(after_restart, first)
        self.assertNotEqual(matrix_transaction_id("$other:reply"), first)

    def test_completed_event_is_skipped_after_bridge_restart(self):
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

    def test_processing_event_reuses_attempt_after_bridge_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"
            first = BridgeState.load(path)
            self.assertEqual(first.begin_event("$task", "!team:matrix.local"), 1)
            first.save()

            restarted = BridgeState.load(path)

            self.assertEqual(restarted.begin_event("$task", "!team:matrix.local"), 1)

    def test_failed_event_keeps_retry_count_across_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "matrix-bridge-state.json"
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

    @patch("matrix_bridge.mc")
    def test_pending_output_is_staged_and_can_be_restored_after_restart(self, mocked_mc):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "matrix_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            workspace = Path(directory)
            output = workspace / "outbox" / "result.txt"
            output.parent.mkdir()
            output.write_text("durable result", encoding="utf-8")

            sync_output_paths("dsh-worker", workspace, ["result.txt"])

            mocked_mc.assert_called_once_with(
                "cp",
                str(output.resolve()),
                "agentteams/bucket/agents/dsh-worker/workspace/outbox/result.txt",
            )
            output.unlink()

            def restore_copy(*args, **_kwargs):
                Path(args[2]).write_text("durable result", encoding="utf-8")

            mocked_mc.reset_mock()
            mocked_mc.side_effect = restore_copy
            restore_output_paths("dsh-worker", workspace, ["result.txt"])

            self.assertEqual(output.read_text(encoding="utf-8"), "durable result")


if __name__ == "__main__":
    unittest.main()
