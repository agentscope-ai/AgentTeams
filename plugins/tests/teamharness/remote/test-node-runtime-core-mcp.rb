#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "fileutils"
require "open3"
require "pathname"
require "tmpdir"
require "webrick"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
core_server = repo_root / "plugins/teamharness/remote/node-runtime-core/mcp/server.js"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert(condition, message)
  fail!(message) unless condition
end

def write_frame(io, payload)
  body = JSON.generate(payload)
  io.write("Content-Length: #{body.bytesize}\r\n\r\n#{body}")
  io.flush
end

def read_frame(io)
  ready = IO.select([io], nil, nil, 5)
  fail!("timeout waiting for MCP response") unless ready
  JSON.parse(io.gets)
end

def rpc(stdin, stdout, id, method, params = nil)
  request = { jsonrpc: "2.0", id: id, method: method }
  request[:params] = params if params
  write_frame(stdin, request)
  response = read_frame(stdout)
  fail!("MCP error for #{method}: #{response["error"].inspect}") if response["error"]
  response.fetch("result")
end

class FakeBrokerOssServlet < WEBrick::HTTPServlet::AbstractServlet
  def initialize(server, objects, port)
    super(server)
    @objects = objects
    @port = port
  end

  def do_GET(req, res)
    handle(req, res)
  end

  def do_PUT(req, res)
    handle(req, res)
  end

  def handle(req, res)
    case [req.request_method, req.path]
    when ["GET", "/v1/runtime/context"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        ok: true,
        runtime: "core-test",
        teamName: "zhuoguang-test",
        memberName: "worker-01",
        role: "remote-member",
        storage: {
          sharedPrefix: "teams/zhuoguang-test",
          globalSharedPrefix: "shared",
          memberPrefix: "agents/worker-01"
        }
      )
    when ["GET", "/v1/credentials/storage"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        accessKeyId: "ak",
        accessKeySecret: "sk",
        securityToken: "sts",
        endpoint: "http://127.0.0.1:#{@port}",
        bucket: "test-bucket"
      )
    when ["GET", "/test-bucket/"]
      prefix = req.query["prefix"].to_s
      keys = @objects.keys.select { |key| key.start_with?(prefix) }
      res["Content-Type"] = "application/xml"
      res.body = "<ListBucketResult>#{keys.map { |key| "<Contents><Key>#{key}</Key></Contents>" }.join}</ListBucketResult>"
    else
      if req.path.start_with?("/test-bucket/") && req.request_method == "PUT"
        @objects[req.path.sub(%r{^/test-bucket/}, "")] = req.body
        res.body = ""
      elsif req.path.start_with?("/test-bucket/") && req.request_method == "GET"
        key = req.path.sub(%r{^/test-bucket/}, "")
        if @objects.key?(key)
          res.body = @objects.fetch(key)
        else
          res.status = 404
          res.body = "not found"
        end
      else
        res.status = 404
        res.body = "not found"
      end
    end
  end
end

Dir.mktmpdir("teamharness-node-runtime-core-mcp-test-") do |tmp|
  objects = {}
  server = WEBrick::HTTPServer.new(
    BindAddress: "127.0.0.1",
    Port: 0,
    Logger: WEBrick::Log.new(File::NULL),
    AccessLog: []
  )
  port = server.listeners.first.addr[1]
  server.mount("/", FakeBrokerOssServlet, objects, port)
  thread = Thread.new { server.start }
  token_file = File.join(tmp, "credential-token")
  descriptor_file = File.join(tmp, "credential-broker.json")
  File.write(token_file, "broker-token\n")
  File.write(descriptor_file, JSON.generate(endpoint: "http://127.0.0.1:#{port}", tokenFile: token_file))

  bootstrap = <<~JS
    const core = require(#{core_server.to_s.inspect});
    core.runMcpServer({
      runtime: "core-test",
      serverName: "teamharness-core-test",
      healthDescription: "Core MCP test server"
    });
  JS
  Open3.popen3(
    { "TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR" => descriptor_file },
    "node",
    "-e",
    bootstrap
  ) do |stdin, stdout, _stderr, wait_thread|
    begin
      init = rpc(stdin, stdout, 1, "initialize", {})
      assert(init.dig("serverInfo", "name") == "teamharness-core-test", "initialize server name mismatch")

      tools = rpc(stdin, stdout, 2, "tools/list").fetch("tools").map { |tool| tool.fetch("name") }
      assert(tools.include?("health"), "health tool missing")
      assert(tools.include?("filesync"), "filesync tool missing")
      assert(tools.include?("taskflow"), "taskflow tool missing")
      assert(!tools.include?("message"), "message tool should be hidden for remote-member")

      health_text = rpc(stdin, stdout, 3, "tools/call", name: "health", arguments: {}).fetch("content").first.fetch("text")
      health = JSON.parse(health_text)
      assert(health["runtime"] == "core-test", "health runtime mismatch")
      assert(health.dig("context", "teamName") == "zhuoguang-test", "health context team mismatch")

      message_text = rpc(stdin, stdout, 4, "tools/call", name: "message", arguments: {
        action: "send",
        target: "room:!room:test",
        text: "@leader hello",
        role: "leader",
        dryRun: true
      }).fetch("content").first.fetch("text")
      message = JSON.parse(message_text)
      assert(message["ok"] == false, "message direct call must be forbidden for remote-member")
      assert(message["error"] == "forbidden_tool", "message direct call must return forbidden_tool")

      room_text = rpc(stdin, stdout, 5, "tools/call", name: "roomflow", arguments: {
        action: "describe_room",
        target: "room:!room:test",
        dryRun: true
      }).fetch("content").first.fetch("text")
      room = JSON.parse(room_text)
      assert(room["ok"] == true, "roomflow describe_room dryRun must succeed")
      assert(room["roomId"] == "!room:test", "roomflow describe_room must return roomId")

      room_create_text = rpc(stdin, stdout, 6, "tools/call", name: "roomflow", arguments: {
        action: "create_task_room",
        taskId: "core-room-01",
        name: "Core Room",
        invite: ["@worker:test"],
        admin: "@admin:test",
        role: "leader",
        dryRun: true
      }).fetch("content").first.fetch("text")
      room_create = JSON.parse(room_create_text)
      assert(room_create["ok"] == false, "roomflow create_task_room must be forbidden for remote-member")
      assert(room_create["error"].include?("requires leader role"), "roomflow create_task_room must require leader role")

      local_file = File.join(tmp, "shared/tasks/core.txt")
      FileUtils.mkdir_p(File.dirname(local_file))
      File.write(local_file, "hello from core mcp\n")
      push_text = rpc(stdin, stdout, 7, "tools/call", name: "filesync", arguments: {
        action: "push",
        path: "shared/tasks/core.txt",
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      push = JSON.parse(push_text)
      assert(push["ok"] == true, "filesync push must succeed")
      assert(objects["teams/zhuoguang-test/tasks/core.txt"] == "hello from core mcp\n", "OSS object not pushed")

      readonly_text = rpc(stdin, stdout, 8, "tools/call", name: "filesync", arguments: {
        action: "push",
        kind: "global-shared",
        path: "global-shared/core.txt",
        workspaceDir: tmp,
        dryRun: true
      }).fetch("content").first.fetch("text")
      readonly = JSON.parse(readonly_text)
      assert(readonly["ok"] == false, "global-shared filesync push must be rejected")

      project_text = rpc(stdin, stdout, 9, "tools/call", name: "projectflow", arguments: {
        action: "create_project",
        projectId: "project-01",
        title: "Project One",
        role: "leader",
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      project = JSON.parse(project_text)
      assert(project["ok"] == false, "projectflow create_project must be forbidden for remote-member")
      assert(project["error"].include?("requires leader role"), "projectflow create_project must require leader role")

      task_id = "core-task-01"
      objects["teams/zhuoguang-test/tasks/#{task_id}/meta.json"] = JSON.pretty_generate(
        taskId: task_id,
        projectId: "project-01",
        roomId: "!room:test",
        status: "assigned",
        specPath: "shared/tasks/#{task_id}/spec.md",
        taskTitle: "Remote core task",
        assignedTo: "worker-01",
        createdAt: "2026-06-26T07:00:00Z"
      ) + "\n"
      objects["teams/zhuoguang-test/tasks/#{task_id}/spec.md"] = "# Spec\n\nDo the thing.\n"

      ack_text = rpc(stdin, stdout, 10, "tools/call", name: "taskflow", arguments: {
        action: "ack_task",
        taskId: task_id,
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      ack = JSON.parse(ack_text)
      assert(ack["ok"] == true, "taskflow ack_task must succeed")
      assert(ack.dig("task", "status") == "in_progress", "ack_task must mark task in_progress")
      assert(ack["spec"].include?("Do the thing."), "ack_task must return pulled spec")
      pushed_meta = JSON.parse(objects.fetch("teams/zhuoguang-test/tasks/#{task_id}/meta.json"))
      assert(pushed_meta["status"] == "in_progress", "ack_task must push updated task meta")
      assert(pushed_meta["task_title"] == "Remote core task", "ack_task must convert console task_title")
      assert(pushed_meta["assigned_to"] == "worker-01", "ack_task must preserve console assigned_to")
      assigned_at = pushed_meta["assigned_at"]
      assert(assigned_at == "2026-06-26T07:00:00Z", "ack_task must convert console assigned_at")
      assert(!pushed_meta.key?("taskId"), "ack_task must not persist camelCase taskId")

      submit_text = rpc(stdin, stdout, 11, "tools/call", name: "taskflow", arguments: {
        action: "submit_task",
        taskId: task_id,
        workspaceDir: tmp,
        status: "SUCCESS",
        summary: "Done",
        deliverables: ["shared/tasks/#{task_id}/result.md"]
      }).fetch("content").first.fetch("text")
      submit = JSON.parse(submit_text)
      assert(submit["ok"] == true, "taskflow submit_task must succeed")
      assert(submit.dig("task", "status") == "submitted", "submit_task must mark task submitted")
      submitted_meta = JSON.parse(objects.fetch("teams/zhuoguang-test/tasks/#{task_id}/meta.json"))
      assert(submitted_meta["assigned_at"] == assigned_at, "submit_task must preserve assigned_at")
      assert(submitted_meta["task_title"] == "Remote core task", "submit_task must preserve task_title")
      assert(objects.fetch("teams/zhuoguang-test/tasks/#{task_id}/result.md").include?("- Summary: Done"), "submit_task must push result.md")
    ensure
      Process.kill("TERM", wait_thread.pid) rescue nil
    end
  end
ensure
  server.shutdown if server
  thread.join if thread
end
