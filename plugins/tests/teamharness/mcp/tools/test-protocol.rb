#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../../..").expand_path
server = repo_root / "plugins/teamharness/mcp/server.py"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

input = [
  {
    "jsonrpc" => "2.0",
    "id" => 1,
    "method" => "initialize",
    "params" => {}
  },
  {
    "jsonrpc" => "2.0",
    "method" => "notifications/initialized",
    "params" => {}
  },
  {
    "jsonrpc" => "2.0",
    "id" => 2,
    "method" => "tools/list",
    "params" => {}
  }
].map { |item| JSON.generate(item) }.join("\n") + "\n"

stdout, stderr, status = Open3.capture3("python3", server.to_s, stdin_data: input, chdir: repo_root.to_s)
fail!(["teamharness MCP protocol test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

lines = stdout.lines.map(&:strip).reject(&:empty?)
fail!("notification should not produce a JSON-RPC response: #{lines.inspect}") unless lines.length == 2

initialize_response = JSON.parse(lines.fetch(0))
tools_response = JSON.parse(lines.fetch(1))

fail!("initialize response id mismatch: #{initialize_response.inspect}") unless initialize_response["id"] == 1
fail!("tools/list response id mismatch: #{tools_response.inspect}") unless tools_response["id"] == 2

tools = tools_response.dig("result", "tools") || []
tool_names = tools.map { |tool| tool["name"] }
expected = %w[health message roomflow filesync artifact projectflow taskflow]
fail!("unexpected tools: #{tool_names.inspect}") unless tool_names == expected

tool_by_name = tools.to_h { |tool| [tool["name"], tool] }
expected_keywords = {
  "health" => ["MCP server", "not runtime worker health"],
  "message" => ["cross-room", "cross-channel", "cross-session", "current room/session", "PROJECT_REQUESTED", "self-trigger", "requester/source"],
  "roomflow" => ["task rooms", "execution-channel", "not requester reply channels"],
  "filesync" => ["shared/projects", "shared/tasks", "not periodic workspace sync"],
  "artifact" => ["workspace file", "Matrix room", "m.file"],
  "projectflow" => ["Project Work", "DAG", "Loop", "ordinary direct replies"],
  "taskflow" => ["leader delegates", "worker", "ordinary conversation"]
}

expected_keywords.each do |name, keywords|
  tool = tool_by_name.fetch(name)
  description = tool["description"].to_s
  fail!("generic description for #{name}: #{description.inspect}") if description == "TeamHarness #{name} tool"
  keywords.each do |keyword|
    fail!("description for #{name} must mention #{keyword.inspect}: #{description.inspect}") unless description.include?(keyword)
  end
  schema = tool["inputSchema"]
  fail!("missing object input schema for #{name}: #{tool.inspect}") unless schema.is_a?(Hash) && schema["type"] == "object"
  fail!("missing schema properties for #{name}: #{schema.inspect}") unless schema["properties"].is_a?(Hash)
end

message_schema = tool_by_name.fetch("message").fetch("inputSchema")
message_properties = message_schema.fetch("properties")
fail!("message schema must expose sender for trigger routing") unless message_properties.key?("sender")
fail!("message schema must expose agentId for same-agent routing") unless message_properties.key?("agentId")
fail!("message schema must expose top-level type alias") unless message_properties.key?("type")
target_schema = JSON.generate(message_properties.fetch("target"))
fail!("message schema target must use Matrix room string payloads") unless target_schema.include?("room:!room:domain") && target_schema.include?("string")
fail!("message schema message must support typed trigger payloads") unless JSON.generate(message_properties.fetch("message")).include?("PROJECT_REQUESTED")
fail!("message schema must document PROJECT_REQUESTED") unless JSON.generate(message_schema).include?("PROJECT_REQUESTED")

puts JSON.pretty_generate(
  "ok" => true,
  "responses" => lines.length,
  "tools" => tool_names
)
