#!/usr/bin/env ruby
# frozen_string_literal: true

require "digest"
require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../../..").expand_path
mcp_dir = repo_root / "plugins/teamharness/mcp"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

Dir.mktmpdir("teamharness-artifact-") do |dir|
  root = Pathname.new(dir)
  workspace = root / "workspace"

  python_test = <<~PY
    import hashlib
    import http.server
    import json
    import os
    import pathlib
    import socketserver
    import sys
    import threading
    import urllib.parse

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import call_tool

    workspace = pathlib.Path("#{workspace}")
    report = workspace / "reports/demo.txt"
    report.parent.mkdir(parents=True, exist_ok=True)
    report.write_text("demo report\\n", encoding="utf-8")

    matrix = {
        "uploads": [],
        "events": [],
        "seen_txns": {},
        # Known room events so the parent-event sender lookup (GET
        # /rooms/<room>/event/<id>) has something to resolve.
        "room_events": {
            "$delegate-event": {"sender": "@leader:example.test"},
        },
    }

    class MatrixHandler(http.server.BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            return

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path != "/_matrix/media/v3/upload":
                self.send_response(404)
                self.end_headers()
                return
            query = urllib.parse.parse_qs(parsed.query)
            filename = query.get("filename", ["artifact"])[0]
            matrix["uploads"].append({
                "filename": filename,
                "body": body.decode("utf-8", errors="replace"),
                "auth": self.headers.get("Authorization"),
                "content_type": self.headers.get("Content-Type"),
            })
            # Stable mxc per (filename, content) so a retried publish of
            # the same file re-uploads to the same media URI.
            digest = hashlib.sha256((filename + body.decode("utf-8", "replace")).encode("utf-8")).hexdigest()[:16]
            payload = {"content_uri": f"mxc://example.test/{digest}"}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            parts = [urllib.parse.unquote(part) for part in parsed.path.split("/") if part]
            # /_matrix/client/v3/rooms/<room>/event/<id>
            if len(parts) == 7 and parts[0] == "_matrix" and parts[5] == "event":
                event = matrix["room_events"].get(parts[6])
                if event:
                    body = json.dumps(event).encode("utf-8")
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(body)
                    return
            self.send_response(404)
            self.end_headers()

        def do_PUT(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if "/send/m.room.message/" not in parsed.path:
                self.send_response(404)
                self.end_headers()
                return
            txn = parsed.path.rsplit("/", 1)[-1]
            if txn in matrix["seen_txns"]:
                # Real Matrix transaction-id idempotency: the event was
                # already recorded server-side, return its id, no new event.
                payload = {"event_id": matrix["seen_txns"][txn]}
            else:
                event_id = f"$artifact-event-{len(matrix['events']) + 1}"
                matrix["seen_txns"][txn] = event_id
                matrix["events"].append({
                    "txn": txn,
                    "path": parsed.path,
                    "auth": self.headers.get("Authorization"),
                    "content": json.loads(body.decode("utf-8") or "{}"),
                })
                payload = {"event_id": event_id}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

    class MatrixServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True

    matrix_server = MatrixServer(("127.0.0.1", 0), MatrixHandler)
    matrix_thread = threading.Thread(target=matrix_server.serve_forever, daemon=True)
    matrix_thread.start()
    os.environ["AGENTTEAMS_MATRIX_URL"] = f"http://127.0.0.1:{matrix_server.server_address[1]}"
    os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"
    context_file = workspace / ".teamharness-matrix-context.json"
    os.environ["TEAMHARNESS_MATRIX_CONTEXT_FILE"] = str(context_file)

    def payload(args):
        result = call_tool("artifact", args)
        return json.loads(result["content"][0]["text"])

    def last_file_event():
        event = matrix["events"][-2]["content"]
        if event.get("msgtype") != "m.file":
            raise AssertionError(f"expected file event before summary: {matrix['events'][-2:]!r}")
        return matrix["events"][-2], event

    def last_summary_event():
        record = matrix["events"][-1]
        event = record["content"]
        if event.get("msgtype") != "m.text":
            raise AssertionError(f"expected summary text event after file event: {record!r}")
        return record, event

    # --- publish 1: explicit parent event, no resolvable mention ---------
    published = payload({
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/demo.txt",
        "target": "room:!room:example.test",
        "filename": "demo-report.txt",
        "parentEventId": "$parent-text",
    })
    if not published.get("ok"):
        raise AssertionError(f"artifact publish_file failed: {published!r}")
    artifact = published.get("artifact") or {}
    if artifact.get("status") != "published":
        raise AssertionError(f"artifact was not published: {published!r}")
    if artifact.get("sourcePath") != "reports/demo.txt" or artifact.get("filename") != "demo-report.txt":
        raise AssertionError(f"artifact metadata mismatch: {published!r}")
    if not artifact.get("mxcUri") or not artifact.get("eventId"):
        raise AssertionError(f"artifact missing Matrix references: {published!r}")
    if artifact.get("parentEventId") != "$parent-text":
        raise AssertionError(f"artifact missing parent event reference: {published!r}")
    if matrix["uploads"][-1].get("body") != "demo report\\n":
        raise AssertionError(f"uploaded body mismatch: {matrix['uploads']!r}")
    file_record, event = last_file_event()
    if event.get("body") != "demo-report.txt":
        raise AssertionError(f"Matrix file event mismatch: {event!r}")
    if event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$parent-text"}:
        raise AssertionError(f"Matrix file event missing attachment relation: {event!r}")
    if not event.get("url") or not (event.get("info") or {}).get("size"):
        raise AssertionError(f"Matrix file event missing metadata: {event!r}")
    summary_record, summary_event = last_summary_event()
    if "已发布" not in summary_event.get("body", "") or "demo-report.txt" not in summary_event.get("body", ""):
        raise AssertionError(f"publish summary missing default text: {summary_event!r}")
    if "reports/demo.txt" not in summary_event.get("body", ""):
        raise AssertionError(f"publish summary missing source path: {summary_event!r}")
    if "m.mentions" in summary_event:
        raise AssertionError(f"summary without resolvable mention must not carry m.mentions: {summary_event!r}")
    if summary_record["txn"] != file_record["txn"] + "-summary":
        raise AssertionError(f"summary txn must derive from the file txn: {summary_record['txn']!r} vs {file_record['txn']!r}")

    # --- publish 2: context-inferred parent event ------------------------
    context_file.write_text(json.dumps({
        "rooms": {
            "!room:example.test": {
                "attachmentParentEventId": "$context-parent",
                "updatedAt": __import__("time").time(),
            }
        }
    }), encoding="utf-8")
    context_published = payload({
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/demo.txt",
        "target": "room:!room:example.test",
        "filename": "context-demo-report.txt",
    })
    if not context_published.get("ok"):
        raise AssertionError(f"context artifact publish_file failed: {context_published!r}")
    context_artifact = context_published.get("artifact") or {}
    if context_artifact.get("parentEventId") != "$context-parent":
        raise AssertionError(f"context artifact missing inferred parent event reference: {context_published!r}")
    context_record, context_event = last_file_event()
    if context_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$context-parent"}:
        raise AssertionError(f"context Matrix file event missing attachment relation: {context_event!r}")
    _, context_summary = last_summary_event()
    if context_summary.get("msgtype") != "m.text" or "context-demo-report.txt" not in context_summary.get("body", ""):
        raise AssertionError(f"context publish summary mismatch: {context_summary!r}")

    # --- publish 3: explicit mentions (one valid MXID + one invalid) -----
    mention_args = {
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/demo.txt",
        "target": "room:!room:example.test",
        "filename": "mention-report.txt",
        "mentions": ["@leader:example.test", "leader"],
    }
    mention_published = payload(mention_args)
    if not mention_published.get("ok"):
        raise AssertionError(f"mention artifact publish_file failed: {mention_published!r}")
    mention_record, mention_summary = last_summary_event()
    mentions = (mention_summary.get("m.mentions") or {}).get("user_ids", [])
    if mentions != ["@leader:example.test"]:
        raise AssertionError(f"summary m.mentions must keep only valid MXIDs: {mentions!r}")
    if "https://matrix.to/#/%40leader%3Aexample.test" not in mention_summary.get("formatted_body", ""):
        raise AssertionError(f"summary formatted_body missing matrix.to pill: {mention_summary!r}")
    if "leader" in mention_summary.get("formatted_body", "") and "matrix.to" not in mention_summary.get("formatted_body", ""):
        raise AssertionError(f"short mention must not be rendered as plain text: {mention_summary!r}")

    # --- publish 4: parent event sender becomes the mention --------------
    parent_sender_published = payload({
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/demo.txt",
        "target": "room:!room:example.test",
        "filename": "parent-report.txt",
        "parentEventId": "$delegate-event",
    })
    if not parent_sender_published.get("ok"):
        raise AssertionError(f"parent sender publish_file failed: {parent_sender_published!r}")
    _, parent_summary = last_summary_event()
    parent_mentions = (parent_summary.get("m.mentions") or {}).get("user_ids", [])
    if parent_mentions != ["@leader:example.test"]:
        raise AssertionError(f"summary must mention the parent event sender: {parent_mentions!r}")

    # --- publish 5: retry of publish 3 deduplicates via stable txns ------
    retry_published = payload(mention_args)
    if not retry_published.get("ok"):
        raise AssertionError(f"retry publish_file failed: {retry_published!r}")
    retry_artifact = retry_published.get("artifact") or {}
    mention_artifact = mention_published.get("artifact") or {}
    if len(matrix["events"]) != 8:
        raise AssertionError(f"retry must not create new events (txn dedup): {len(matrix['events'])!r}")
    if retry_artifact.get("eventId") != mention_artifact.get("eventId"):
        raise AssertionError(f"retry must reuse the deduplicated file event: {retry_artifact!r} vs {mention_artifact!r}")
    if retry_artifact.get("mxcUri") != mention_artifact.get("mxcUri"):
        raise AssertionError(f"retry must reuse the same media URI: {retry_artifact!r} vs {mention_artifact!r}")

    # --- escape and sensitive guards are unchanged ------------------------
    outside = payload({
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "../outside.txt",
        "target": "room:!room:example.test",
    })
    if outside.get("ok") or "relative" not in str(outside.get("error", "")).lower():
        raise AssertionError(f"workspace escape should fail clearly: {outside!r}")

    secret = workspace / "reports/secret-note.txt"
    secret.write_text("token=abcdefghijklmnopqrstuvwxyz1234567890\\n", encoding="utf-8")
    secret_result = payload({
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/secret-note.txt",
        "target": "room:!room:example.test",
    })
    if secret_result.get("ok") or "sensitive" not in str(secret_result.get("error", "")).lower():
        raise AssertionError(f"sensitive artifact should fail clearly: {secret_result!r}")
    if any(upload.get("filename") == "secret-note.txt" for upload in matrix["uploads"]):
        raise AssertionError(f"sensitive artifact should not upload: {matrix['uploads']!r}")

    matrix_server.shutdown()
    matrix_server.server_close()

    print(json.dumps({
        "ok": True,
        "filename": artifact["filename"],
        "eventId": artifact["eventId"],
        "events": len(matrix["events"]),
    }, ensure_ascii=False))
  PY

  stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness artifact MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.pretty_generate(JSON.parse(stdout))
end
