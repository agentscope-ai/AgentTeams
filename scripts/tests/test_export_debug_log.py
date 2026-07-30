#!/usr/bin/env python3

import importlib.util
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "export-debug-log.py"
SPEC = importlib.util.spec_from_file_location("export_debug_log", SCRIPT)
assert SPEC and SPEC.loader
export_debug_log = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(export_debug_log)


class RedactJsonStringsTest(unittest.TestCase):
    def test_redacts_values_of_secret_named_fields(self):
        data = {
            "password": "short-secret",
            "nested": [{"apiKey": "brief", "label": "visible"}],
        }

        redacted = export_debug_log.redact_json_strings(data)

        self.assertEqual(redacted["password"], "****")
        self.assertEqual(redacted["nested"][0]["apiKey"], "****")
        self.assertEqual(redacted["nested"][0]["label"], "visible")


class FormatEventTest(unittest.TestCase):
    def test_redacts_all_matrix_event_fields(self):
        event = {
            "event_id": "$event",
            "type": "m.room.member",
            "sender": "@13800138000:matrix.example",
            "origin_server_ts": 1_700_000_000_000,
            "content": {
                "displayname": "alice@example.com",
                "avatar_url": "http://192.0.2.10/avatar.png",
                "auth_token": "syt_abcdefghijklmnopqrstuvwxyz",
            },
        }

        record = export_debug_log.format_event(event, redact=True)

        self.assertEqual(record["sender"], "@****:matrix.example")
        self.assertEqual(record["content"]["displayname"], "****")
        self.assertEqual(record["content"]["avatar_url"], "http://****/avatar.png")
        self.assertEqual(record["content"]["auth_token"], "****")

    def test_preserves_all_matrix_event_fields_when_redaction_is_disabled(self):
        event = {
            "event_id": "$event",
            "type": "m.room.message",
            "sender": "@alice:matrix.example",
            "origin_server_ts": 1_700_000_000_000,
            "content": {
                "msgtype": "m.image",
                "body": "alice@example.com",
                "url": "http://192.0.2.10/image.png",
                "m.relates_to": {"token": "syt_abcdefghijklmnopqrstuvwxyz"},
            },
        }

        record = export_debug_log.format_event(event, redact=False)

        self.assertEqual(record["body"], "alice@example.com")
        self.assertEqual(record["url"], "http://192.0.2.10/image.png")
        self.assertEqual(
            record["relates_to"]["token"],
            "syt_abcdefghijklmnopqrstuvwxyz",
        )

    def test_redacts_message_metadata(self):
        event = {
            "event_id": "$event",
            "type": "m.room.message",
            "sender": "@alice:matrix.example",
            "origin_server_ts": 1_700_000_000_000,
            "content": {
                "msgtype": "m.image",
                "body": "alice@example.com",
                "url": "http://192.0.2.10/image.png",
                "m.relates_to": {"token": "syt_abcdefghijklmnopqrstuvwxyz"},
            },
        }

        record = export_debug_log.format_event(event, redact=True)

        self.assertEqual(record["body"], "****")
        self.assertEqual(record["url"], "http://****/image.png")
        self.assertEqual(record["relates_to"]["token"], "****")


if __name__ == "__main__":
    unittest.main()
