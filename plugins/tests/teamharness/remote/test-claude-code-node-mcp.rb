#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "fileutils"
require "open3"
require "pathname"
require "tmpdir"
require "webrick"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
mcp_server = repo_root / "plugins/teamharness/remote/claude-code/node-mcp/server.js"

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
  def initialize(server, objects, requests, port)
    super(server)
    @objects = objects
    @requests = requests
    @port = port
  end

  def do_GET(req, res)
    handle(req, res)
  end

  def do_POST(req, res)
    handle(req, res)
  end

  def do_PUT(req, res)
    handle(req, res)
  end

  def handle(req, res)
    @requests << [req.request_method, req.path, req.query, req.body]
    case [req.request_method, req.path]
    when ["GET", "/v1/runtime/context"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        ok: true,
        runtime: "claude-code",
        teamName: "zhuoguang-test",
        memberName: "worker-01",
        role: "remote-member",
        storage: {
          sharedPrefix: "teams/zhuoguang-test",
          globalSharedPrefix: "shared",
          memberPrefix: "agents/worker-01"
        },
        skillRegistry: {
          url: "nacos://market.example:80/public",
          authType: "sts-hiclaw"
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
      if req.path.start_with?("/test-bucket/")
        key = req.path.sub(%r{^/test-bucket/}, "")
        if req.request_method == "PUT"
          @objects[key] = req.body
          res.body = ""
        elsif req.request_method == "GET" && @objects.key?(key)
          res.body = @objects[key]
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

Dir.mktmpdir("teamharness-node-mcp-test-") do |tmp|
  objects = {}
  requests = []
  server = WEBrick::HTTPServer.new(
    BindAddress: "127.0.0.1",
    Port: 0,
    Logger: WEBrick::Log.new(File::NULL),
    AccessLog: []
  )
  port = server.listeners.first.addr[1]
  server.mount("/", FakeBrokerOssServlet, objects, requests, port)
  thread = Thread.new { server.start }
  token_file = File.join(tmp, "credential-token")
  descriptor_file = File.join(tmp, "credential-broker.json")
  File.write(token_file, "broker-token\n")
  File.write(descriptor_file, JSON.generate(endpoint: "http://127.0.0.1:#{port}", tokenFile: token_file))

  Open3.popen3(
    {
      "TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR" => descriptor_file
    },
    "node",
    mcp_server.to_s
  ) do |stdin, stdout, _stderr, wait_thread|
    begin
      init = rpc(stdin, stdout, 1, "initialize", {})
      assert(init.dig("serverInfo", "name") == "teamharness-claude-code", "initialize server name mismatch")

      tools = rpc(stdin, stdout, 2, "tools/list").fetch("tools").map { |tool| tool.fetch("name") }
      assert(tools.include?("health"), "health tool missing")
      assert(tools.include?("filesync"), "filesync tool missing")
      assert(!tools.include?("message"), "message tool should be hidden for remote-member")

      health_text = rpc(stdin, stdout, 3, "tools/call", name: "health", arguments: {}).fetch("content").first.fetch("text")
      health = JSON.parse(health_text)
      assert(health["ok"] == true, "health must return ok")
      assert(health["runtime"] == "claude-code", "health runtime must be claude-code")
      assert(health["broker"] == true, "health must see broker")
      assert(health.dig("context", "teamName") == "zhuoguang-test", "health context team mismatch")

      dry_text = rpc(stdin, stdout, 4, "tools/call", name: "filesync", arguments: {
        action: "list",
        path: "shared/tasks/",
        dryRun: true,
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      dry = JSON.parse(dry_text)
      assert(dry["ok"] == true, "filesync dryRun must succeed")
      assert(dry["command"].nil?, "filesync dryRun must not return mc command")
      assert(dry["remotePath"] == "teams/zhuoguang-test/tasks", "dryRun remote path mismatch")

      local_file = File.join(tmp, "shared/tasks/demo.txt")
      FileUtils.mkdir_p(File.dirname(local_file))
      File.write(local_file, "hello from node mcp\n")
      push_text = rpc(stdin, stdout, 5, "tools/call", name: "filesync", arguments: {
        action: "push",
        path: "shared/tasks/demo.txt",
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      push = JSON.parse(push_text)
      assert(push["ok"] == true, "filesync push must succeed")
      assert(objects["teams/zhuoguang-test/tasks/demo.txt"] == "hello from node mcp\n", "OSS object not pushed")

      list_text = rpc(stdin, stdout, 6, "tools/call", name: "filesync", arguments: {
        action: "list",
        path: "shared/tasks/",
        workspaceDir: tmp
      }).fetch("content").first.fetch("text")
      list = JSON.parse(list_text)
      assert(list["ok"] == true, "filesync list must succeed")
      assert(list["keys"].include?("teams/zhuoguang-test/tasks/demo.txt"), "filesync list missing pushed object")
    ensure
      Process.kill("TERM", wait_thread.pid) rescue nil
    end
  end
ensure
  server.shutdown if server
  thread.join if thread
end
