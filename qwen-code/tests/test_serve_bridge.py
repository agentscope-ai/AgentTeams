import hashlib
import io
import json
import sys
import tempfile
import threading
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import MagicMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))

from serve_bridge import (  # noqa: E402
    HealthHandler,
    ServeClient,
    changed_outbox_files,
    materialize_attachment,
    output_remote_path,
    push_runtime_state,
    refresh_runtime_config,
    required_env,
    reply_mention_user_ids,
    restore_output_paths,
    run_serve,
    runtime_agent_user_ids,
    runtime_matrix_context,
    runtime_section,
    safe_filename,
    send_output_paths,
    send_workspace_outputs,
    snapshot_outbox,
    source_agent_reply_user_id,
    sync_output_paths,
    sync_sessions,
    workspace_output_path,
    workspace_subdirectory,
)


class FakeHeaders:
    def __init__(self, mapping=None) -> None:
        self._map = {str(key).lower(): str(value) for key, value in (mapping or {}).items()}

    def get(self, name, default=None):
        return self._map.get(str(name).lower(), default)

    def get_content_type(self):
        return self._map.get("content-type")


class FakeResponse:
    """Minimal urlopen() response: bytes payload + header map, seekable read, line iteration."""

    def __init__(self, payload: bytes = b"", headers=None, status: int = 200) -> None:
        self._payload = payload
        self.status = status
        self.headers = FakeHeaders(headers)

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            data, self._payload = self._payload, b""
        else:
            data, self._payload = self._payload[:size], self._payload[size:]
        return data

    def __iter__(self):
        return iter(self._payload.splitlines(keepends=True))

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def http_error(request, code: int) -> urllib.error.HTTPError:
    return urllib.error.HTTPError(request.full_url, code, "error", None, None)


class RequiredEnvTest(unittest.TestCase):
    def test_missing_env_raises(self):
        with patch.dict("serve_bridge.os.environ", {}, clear=True):
            with self.assertRaisesRegex(RuntimeError, "AGENTTEAMS_QC_TEST_VAR is required"):
                required_env("AGENTTEAMS_QC_TEST_VAR")

    def test_env_value_is_stripped(self):
        with patch.dict("serve_bridge.os.environ", {"AGENTTEAMS_QC_TEST_VAR": "  value  "}):
            self.assertEqual(required_env("AGENTTEAMS_QC_TEST_VAR"), "value")


class RuntimeYamlTest(unittest.TestCase):
    def test_runtime_section_reads_scalars_and_stops_at_next_top_level(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "runtime.yaml"
            path.write_text(
                "member:\n"
                "  matrixUserId: '@w:matrix.local'\n"
                "  personalRoomId: \"!p:matrix.local\"\n"
                "  nested:\n"
                "    deeper: ignored\n"
                "team:\n"
                "  teamRoomId: '!t:matrix.local'\n",
                encoding="utf-8",
            )

            self.assertEqual(
                runtime_section(path, "member"),
                {"matrixUserId": "@w:matrix.local", "personalRoomId": "!p:matrix.local", "nested": ""},
            )
            self.assertEqual(runtime_section(path, "team"), {"teamRoomId": "!t:matrix.local"})

    def test_runtime_matrix_context_uses_projected_ids(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text(
                "member:\n"
                "  matrixUserId: '@w:matrix.local'\n"
                "  personalRoomId: '!p:matrix.local'\n"
                "team:\n"
                "  teamRoomId: '!t:matrix.local'\n",
                encoding="utf-8",
            )

            own, rooms = runtime_matrix_context(runtime, "w", "matrix.local")

            self.assertEqual(own, "@w:matrix.local")
            self.assertEqual(rooms, {"!p:matrix.local": "personal", "!t:matrix.local": "team"})

    def test_runtime_matrix_context_falls_back_to_worker_localpart(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text("member:\n  personalRoomId: '!p:matrix.local'\n", encoding="utf-8")

            own, rooms = runtime_matrix_context(runtime, "w", "matrix.local")

            self.assertEqual(own, "@w:matrix.local")
            self.assertEqual(rooms, {"!p:matrix.local": "personal"})

    def test_runtime_matrix_context_empty_when_no_rooms(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text("member:\n  matrixUserId: '@w:matrix.local'\n", encoding="utf-8")

            _own, rooms = runtime_matrix_context(runtime, "w", "matrix.local")
            self.assertEqual(rooms, {})

    def test_runtime_agent_user_ids_filters_agent_roles(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text(
                "team:\n"
                "  members:\n"
                "    - matrixUserId: '@leader:matrix.local'\n"
                "      role: team_leader\n"
                "    - matrixUserId: '@worker:matrix.local'\n"
                "      role: worker\n"
                "    - matrixUserId: '@human:matrix.local'\n"
                "      role: coordinator\n",
                encoding="utf-8",
            )

            self.assertEqual(
                runtime_agent_user_ids(runtime),
                {"@leader:matrix.local", "@worker:matrix.local"},
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

    def test_runtime_agent_user_ids_normalizes_role_separators_and_case(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text(
                "team:\n"
                "  members:\n"
                "    - matrixUserId: '@a:matrix.local'\n"
                "      role: team-leader\n"
                "    - matrixUserId: '@b:matrix.local'\n"
                "      role: WORKER\n"
                "    - matrixUserId: '@c:matrix.local'\n"
                "      role: remote\n"
                "    - matrixUserId: '@d:matrix.local'\n"
                "      role: teammate\n",
                encoding="utf-8",
            )

            self.assertEqual(
                runtime_agent_user_ids(runtime),
                {"@a:matrix.local", "@b:matrix.local", "@c:matrix.local"},
            )

    def test_runtime_agent_user_ids_empty_when_no_team_section(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text("member:\n  matrixUserId: '@w:matrix.local'\n", encoding="utf-8")
            self.assertEqual(runtime_agent_user_ids(runtime), set())


class ReplyMentionTest(unittest.TestCase):
    def test_source_agent_reply_returns_agent_sender(self):
        event = {"sender": "@leader:matrix.local"}
        self.assertEqual(
            source_agent_reply_user_id(event, {"@leader:matrix.local"}, "@worker:matrix.local"),
            "@leader:matrix.local",
        )

    def test_source_agent_reply_suppressed_for_own_auto_and_human_senders(self):
        self.assertEqual(
            source_agent_reply_user_id({"sender": "@worker:matrix.local"}, {"@worker:matrix.local"}, "@worker:matrix.local"),
            "",
        )
        self.assertEqual(
            source_agent_reply_user_id(
                {"sender": "@leader:matrix.local", "auto_source_mention": True},
                {"@leader:matrix.local"},
                "@worker:matrix.local",
            ),
            "",
        )
        self.assertEqual(
            source_agent_reply_user_id({"sender": "@human:matrix.local"}, {"@leader:matrix.local"}, "@worker:matrix.local"),
            "",
        )

    def test_reply_mentions_source_even_when_answer_omits_its_id(self):
        event = {"event_id": "$delegation", "sender": "@leader:matrix.local", "body": "complete the task"}

        self.assertEqual(
            reply_mention_user_ids(
                event,
                "TASK_COMPLETED",
                {"@leader:matrix.local", "@worker:matrix.local"},
                "@worker:matrix.local",
            ),
            ["@leader:matrix.local"],
        )

    def test_reply_mentions_agents_visibly_named_in_the_answer(self):
        event = {"sender": "@human:matrix.local"}

        self.assertEqual(
            reply_mention_user_ids(
                event,
                "cc @leader:matrix.local and @worker2:matrix.local",
                {"@leader:matrix.local", "@worker2:matrix.local"},
                "@worker:matrix.local",
            ),
            ["@leader:matrix.local", "@worker2:matrix.local"],
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


class SafeFilenameTest(unittest.TestCase):
    def test_traversal_and_windows_separators_collapse_to_leaf(self):
        self.assertEqual(safe_filename("../../etc/passwd"), "passwd")
        self.assertEqual(safe_filename("C:\\dir\\file.txt"), "file.txt")
        self.assertEqual(safe_filename("  spaced name.txt  "), "spaced name.txt")

    def test_unsafe_characters_are_replaced(self):
        self.assertEqual(safe_filename("a<b>c:d\"e|f?g*h\x00i.bin"), "a_b_c_d_e_f_g_h_i.bin")

    def test_dot_only_names_fall_back_to_attachment_bin(self):
        self.assertEqual(safe_filename(""), "attachment.bin")
        self.assertEqual(safe_filename("."), "attachment.bin")
        self.assertEqual(safe_filename(".."), "attachment.bin")
        self.assertEqual(safe_filename("/"), "attachment.bin")

    def test_very_long_names_are_truncated_to_180(self):
        self.assertEqual(len(safe_filename("x" * 300)), 180)


class WorkspaceGuardTest(unittest.TestCase):
    def test_subdirectory_create_returns_resolved_pair(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            root, inbox = workspace_subdirectory(workspace, "inbox", create=True)

            self.assertEqual(root, workspace.resolve())
            self.assertEqual(inbox, (workspace / "inbox").resolve())
            self.assertTrue(inbox.is_dir())

    def test_symlink_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as outside:
            workspace = Path(directory)
            try:
                (workspace / "inbox").symlink_to(Path(outside), target_is_directory=True)
            except OSError as error:
                self.skipTest(f"directory symlinks are unavailable: {error}")

            with self.assertRaisesRegex(RuntimeError, "escapes the Workspace"):
                workspace_subdirectory(workspace, "inbox")


class FakeMatrixClient:
    def __init__(self, data=b"a,b\n1,2\n", content_type="text/csv") -> None:
        self.data = data
        self.content_type = content_type
        self.call = None

    def download_media(self, mxc_url, max_bytes):
        self.call = (mxc_url, max_bytes)
        return self.data, self.content_type


class AttachmentReceiveTest(unittest.TestCase):
    def test_attachment_is_written_under_workspace_inbox_with_safe_name(self):
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
            self.assertIn("文件", prompt)
            self.assertIn("text/csv", prompt)
            self.assertIn("outbox/", prompt)

    def test_image_attachment_prompt_uses_image_label_and_event_mimetype(self):
        event = {
            "room_id": "!team:matrix.local",
            "event_id": "$image",
            "sender": "@admin:matrix.local",
            "kind": "image",
            "body": "diagram.png",
            "filename": "diagram.png",
            "mxc_url": "mxc://matrix.local/image-id",
            "mimetype": "image/png",
        }

        with tempfile.TemporaryDirectory() as directory:
            client = FakeMatrixClient(data=b"png-bytes", content_type="application/octet-stream")
            prompt, saved_path = materialize_attachment(event, client, Path(directory), 1024)

            self.assertEqual(client.call, ("mxc://matrix.local/image-id", 1024))
            self.assertEqual(saved_path.name, "diagram.png")
            self.assertIn("图片", prompt)
            self.assertIn("image/png", prompt)

    def test_attachment_rejects_inbox_symlink_outside_workspace(self):
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


class OutboxTest(unittest.TestCase):
    def test_snapshot_outbox_missing_directory_is_empty(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(snapshot_outbox(Path(directory)), {})

    def test_snapshot_outbox_hashes_files_and_skips_symlinks(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as outside:
            workspace = Path(directory)
            outbox = workspace / "outbox"
            outbox.mkdir()
            (outbox / "a.txt").write_text("alpha", encoding="utf-8")
            (outbox / "sub").mkdir()
            (outbox / "sub" / "b.txt").write_text("beta", encoding="utf-8")
            outside_target = Path(outside) / "secret.txt"
            outside_target.write_text("secret", encoding="utf-8")
            try:
                (outbox / "link.txt").symlink_to(outside_target)
            except OSError as error:
                self.skipTest(f"symlinks are unavailable: {error}")

            self.assertEqual(
                snapshot_outbox(workspace),
                {
                    "a.txt": hashlib.sha256(b"alpha").hexdigest(),
                    "sub/b.txt": hashlib.sha256(b"beta").hexdigest(),
                },
            )

    def test_changed_outbox_files_reports_new_and_modified(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            outbox = workspace / "outbox"
            outbox.mkdir()
            (outbox / "old.txt").write_text("unchanged", encoding="utf-8")
            (outbox / "changed.txt").write_text("before", encoding="utf-8")
            before = snapshot_outbox(workspace)

            (outbox / "changed.txt").write_text("after", encoding="utf-8")
            (outbox / "result.png").write_bytes(b"png-result")

            self.assertEqual(changed_outbox_files(workspace, before), ["changed.txt", "result.png"])

    def test_workspace_output_path_rejects_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            (workspace / "outbox").mkdir()

            with self.assertRaisesRegex(RuntimeError, "escapes outbox"):
                workspace_output_path(workspace, "../state.json")

    def test_output_remote_path_builds_agent_prefix(self):
        with patch.dict("serve_bridge.os.environ", {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket/"}):
            self.assertEqual(
                output_remote_path("w", "result/notes.txt"),
                "agentteams/bucket/agents/w/workspace/outbox/result/notes.txt",
            )

        with patch.dict("serve_bridge.os.environ", {}, clear=True):
            with self.assertRaisesRegex(RuntimeError, "AGENTTEAMS_STORAGE_PREFIX is required"):
                output_remote_path("w", "x.txt")

    @patch("serve_bridge.mc")
    def test_sync_output_paths_copies_to_remote(self, mocked_mc):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            workspace = Path(directory)
            output = workspace / "outbox" / "result.txt"
            output.parent.mkdir()
            output.write_text("durable result", encoding="utf-8")

            sync_output_paths("w", workspace, ["result.txt"])

            mocked_mc.assert_called_once_with(
                "cp",
                str(output.resolve()),
                "agentteams/bucket/agents/w/workspace/outbox/result.txt",
            )

    @patch("serve_bridge.mc")
    def test_sync_output_paths_rejects_missing_file(self, mocked_mc):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            with self.assertRaisesRegex(RuntimeError, "Workspace output is unavailable"):
                sync_output_paths("w", Path(directory), ["missing.txt"])

            mocked_mc.assert_not_called()

    @patch("serve_bridge.mc")
    def test_pending_output_is_staged_and_can_be_restored_after_restart(self, mocked_mc):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            workspace = Path(directory)
            output = workspace / "outbox" / "result.txt"
            output.parent.mkdir()
            output.write_text("durable result", encoding="utf-8")

            sync_output_paths("w", workspace, ["result.txt"])
            output.unlink()

            def restore_copy(*args, **_kwargs):
                Path(args[2]).write_text("durable result", encoding="utf-8")

            mocked_mc.reset_mock()
            mocked_mc.side_effect = restore_copy
            restore_output_paths("w", workspace, ["result.txt"])

            self.assertEqual(output.read_text(encoding="utf-8"), "durable result")

    @patch("serve_bridge.mc")
    def test_restore_output_paths_skips_existing_files(self, mocked_mc):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            workspace = Path(directory)
            output = workspace / "outbox" / "result.txt"
            output.parent.mkdir()
            output.write_text("durable result", encoding="utf-8")

            restore_output_paths("w", workspace, ["result.txt"])

            mocked_mc.assert_not_called()
            self.assertEqual(output.read_text(encoding="utf-8"), "durable result")


class RecordingFileClient:
    def __init__(self) -> None:
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
        self.sent.append((room_id, path.name, reply_to, transaction_key, mentioned_user_ids, auto_source_mention))
        return "$uploaded"


class SendOutputsTest(unittest.TestCase):
    def test_send_output_paths_sorts_and_keys_transactions(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            outbox = workspace / "outbox"
            outbox.mkdir()
            (outbox / "b.txt").write_text("b", encoding="utf-8")
            (outbox / "a.txt").write_text("a", encoding="utf-8")

            client = RecordingFileClient()
            sent = send_output_paths(client, "!t:m", "$task", workspace, ["b.txt", "a.txt"], ["@leader:m"], True)

            self.assertEqual([path.name for path in sent], ["a.txt", "b.txt"])
            self.assertEqual(client.sent[0][3], "$task:file:a.txt")
            self.assertEqual(client.sent[1][3], "$task:file:b.txt")
            self.assertTrue(all(call[2] == "$task" for call in client.sent))
            self.assertTrue(all(call[4] == ["@leader:m"] for call in client.sent))
            self.assertTrue(all(call[5] is True for call in client.sent))

    def test_send_output_paths_rejects_missing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            (workspace / "outbox").mkdir()
            client = RecordingFileClient()

            with self.assertRaisesRegex(RuntimeError, "Workspace output is unavailable"):
                send_output_paths(client, "!t:m", "$task", workspace, ["missing.txt"])

            self.assertEqual(client.sent, [])

    def test_only_new_or_changed_outbox_files_are_sent(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            outbox = workspace / "outbox"
            outbox.mkdir()
            (outbox / "old.txt").write_text("unchanged", encoding="utf-8")
            (outbox / "changed.txt").write_text("before", encoding="utf-8")
            before = snapshot_outbox(workspace)

            (outbox / "changed.txt").write_text("after", encoding="utf-8")
            (outbox / "result.png").write_bytes(b"png-result")
            client = RecordingFileClient()
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


class ServeClientTest(unittest.TestCase):
    def test_health_reports_http_status(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")

        with patch("urllib.request.urlopen", return_value=FakeResponse(b"{}")):
            self.assertTrue(client.health())

        def not_found(request, timeout=None):
            raise http_error(request, 404)

        with patch("urllib.request.urlopen", side_effect=not_found):
            self.assertFalse(client.health())

    def test_status_decodes_json_and_empty_on_error(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")

        with patch(
            "urllib.request.urlopen",
            return_value=FakeResponse(json.dumps({"hasActivePrompt": True}).encode("utf-8")),
        ):
            self.assertEqual(client.status("s1"), {"hasActivePrompt": True})

        def not_found(request, timeout=None):
            raise http_error(request, 404)

        with patch("urllib.request.urlopen", side_effect=not_found):
            self.assertEqual(client.status("s1"), {})

    def test_create_or_load_prefers_create_then_falls_back_to_load(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        seen = []

        def urlopen(request, timeout=None):
            seen.append(request)
            if request.full_url.endswith("/session"):
                raise http_error(request, 404)
            return FakeResponse(b"{}")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            client.create_or_load("s1", "/workspace")

        self.assertTrue(seen[0].full_url.endswith("/session"))
        self.assertTrue(seen[1].full_url.endswith("/session/s1/load"))
        self.assertEqual(seen[0].get_method(), "POST")
        self.assertEqual(json.loads(seen[0].data), {"sessionId": "s1", "cwd": "/workspace"})

    def test_create_or_load_raises_when_both_attempts_fail(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")

        def conflict(request, timeout=None):
            raise http_error(request, 409)

        with patch("urllib.request.urlopen", side_effect=conflict):
            with self.assertRaisesRegex(RuntimeError, "create/load failed \\(http 409\\)"):
                client.create_or_load("s1", "/workspace")

    def test_pending_permissions_filters_non_dict_items(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        body = {"pendingInteractions": [{"requestId": "r1"}, "junk", 42, {"requestId": "r2"}]}

        with patch("urllib.request.urlopen", return_value=FakeResponse(json.dumps(body).encode("utf-8"))):
            self.assertEqual(
                client.pending_permissions("s1"),
                [{"requestId": "r1"}, {"requestId": "r2"}],
            )

        with patch("urllib.request.urlopen", return_value=FakeResponse(b"{}")):
            self.assertEqual(client.pending_permissions("s1"), [])

    def test_vote_posts_first_option_and_reports_settlement(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        seen = []

        def urlopen(request, timeout=None):
            seen.append(request)
            return FakeResponse(b"{}")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            self.assertTrue(client.vote("s1", "r1", "allow"))

        request = seen[0]
        self.assertTrue(request.full_url.endswith("/session/s1/permission/r1"))
        self.assertEqual(json.loads(request.data), {"outcome": {"outcome": "selected", "optionId": "allow"}})

        def already_settled(request, timeout=None):
            raise http_error(request, 404)

        with patch("urllib.request.urlopen", side_effect=already_settled):
            self.assertFalse(client.vote("s1", "r1", "allow"))

    def test_prompt_accepts_accepted_codes_and_rejects_400(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        seen = []

        def urlopen(request, timeout=None):
            seen.append(request)
            return FakeResponse(b"{}", status=202)

        with patch("urllib.request.urlopen", side_effect=urlopen):
            client.prompt("s1", "do it")

        self.assertTrue(seen[0].full_url.endswith("/session/s1/prompt"))
        self.assertEqual(json.loads(seen[0].data), {"prompt": [{"type": "text", "text": "do it"}]})

        def rejected(request, timeout=None):
            raise http_error(request, 400)

        with patch("urllib.request.urlopen", side_effect=rejected):
            with self.assertRaisesRegex(RuntimeError, "serve prompt rejected \\(http 400\\)"):
                client.prompt("s1", "do it")

    def test_collect_answer_streams_chunks_until_turn_idle(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        status_bodies = [{"hasActivePrompt": True}, {"hasActivePrompt": False}]
        sse = b"".join(
            (f"data: {json.dumps(obj)}\n\n").encode("utf-8")
            for obj in (
                {"data": {"update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "Hello "}}}},
                {"data": {"update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "world"}}}},
            )
        )
        seen = []

        def urlopen(request, timeout=None):
            url = request.full_url
            seen.append(url)
            if url.endswith("/status"):
                body = status_bodies.pop(0) if len(status_bodies) > 1 else status_bodies[0]
                return FakeResponse(json.dumps(body).encode("utf-8"))
            if url.endswith("/events"):
                return FakeResponse(sse)
            return FakeResponse(b"{}")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            answer = client.collect_answer("s1", Path("/workspace"), 30, stop=threading.Event())

        self.assertEqual(answer, "Hello world")
        self.assertTrue(any(url.endswith("/events") for url in seen))

    def test_collect_answer_votes_first_option_on_permission_prompt(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        pending = {
            "hasActivePrompt": True,
            "pendingInteractions": [{"requestId": "r1", "options": [{"optionId": "deny"}, {"optionId": "allow"}]}],
        }
        # The first loop iteration calls status() twice (summary +
        # pending_permissions); the permission must still be visible on both,
        # then the turn settles before the next iteration.
        status_bodies = [pending, dict(pending), {"hasActivePrompt": False}]
        sse = (
            "data: "
            + json.dumps({"data": {"update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "ok"}}}})
            + "\n\n"
        ).encode("utf-8")
        votes = []

        def urlopen(request, timeout=None):
            url = request.full_url
            if url.endswith("/permission/r1"):
                votes.append(request)
                return FakeResponse(b"{}")
            if url.endswith("/status"):
                body = status_bodies.pop(0) if len(status_bodies) > 1 else status_bodies[0]
                return FakeResponse(json.dumps(body).encode("utf-8"))
            if url.endswith("/events"):
                return FakeResponse(sse)
            return FakeResponse(b"{}")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            answer = client.collect_answer("s1", Path("/workspace"), 30, stop=threading.Event())

        self.assertEqual(answer, "ok")
        self.assertEqual(len(votes), 1)
        self.assertEqual(json.loads(votes[0].data), {"outcome": {"outcome": "selected", "optionId": "deny"}})

    def test_collect_answer_reports_turn_error(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        status_bodies = [{"hasActivePrompt": True, "hasTurnError": True, "turnError": "model 500"}]

        def urlopen(request, timeout=None):
            if request.full_url.endswith("/status"):
                body = status_bodies.pop(0) if len(status_bodies) > 1 else status_bodies[0]
                return FakeResponse(json.dumps(body).encode("utf-8"))
            return FakeResponse(b"{}")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            with self.assertRaisesRegex(RuntimeError, "qwen-code turn failed: model 500"):
                client.collect_answer("s1", Path("/workspace"), 30, stop=threading.Event())

    def test_collect_answer_times_out_when_turn_never_finishes(self):
        client = ServeClient("http://127.0.0.1:8088", "tok")
        status_bodies = [{"hasActivePrompt": True}]

        def urlopen(request, timeout=None):
            if request.full_url.endswith("/status"):
                body = status_bodies.pop(0) if len(status_bodies) > 1 else status_bodies[0]
                return FakeResponse(json.dumps(body).encode("utf-8"))
            return FakeResponse(b"")

        with patch("urllib.request.urlopen", side_effect=urlopen):
            with self.assertRaises(TimeoutError):
                client.collect_answer("s1", Path("/workspace"), 0, stop=threading.Event())

    def test_run_serve_drives_create_prompt_collect(self):
        serve = MagicMock()
        serve.collect_answer.return_value = "final answer"
        stop = threading.Event()

        answer = run_serve("task", Path("/workspace"), 300, session_id="s1", resume=True, client=serve, stop=stop)

        self.assertEqual(answer, "final answer")
        serve.create_or_load.assert_called_once_with("s1", "/workspace")
        serve.prompt.assert_called_once_with("s1", "task")
        serve.collect_answer.assert_called_once_with("s1", Path("/workspace"), 300, stop=stop)


class HealthHandlerTest(unittest.TestCase):
    def _handler(self, path: str) -> HealthHandler:
        handler = HealthHandler.__new__(HealthHandler)
        handler.path = path
        handler.command = "GET"
        handler.request_version = "HTTP/1.1"
        handler.protocol_version = "HTTP/1.0"
        handler.requestline = f"GET {path} HTTP/1.1"
        handler.client_address = ("127.0.0.1", 0)
        handler._headers_buffer = []
        handler.wfile = io.BytesIO()
        return handler

    def test_healthz_returns_ok_json(self):
        handler = self._handler("/healthz")
        handler.do_GET()

        payload = handler.wfile.getvalue()
        self.assertIn(b"200 OK", payload)
        self.assertIn(b'"ok":true', payload)
        self.assertIn(b'"runtime":"qwen-code"', payload)
        self.assertIn(b"Content-Type: application/json", payload)

    def test_unknown_path_returns_404(self):
        handler = self._handler("/nope")
        handler.do_GET()

        payload = handler.wfile.getvalue()
        self.assertIn(b"404 Not Found", payload)
        self.assertNotIn(b'"ok":true', payload)


class RuntimeStateSyncTest(unittest.TestCase):
    def test_push_runtime_state_copies_to_agent_prefix(self):
        with tempfile.TemporaryDirectory() as directory, patch("serve_bridge.mc") as mocked_mc, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket/"},
        ):
            state_path = Path(directory) / "matrix-bridge-state.json"
            state_path.write_text("{}", encoding="utf-8")

            push_runtime_state("w", state_path)

            mocked_mc.assert_called_once_with(
                "cp",
                str(state_path),
                "agentteams/bucket/agents/w/runtime/matrix-bridge-state.json",
            )

    def test_refresh_runtime_config_replaces_when_changed(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text("member:\n  matrixUserId: '@w:m'\n", encoding="utf-8")

            def fake_cp(*args, **_kwargs):
                Path(args[2]).write_text("member:\n  matrixUserId: '@w:m'\n  personalRoomId: '!p:m'\n", encoding="utf-8")

            with patch("serve_bridge.mc", side_effect=fake_cp) as mocked_mc:
                changed = refresh_runtime_config("w", runtime)

            self.assertTrue(changed)
            self.assertIn("personalRoomId", runtime.read_text(encoding="utf-8"))
            self.assertFalse(runtime.with_suffix(".yaml.remote").exists())
            self.assertEqual(mocked_mc.call_args.args[0], "cp")
            self.assertEqual(mocked_mc.call_args.args[1], "agentteams/bucket/agents/w/runtime/runtime.yaml")

    def test_refresh_runtime_config_noop_when_unchanged(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            runtime = Path(directory) / "runtime.yaml"
            original = "member:\n  matrixUserId: '@w:m'\n"
            runtime.write_text(original, encoding="utf-8")

            def fake_cp(*args, **_kwargs):
                Path(args[2]).write_text(original, encoding="utf-8")

            with patch("serve_bridge.mc", side_effect=fake_cp):
                changed = refresh_runtime_config("w", runtime)

            self.assertFalse(changed)
            self.assertEqual(runtime.read_text(encoding="utf-8"), original)
            self.assertFalse(runtime.with_suffix(".yaml.remote").exists())

    def test_refresh_runtime_config_rejects_empty_remote(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            "serve_bridge.os.environ",
            {"AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            runtime = Path(directory) / "runtime.yaml"
            runtime.write_text("member:\n  matrixUserId: '@w:m'\n", encoding="utf-8")

            with patch("serve_bridge.mc", side_effect=lambda *args, **kwargs: None):
                with self.assertRaisesRegex(RuntimeError, "runtime config is empty"):
                    refresh_runtime_config("w", runtime)

    def test_sync_sessions_mirrors_when_present(self):
        with tempfile.TemporaryDirectory() as directory, patch("serve_bridge.mc") as mocked_mc, patch.dict(
            "serve_bridge.os.environ",
            {"QWEN_HOME": directory, "AGENTTEAMS_STORAGE_PREFIX": "agentteams/bucket"},
        ):
            Path(directory, "sessions").mkdir()

            sync_sessions("w")

            mocked_mc.assert_called_once_with(
                "mirror",
                f"{Path(directory)}/sessions/",
                "agentteams/bucket/agents/w/.qc/sessions/",
                "--overwrite",
            )

    def test_sync_sessions_noop_without_sessions_dir(self):
        with tempfile.TemporaryDirectory() as directory, patch("serve_bridge.mc") as mocked_mc, patch.dict(
            "serve_bridge.os.environ",
            {"QWEN_HOME": directory},
        ):
            sync_sessions("w")
            mocked_mc.assert_not_called()


if __name__ == "__main__":
    unittest.main()
