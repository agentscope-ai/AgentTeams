#!/usr/bin/env ruby
# frozen_string_literal: true

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

    matrix = {"uploads": [], "events": []}

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
            payload = {"content_uri": f"mxc://example.test/{len(matrix['uploads'])}-{filename}"}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

        def do_PUT(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if "/send/m.room.message/" not in parsed.path:
                self.send_response(404)
                self.end_headers()
                return
            matrix["events"].append({
                "path": parsed.path,
                "auth": self.headers.get("Authorization"),
                "content": json.loads(body.decode("utf-8") or "{}"),
            })
            payload = {"event_id": f"$artifact-event-{len(matrix['events'])}"}
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
    event = matrix["events"][-1]["content"]
    if event.get("msgtype") != "m.file" or event.get("body") != "demo-report.txt":
        raise AssertionError(f"Matrix file event mismatch: {event!r}")
    if event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$parent-text"}:
        raise AssertionError(f"Matrix file event missing attachment relation: {event!r}")
    if not event.get("url") or not (event.get("info") or {}).get("size"):
        raise AssertionError(f"Matrix file event missing metadata: {event!r}")

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
    context_event = matrix["events"][-1]["content"]
    if context_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$context-parent"}:
        raise AssertionError(f"context Matrix file event missing attachment relation: {context_event!r}")

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
    }, ensure_ascii=False))
  PY

  stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness artifact MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.pretty_generate(JSON.parse(stdout))
end
