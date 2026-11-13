"""Exercise durable input evidence and receipt delivery without a model response."""
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock

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

    def test_deepseek_and_openhuman_require_saved_input_checkpoints(self):
        for runtime, directory in [("deepseek", "dsh-checkpoints"), ("openhuman", "matrix-sessions")]:
            with self.subTest(runtime=runtime):
                scan_inputs(self.receipts, runtime, {})
                path = self.root / ".agentteams" / directory / "input.json"
                path.parent.mkdir(parents=True)
                path.write_text(json.dumps({"receipts": [{"room_id": "!room:hs", "event_id": "$" + runtime}]}))
                scan_inputs(self.receipts, runtime, {})
                self.assertIn(("$" + runtime, "read"), self.states())

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
