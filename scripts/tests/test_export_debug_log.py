#!/usr/bin/env python3

import importlib.util
import json
import subprocess
import unittest
from pathlib import Path
from unittest import mock


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


class ExportContainerLogsTest(unittest.TestCase):
    def test_exports_container_state_and_redacted_logs(self):
        def run_docker(command, **_kwargs):
            if command[1:3] == ["ps", "-a"]:
                return subprocess.CompletedProcess(command, 0, "agentteams-worker-alice\n", "")
            if command[1:3] == ["inspect", "--format={{json .State}}"]:
                return subprocess.CompletedProcess(
                    command,
                    0,
                    '{"Status":"running","ExitCode":0,"OOMKilled":false}\n',
                    "",
                )
            if command[1:3] == ["inspect", "--format={{json .Config.Image}}"]:
                return subprocess.CompletedProcess(command, 0, '"agentteams/qwenpaw-worker:test"\n', "")
            if command[1:3] == ["inspect", "--format={{.RestartCount}}"]:
                return subprocess.CompletedProcess(command, 0, "2\n", "")
            if command[1] == "logs":
                return subprocess.CompletedProcess(
                    command,
                    0,
                    "qwenpaw app exited api_key=secret-value\n",
                    "",
                )
            raise AssertionError(f"unexpected docker command: {command}")

        with mock.patch.object(export_debug_log.subprocess, "run", side_effect=run_docker):
            from tempfile import TemporaryDirectory

            with TemporaryDirectory() as tmp:
                out_dir = Path(tmp)
                count = export_debug_log.export_container_logs(
                    out_dir,
                    since_epoch=1_700_000_000,
                    redact=True,
                    container_filter="worker-alice",
                )

                self.assertEqual(count, 1)
                state = json.loads((out_dir / "agentteams-worker-alice.state.json").read_text())
                self.assertEqual(state["restart_count"], 2)
                self.assertEqual(state["image"], "agentteams/qwenpaw-worker:test")
                self.assertEqual(state["state"]["Status"], "running")
                self.assertEqual(
                    (out_dir / "agentteams-worker-alice.log").read_text(),
                    "qwenpaw app exited api_key=****\n",
                )


if __name__ == "__main__":
    unittest.main()
