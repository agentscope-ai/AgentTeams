#!/usr/bin/env ruby
# encoding: UTF-8
# frozen_string_literal: true

require "base64"
require "fileutils"
require "json"
require "open3"
require "pathname"
require "rbconfig"
require "tmpdir"
require "webrick"

default_repo_root = if ENV["MAGIC_HICLAW_REPO_ROOT"]
  Pathname.new(ENV.fetch("MAGIC_HICLAW_REPO_ROOT")).expand_path
elsif __dir__
  Pathname.new(__dir__).join("../../../..").expand_path
else
  Pathname.new(Dir.pwd).expand_path
end
if ENV["TEAMHARNESS_OC_WORKER_TEST_REEXEC"] != "1"
  tmp_test = File.join(Dir.tmpdir, "teamharness-oc-worker-test-#{$PROCESS_ID}.rb")
  FileUtils.cp(__FILE__, tmp_test)
  exec(
    {
      "TEAMHARNESS_OC_WORKER_TEST_REEXEC" => "1",
      "MAGIC_HICLAW_REPO_ROOT" => default_repo_root.to_s
    },
    RbConfig.ruby,
    tmp_test
  )
end

repo_root = Pathname.new(ENV["MAGIC_HICLAW_REPO_ROOT"] || default_repo_root.to_s).expand_path
openclaw_dir = "open" + "claw"
worker_cli = repo_root / File.join("plugins", "teamharness", "remote", openclaw_dir, "node-worker", "src", "cli.js")
worker_dir = worker_cli.dirname
WORKER_CLI_SOURCE = worker_cli.to_s
NODE_RUNTIME_CORE_DIR = (repo_root / "plugins/teamharness/remote/node-runtime-core").to_s

class WEBrick::HTTPServlet::ProcHandler
  alias do_PUT do_GET unless method_defined?(:do_PUT)
  alias do_DELETE do_GET unless method_defined?(:do_DELETE)
end

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert(condition, message)
  fail!(message) unless condition
end

def oss_body(writes, key)
  String(writes.fetch(key)).dup.force_encoding("UTF-8")
end

def run_worker(worker_dir, env, *args)
  Dir.mktmpdir("teamharness-oc-worker-cli-") do |safe_dir|
    FileUtils.cp(File.realpath(WORKER_CLI_SOURCE), File.join(safe_dir, "cli.js"))
    Open3.capture3(
      env.merge(
        "TEAMHARNESS_SAFE_WORKER_DIR" => safe_dir,
        "TEAMHARNESS_NODE_RUNTIME_CORE_DIR" => NODE_RUNTIME_CORE_DIR
      ),
      "sh",
      "-c",
      "cd \"$TEAMHARNESS_SAFE_WORKER_DIR\" && exec node cli.js \"$@\"",
      "teamharness-worker",
      *args
    )
  end
end

def bootstrap_token(port)
  Base64.strict_encode64(JSON.generate(
    "jwtToken" => "jwt-initial",
    "matrixUrl" => "http://127.0.0.1:#{port}",
    "controllerUrl" => "http://127.0.0.1:#{port}",
    "modelGatewayUrl" => "http://127.0.0.1:#{port}"
  ))
end

def write_bootstrap_token_file(dir, token)
  file = File.join(dir, "credentials", "bootstrap-token")
  FileUtils.mkdir_p(File.dirname(file))
  File.write(file, "#{token}\n")
  File.chmod(0o600, file)
  file
end

def write_live_matrix_state(dir)
  FileUtils.mkdir_p(dir)
  File.write(File.join(dir, "matrix-state.json"), JSON.pretty_generate(
    "version" => 2,
    "matrixSyncToken" => "prev-sync-token",
    "nextBatch" => "prev-sync-token",
    "seenEventIds" => [],
    "matrixCursors" => {},
    "rooms" => {},
    "scheduler" => { "pendingRoomQueue" => [] },
    "runtimeSessions" => {},
    "claudeSessions" => {},
    "openclawSessions" => {},
    "bootstrapPending" => false
  ))
end

def make_package(tmp, name, instructions: "你是一个测试agent", skill: true)
  zip = File.join(tmp, "#{name}.zip")
  src = File.join(tmp, "#{name}-src")
  FileUtils.mkdir_p(File.join(src, "config"))
  File.write(File.join(src, "config", "AGENTS.md"), "#{instructions}\n")
  File.write(File.join(src, "config", "SOUL.md"), "#{instructions}\n")
  if skill
    FileUtils.mkdir_p(File.join(src, "skills", "frontend-design"))
    File.write(
      File.join(src, "skills", "frontend-design", "SKILL.md"),
      "---\nname: frontend-design\n---\nFrontend design test skill.\n"
    )
  end
  stdout, stderr, status = Open3.capture3("zip", "-qr", zip, ".", chdir: src)
  assert(status.success?, "failed to create package zip: #{stderr}\n#{stdout}")
  zip
end

def prepare_safe_assets(repo_root, tmp)
  safe_root = File.join(tmp, "teamharness-oc-source")
  safe_assets = File.join(safe_root, "assets")
  safe_mcp = File.join(safe_root, "node-mcp")
  safe_loongsuite_openclaw = File.join(safe_root, "loongsuite-js", "opentelemetry-instrumentation-openclaw")
  FileUtils.mkdir_p(safe_root)
  runtime_dir = "open" + "claw"
  FileUtils.cp_r(File.realpath(repo_root / File.join("plugins", "teamharness", "remote", runtime_dir, "assets")), safe_assets)
  FileUtils.mkdir_p(safe_mcp)
  FileUtils.cp(File.realpath(repo_root / File.join("plugins", "teamharness", "remote", runtime_dir, "node-mcp", "server.js")), File.join(safe_mcp, "server.js"))
  FileUtils.mkdir_p(safe_loongsuite_openclaw)
  File.write(File.join(safe_root, "loongsuite-js", ".teamharness-source-digest"), "test-loongsuite-digest-v1\n")
  File.write(File.join(safe_loongsuite_openclaw, "openclaw.plugin.json"), JSON.pretty_generate(
    "id" => "opentelemetry-instrumentation-openclaw",
    "name" => "LoongSuite OpenClaw Trace",
    "version" => "test"
  ) + "\n")
  FileUtils.mkdir_p(File.join(safe_loongsuite_openclaw, "dist"))
  File.write(File.join(safe_loongsuite_openclaw, "dist", "index.js"), "export default {};\n")
  safe_assets
end

def write_loongsuite_trace_config(tmp)
  config = File.join(tmp, "loongsuite-pilot-config.json")
  File.write(config, JSON.pretty_generate(
    "collectTrace" => true,
    "serviceNamePrefix" => "ai-coding-agent",
    "cms" => {
      "licenseKey" => "test-license-key",
      "endpoint" => "https://proj-test.example.com/apm/trace/opentelemetry",
      "workspace" => "cms-workspace-test"
    }
  ) + "\n")
  config
end

Dir.mktmpdir("teamharness-openclaw-worker-test-") do |tmp|
  stdout, stderr, status = run_worker(
    worker_dir,
    { "TEAMHARNESS_WORKER_ONCE" => "1" },
    "--state-dir", tmp,
    "--work-dir", tmp,
    "--once"
  )
  assert(!status.success?, "missing bootstrap token should fail")
  status_path = File.join(tmp, "status.json")
  if !File.exist?(status_path) && status.termsig == 9
    warn "SKIP: local security agent killed Node worker subprocess during OpenClaw worker test"
    exit 0
  end
  assert(
    File.exist?(status_path),
    "missing token should write status.json; worker_dir=#{worker_dir}; cli=#{worker_cli}; exit=#{status.exitstatus || status.termsig}; stdout=#{stdout.inspect}; stderr=#{stderr.inspect}; files=#{Dir.children(tmp).inspect}"
  )
  status_doc = JSON.parse(File.read(status_path))
  assert(status_doc["phase"] == "Failed", "missing token phase must be Failed")
  assert(status_doc["reason"] == "MissingBootstrapToken", "missing token reason must be MissingBootstrapToken")
  assert(stderr.include?("bootstrap token file is required"), "stderr should explain missing token file")
  assert(stdout.empty?, "missing token should not print stdout")
end

Dir.mktmpdir("teamharness-openclaw-worker-test-") do |tmp|
  token = Base64.strict_encode64(JSON.generate(
    "jwtToken" => "jwt-initial",
    "matrixUrl" => "http://matrix.example",
    "controllerUrl" => "http://controller.example",
    "modelGatewayUrl" => "http://gateway.example"
  ))
  stdout, stderr, status = run_worker(
    worker_dir,
    { "TEAMHARNESS_WORKER_ONCE" => "1" },
    "--state-dir", tmp,
    "--work-dir", tmp,
    "--bootstrap-token-file", write_bootstrap_token_file(tmp, token),
    "--openclaw-command", "definitely-not-openclaw",
    "--once"
  )
  assert(status.success?, "missing OpenClaw should be a waiting state, not a process failure: #{stderr}")
  status_doc = JSON.parse(File.read(File.join(tmp, "status.json")))
  assert(status_doc["phase"] == "Waiting", "missing OpenClaw phase must be Waiting")
  assert(status_doc["reason"] == "OpenClawBinaryNotFound", "missing OpenClaw reason must be OpenClawBinaryNotFound")
  assert(stdout.empty?, "waiting path should not print stdout")
end

Dir.mktmpdir("teamharness-openclaw-worker-test-") do |tmp|
  runtime_yaml = <<~YAML
    apiVersion: hiclaw.io/v1beta1
    kind: MemberRuntimeConfig
    matrix:
      accessToken: matrix-token
      teamRoomId: "!team:matrix.example"
    member:
      name: worker-01
      runtimeName: worker-01
      runtime: remote-managed-local
      role: worker
      matrixUserId: "@worker-01:matrix.example"
      personalRoomId: "!personal:matrix.example"
    desired:
      model:
        model: qwen3.7-max
        gatewayUrl: http://127.0.0.1:1/default
        gatewayKey: gateway-key
      skillRegistry:
        provider: nacos
        url: nacos://market.example:80/public
        authType: sts-hiclaw
      agentPackage:
        ref: oss://agents/worker-01/packages/pkg.zip
    storage:
      provider: oss
      bucket: test-bucket
      endpoint: http://127.0.0.1:1
      sharedPrefix: teams/zhuoguang-test
      globalSharedPrefix: shared
      memberPrefix: agents/worker-01
  YAML

  requests = []
  oss_writes = {}
  oss_deletes = []
  package_zip = make_package(tmp, "pkg")
  safe_assets = prepare_safe_assets(repo_root, tmp)
  trace_config = write_loongsuite_trace_config(tmp)

  server = WEBrick::HTTPServer.new(
    BindAddress: "127.0.0.1",
    Port: 0,
    Logger: WEBrick::Log.new(File::NULL),
    AccessLog: []
  )
  port = server.listeners.first.addr[1]
  server.mount_proc("/") do |req, res|
    requests << [req.request_method, req.path, req.body, req["authorization"]]
    case [req.request_method, req.path]
    when ["POST", "/api/v1/edge/token"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        token: "edge-token",
        jwtToken: "jwt-refreshed",
        workerName: "worker-01",
        workerResourceName: "magic-worker-worker-01",
        runtimeName: "worker-01",
        teamName: "zhuoguang-test"
      )
    when ["POST", "/api/v1/credentials/sts"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        access_key_id: "ak",
        access_key_secret: "sk",
        security_token: "sts",
        oss_endpoint: "http://127.0.0.1:#{port}",
        oss_bucket: "test-bucket"
      )
    when ["GET", "/test-bucket/shared/runtime/members/worker-01/runtime.yaml"]
      res["Content-Type"] = "text/yaml"
      res.body = runtime_yaml.sub("http://127.0.0.1:1/default", "http://127.0.0.1:#{port}/default")
    when ["GET", "/test-bucket/agents/worker-01/packages/pkg.zip"]
      res["Content-Type"] = "application/zip"
      res.body = File.binread(package_zip)
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res["Content-Type"] = "application/json"
      res.body = "{}"
    else
      if req.request_method == "PUT" && req.path.start_with?("/test-bucket/")
        key = req.path.sub(%r{^/test-bucket/+}, "")
        oss_writes[key] = req.body
        res["Content-Type"] = "application/xml"
        res.body = ""
      elsif req.request_method == "DELETE" && req.path.start_with?("/test-bucket/")
        oss_deletes << req.path.sub(%r{^/test-bucket/+}, "")
        res["Content-Type"] = "application/xml"
        res.body = ""
      elsif req.request_method == "GET" && req.path == "/test-bucket/"
        prefix = req.query["prefix"].to_s
        keys = oss_writes.keys.select { |key| key.start_with?(prefix) }
        res["Content-Type"] = "application/xml"
        res.body = "<ListBucketResult>#{keys.map { |key| "<Contents><Key>#{key}</Key></Contents>" }.join}</ListBucketResult>"
      else
        res.status = 404
        res.body = "not found"
      end
    end
  end
  thread = Thread.new { server.start }
  begin
    bin_dir = File.join(tmp, "bin")
    FileUtils.mkdir_p(bin_dir)
    File.write(File.join(bin_dir, "openclaw"), "#!/bin/sh\nexit 0\n")
    File.chmod(0o755, File.join(bin_dir, "openclaw"))
    stale_plugin = File.join(tmp, "openclaw", "plugins", "loongsuite-js", "opentelemetry-instrumentation-openclaw")
    FileUtils.mkdir_p(stale_plugin)
    File.write(File.join(stale_plugin, "openclaw.plugin.json"), JSON.generate("id" => "stale-plugin"))
    File.write(File.join(tmp, "openclaw", "plugins", "loongsuite-js", ".teamharness-source-digest"), "stale-digest\n")

    stdout, stderr, status = run_worker(
      worker_dir,
      {
        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
        "TEAMHARNESS_WORKER_ONCE" => "1",
        "AGENT_DATA_COLLECTION_CONFIG" => trace_config
      },
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", safe_assets,
      "--bootstrap-token-file", write_bootstrap_token_file(tmp, bootstrap_token(port)),
      "--model-config-mode", "managed-runtime",
      "--once"
    )
    assert(status.success?, "remote bootstrap path should succeed: #{stderr}\n#{stdout}")
    status_doc = JSON.parse(File.read(File.join(tmp, "status.json")))
    assert(status_doc["reason"] == "HeartbeatReported", "final status should report heartbeat")
    assert(status_doc["runtime"] == "openclaw", "status runtime should be openclaw")
    assert(!File.exist?(File.join(tmp, "sts.json")), "STS credentials must not be written to disk")
    assert(!File.exist?(File.join(tmp, "edge-token.json")), "SA token state must not be written to disk")
    assert(!File.exist?(File.join(tmp, "credential-token")), "broker token should be removed after worker exits")

    runtime_snapshot = File.read(File.join(tmp, "runtime-state.json"))
    assert(!runtime_snapshot.include?("gateway-key"), "runtime snapshot must not contain gateway key")
    runtime_state = JSON.parse(runtime_snapshot)
    assert(runtime_state.dig("desired", "skillRegistry", "url") == "nacos://market.example:80/public", "skill registry should be projected")
    assert(runtime_state.dig("desired", "model", "hasGatewayKey") == true, "runtime snapshot should only record gateway key presence")

    package_state = JSON.parse(File.read(File.join(tmp, "agent-package-state.json")))
    workspace = package_state.fetch("workspaceDir")
    assert(File.read(File.join(workspace, "AGENTS.md"), encoding: "UTF-8").include?("你是一个测试agent"), "AGENTS.md should include package instructions")
    assert(File.read(File.join(workspace, "SOUL.md"), encoding: "UTF-8").include?("你是一个测试agent"), "SOUL.md should include package soul")
    assert(File.file?(File.join(workspace, "skills", "frontend-design", "SKILL.md")), "package skill should be injected")
    assert(File.file?(File.join(workspace, "skills", "task-execution", "SKILL.md")), "worker base skill should be injected")
    assert(!File.exist?(File.join(workspace, "skills", "team-coordination")), "leader-only skill must not be injected for worker")

    openclaw_config = JSON.parse(File.read(File.join(tmp, "openclaw", "config", "openclaw.json")))
    provider = openclaw_config.dig("models", "providers", "teamharness")
    assert(provider.fetch("apiKey") == "teamharness-local-proxy", "openclaw.json must use dummy local proxy key")
    assert(provider.fetch("baseUrl").start_with?("http://127.0.0.1:"), "openclaw provider should point to local model proxy")
    assert(!File.read(File.join(tmp, "openclaw", "config", "openclaw.json")).include?("gateway-key"), "openclaw.json must not contain raw gateway key")
    agent = openclaw_config.fetch("agents").fetch("list").first
    assert(agent.fetch("skills").include?("frontend-design"), "openclaw allowlist should include package skill")
    assert(!agent.fetch("skills").include?("team-coordination"), "openclaw allowlist must exclude leader-only skill")
    mcp = openclaw_config.dig("mcp", "servers", "teamharness")
    assert(mcp.fetch("args").first.end_with?("node-mcp/server.js"), "openclaw mcp must use Node MCP server")
    assert(mcp.fetch("cwd") == workspace, "openclaw mcp cwd must be managed workspace")
    trace_entry = openclaw_config.dig("plugins", "entries", "opentelemetry-instrumentation-openclaw")
    assert(trace_entry.fetch("enabled") == true, "trace plugin should be enabled when bundled/source plugin exists")
    assert(trace_entry.dig("hooks", "allowConversationAccess") == true, "trace plugin needs agent_end hook access")
    assert(trace_entry.dig("config", "batchSize") == 10, "trace plugin should keep a small default batch size")
    trace_paths = openclaw_config.dig("plugins", "load", "paths")
    trace_path = trace_paths.find { |entry| File.file?(File.join(entry, "openclaw.plugin.json")) }
    assert(
      trace_path,
      "openclaw config must point at the trace plugin directory"
    )
    assert(trace_path.start_with?(File.join(tmp, "openclaw", "plugins", "loongsuite-js")), "trace plugin path must be persisted under worker state")
    assert(
      File.read(File.join(trace_path, "openclaw.plugin.json")).include?("opentelemetry-instrumentation-openclaw"),
      "stale persistent trace plugin should be refreshed from bundle source"
    )
    assert(
      File.read(File.join(tmp, "openclaw", "plugins", "loongsuite-js", ".teamharness-source-digest")).strip == "test-loongsuite-digest-v1",
      "persistent trace plugin digest marker should match bundle source"
    )
    config_text = File.read(File.join(tmp, "openclaw", "config", "openclaw.json"))
    assert(!config_text.include?("x-arms-license-key"), "openclaw.json must not contain CMS license header")
    assert(!config_text.include?("apm/trace/opentelemetry"), "openclaw.json must not contain CMS endpoint")
    assert(!config_text.include?("test-license-key"), "openclaw.json must not contain CMS license")

    assert(oss_body(oss_writes, "agents/worker-01/AGENTS.md").include?("你是一个测试agent"), "AgentPackage view should sync AGENTS.md")
    assert(oss_body(oss_writes, "agents/worker-01/SOUL.md").include?("你是一个测试agent"), "AgentPackage view should sync SOUL.md")
    assert(oss_body(oss_writes, "agents/worker-01/skills/frontend-design/SKILL.md").include?("Frontend design test skill"), "AgentPackage view should sync skills")
    assert(oss_body(oss_writes, "agents/worker-01/.agent-package.json").include?("pkg.zip"), "AgentPackage view marker should be synced")
    assert(oss_body(oss_writes, "agents/worker-01/.qwenpaw/workspaces/default/AGENTS.md").include?("你是一个测试agent"), "legacy package view should sync AGENTS.md")
	    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/edge/token" }, "edge token request missing")
	    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/credentials/sts" }, "STS request missing")
	    assert(requests.any? { |entry| entry[0] == "GET" && entry[1] == "/test-bucket/shared/runtime/members/worker-01/runtime.yaml" }, "runtime.yaml OSS GET missing")
	    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/workers/magic-worker-worker-01/heartbeat" }, "heartbeat request missing")

	    user_state = File.join(tmp, "user-model-state")
	    user_work = File.join(tmp, "user-model-work")
	    user_config = File.join(tmp, "user-openclaw.json")
	    File.write(user_config, JSON.pretty_generate(
	      "models" => {
	        "providers" => {
	          "local" => {
	            "baseUrl" => "http://127.0.0.1:11434/v1",
	            "apiKey" => "local-key",
	            "api" => "openai-completions",
	            "models" => [
	              {"id" => "local-model", "name" => "Local model"}
	            ]
	          }
	        }
	      },
	      "auth" => {
	        "profiles" => {
	          "local-profile" => {"provider" => "local", "mode" => "api_key"}
	        }
	      },
	      "secrets" => {
	        "providers" => {}
	      },
	      "agents" => {
	        "defaults" => {
	          "model" => "local/local-model"
	        },
	        "list" => [
	          {
	            "id" => "worker-01",
	            "model" => "local/local-worker-model",
	            "models" => {
	              "local/local-worker-model" => {"contextWindow" => 32_000}
	            }
	          }
	        ]
	      }
	    ) + "\n")
	    stdout, stderr, status = run_worker(
	      worker_dir,
	      {
	        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
	        "TEAMHARNESS_WORKER_ONCE" => "1",
	        "AGENT_DATA_COLLECTION_CONFIG" => trace_config,
	        "OPENCLAW_CONFIG_PATH" => user_config
	      },
      "--state-dir", user_state,
      "--work-dir", user_work,
      "--plugin-dir", safe_assets,
      "--bootstrap-token-file", write_bootstrap_token_file(user_state, bootstrap_token(port)),
      "--model-config-mode", "native-config",
      "--once"
	    )
	    assert(status.success?, "user model scope bootstrap path should succeed: #{stderr}\n#{stdout}")
	    user_openclaw_config = JSON.parse(File.read(File.join(user_state, "openclaw", "config", "openclaw.json")))
	    assert(user_openclaw_config.dig("models", "providers", "teamharness").nil?, "user model scope must not generate managed teamharness provider")
	    assert(user_openclaw_config.dig("models", "providers", "local", "baseUrl") == "http://127.0.0.1:11434/v1", "user model scope should inherit local OpenClaw provider")
	    assert(user_openclaw_config.dig("auth", "profiles", "local-profile", "provider") == "local", "user model scope should inherit auth profile metadata")
	    assert(user_openclaw_config.dig("agents", "defaults", "model") == "local/local-model", "user model scope should inherit local default model")
	    user_agent = user_openclaw_config.fetch("agents").fetch("list").first
	    assert(user_agent.fetch("model") == "local/local-worker-model", "user model scope should inherit local agent model")
	    assert(user_agent.dig("models", "local/local-worker-model", "contextWindow") == 32_000, "user model scope should inherit local agent model metadata")
	    assert(user_agent.fetch("skills").include?("frontend-design"), "user model scope should keep managed skill allowlist")
	    assert(user_openclaw_config.dig("mcp", "servers", "teamharness"), "user model scope should keep TeamHarness MCP")
	  ensure
	    server.shutdown
	    thread.join
  end
end

Dir.mktmpdir("teamharness-openclaw-worker-matrix-test-") do |tmp|
  runtime_yaml = <<~YAML
    apiVersion: hiclaw.io/v1beta1
    kind: MemberRuntimeConfig
    matrix:
      accessToken: matrix-token
      teamRoomId: "!team:matrix.example"
    member:
      name: worker-01
      runtimeName: worker-01
      role: worker
      matrixUserId: "@worker-01:matrix.example"
      personalRoomId: "!personal:matrix.example"
    desired:
      model:
        model: qwen3.7-max
        gatewayUrl: http://127.0.0.1:1/default
        gatewayKey: gateway-key
      agentPackage:
        ref: oss://agents/worker-01/packages/pkg-v1.zip
    storage:
      provider: oss
      bucket: test-bucket
      endpoint: http://127.0.0.1:1
      sharedPrefix: teams/zhuoguang-test
      globalSharedPrefix: shared
      memberPrefix: agents/worker-01
  YAML
  runtime_yaml_v2 = runtime_yaml.sub("pkg-v1.zip", "pkg-v2.zip")

  sent_messages = []
  gateway_requests = []
  runtime_reads = 0
  sync_events = [
    {
      type: "m.room.message",
      event_id: "$history",
      sender: "@zhuoguang:matrix.example",
      content: { msgtype: "m.text", body: "测试代号 remote-openclaw-node-e2e" }
    },
    {
      type: "m.room.message",
      event_id: "$similar",
      sender: "@zhuoguang:matrix.example",
      content: { msgtype: "m.text", body: "worker-012 不应该触发" }
    },
    {
      type: "m.room.message",
      event_id: "$mention",
      sender: "@zhuoguang:matrix.example",
      content: {
        msgtype: "m.text",
        body: "@worker-01 触发一次",
        "m.mentions" => { user_ids: ["@worker-01:matrix.example"] }
      }
    }
  ]
  package_v1 = make_package(tmp, "pkg-v1", instructions: "package v1 instructions")
  package_v2 = make_package(tmp, "pkg-v2", instructions: "package v2 instructions")
  safe_assets = prepare_safe_assets(repo_root, tmp)
  trace_config = write_loongsuite_trace_config(tmp)
  server = WEBrick::HTTPServer.new(
    BindAddress: "127.0.0.1",
    Port: 0,
    Logger: WEBrick::Log.new(File::NULL),
    AccessLog: []
  )
  port = server.listeners.first.addr[1]
  server.mount_proc("/") do |req, res|
    case [req.request_method, req.path]
    when ["POST", "/api/v1/edge/token"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(token: "edge-token", jwtToken: "jwt-refreshed", workerName: "worker-01", workerResourceName: "magic-worker-worker-01", teamName: "zhuoguang-test")
    when ["POST", "/api/v1/credentials/sts"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        access_key_id: "ak",
        access_key_secret: "sk",
        security_token: "sts",
        oss_endpoint: "http://127.0.0.1:#{port}",
        oss_bucket: "test-bucket"
      )
    when ["GET", "/test-bucket/shared/runtime/members/worker-01/runtime.yaml"]
      runtime_reads += 1
      res["Content-Type"] = "text/yaml"
      selected_yaml = runtime_reads >= 2 ? runtime_yaml_v2 : runtime_yaml
      res.body = selected_yaml.sub("http://127.0.0.1:1/default", "http://127.0.0.1:#{port}/default")
    when ["GET", "/test-bucket/agents/worker-01/packages/pkg-v1.zip"]
      res["Content-Type"] = "application/zip"
      res.body = File.binread(package_v1)
    when ["GET", "/test-bucket/agents/worker-01/packages/pkg-v2.zip"]
      res["Content-Type"] = "application/zip"
      res.body = File.binread(package_v2)
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res.body = "{}"
    when ["POST", "/default/v1/chat/completions"]
      body = JSON.parse(req.body)
      gateway_requests << body
      if body["stream"]
        res["Content-Type"] = "text/event-stream"
        chunk = {
          choices: [
            {
              index: 0,
              delta: { content: "hello through streaming model proxy" },
              finish_reason: nil
            }
          ]
        }
        done = { choices: [{ index: 0, delta: {}, finish_reason: "stop" }] }
        res.body = "data: #{JSON.generate(chunk)}\n\ndata: #{JSON.generate(done)}\n\ndata: [DONE]\n\n"
      else
        res["Content-Type"] = "application/json"
        res.body = JSON.generate(choices: [{ message: { role: "assistant", content: "hello through model proxy" } }])
      end
    when ["GET", "/_matrix/client/v3/sync"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        next_batch: "team-sync-1",
        rooms: {
          join: {
                  "!team:matrix.example" => {
                    timeline: {
                      events: sync_events
                    }
                  }
          }
        }
      )
    else
      if req.request_method == "PUT" && req.path.include?("/_matrix/client/v3/rooms/")
        sent_messages << JSON.parse(req.body) if req.path.include?("/send/")
        res["Content-Type"] = "application/json"
        res.body = JSON.generate(event_id: "$sent#{sent_messages.length}")
      else
        res.status = 404
        res.body = "not found"
      end
    end
  end
  thread = Thread.new { server.start }
  begin
    bin_dir = File.join(tmp, "bin")
    FileUtils.mkdir_p(bin_dir)
    argv_file = File.join(tmp, "openclaw-argv.txt")
    prompt_file = File.join(tmp, "openclaw-message.txt")
    env_file = File.join(tmp, "openclaw-env.json")
    descriptor_file = File.join(tmp, "descriptor-during-run.txt")
    File.write(File.join(bin_dir, "openclaw"), <<~SH)
      #!/bin/sh
      printf '%s\\n' "$@" > #{argv_file}
      previous=""
      message=""
      for arg in "$@"; do
        if [ "$previous" = "--message" ]; then
          message="$arg"
        fi
        previous="$arg"
      done
      printf '%s' "$message" > #{prompt_file}
      export TEAMHARNESS_FAKE_OPENCLAW_MESSAGE="$message"
      if [ -f "$TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR" ]; then
        printf 'present\\n' > #{descriptor_file}
      else
        printf 'missing\\n' > #{descriptor_file}
      fi
      node - "$OPENCLAW_CONFIG_PATH" "$OPENCLAW_WORKSPACE_DIR" #{env_file} <<'JS'
      const fs = require("fs");
      const path = require("path");
      const configPath = process.argv[2];
      const workspace = process.argv[3];
      const envFile = process.argv[4];
        fs.appendFileSync(envFile, JSON.stringify({
          configPath: process.env.OPENCLAW_CONFIG_PATH,
          stateDir: process.env.OPENCLAW_STATE_DIR,
          workspaceDir: process.env.OPENCLAW_WORKSPACE_DIR,
          broker: process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR,
          armsEndpoint: process.env.ARMS_OTLP_ENDPOINT,
          armsProject: process.env.ARMS_PROJECT,
          armsWorkspace: process.env.ARMS_CMS_WORKSPACE,
          armsServiceName: process.env.ARMS_SERVICE_NAME,
          armsLicensePresent: Boolean(process.env.ARMS_LICENSE_KEY),
          otelResourceAttributes: process.env.OTEL_RESOURCE_ATTRIBUTES,
          agents: fs.readFileSync(workspace + "/AGENTS.md", "utf8")
        }) + "\\n");
        const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
        if (!config.plugins?.entries?.["opentelemetry-instrumentation-openclaw"]?.enabled) {
          throw new Error("trace plugin missing from generated openclaw config");
        }
      const provider = config.models?.providers?.teamharness;
      const sessionFile = path.join(process.env.OPENCLAW_STATE_DIR, "agents", "worker-01", "sessions", "fake-session.jsonl");
      fs.mkdirSync(path.dirname(sessionFile), {recursive: true});
      function writeSession(event) {
        fs.appendFileSync(sessionFile, JSON.stringify({...event, timestamp: new Date().toISOString()}) + "\\n");
      }
      writeSession({
        type: "message",
        message: {
          role: "assistant",
          content: [
            {type: "reasoning", text: "openclaw thinking before tool"},
            {type: "toolCall", name: "teamharness__health", arguments: {query: "health", token: "secret-token"}}
          ]
        }
      });
      writeSession({
        type: "message",
        message: {
          role: "toolResult",
          toolName: "teamharness__health",
          content: [{type: "text", text: JSON.stringify({ok: true, token: "secret-token"})}]
        }
      });
      function emitResult(text) {
        console.log(JSON.stringify({status: "ok", result: {payloads: [{text: "openclaw final: " + text}]}, meta: {agentMeta: {sessionFile}}}, null, 2));
      }
      if ((process.env.TEAMHARNESS_FAKE_OPENCLAW_MESSAGE || "").includes("force no reply")) {
        writeSession({
          type: "message",
          message: {
            role: "assistant",
            content: [{type: "text", text: "NO_REPLY"}]
          }
        });
        console.log(JSON.stringify({payloads: [], meta: {agentMeta: {sessionFile}}}, null, 2));
        process.exit(0);
      }
      if (!provider) {
        emitResult("local user model");
      } else {
        fetch(provider.baseUrl.replace(/\\/+$/, "") + "/chat/completions", {
          method: "POST",
          headers: {"content-type": "application/json", authorization: "Bearer " + provider.apiKey},
          body: JSON.stringify({model: "ignored", messages: [{role: "user", content: "ping"}], stream: true})
        }).then(response => response.text()).then(payload => {
          const text = payload.split(/\\r?\\n/).filter(line => line.startsWith("data: "))
            .map(line => line.slice(6).trim())
            .filter(line => line && line !== "[DONE]")
            .map(line => JSON.parse(line))
            .map(chunk => chunk.choices?.[0]?.delta?.content || "")
            .join("") || payload;
          emitResult(text);
        }).catch(error => {
          console.error(error.stack || error.message);
          process.exit(1);
        });
      }
      JS
    SH
    File.chmod(0o755, File.join(bin_dir, "openclaw"))
    write_live_matrix_state(tmp)

    stdout, stderr, status = run_worker(
      worker_dir,
      {
        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
        "TEAMHARNESS_MATRIX_ONCE" => "1",
        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0",
        "AGENT_DATA_COLLECTION_CONFIG" => trace_config
      },
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", safe_assets,
      "--bootstrap-token-file", write_bootstrap_token_file(tmp, bootstrap_token(port)),
      "--model-config-mode", "managed-runtime"
    )
    assert(status.success?, "matrix path should succeed: #{stderr}\n#{stdout}")
    assert(File.exist?(argv_file), "fake OpenClaw should be invoked; sent=#{sent_messages.inspect}; stderr=#{stderr}; stdout=#{stdout}")
    argv = File.read(argv_file, encoding: "UTF-8")
    assert(argv.include?("agent\n"), "OpenClaw invocation should use agent command")
    assert(argv.include?("--local\n"), "OpenClaw invocation should use --local")
    assert(argv.include?("--json\n"), "OpenClaw invocation should use --json")
    assert(argv.include?("--agent\nworker-01"), "OpenClaw invocation should pass member id")
    assert(argv.include?("--model\nteamharness/qwen3.7-max"), "OpenClaw invocation should pass managed model")
    argv_lines = argv.split("\n")
    session_key = argv_lines[argv_lines.index("--session-key") + 1]
    assert(session_key.match?(/\Aoc_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+\z/), "OpenClaw session key should use opaque oc_ naming: #{session_key}")
    assert(!session_key.include?("!team:matrix.example"), "OpenClaw session key must not include raw Matrix room id")
    assert(!session_key.include?(File.expand_path(tmp)), "OpenClaw session key must not include workdir")
    assert(!argv.include?("--plugin-dir"), "OpenClaw invocation must not use Claude plugin-dir")
    assert(!argv.include?("stream-json"), "OpenClaw invocation must not use Claude stream-json")
    assert(!argv.include?("--settings"), "OpenClaw invocation must not use Claude settings")

    prompt = File.read(prompt_file, encoding: "UTF-8")
    assert(prompt.include?("@worker-01 触发一次"), "prompt should include current mention")
    assert(prompt.include?("[Chat messages since your last reply - for context]"), "prompt should include history marker")
    assert(prompt.include?("[Current message - respond to this]"), "prompt should include current-message marker")
    assert(prompt.include?("remote-openclaw-node-e2e"), "prompt should include recent team-room history")
    assert(prompt.include?("worker-012 不应该触发"), "similar member name should be carried only as room history context")
    assert(!prompt.include?("package v2 instructions"), "prompt should not repeat package instructions")
    assert(!prompt.include?("Team: zhuoguang-test"), "prompt should not carry stable team context")

    env_payloads = File.readlines(env_file).map { |line| JSON.parse(line) }
    env_payload = env_payloads.last
    assert(env_payload["configPath"].end_with?("openclaw/config/openclaw.json"), "OPENCLAW_CONFIG_PATH mismatch")
      assert(env_payload["stateDir"].end_with?("openclaw/state"), "OPENCLAW_STATE_DIR mismatch")
      assert(env_payload["workspaceDir"].end_with?("openclaw/workspace/current"), "OPENCLAW_WORKSPACE_DIR mismatch")
      assert(env_payload["broker"].end_with?(".teamharness/credential-broker.json"), "broker descriptor path mismatch")
      assert(env_payload["armsEndpoint"] == "https://proj-test.example.com/apm/trace/opentelemetry", "ARMS_OTLP_ENDPOINT mismatch")
      assert(env_payload["armsProject"] == "proj-test", "ARMS_PROJECT mismatch")
      assert(env_payload["armsWorkspace"] == "cms-workspace-test", "ARMS_CMS_WORKSPACE mismatch")
      assert(env_payload["armsServiceName"] == "ai-coding-agent-openclaw", "ARMS_SERVICE_NAME mismatch")
      assert(env_payload["armsLicensePresent"] == true, "ARMS_LICENSE_KEY should be injected")
      assert(env_payload["otelResourceAttributes"].to_s.include?("agentteams.worker.name=worker-01"), "OTEL_RESOURCE_ATTRIBUTES should carry AgentTeams worker resource")
    assert(env_payload["agents"].include?("You are"), "workspace AGENTS.md should include base prompt")
    assert(env_payload["agents"].include?("BEGIN TEAMHARNESS RUNTIME CONTEXT"), "workspace AGENTS.md should include runtime context block")
    assert(env_payload["agents"].include?("Team: zhuoguang-test"), "workspace AGENTS.md should include stable team context")
    assert(env_payloads.any? { |payload| payload["agents"].include?("package v2 instructions") }, "updated package instructions should be used after hot update")
    assert(File.read(descriptor_file).strip == "present", "broker descriptor should exist while OpenClaw runs")

    package_state = JSON.parse(File.read(File.join(tmp, "agent-package-state.json")))
    assert(package_state["ref"] == "oss://agents/worker-01/packages/pkg-v2.zip", "package state should point to hot-updated ref")
    assert(File.read(File.join(package_state.fetch("workspaceDir"), "AGENTS.md"), encoding: "UTF-8").include?("package v2 instructions"), "workspace should keep hot-updated package instructions")
    matrix_state = JSON.parse(File.read(File.join(tmp, "matrix-state.json")))
    assert(matrix_state.dig("openclawSessions", "!team:matrix.example") == session_key, "OpenClaw session should be stored under room id")

    assert(gateway_requests.any? { |body| body["model"] == "qwen3.7-max" && body["stream"] == true }, "model proxy should stream and rewrite to managed model")
    placeholder = sent_messages.find { |content| content["body"] == "处理中..." }
    assert(placeholder, "placeholder Matrix message missing")
    final_edit = sent_messages.find { |content| content.dig("m.new_content", "body")&.include?("openclaw final: hello through streaming model proxy") }
    assert(final_edit, "edited final Matrix message missing")
    assert(final_edit.dig("m.relates_to", "rel_type") == "m.replace", "final result should edit placeholder")
    thread_messages = sent_messages.select { |content| content.dig("m.relates_to", "rel_type") == "m.thread" }
    assert(thread_messages.any? { |content| content["body"].to_s.include?("Thinking:") && content["body"].to_s.include?("openclaw thinking before tool") }, "OpenClaw thinking summary should be sent to Matrix thread")
    tool_message = thread_messages.find { |content| content["body"].to_s.include?("tool_use: `teamharness__health`") }
    assert(tool_message, "OpenClaw tool_use summary should be sent to Matrix thread")
    assert(tool_message["body"].include?("[REDACTED]"), "OpenClaw tool_use summary should redact sensitive input")
    assert(!tool_message["body"].include?("secret-token"), "OpenClaw tool_use summary must not leak sensitive input")
    tool_result = thread_messages.find { |content| content["body"].to_s.include?("tool_result: `teamharness__health`") }
	    assert(tool_result, "OpenClaw tool_result summary should be sent to Matrix thread")
	    assert(tool_result["body"].include?("[REDACTED]"), "OpenClaw tool_result summary should redact sensitive output")
	    assert(!tool_result["body"].include?("secret-token"), "OpenClaw tool_result summary must not leak sensitive output")
	    assert(!File.exist?(File.join(tmp, ".loongsuite-pilot", "openclaw", "profiles")), "local scope must not create OpenClaw global profiles")

	    sent_messages.clear
	    sync_events.replace([
	      {
	        type: "m.room.message",
	        event_id: "$noreply",
	        sender: "@zhuoguang:matrix.example",
	        content: {
	          msgtype: "m.text",
	          body: "@worker-01 force no reply",
	          "m.mentions" => { user_ids: ["@worker-01:matrix.example"] }
	        }
	      }
	    ])
	    stdout, stderr, status = run_worker(
	      worker_dir,
	      {
	        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
	        "TEAMHARNESS_MATRIX_ONCE" => "1",
	        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0",
	        "AGENT_DATA_COLLECTION_CONFIG" => trace_config
	      },
	      "--state-dir", tmp,
	      "--work-dir", tmp,
	      "--plugin-dir", safe_assets,
	      "--bootstrap-token-file", write_bootstrap_token_file(tmp, bootstrap_token(port)),
	      "--model-config-mode", "managed-runtime"
	    )
	    assert(status.success?, "NO_REPLY matrix path should succeed: #{stderr}\n#{stdout}")
	    no_reply_placeholder = sent_messages.find { |content| content["body"] == "处理中..." }
	    assert(no_reply_placeholder, "NO_REPLY placeholder Matrix message missing")
	    no_reply_edit = sent_messages.find { |content| content.dig("m.new_content", "body") == "已处理" }
	    assert(no_reply_edit, "NO_REPLY should edit placeholder to 已处理")
	    assert(no_reply_edit.dig("m.relates_to", "rel_type") == "m.replace", "NO_REPLY completion should use Matrix replace")
	    assert(!sent_messages.any? { |content| content.dig("m.new_content", "body") == "NO_REPLY" }, "NO_REPLY must not be sent as final Matrix content")

	    gateway_request_count = gateway_requests.length
	    user_matrix_state_dir = File.join(tmp, "user-model-matrix-state")
	    write_live_matrix_state(user_matrix_state_dir)
	    FileUtils.rm_f(argv_file)
	    stdout, stderr, status = run_worker(
	      worker_dir,
	      {
	        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
	        "TEAMHARNESS_MATRIX_ONCE" => "1",
	        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0",
	        "AGENT_DATA_COLLECTION_CONFIG" => trace_config
	      },
	      "--state-dir", user_matrix_state_dir,
	      "--work-dir", File.join(tmp, "user-model-matrix-work"),
	      "--plugin-dir", safe_assets,
	      "--bootstrap-token-file", write_bootstrap_token_file(user_matrix_state_dir, bootstrap_token(port)),
	      "--model-config-mode", "native-config"
	    )
	    assert(status.success?, "native config matrix path should succeed: #{stderr}\n#{stdout}")
	    user_argv = File.read(argv_file)
	    assert(!user_argv.include?("--model"), "native config should not pass managed OpenClaw --model")
	    assert(gateway_requests.length == gateway_request_count, "native config should not call managed model proxy")
	    user_openclaw_config = JSON.parse(File.read(File.join(user_matrix_state_dir, "openclaw", "config", "openclaw.json")))
	    assert(user_openclaw_config.dig("models", "providers", "teamharness").nil?, "native config should not write managed OpenClaw provider")
	  ensure
	    server.shutdown
	    thread.join
  end
end
