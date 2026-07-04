#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
mcp_dir = repo_root / "plugins/teamharness/mcp"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

python_test = <<~PY
  import json
  import pathlib
  import sys

  sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
  from server import handle_request

  init = handle_request({
      "jsonrpc": "2.0",
      "id": 1,
      "method": "initialize",
      "params": {},
  })
  if init["result"]["serverInfo"]["name"] != "teamharness":
      raise AssertionError(f"unexpected server info: {init!r}")

  tools = handle_request({
      "jsonrpc": "2.0",
      "id": 2,
      "method": "tools/list",
      "params": {},
  })["result"]["tools"]
  names = [tool["name"] for tool in tools]
  expected = ["health", "message", "roomflow", "filesync", "projectflow", "taskflow"]
  if names != expected:
      raise AssertionError(f"unexpected tools: {names!r}")

  unknown = handle_request({
      "jsonrpc": "2.0",
      "id": 3,
      "method": "tools/call",
      "params": {"name": "missing", "arguments": {}},
  })
  payload = json.loads(unknown["result"]["content"][0]["text"])
  if payload.get("error") != "unknown_tool":
      raise AssertionError(f"unexpected unknown-tool response: {payload!r}")

  print(json.dumps({"ok": True, "tools": names}, ensure_ascii=False))
PY

stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
fail!(["teamharness MCP server test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

puts JSON.pretty_generate(JSON.parse(stdout))
