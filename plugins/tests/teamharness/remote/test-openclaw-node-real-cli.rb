#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"
require "webrick"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert(condition, message)
  fail!(message) unless condition
end

def command_output(*command)
  stdout, stderr, status = Open3.capture3(*command)
  [stdout, stderr, status]
end

def command_available?(command)
  _stdout, _stderr, status = command_output("sh", "-c", "command -v #{command} >/dev/null 2>&1")
  status.success?
end

def capture_with_timeout(env, command, timeout_seconds)
  stdout = +""
  stderr = +""
  status = nil
  Open3.popen3(env, *command) do |stdin, out, err, wait_thread|
    stdin.close
    deadline = Time.now + timeout_seconds
    until wait_thread.join(0.1)
      if Time.now > deadline
        Process.kill("TERM", wait_thread.pid) rescue nil
        sleep 0.5
        Process.kill("KILL", wait_thread.pid) rescue nil
        fail!("command timed out: #{command.join(" ")}")
      end
    end
    stdout = out.read
    stderr = err.read
    status = wait_thread.value
  end
  [stdout, stderr, status]
end

def parse_openclaw_output(raw)
  text = raw.to_s.strip
  return {} if text.empty?

  JSON.parse(text)
rescue JSON::ParserError
  json_line = text.lines.reverse.find { |line| line.strip.start_with?("{") && line.strip.end_with?("}") }
  json_line ? JSON.parse(json_line) : {}
end

def payload_text(doc)
  result = doc["result"].is_a?(Hash) ? doc["result"] : doc
  Array(result["payloads"]).filter_map { |payload| payload["text"] if payload.is_a?(Hash) }.join(" ")
end

unless command_available?("openclaw")
  warn "SKIP: openclaw command not found; real OpenClaw CLI smoke not run"
  exit 0
end

help, _help_err, help_status = command_output("openclaw", "agent", "--help")
fail!("openclaw agent --help failed") unless help_status.success?
["--local", "--json", "--agent", "--session-key", "--model", "--message"].each do |flag|
  assert(help.include?(flag), "openclaw agent --help must include #{flag}")
end

Dir.mktmpdir("teamharness-openclaw-real-cli-test-") do |tmp|
  server = WEBrick::HTTPServer.new(
    BindAddress: "127.0.0.1",
    Port: 0,
    Logger: WEBrick::Log.new(File::NULL),
    AccessLog: []
  )
  port = server.listeners.first.addr[1]
  server.mount_proc("/") do |req, res|
    if req.request_method == "POST" && req.path == "/v1/chat/completions"
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        id: "chatcmpl-openclaw-smoke",
        object: "chat.completion",
        choices: [
          {
            index: 0,
            message: { role: "assistant", content: "real openclaw smoke ok" },
            finish_reason: "stop"
          }
        ]
      )
    else
      res.status = 404
      res.body = JSON.generate(error: "not found")
    end
  end
  thread = Thread.new { server.start }

  begin
    workspace = Pathname.new(tmp) / "workspace"
    state = Pathname.new(tmp) / "state"
    config = Pathname.new(tmp) / "openclaw.json"
    workspace.mkpath
    state.mkpath
    config.write(JSON.pretty_generate(
      agents: {
        defaults: {
          skipBootstrap: true,
          workspace: workspace.to_s,
          model: "teamharness/test-model"
        },
        list: [
          {
            id: "worker-01",
            workspace: workspace.to_s,
            model: "teamharness/test-model",
            skills: []
          }
        ]
      },
      models: {
        providers: {
          teamharness: {
            baseUrl: "http://127.0.0.1:#{port}/v1",
            apiKey: "teamharness-local-proxy",
            api: "openai-completions",
            agentRuntime: { id: "openclaw" },
            models: [
              { id: "test-model", name: "test-model" }
            ]
          }
        }
      }
    ))

    stdout, stderr, status = capture_with_timeout(
      {
        "OPENCLAW_CONFIG_PATH" => config.to_s,
        "OPENCLAW_STATE_DIR" => state.to_s,
        "OPENCLAW_WORKSPACE_DIR" => workspace.to_s
      },
      [
        "openclaw", "agent", "--local", "--json",
        "--agent", "worker-01",
        "--session-key", "smoke",
        "--model", "teamharness/test-model",
        "--message", "Say smoke ok"
      ],
      45
    )
    fail!("openclaw smoke failed:\n#{stderr}\n#{stdout}") unless status.success?

    doc = parse_openclaw_output(stdout)
    assert(payload_text(doc).include?("real openclaw smoke ok"), "OpenClaw JSON payload text mismatch: #{stdout}")
  ensure
    server.shutdown
    thread.join
  end
end
