"""Exercise durable input evidence and receipt delivery without a model response."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from agentteams_receipts import Receipts, capture_audience, scan_inputs


class HarnessReceiptTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.receipts = Receipts(self.root)
        self.receipts.enqueue("!room:hs", "$input", "received")

    def states(self):
        with self.receipts.connect() as db:
            return set(db.execute("SELECT event,status FROM receipts").fetchall())

    def test_outbox_connections_close_after_each_transaction(self):
        with self.receipts.connect() as db:
            db.execute("SELECT 1")
        with self.assertRaises(sqlite3.ProgrammingError):
            db.execute("SELECT 1")

    def test_receipts_do_not_evict_messages_from_the_polling_window(self):
        events = [{"type": "com.agentteams.session_receipt", "event_id": f"$receipt-{i}"} for i in range(25)]
        events.append({"type": "m.room.message", "event_id": "$completed", "content": {"body": "TASK_COMPLETED"}})

        class Matrix(BaseHTTPRequestHandler):
            def do_GET(self):
                query = parse_qs(urlsplit(self.path).query)
                event_filter = json.loads(query.get("filter", ["{}"])[0])
                visible = [event for event in events if event["type"] not in event_filter.get("not_types", [])]
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps({"chunk": visible[:int(query["limit"][0])]}).encode())

            def log_message(self, *_args):
                pass

        with ThreadingHTTPServer(("127.0.0.1", 0), Matrix) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                helper = Path(__file__).resolve().parent / "lib/matrix-client.sh"
                result = subprocess.run(["bash", "-c", '''
                    source "$1"
                    exec_in_manager() { "$@"; }
                    TEST_MATRIX_DIRECT_URL="$MATRIX_FIXTURE_URL"
                    matrix_read_messages test-only '!room:hs' 20
                ''', "fixture", str(helper)], env={**os.environ,
                    "TEST_CONTROLLER_CONTAINER": "fixture", "TEST_AGENT_CONTAINER": "fixture",
                    "MATRIX_FIXTURE_URL": f"http://127.0.0.1:{server.server_port}"},
                    capture_output=True, text=True, check=True)
                self.assertEqual([event["event_id"] for event in json.loads(result.stdout)["chunk"]], ["$completed"])
            finally:
                server.shutdown()
                thread.join()
    def test_queue_and_body_claims_are_not_read_evidence(self):
        path = self.root / ".openclaw/agents/main/sessions/one.jsonl"
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps({"message": {"role": "user", "content": "$input was read"}}) + "\n")
        scan_inputs(self.receipts, "openclaw", {})
        self.assertEqual(self.states(), {("$input", "received")})

    def test_openclaw_only_committed_native_user_rows_acknowledge(self):
        path = self.root / ".openclaw/agents/main/sessions/one.jsonl"
        path.parent.mkdir(parents=True)
        row = {"message": {"role": "user", "__openclaw": {"transport": {"channel": "matrix", "messageId": "$input"}}}}
        path.write_text(json.dumps(row))
        offsets = {}
        scan_inputs(self.receipts, "openclaw", offsets)
        self.assertNotIn(("$input", "read"), self.states())
        with path.open("a") as stream:
            stream.write("\n")
        scan_inputs(self.receipts, "openclaw", offsets)
        self.assertIn(("$input", "read"), self.states())

    def test_native_commit_before_matrix_sync_is_reconciled(self):
        self.receipts.committed(["$later"])
        self.receipts.enqueue("!room:hs", "$later", "received")
        self.assertIn(("$later", "read"), self.states())

    def test_openclaw_compressed_native_sqlite_rows(self):
        major = int(subprocess.check_output(["node", "-p", "process.versions.node.split('.')[0]"], text=True))
        if major < 24:
            self.skipTest("The pinned OpenClaw Harness uses Node 24")
        path = self.root / ".openclaw/agents/main/agent/openclaw-agent.sqlite"
        path.parent.mkdir(parents=True)
        subprocess.run(["node", "--input-type=module", "-e", """
            import { DatabaseSync } from 'node:sqlite';
            import { zstdCompressSync } from 'node:zlib';
            const db = new DatabaseSync(process.argv[1]);
            db.exec('CREATE TABLE transcript_events(event_json TEXT,event_zstd BLOB)');
            const entry = {message:{role:'user',__openclaw:{transport:{channel:'matrix',messageId:'$input'}}}};
            db.prepare('INSERT INTO transcript_events VALUES(NULL,?)').run(zstdCompressSync(JSON.stringify(entry)));
            db.close();
        """, str(path)], check=True, capture_output=True)
        scan_inputs(self.receipts, "openclaw", {})
        self.assertIn(("$input", "read"), self.states())

    def test_hermes_database_commit_is_the_boundary_and_history_ids_are_exact(self):
        path = self.root / ".hermes/state.db"
        path.parent.mkdir(parents=True)
        with sqlite3.connect(path) as db:
            db.executescript("CREATE TABLE sessions(id TEXT,source TEXT); CREATE TABLE messages(session_id TEXT,role TEXT,platform_message_id TEXT);")
            db.execute("INSERT INTO sessions VALUES('session','matrix')")
        self.receipts.link("!room:hs", "$input", ["$retained"])
        connection = sqlite3.connect(path)
        self.addCleanup(connection.close)
        connection.execute("INSERT INTO messages VALUES('session','user','$input')")
        scan_inputs(self.receipts, "hermes", {})
        self.assertNotIn(("$input", "read"), self.states())
        connection.commit()
        scan_inputs(self.receipts, "hermes", {})
        self.assertTrue({("$input", "read"), ("$retained", "read")} <= self.states())
        self.assertNotIn(("$truncated", "read"), self.states())

    def test_managed_hermes_read_waits_for_matching_common_native_fact(self):
        path = self.root / ".hermes/state.db"
        path.parent.mkdir(parents=True)
        with sqlite3.connect(path) as db:
            db.executescript("CREATE TABLE sessions(id TEXT,source TEXT); CREATE TABLE messages(session_id TEXT,role TEXT,platform_message_id TEXT);")
            db.execute("INSERT INTO sessions VALUES('native-session','matrix')")
            db.execute("INSERT INTO messages VALUES('native-session','user','$input')")
        matrix = Mock()
        fact = {"runtime": "hermes", "native_ref": "native-session", "native_digest": "a" * 64}
        with patch("task_recovery.enabled", return_value=True), \
                patch("worker_matrix_state.WorkerMatrix", return_value=matrix):
            for case in ["legacy", "pending", "old-start", "foreign", "wrong-session", "empty-digest", "unavailable"]:
                with self.subTest(case=case):
                    matrix.input.return_value = None if case == "legacy" else {}
                    proof = dict(fact)
                    if case == "foreign": proof["runtime"] = "qwenpaw"
                    if case == "wrong-session": proof["native_ref"] = "older-session"
                    if case == "empty-digest": proof["native_digest"] = ""
                    view = {"current": case != "old-start", "input": {"native_commit": proof}}
                    if case == "pending": view["input"] = {}
                    matrix.native_input.return_value = view
                    matrix.native_input.side_effect = OSError("common fact unavailable") if case == "unavailable" else None
                    if case == "unavailable":
                        with self.assertRaises(OSError):
                            scan_inputs(self.receipts, "hermes", {})
                    else:
                        scan_inputs(self.receipts, "hermes", {})
                    self.assertNotIn(("$input", "read"), self.states())
            matrix.native_input.side_effect = None
            matrix.native_input.return_value = {"current": True, "input": {"native_commit": fact}}
            scan_inputs(self.receipts, "hermes", {})
            self.assertIn(("$input", "read"), self.states())

    def test_deepseek_and_openhuman_require_saved_input_checkpoints(self):
        for runtime, directory in [("deepseek", "dsh-checkpoints"), ("openhuman", "matrix-sessions")]:
            with self.subTest(runtime=runtime):
                scan_inputs(self.receipts, runtime, {})
                path = self.root / ".agentteams" / directory / "input.json"
                path.parent.mkdir(parents=True)
                path.write_text(json.dumps({"receipts": [{"room_id": "!room:hs", "event_id": "$" + runtime}]}))
                scan_inputs(self.receipts, runtime, {})
                self.assertIn(("$" + runtime, "read"), self.states())

    def test_managed_codex_journal_cannot_ack_an_assignment(self):
        path = self.root / "runtime/codex-bridge-state.json"
        path.parent.mkdir()
        path.write_text(json.dumps({"events": {"turn": {"input_checkpointed": True, "event": {
            "room_id": "!room:hs", "event_id": "recovery:T", "session_source_event_ids": ["$source"]}}}}))
        with patch.dict(os.environ, {"AGENTTEAMS_CODEX_NATIVE_INPUTS":"1", "AGENTTEAMS_CONTROLLER_URL":"test",
                                     "AGENTTEAMS_PROCESS_ID":"process", "AGENTTEAMS_AGENT_ROLE":"worker"}):
            scan_inputs(self.receipts, "codex", {})
            self.assertNotIn(("$source", "read"), self.states())
            self.receipts.enqueue("!room:hs", "$source", "received")
            self.receipts.committed(["$source"])
            self.assertIn(("$source", "read"), self.states())

    def test_codex_running_and_answer_failure_do_not_imply_or_undo_read(self):
        path = self.root / "runtime/codex-bridge-state.json"
        path.parent.mkdir()
        record = {"event": {"room_id": "!room:hs", "event_id": "$input"}, "status": "running"}
        path.write_text(json.dumps({"events": {"$input": record}}))
        scan_inputs(self.receipts, "codex", {})
        self.assertNotIn(("$input", "read"), self.states())
        record.update(input_checkpointed=True, status="failed")
        record["event"]["session_source_event_ids"] = ["$input", "$supplement"]
        path.write_text(json.dumps({"events": {"$input": record}}))
        scan_inputs(self.receipts, "codex", {})
        self.assertIn(("$input", "read"), self.states())
        self.assertIn(("$supplement", "read"), self.states())

    def test_failed_delivery_recovers_after_restart_with_same_transaction(self):
        self.receipts.enqueue("!room:hs", "$input")
        failing = Mock(side_effect=OSError("offline"))
        self.receipts.flush(failing)
        retry = Mock(return_value={})
        restored = Receipts(self.root)
        restored.flush(retry)
        self.assertEqual(failing.call_args_list, retry.call_args_list)
        retry.reset_mock()
        restored.flush(retry)
        retry.assert_not_called()

    def test_outbound_snapshot_is_captured_before_send_and_skips_edits(self):
        request = Mock(return_value={"joined": {"@a:hs": {"display_name": "Alice"}}})
        content = capture_audience(request, "!room:hs", {"msgtype": "m.text", "body": "hello"})
        self.assertEqual(content["com.agentteams.read_audience"]["members"][0]["user_id"], "@a:hs")
        request.reset_mock()
        edit = {"msgtype": "m.text", "m.relates_to": {"rel_type": "m.replace"}}
        self.assertEqual(capture_audience(request, "!room:hs", edit), edit)
        request.assert_not_called()


if __name__ == "__main__":
    unittest.main()
