import json
import re
import sys
import tempfile
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))

from matrix_channel import (  # noqa: E402
    MatrixClient,
    matrix_events,
    matrix_transaction_id,
    random_transaction_key,
    text_events,
    visible_matrix_user_ids,
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
    return urllib.error.HTTPError(request.full_url, code, "Not Found", None, None)


class MatrixEventsTest(unittest.TestCase):
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

    def test_ignores_blank_and_redacted_events(self):
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

    def test_team_agent_message_requires_a_structured_mention(self):
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

    def test_unmentioned_team_human_is_quiet_but_personal_remains_conversational(self):
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

    def test_image_and_file_events_are_extracted_with_media_fields(self):
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
        self.assertEqual(events[0]["mxc_url"], "mxc://matrix.local/image-id")
        self.assertEqual(events[1]["mxc_url"], "mxc://matrix.local/file-id")
        self.assertEqual(events[1]["mimetype"], "text/csv")
        self.assertEqual(events[1]["filename"], "../../budget.csv")

    def test_non_text_event_without_mxc_url_is_dropped(self):
        payload = {
            "rooms": {
                "join": {
                    "!team:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "event_id": "$bad-url",
                                    "sender": "@admin:matrix.local",
                                    "type": "m.room.message",
                                    "content": {
                                        "msgtype": "m.image",
                                        "body": "broken.png",
                                        "url": "https://example.com/broken.png",
                                        "m.mentions": {"user_ids": ["@dsh:matrix.local"]},
                                    },
                                }
                            ]
                        }
                    }
                }
            }
        }

        self.assertEqual(matrix_events(payload, {"!team:matrix.local": "team"}, "@dsh:matrix.local"), [])

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


class MentionDetectionTest(unittest.TestCase):
    def test_visible_known_agent_ids_become_matrix_mentions(self):
        allowed = {"@leader:matrix.local", "@worker:matrix.local"}

        self.assertEqual(
            visible_matrix_user_ids("@leader:matrix.local，任务完成", allowed),
            ["@leader:matrix.local"],
        )
        self.assertEqual(visible_matrix_user_ids("email@leader:matrix.local", allowed), [])

    def test_visible_known_agent_ids_accept_punctuation_but_not_ports_or_longer_ids(self):
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

    def test_multiple_known_ids_are_returned_sorted(self):
        allowed = {"@b:matrix.local", "@a:matrix.local"}
        self.assertEqual(
            visible_matrix_user_ids("ping @b:matrix.local then @a:matrix.local", allowed),
            ["@a:matrix.local", "@b:matrix.local"],
        )


class TransactionIdTest(unittest.TestCase):
    def test_random_transaction_key_is_32_hex_chars(self):
        self.assertRegex(random_transaction_key(), r"^[0-9a-f]{32}$")
        self.assertNotEqual(random_transaction_key(), random_transaction_key())

    def test_matrix_transaction_is_stable_for_the_same_source_event(self):
        first = matrix_transaction_id("$task:reply")
        after_restart = matrix_transaction_id("$task:reply")

        self.assertEqual(after_restart, first)
        self.assertRegex(first, r"^dsh-[0-9a-f]{48}$")
        self.assertNotEqual(matrix_transaction_id("$other:reply"), first)


class MatrixClientTextTest(unittest.TestCase):
    def test_send_text_builds_reply_content_with_sorted_mentions(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "send_content", return_value="$reply") as send_content:
            result = client.send_text(
                "!team:matrix.local",
                "@leader:matrix.local，任务完成",
                "$task",
                "$task:reply",
                ["@leader:matrix.local", "@worker:matrix.local", "@leader:matrix.local"],
            )

        self.assertEqual(result, "$reply")
        room_id, content, transaction_key = send_content.call_args.args
        self.assertEqual(room_id, "!team:matrix.local")
        self.assertEqual(content["msgtype"], "m.text")
        self.assertEqual(content["m.relates_to"], {"m.in_reply_to": {"event_id": "$task"}})
        self.assertEqual(content["m.mentions"], {"user_ids": ["@leader:matrix.local", "@worker:matrix.local"]})
        self.assertEqual(transaction_key, "$task:reply")

    def test_send_text_omits_optional_fields_when_absent(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "send_content", return_value="$reply") as send_content:
            client.send_text("!personal:matrix.local", "plain answer")

        _room_id, content, _transaction_key = send_content.call_args.args
        self.assertNotIn("m.relates_to", content)
        self.assertNotIn("m.mentions", content)
        self.assertNotIn("com.agentteams.auto_source_mention", content)

    def test_send_text_marks_an_automatically_targeted_source_agent(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "send_content", return_value="$reply") as send_content:
            client.send_text("!team:matrix.local", "TASK_COMPLETED", "$delegation", "$delegation:reply", ["@leader:matrix.local"], True)

        content = send_content.call_args.args[1]
        self.assertTrue(content["com.agentteams.auto_source_mention"])

    def test_send_content_uses_deterministic_transaction_path(self):
        client = MatrixClient("http://matrix.local/", "token")
        with patch.object(client, "request", return_value={"event_id": "$sent"}) as request:
            event_id = client.send_content("!room:matrix.local", {"msgtype": "m.text", "body": "hi"}, "key-1")

        self.assertEqual(event_id, "$sent")
        method, path, body = request.call_args.args
        self.assertEqual(method, "PUT")
        self.assertIn("/_matrix/client/v3/rooms/%21room%3Amatrix.local/send/m.room.message/", path)
        self.assertEqual(path.split("/")[-1], matrix_transaction_id("key-1"))
        self.assertEqual(body, {"msgtype": "m.text", "body": "hi"})

    def test_sync_omits_since_until_the_first_batch(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch.object(client, "request", return_value={"next_batch": "t_1"}) as request:
            client.sync(None, 30_000)

        _method, path = request.call_args.args[:2]
        self.assertIn("timeout=30000", path)
        self.assertNotIn("since=", path)

        with patch.object(client, "request", return_value={"next_batch": "t_2"}) as request:
            client.sync("t_1", 30_000)

        _method, path = request.call_args.args[:2]
        self.assertIn("since=t_1", path)


class MatrixClientSendFileTest(unittest.TestCase):
    def test_returned_file_is_sent_as_image_when_mimetype_matches(self):
        client = MatrixClient("http://matrix.local", "token")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.png"
            path.write_bytes(b"png-bytes")
            with (
                patch.object(client, "upload_media", return_value=("mxc://matrix.local/result", "image/png", 9)),
                patch.object(client, "send_content", return_value="$file") as send_content,
            ):
                event_id = client.send_file("!team:matrix.local", path, "$delegation", "$delegation:file:result.png", ["@leader:matrix.local"], True)

        self.assertEqual(event_id, "$file")
        content = send_content.call_args.args[1]
        self.assertEqual(content["msgtype"], "m.image")
        self.assertEqual(content["url"], "mxc://matrix.local/result")
        self.assertEqual(content["info"], {"mimetype": "image/png", "size": 9})
        self.assertEqual(content["m.mentions"], {"user_ids": ["@leader:matrix.local"]})
        self.assertTrue(content["com.agentteams.auto_source_mention"])

    def test_returned_file_is_sent_as_file_when_mimetype_is_not_image(self):
        client = MatrixClient("http://matrix.local", "token")
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "notes.txt"
            path.write_text("done", encoding="utf-8")
            with (
                patch.object(client, "upload_media", return_value=("mxc://matrix.local/notes", "text/plain", 4)),
                patch.object(client, "send_content", return_value="$file") as send_content,
            ):
                client.send_file("!team:matrix.local", path, "$delegation", "$delegation:file:notes.txt")

        content = send_content.call_args.args[1]
        self.assertEqual(content["msgtype"], "m.file")
        self.assertNotIn("m.mentions", content)
        self.assertNotIn("com.agentteams.auto_source_mention", content)


class MatrixClientMediaTest(unittest.TestCase):
    def test_download_media_preferred_v1_path_and_content_type(self):
        client = MatrixClient("http://matrix.local", "token")
        seen = []

        def fake_urlopen(request, timeout=None):
            seen.append(request)
            return FakeResponse(b"abc", {"Content-Type": "text/plain"})

        with patch("urllib.request.urlopen", side_effect=fake_urlopen):
            data, content_type = client.download_media("mxc://matrix.local/img id", 1024)

        self.assertEqual((data, content_type), (b"abc", "text/plain"))
        self.assertEqual(len(seen), 1)
        self.assertIn("/_matrix/client/v1/media/download/matrix.local/img%20id", seen[0].full_url)
        self.assertEqual(seen[0].get_header("Authorization"), "Bearer token")

    def test_download_media_falls_back_to_legacy_v3_path_when_v1_missing(self):
        client = MatrixClient("http://matrix.local", "token")
        seen = []

        def fake_urlopen(request, timeout=None):
            seen.append(request)
            if len(seen) == 1:
                raise http_error(request, 404)
            return FakeResponse(b"legacy", {})

        with patch("urllib.request.urlopen", side_effect=fake_urlopen):
            data, content_type = client.download_media("mxc://matrix.local/abc", 1024)

        self.assertEqual((data, content_type), (b"legacy", "application/octet-stream"))
        self.assertIn("/_matrix/media/v3/download/matrix.local/abc", seen[1].full_url)

    def test_download_media_rejects_announced_oversized_attachment(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch(
            "urllib.request.urlopen",
            return_value=FakeResponse(b"", {"Content-Length": "1048576"}),
        ):
            with self.assertRaisesRegex(RuntimeError, "exceeds 1024 bytes"):
                client.download_media("mxc://matrix.local/big", 1024)

    def test_download_media_rejects_payload_longer_than_limit(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch("urllib.request.urlopen", return_value=FakeResponse(b"x" * 2048, {})):
            with self.assertRaisesRegex(RuntimeError, "exceeds 1024 bytes"):
                client.download_media("mxc://matrix.local/big", 1024)

    def test_download_path_rejects_invalid_media_urls(self):
        client = MatrixClient("http://matrix.local", "token")
        for url in ("http://matrix.local/abc", "mxc:///abc", "mxc://matrix.local/"):
            with self.assertRaisesRegex(RuntimeError, "invalid Matrix media URL"):
                client._download_path(url, "/_matrix/client/v1/media/download")

    def test_upload_media_returns_content_uri_and_inferred_mimetype(self):
        client = MatrixClient("http://matrix.local", "token")
        seen = []

        def fake_urlopen(request, timeout=None):
            seen.append(request)
            return FakeResponse(json.dumps({"content_uri": "mxc://matrix.local/up"}).encode("utf-8"), {})

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.png"
            path.write_bytes(b"png-bytes")
            with patch("urllib.request.urlopen", side_effect=fake_urlopen):
                content_uri, mimetype, size = client.upload_media(path)

        self.assertEqual((content_uri, mimetype, size), ("mxc://matrix.local/up", "image/png", 9))
        request = seen[0]
        self.assertEqual(request.get_method(), "POST")
        self.assertIn("/_matrix/client/v1/media/upload?filename=result.png", request.full_url)
        self.assertEqual(request.headers.get("Content-type"), "image/png")
        self.assertEqual(request.headers.get("Content-length"), "9")

    def test_upload_media_falls_back_to_legacy_v3_path_when_v1_missing(self):
        client = MatrixClient("http://matrix.local", "token")
        seen = []

        def fake_urlopen(request, timeout=None):
            seen.append(request)
            if len(seen) == 1:
                raise http_error(request, 405)
            return FakeResponse(b'{"content_uri": "mxc://matrix.local/up"}', {})

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "a.txt"
            path.write_text("x", encoding="utf-8")
            with patch("urllib.request.urlopen", side_effect=fake_urlopen):
                content_uri, _mimetype, _size = client.upload_media(path)

        self.assertEqual(content_uri, "mxc://matrix.local/up")
        self.assertIn("/_matrix/media/v3/upload?filename=a.txt", seen[1].full_url)

    def test_upload_media_rejects_response_without_content_uri(self):
        client = MatrixClient("http://matrix.local", "token")
        with patch("urllib.request.urlopen", return_value=FakeResponse(b"{}", {})):
            with tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "a.txt"
                path.write_text("x", encoding="utf-8")
                with self.assertRaisesRegex(RuntimeError, "no content_uri"):
                    client.upload_media(path)


if __name__ == "__main__":
    unittest.main()
