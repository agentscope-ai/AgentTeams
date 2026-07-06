#!/usr/bin/env ruby
# frozen_string_literal: true

require "base64"
require "digest"
require "fileutils"
require "json"
require "open3"
require "pathname"
require "tmpdir"
require "webrick"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
worker_cli = repo_root / "plugins/teamharness/remote/claude-code/node-worker/src/cli.js"

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

def run_node(env, *args)
  Open3.capture3(env, "node", *args)
end

def write_bootstrap_token(dir, token)
  file = File.join(dir, "bootstrap-token")
  File.write(file, "#{token}\n")
  file
end

def wait_until(message, timeout: 10)
  deadline = Time.now + timeout
  until yield
    fail!(message) if Time.now >= deadline
    sleep 0.05
  end
end

def stop_process(pid)
  Process.kill("TERM", pid)
  Process.wait(pid)
rescue Errno::ESRCH, Errno::ECHILD
  nil
end

Dir.mktmpdir("teamharness-node-worker-test-") do |tmp|
  stdout, stderr, status = run_node({ "TEAMHARNESS_WORKER_ONCE" => "1" }, worker_cli.to_s, "--state-dir", tmp, "--work-dir", tmp, "--once")
  assert(!status.success?, "missing bootstrap token should fail")
  status_doc = JSON.parse(File.read(File.join(tmp, "status.json")))
  assert(status_doc["phase"] == "Failed", "missing token phase must be Failed")
  assert(status_doc["reason"] == "MissingBootstrapToken", "missing token reason must be MissingBootstrapToken")
  assert(stderr.include?("bootstrap token file is required"), "stderr should explain missing token")
  assert(stdout.empty?, "missing token should not print stdout")
end

Dir.mktmpdir("teamharness-node-worker-test-") do |tmp|
  token = Base64.strict_encode64(JSON.generate(
    "jwtToken" => "jwt-initial",
    "matrixUrl" => "http://matrix.example",
    "controllerUrl" => "http://controller.example",
    "modelGatewayUrl" => "http://gateway.example"
  ))
  stdout, stderr, status = run_node(
    { "TEAMHARNESS_WORKER_ONCE" => "1" },
    worker_cli.to_s,
    "--state-dir", tmp,
    "--work-dir", tmp,
    "--bootstrap-token-file", write_bootstrap_token(tmp, token),
    "--claude-command", "definitely-not-claude-code",
    "--once"
  )
  assert(status.success?, "missing Claude Code should be a waiting state, not a process failure: #{stderr}")
  status_doc = JSON.parse(File.read(File.join(tmp, "status.json")))
  assert(status_doc["phase"] == "Waiting", "missing Claude phase must be Waiting")
  assert(status_doc["reason"] == "ClaudeCodeNotFound", "missing Claude reason must be ClaudeCodeNotFound")
  assert(stdout.empty?, "waiting path should not print stdout")
end

Dir.mktmpdir("teamharness-node-worker-test-") do |tmp|
  runtime_yaml = <<~YAML
    apiVersion: hiclaw.io/v1beta1
    kind: MemberRuntimeConfig
    member:
      name: worker-01
      runtimeName: worker-01
      runtime: remote-managed-local
      role: worker
      personalRoomId: "!personal:matrix.example"
    desired:
      model:
        model: qwen3.7-max
        gatewayUrl: http://gateway.example/default
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
  package_zip = File.join(tmp, "pkg.zip")
  package_src = File.join(tmp, "pkg-src")
  FileUtils.mkdir_p(File.join(package_src, "config"))
  FileUtils.mkdir_p(File.join(package_src, "skills", "frontend-design"))
  File.write(File.join(package_src, "config", "AGENTS.md"), "你是一个测试agent\n")
  File.write(File.join(package_src, "config", "SOUL.md"), "你是一个测试agent\n")
  File.write(File.join(package_src, "skills", "frontend-design", "SKILL.md"), "---\nname: frontend-design\n---\nFrontend design test skill.\n")
  stdout, stderr, status = Open3.capture3("zip", "-qr", package_zip, ".", chdir: package_src)
  assert(status.success?, "failed to create package zip: #{stderr}\n#{stdout}")

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
      res.body = runtime_yaml
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
      else
      res.status = 404
      res.body = "not found"
      end
    end
  end
  thread = Thread.new { server.start }
  begin
    bin_dir = File.join(tmp, "bin")
    Dir.mkdir(bin_dir)
    plugin_dir = File.join(tmp, "plugin")
    Dir.mkdir(plugin_dir)
    claude = File.join(bin_dir, "fake-claude")
    File.write(claude, "#!/bin/sh\nexit 0\n")
    File.chmod(0o755, claude)

    token = Base64.strict_encode64(JSON.generate(
      "jwtToken" => "jwt-initial",
      "matrixUrl" => "http://matrix.example",
      "controllerUrl" => "http://127.0.0.1:#{port}",
      "modelGatewayUrl" => "http://gateway.example"
    ))
    stdout, stderr, status = run_node(
      { "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}", "TEAMHARNESS_WORKER_ONCE" => "1" },
      worker_cli.to_s,
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--claude-command", "fake-claude",
      "--once"
    )
    assert(status.success?, "remote bootstrap path should succeed: #{stderr}\n#{stdout}")
	    status_doc = JSON.parse(File.read(File.join(tmp, "status.json")))
	    assert(status_doc["reason"] == "HeartbeatReported", "final status should report heartbeat")
	    assert(!File.exist?(File.join(tmp, "sts.json")), "STS credentials must not be written to disk")
	    assert(!File.exist?(File.join(tmp, "edge-token.json")), "SA token state must not be written to disk")
	    assert(!File.exist?(File.join(tmp, "credential-token")), "broker token should be removed after worker exits")
	    assert(!File.exist?(File.join(tmp, "credential-broker.json")), "broker descriptor should be removed after worker exits")
	    edge_state = JSON.parse(File.read(File.join(tmp, "edge-state.json")))
	    assert(!edge_state.key?("token"), "edge-state must not contain SA token")
	    assert(!edge_state.key?("workerUuid"), "edge-state must not contain worker UUID")
	    runtime_state = JSON.parse(File.read(File.join(tmp, "runtime-state.json")))
	    assert(runtime_state.dig("member", "name") == "worker-01", "runtime member name should be parsed")
	    assert(runtime_state.dig("desired", "skillRegistry", "url") == "nacos://market.example:80/public", "skill registry should be projected")
	    assert(runtime_state.dig("desired", "model", "hasGatewayKey") == true, "runtime snapshot should only record gateway key presence")
	    runtime_snapshot = File.read(File.join(tmp, "runtime-state.json"))
	    assert(!runtime_snapshot.include?("gateway-key"), "runtime snapshot must not contain gateway key")
	    package_state = JSON.parse(File.read(File.join(tmp, "agent-package-state.json")))
	    overlay_dir = package_state.fetch("overlayDir")
	    assert(File.read(File.join(overlay_dir, ".teamharness", "runtime-context", "AGENTS.md"), encoding: "UTF-8").include?("你是一个测试agent"), "AGENTS.md should be injected")
    assert(File.read(File.join(overlay_dir, ".teamharness", "runtime-context", "SOUL.md"), encoding: "UTF-8").include?("你是一个测试agent"), "SOUL.md should be injected")
    assert(File.file?(File.join(overlay_dir, "skills", "frontend-design", "SKILL.md")), "frontend-design skill should be injected")
    assert(oss_body(oss_writes, "agents/worker-01/AGENTS.md").include?("你是一个测试agent"), "AgentPackage view should sync AGENTS.md")
    assert(oss_body(oss_writes, "agents/worker-01/SOUL.md").include?("你是一个测试agent"), "AgentPackage view should sync SOUL.md")
    assert(oss_body(oss_writes, "agents/worker-01/skills/frontend-design/SKILL.md").include?("Frontend design test skill"), "AgentPackage view should sync skills")
    assert(oss_body(oss_writes, "agents/worker-01/.agent-package.json").include?("pkg.zip"), "AgentPackage view marker should be synced")
    assert(oss_body(oss_writes, "agents/worker-01/.qwenpaw/workspaces/default/AGENTS.md").include?("你是一个测试agent"), "AgentPackage view should sync qwenpaw-compatible AGENTS.md")
    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/edge/token" }, "edge token request missing")
    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/credentials/sts" }, "STS request missing")
    assert(requests.any? { |entry| entry[0] == "GET" && entry[1] == "/test-bucket/shared/runtime/members/worker-01/runtime.yaml" }, "runtime.yaml OSS GET missing")
    assert(requests.any? { |entry| entry[0] == "POST" && entry[1] == "/api/v1/workers/magic-worker-worker-01/heartbeat" }, "heartbeat request missing")

    File.write(File.join(plugin_dir, "base-marker.txt"), "new bundle marker\n")
    stdout, stderr, status = run_node(
      { "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}", "TEAMHARNESS_WORKER_ONCE" => "1" },
      worker_cli.to_s,
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--claude-command", "fake-claude",
      "--once"
    )
    assert(status.success?, "same package ref should survive base plugin update: #{stderr}\n#{stdout}")
    updated_package_state = JSON.parse(File.read(File.join(tmp, "agent-package-state.json")))
    updated_overlay_dir = updated_package_state.fetch("overlayDir")
    assert(File.file?(File.join(updated_overlay_dir, "base-marker.txt")), "same package ref should rebuild overlay when base plugin changes")
    assert(updated_package_state["basePluginDigest"] != package_state["basePluginDigest"], "base plugin digest should change after bundle update")
  ensure
    server.shutdown
    thread.join
  end
end

Dir.mktmpdir("teamharness-node-worker-matrix-test-") do |tmp|
  runtime_yaml_v1 = <<~YAML
    apiVersion: hiclaw.io/v1beta1
    kind: MemberRuntimeConfig
    matrix:
      accessToken: matrix-token
    member:
      name: worker-01
      runtimeName: worker-01
      role: worker
      matrixUserId: "@worker-01:matrix.example"
      personalRoomId: "!personal:matrix.example"
    desired:
      model:
        model: qwen3.7-max
        gatewayUrl: http://runtime-gateway.example/default
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
  runtime_yaml_v2 = runtime_yaml_v1.sub("pkg-v1.zip", "pkg-v2.zip")

  package_v1 = File.join(tmp, "pkg-v1.zip")
  package_v2 = File.join(tmp, "pkg-v2.zip")
  [["package v1 instructions", package_v1], ["package v2 instructions", package_v2]].each do |instructions, zip_path|
    package_src = File.join(tmp, "pkg-src-#{File.basename(zip_path, ".zip")}")
    FileUtils.mkdir_p(File.join(package_src, "config"))
    File.write(File.join(package_src, "config", "AGENTS.md"), "#{instructions}\n")
    stdout, stderr, status = Open3.capture3("zip", "-qr", zip_path, ".", chdir: package_src)
    assert(status.success?, "failed to create #{zip_path}: #{stderr}\n#{stdout}")
  end

  sent_messages = []
  gateway_requests = []
  runtime_reads = 0
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
      res.body = JSON.generate(token: "edge-token", jwtToken: "jwt-refreshed", workerName: "worker-01", workerResourceName: "magic-worker-worker-01")
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
      res.body = runtime_reads >= 2 ? runtime_yaml_v2 : runtime_yaml_v1
    when ["GET", "/test-bucket/agents/worker-01/packages/pkg-v1.zip"]
      res["Content-Type"] = "application/zip"
      res.body = File.binread(package_v1)
    when ["GET", "/test-bucket/agents/worker-01/packages/pkg-v2.zip"]
      res["Content-Type"] = "application/zip"
      res.body = File.binread(package_v2)
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res.body = "{}"
    when ["POST", "/default/v1/chat/completions"]
      gateway_requests << JSON.parse(req.body)
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        choices: [
          {
            message: {
              role: "assistant",
              content: "hello through model proxy"
            }
          }
        ]
      )
    when ["GET", "/_matrix/client/v3/sync"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        next_batch: "s1",
        rooms: {
          join: {
            "!personal:matrix.example" => {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$event1",
                    sender: "@zhuoguang:matrix.example",
                    content: {
                      msgtype: "m.text",
                      body: "你好"
                    }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$event2",
                    sender: "@zhuoguang:matrix.example",
                    content: {
                      msgtype: "m.text",
                      body: "再介绍一下"
                    }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$event3",
                    sender: "@zhuoguang:matrix.example",
                    content: {
                      msgtype: "m.text",
                      body: "第三次介绍"
                    }
                  }
                ]
              }
            }
          }
        }
      )
    else
      if req.request_method == "PUT" && req.path.include?("/_matrix/client/v3/rooms/")
        sent_messages << JSON.parse(req.body)
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
	    Dir.mkdir(bin_dir)
	    plugin_dir = File.join(tmp, "plugin")
	    Dir.mkdir(plugin_dir)
	    FileUtils.mkdir_p(File.join(plugin_dir, "prompts", "team"))
	    FileUtils.mkdir_p(File.join(plugin_dir, "prompts", "agent"))
	    FileUtils.mkdir_p(File.join(plugin_dir, "teamharness-assets"))
	    FileUtils.mkdir_p(File.join(plugin_dir, "skills", "task-execution"))
	    FileUtils.mkdir_p(File.join(plugin_dir, "skills", "team-coordination"))
	    File.write(File.join(plugin_dir, "prompts", "team", "TEAMS.md"), "Team contract prompt\n")
	    File.write(File.join(plugin_dir, "prompts", "agent", "worker.md"), "Worker role prompt\n")
	    File.write(File.join(plugin_dir, "skills", "task-execution", "SKILL.md"), "task execution skill\n")
	    File.write(File.join(plugin_dir, "skills", "team-coordination", "SKILL.md"), "leader coordination skill\n")
	    File.write(File.join(plugin_dir, "teamharness-assets", "plugin.yaml"), <<~YAML)
	      skills:
	        team:
	          - id: task-execution
	            path: skills/team/task-execution
	            roles: [worker, remote-member]
	          - id: team-coordination
	            path: skills/team/team-coordination
	            roles: [leader]
	    YAML
	    claude_argv = File.join(tmp, "claude-argv.txt")
	    claude_prompts = File.join(tmp, "claude-prompts.txt")
	    claude_system_prompts = File.join(tmp, "claude-system-prompts.txt")
	    descriptor_during_run = File.join(tmp, "descriptor-during-run.txt")
	    helper_output = File.join(tmp, "helper-output.txt")
	    matrix_credentials = File.join(tmp, "matrix-credentials.json")
	    settings_snapshot = File.join(tmp, "managed-settings.json")
	    settings_runtime_env = File.join(tmp, "settings-runtime-env.json")
	    home_dir = File.join(tmp, "home")
	    FileUtils.mkdir_p(File.join(home_dir, ".claude"))
	    File.write(
	      File.join(home_dir, ".claude", "settings.json"),
	      JSON.pretty_generate(
	        "hooks" => {
	          "Stop" => [
	            {
	              "matcher" => "*",
	              "hooks" => [
	                {
	                  "type" => "command",
	                  "command" => "user-global-hook stop"
	                }
	              ]
	            }
	          ]
	        }
	      )
	    )
	    loongsuite_data = File.join(tmp, "loongsuite-data")
	    hook_path = File.join(loongsuite_data, "hooks", "claude-code-loongsuite-pilot-hook.sh")
	    FileUtils.mkdir_p(File.dirname(hook_path))
	    File.write(hook_path, "#!/bin/sh\nexit 0\n")
	    File.chmod(0o755, hook_path)
	    claude = File.join(bin_dir, "fake-claude")
	    File.write(claude, <<~SH)
#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "2.1.141"
  exit 0
fi
if [ "$1" = "--help" ]; then
  echo "--settings --append-system-prompt[-file]"
  exit 0
fi
printf '%s\\n' "$@" >> #{claude_argv}
plugin_arg=""
settings_arg=""
system_prompt_arg=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--plugin-dir" ]; then
    plugin_arg="$arg"
  fi
  if [ "$previous" = "--settings" ]; then
    settings_arg="$arg"
  fi
  if [ "$previous" = "--append-system-prompt-file" ]; then
    system_prompt_arg="$arg"
  fi
  previous="$arg"
done
if [ -n "$system_prompt_arg" ]; then
  cat "$system_prompt_arg" >> #{claude_system_prompts}
  printf '\\n---SYSTEM---\\n' >> #{claude_system_prompts}
fi
cat >> #{claude_prompts}
printf '\\n---PROMPT---\\n' >> #{claude_prompts}
if [ -n "$plugin_arg" ] && [ -f "$plugin_arg/.teamharness/credential-broker.json" ]; then
  printf 'present\\n' > #{descriptor_during_run}
  if [ -n "$settings_arg" ]; then
    node -e '
const fs = require("fs");
const {execFileSync} = require("child_process");
const settings = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
fs.writeFileSync(process.argv[5], JSON.stringify(settings, null, 2));
const key = execFileSync(settings.apiKeyHelper, {encoding: "utf8"}).trim();
fs.writeFileSync(process.argv[2], key + "\\n");
fs.writeFileSync(process.argv[6], JSON.stringify({
  baseUrl: settings.env?.ANTHROPIC_BASE_URL || "",
  model: settings.env?.ANTHROPIC_MODEL || "",
  apiKey: key
}));
const descriptor = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));
const token = fs.readFileSync(descriptor.tokenFile, "utf8").trim();
fetch(descriptor.endpoint.replace(/\\/+$/, "") + "/v1/credentials/matrix", {
  headers: {Authorization: "Bearer " + token, Accept: "application/json"}
}).then(response => response.json()).then(payload => {
  fs.writeFileSync(process.argv[4], JSON.stringify(payload));
});
' "$settings_arg" #{helper_output} "$plugin_arg/.teamharness/credential-broker.json" #{matrix_credentials} #{settings_snapshot} #{settings_runtime_env}
  fi
else
  printf 'missing\\n' > #{descriptor_during_run}
fi
node -e '
(async () => {
  const fs = require("fs");
  const runtimeEnvPath = #{JSON.generate(settings_runtime_env)};
  const runtimeEnv = fs.existsSync(runtimeEnvPath) ? JSON.parse(fs.readFileSync(runtimeEnvPath, "utf8")) : {};
  const base = runtimeEnv.baseUrl || process.env.ANTHROPIC_BASE_URL;
  const key = runtimeEnv.apiKey || process.env.ANTHROPIC_API_KEY;
  const model = runtimeEnv.model || process.env.ANTHROPIC_MODEL;
  if (!base) throw new Error("missing ANTHROPIC_BASE_URL");
  const response = await fetch(`${base}/v1/messages`, {
    method: "POST",
    headers: {"content-type": "application/json", "x-api-key": key},
    body: JSON.stringify({model, messages: [{role: "user", content: "ping"}]})
  });
	  const json = await response.json();
	  const text = json.content?.[0]?.text || JSON.stringify(json);
	  console.log(JSON.stringify({type: "assistant", message: {content: [
	    {type: "thinking", text: "checking context"},
	    {type: "tool_use", name: "Bash", id: "toolu_1", input: {command: "pwd", apiKey: "should-not-leak"}},
	    {type: "text", text: "intermediate **markdown**"}
	  ]}, session_id: "session-1"}));
	  console.log(JSON.stringify({type: "result", result: text + "\\n\\n- item `code`", session_id: "session-1"}));
	})().catch(error => {
  console.error(`base=${process.env.ANTHROPIC_BASE_URL || ""}`);
  console.error(error.stack || error.message);
  process.exit(1);
});
'
	    SH
	    File.chmod(0o755, claude)
	    token = Base64.strict_encode64(JSON.generate(
	      "jwtToken" => "jwt-initial",
      "matrixUrl" => "http://127.0.0.1:#{port}",
      "controllerUrl" => "http://127.0.0.1:#{port}",
      "modelGatewayUrl" => "http://127.0.0.1:#{port}"
    ))
    File.write(File.join(tmp, "matrix-state.json"), JSON.generate(
      matrixSyncToken: "",
      matrixCursors: {},
      runtimeSessions: { "claude-code" => {} }
    ))
    stdout, stderr, status = run_node(
      {
        "HOME" => home_dir,
        "LOONGSUITE_PILOT_DATA_DIR" => loongsuite_data,
        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
        "TEAMHARNESS_MATRIX_ONCE" => "1",
        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0"
      },
      worker_cli.to_s,
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--claude-command", "fake-claude",
      "--model-config-mode", "managed-runtime"
    )
	    assert(status.success?, "matrix path should succeed: #{stderr}\n#{stdout}")
	    assert(!File.exist?(File.join(tmp, "sts.json")), "matrix path must not write STS credentials")
	    assert(!File.exist?(File.join(tmp, "edge-token.json")), "matrix path must not write SA token state")
	    assert(!File.exist?(File.join(tmp, "credential-token")), "matrix path should clean broker token after exit")
	    assert(!File.exist?(File.join(plugin_dir, ".teamharness", "credential-broker.json")), "matrix path should clean plugin broker descriptor after exit")
	    assert(!File.exist?(File.join(tmp, "mcp-runtime.json")), "matrix path should not leave runtime MCP config")
	    assert(!File.read(File.join(tmp, "runtime-state.json")).include?("gateway-key"), "matrix path runtime snapshot must not contain gateway key")
	    assert(File.exist?(claude_argv), "fake Claude should be invoked; sent=#{sent_messages.inspect}; stderr=#{stderr}; stdout=#{stdout}")
	    argv = File.read(claude_argv)
	    assert(argv.include?("--plugin-dir"), "Claude invocation should use plugin-dir MCP loading like the Python worker")
	    assert(!argv.include?("--strict-mcp-config"), "Claude invocation should not bypass plugin MCP loading")
	    assert(!argv.include?("--mcp-config"), "Claude invocation should not pass a separate MCP config")
	    assert(argv.include?("WaitForMcpServers"), "Claude invocation should allow waiting for plugin MCP readiness")
	    assert(argv.include?("mcp__plugin_teamharness-claude-code_teamharness__health"), "Claude invocation should allow plugin-scoped MCP tools")
	    assert(argv.include?("mcp__plugin_teamharness-claude-code_teamharness__message"), "Claude invocation should allow plugin-scoped message tool")
		    assert(argv.include?("--append-system-prompt-file"), "Claude invocation should use managed system prompt file")
		    assert(argv.include?("--permission-mode"), "Claude invocation should always pass permission mode")
		    assert(argv.include?("bypassPermissions"), "Claude invocation should default to bypassPermissions")
		    runtime_plugin_dir = File.read(claude_argv).lines.each_cons(2).find { |previous, _current| previous.strip == "--plugin-dir" }&.last&.strip
		    assert(runtime_plugin_dir && !runtime_plugin_dir.empty?, "Claude invocation should include runtime plugin dir")
		    assert(File.file?(File.join(runtime_plugin_dir, "skills", "task-execution", "SKILL.md")), "worker skill should be kept in runtime plugin")
		    assert(!File.exist?(File.join(runtime_plugin_dir, "skills", "team-coordination")), "leader-only skill must be filtered for worker role")
		    mcp_config = JSON.parse(File.read(File.join(runtime_plugin_dir, ".mcp.json"))).fetch("mcpServers").fetch("teamharness")
	    assert(mcp_config.fetch("command").start_with?("/"), "runtime .mcp.json should use absolute node command")
	    assert(mcp_config.fetch("args").first == File.join(runtime_plugin_dir, "teamharness-assets", "mcp", "server.js"), "runtime .mcp.json should use absolute MCP server path")
	    assert(mcp_config.fetch("cwd") == File.join(runtime_plugin_dir, "teamharness-assets"), "runtime .mcp.json should use absolute MCP cwd")
	    assert(File.read(descriptor_during_run).strip == "present", "plugin broker descriptor should exist while Claude runs")
		    prompts = File.read(claude_prompts, encoding: "UTF-8")
		    system_prompts = File.read(claude_system_prompts, encoding: "UTF-8")
		    assert(system_prompts.include?("Team contract prompt"), "team prompt should be injected into system prompt")
		    assert(system_prompts.include?("Worker role prompt"), "role prompt should be injected into system prompt")
		    assert(system_prompts.include?("package v2 instructions"), "updated package instructions should be injected into system prompt after runtime refresh")
	    assert(!prompts.include?("package v1 instructions"), "stdin prompt should not include package instructions")
	    assert(!prompts.include?("package v2 instructions"), "stdin prompt should not include updated package instructions")
	    assert(prompts.include?("[Current message - respond to this]"), "stdin prompt should include current-message marker")
	    assert(prompts.include?("[1] @zhuoguang:matrix.example: 你好"), "merged turn should include first pending message")
	    assert(prompts.include?("[2] @zhuoguang:matrix.example: 再介绍一下"), "merged turn should include second pending message")
	    assert(prompts.include?("[3] @zhuoguang:matrix.example: 第三次介绍"), "merged turn should include third pending message")
	    assert(!argv.include?("--resume\nsession-1"), "merged same-batch turn should invoke Claude once without resuming itself")
	    assert(argv.include?("--settings"), "Claude invocation should use managed settings")
	    managed_settings = JSON.parse(File.read(settings_snapshot))
	    assert(managed_settings.dig("env", "ANTHROPIC_MODEL") == "qwen3.7-max", "managed settings should carry runtime model")
	    assert(managed_settings.dig("hooks", "Stop", 0, "hooks", 0, "command").include?(hook_path), "managed settings should include LoongSuite Stop hook")
	    assert(managed_settings.dig("hooks", "SubagentStart", 0, "hooks", 0, "command").include?("subagent-start"), "managed settings should include LoongSuite SubagentStart hook")
	    assert(managed_settings.dig("hooks", "SubagentStop", 0, "hooks", 0, "command").include?("subagent-stop"), "managed settings should include LoongSuite SubagentStop hook")
	    assert(!JSON.generate(managed_settings).include?("user-global-hook"), "managed settings must not inherit user hooks")
	    assert(File.read(helper_output).strip != "gateway-key", "apiKeyHelper should return the local proxy token, not the raw gateway key")
	    matrix_payload = JSON.parse(File.read(matrix_credentials))
	    assert(matrix_payload["accessToken"] == "matrix-token", "broker should expose matrix credentials")
	    assert(matrix_payload["personalRoomId"] == "!personal:matrix.example", "broker should expose personal room")
	    assert(
      gateway_requests.any? { |body| body["model"] == "qwen3.7-max" },
      "model proxy should call OpenAI-compatible gateway; sent=#{sent_messages.inspect}"
    )
	    placeholder = sent_messages.find { |content| content["body"] == "处理中..." }
	    assert(placeholder, "placeholder Matrix message missing")
	    assert(placeholder["msgtype"] == "m.notice", "placeholder should use notice msgtype")
	    assert(!placeholder.key?("formatted_body"), "placeholder should be plain text like Python worker")
	    thread_messages = sent_messages.select { |content| content.dig("m.relates_to", "rel_type") == "m.thread" }
	    assert(thread_messages.any? { |content| content["body"].include?("Thinking:") && content["body"].include?("checking context") }, "thinking thread message missing")
	    tool_message = thread_messages.find { |content| content["body"].include?("tool_use: `Bash`") }
	    assert(tool_message, "tool_use thread message missing")
	    assert(tool_message["body"].include?('"command": "pwd"'), "tool_use thread message should include arguments")
	    assert(tool_message["body"].include?('"apiKey": "[REDACTED]"'), "tool_use thread message should redact sensitive arguments")
	    assert(!tool_message["body"].include?("should-not-leak"), "tool_use thread message must not leak sensitive arguments")
	    assert(thread_messages.all? { |content| content["format"] == "org.matrix.custom.html" }, "thread messages should be formatted")
	    final_edit = sent_messages.find { |content| content.dig("m.new_content", "body")&.include?("hello through model proxy") }
	    assert(final_edit, "edited final Matrix message missing")
	    assert(final_edit.dig("m.relates_to", "rel_type") == "m.replace", "final result should edit placeholder")
	    assert(final_edit.dig("m.new_content", "formatted_body")&.include?("<code>code</code>"), "final result should include formatted markdown body")
  ensure
    server.shutdown
    thread.join
  end
end

Dir.mktmpdir("teamharness-node-worker-team-trigger-test-") do |tmp|
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
        gatewayUrl: http://runtime-gateway.example/default
        gatewayKey: gateway-key
    storage:
      provider: oss
      bucket: test-bucket
      endpoint: http://127.0.0.1:1
      memberPrefix: agents/worker-01
  YAML

  sent_messages = []
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
      res.body = JSON.generate(token: "edge-token", jwtToken: "jwt-refreshed", workerName: "worker-01", workerResourceName: "magic-worker-worker-01")
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
      res.body = runtime_yaml
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res.body = "{}"
    when ["GET", "/_matrix/client/v3/sync"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        next_batch: "team-sync-1",
        rooms: {
          join: {
            "!team:matrix.example" => {
              timeline: {
                events: [
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
                      "m.mentions" => { "user_ids" => ["@worker-01:matrix.example"] }
                    }
                  }
                ]
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
    plugin_dir = File.join(tmp, "plugin")
    claude_argv = File.join(tmp, "claude-user-model-argv.txt")
    claude_env = File.join(tmp, "claude-user-model-env.txt")
    claude_prompts = File.join(tmp, "claude-prompts.txt")
    FileUtils.mkdir_p(bin_dir)
    FileUtils.mkdir_p(plugin_dir)
    session_key = "!team:matrix.example::workdir:#{Digest::SHA256.hexdigest(File.expand_path(tmp))[0, 16]}"
    File.write(File.join(tmp, "matrix-state.json"), JSON.generate(
      matrixSyncToken: "team-sync-0",
      claudeSessions: { session_key => "api-error-session-old" }
    ))
    claude = File.join(bin_dir, "fake-claude")
    File.write(claude, <<~SH)
      #!/bin/sh
      if [ "$1" = "--version" ]; then
        echo "2.1.141"
        exit 0
      fi
      if [ "$1" = "--help" ]; then
        echo "--settings --append-system-prompt[-file]"
        exit 0
      fi
      printf '%s\\n' "$@" > #{claude_argv}
      printf 'ANTHROPIC_MODEL=%s\\nCLAUDE_CONFIG_DIR=%s\\n' "$ANTHROPIC_MODEL" "${CLAUDE_CONFIG_DIR:-}" > #{claude_env}
      cat >> #{claude_prompts}
      printf '\\n---PROMPT---\\n' >> #{claude_prompts}
      printf '%s\\n' '{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"api_error invalid api key"}]},"session_id":"api-error-session"}'
      exit 0
    SH
    File.chmod(0o755, claude)

    token = Base64.strict_encode64(JSON.generate(
      "jwtToken" => "jwt-initial",
      "matrixUrl" => "http://127.0.0.1:#{port}",
      "controllerUrl" => "http://127.0.0.1:#{port}",
      "modelGatewayUrl" => "http://127.0.0.1:#{port}"
    ))
    stdout, stderr, status = run_node(
      {
        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
        "ANTHROPIC_MODEL" => "local-user-model",
        "CLAUDE_CONFIG_DIR" => File.join(tmp, "user-claude-config"),
        "TEAMHARNESS_MATRIX_ONCE" => "1",
        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0"
      },
      worker_cli.to_s,
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--claude-command", "fake-claude",
      "--model-config-mode", "native-config"
    )
    assert(status.success?, "team trigger path should succeed: #{stderr}\n#{stdout}")
    argv = File.read(claude_argv)
    assert(!argv.include?("--settings"), "user model scope should not pass managed settings")
    assert(!argv.include?("--model"), "user model scope should not pass managed model")
    env_text = File.read(claude_env)
    assert(env_text.include?("ANTHROPIC_MODEL=local-user-model"), "user model scope should preserve user Anthropic env")
    assert(env_text.include?("CLAUDE_CONFIG_DIR=#{File.join(tmp, "user-claude-config")}"), "user model scope should preserve user Claude config dir")
    prompts = File.read(claude_prompts, encoding: "UTF-8")
    assert(prompts.scan("---PROMPT---").length == 1, "similar member name should not trigger Claude")
    assert(prompts.include?("@worker-01 触发一次"), "real mention should trigger Claude")
    assert(prompts.include?("worker-012 不应该触发"), "false-positive mention should be carried only as room history context")
    final_edit = sent_messages.find { |content| content.dig("m.new_content", "body")&.include?("Claude Code failed with exit code 1") }
    assert(final_edit, "API error stream should edit placeholder as failure")
    assert(final_edit.dig("m.new_content", "msgtype") == "m.notice", "API error final edit should use notice")
    assert(sent_messages.none? { |content| content.dig("m.relates_to", "rel_type") == "m.thread" && content["body"].to_s.include?("invalid api key") }, "API error text should not be emitted as thread summary")
  ensure
    server.shutdown
    thread.join
  end
end

Dir.mktmpdir("teamharness-node-worker-missing-session-test-") do |tmp|
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
        gatewayUrl: http://runtime-gateway.example/default
        gatewayKey: gateway-key
    storage:
      provider: oss
      bucket: test-bucket
      endpoint: http://127.0.0.1:1
      memberPrefix: agents/worker-01
  YAML

  sent_messages = []
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
      res.body = JSON.generate(token: "edge-token", jwtToken: "jwt-refreshed", workerName: "worker-01", workerResourceName: "magic-worker-worker-01")
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
      res.body = runtime_yaml
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res.body = "{}"
    when ["GET", "/_matrix/client/v3/sync"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(
        next_batch: "team-sync-1",
        rooms: {
          join: {
            "!team:matrix.example" => {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$mention",
                    sender: "@zhuoguang:matrix.example",
                    content: {
                      msgtype: "m.text",
                      body: "@worker-01 继续",
                      "m.mentions" => { "user_ids" => ["@worker-01:matrix.example"] }
                    }
                  }
                ]
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
    plugin_dir = File.join(tmp, "plugin")
    claude_calls = File.join(tmp, "claude-calls.jsonl")
    FileUtils.mkdir_p(bin_dir)
    FileUtils.mkdir_p(plugin_dir)
    session_key = "!team:matrix.example::workdir:#{Digest::SHA256.hexdigest(File.expand_path(tmp))[0, 16]}"
    File.write(File.join(tmp, "matrix-state.json"), JSON.generate(
      matrixSyncToken: "team-sync-0",
      claudeSessions: {
        session_key => "missing-session-old",
        "!team:matrix.example" => "legacy-session-old"
      }
    ))
    claude = File.join(bin_dir, "fake-claude")
    File.write(claude, <<~RUBY)
      #!/usr/bin/env ruby
      require "json"
      if ARGV[0] == "--version"
        puts "2.1.141"
        exit 0
      end
      if ARGV[0] == "--help"
        puts "--settings --append-system-prompt[-file]"
        exit 0
      end
      File.open(#{claude_calls.inspect}, "a") { |file| file.puts(JSON.generate(ARGV)) }
      if ARGV.include?("--resume")
        warn "No conversation found with session ID: missing-session-old"
        exit 1
      end
      puts JSON.generate(type: "result", result: "fresh session handled", session_id: "session-new")
    RUBY
    File.chmod(0o755, claude)

    token = Base64.strict_encode64(JSON.generate(
      "jwtToken" => "jwt-initial",
      "matrixUrl" => "http://127.0.0.1:#{port}",
      "controllerUrl" => "http://127.0.0.1:#{port}",
      "modelGatewayUrl" => "http://127.0.0.1:#{port}"
    ))
    stdout, stderr, status = run_node(
      {
        "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
        "TEAMHARNESS_MATRIX_ONCE" => "1",
        "TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS" => "0"
      },
      worker_cli.to_s,
      "--state-dir", tmp,
      "--work-dir", tmp,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--claude-command", "fake-claude"
    )
    assert(status.success?, "missing session retry should keep worker process healthy: #{stderr}\n#{stdout}")
    calls = File.readlines(claude_calls, chomp: true).map { |line| JSON.parse(line) }
    assert(calls.length == 2, "missing session should retry exactly once")
    assert(calls[0].include?("--resume"), "first call should resume cached session")
    assert(calls[0].include?("missing-session-old"), "first call should use old session")
    assert(!calls[1].include?("--resume"), "retry should start a fresh session")
    state = JSON.parse(File.read(File.join(tmp, "matrix-state.json")))
    assert(state.dig("claudeSessions", session_key) == "session-new", "retry should store the fresh session")
    assert(!state.fetch("claudeSessions", {}).key?("!team:matrix.example"), "retry should drop legacy room session")
    final_edit = sent_messages.find { |content| content.dig("m.new_content", "body")&.include?("fresh session handled") }
    assert(final_edit, "retry success should edit placeholder with final result")
  ensure
    server.shutdown
    thread.join
  end
end

Dir.mktmpdir("teamharness-node-worker-global-test-") do |tmp|
  runtime_yaml = <<~YAML
    apiVersion: hiclaw.io/v1beta1
    kind: MemberRuntimeConfig
    matrix:
      accessToken: matrix-token
    member:
      name: worker-01
      runtimeName: worker-01
      role: worker
      matrixUserId: "@worker-01:matrix.example"
      personalRoomId: "!personal:matrix.example"
    desired:
      model:
        model: qwen3.7-max
        gatewayUrl: http://runtime-gateway.example/default
        gatewayKey: gateway-key
    storage:
      provider: oss
      bucket: test-bucket
      endpoint: http://127.0.0.1:1
      memberPrefix: agents/worker-01
  YAML

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
      res.body = runtime_yaml
    when ["POST", "/api/v1/workers/magic-worker-worker-01/heartbeat"]
      res["Content-Type"] = "application/json"
      res.body = "{}"
    when ["GET", "/_matrix/client/v3/sync"]
      res["Content-Type"] = "application/json"
      res.body = JSON.generate(next_batch: "sync-1", rooms: { join: {} })
    else
      if req.request_method == "PUT" && req.path.include?("/_matrix/client/v3/rooms/")
        res["Content-Type"] = "application/json"
        res.body = JSON.generate(room_id: "!personal:matrix.example")
      else
        res.status = 404
        res.body = "not found"
      end
    end
  end

  thread = Thread.new { server.start }
  pid = nil
  begin
    bin_dir = File.join(tmp, "bin")
    managed_bin = File.join(tmp, "managed-bin")
    managed_claude = File.join(tmp, "managed-claude")
    profile = File.join(tmp, "profile")
    plugin_dir = File.join(tmp, "plugin")
    state_dir = File.join(tmp, "state")
    work_dir = File.join(tmp, "work")
    claude_log = File.join(tmp, "claude-global.jsonl")
    [bin_dir, managed_bin, managed_claude, plugin_dir, state_dir, work_dir].each { |dir| FileUtils.mkdir_p(dir) }
    File.write(File.join(bin_dir, "claude"), <<~SH)
      #!/bin/sh
      if [ "$1" = "--version" ]; then
        echo "2.1.141"
        exit 0
      fi
      if [ "$1" = "--help" ]; then
        echo "--settings --append-system-prompt[-file]"
        exit 0
      fi
      node -e '
      const fs = require("fs");
      const {execFileSync} = require("child_process");
      const argv = process.argv.slice(2);
      const settingsIndex = argv.indexOf("--settings");
      const settingsPath = settingsIndex >= 0 ? argv[settingsIndex + 1] : "";
      const settings = settingsPath ? JSON.parse(fs.readFileSync(settingsPath, "utf8")) : {};
      const settingsEnv = settings.env || {};
      const apiKey = settings.apiKeyHelper ? execFileSync(settings.apiKeyHelper, {encoding: "utf8"}).trim() : (process.env.ANTHROPIC_API_KEY || "");
      fs.appendFileSync(process.argv[1], JSON.stringify({
        argv,
        model: settingsEnv.ANTHROPIC_MODEL || process.env.ANTHROPIC_MODEL || "",
        baseUrl: settingsEnv.ANTHROPIC_BASE_URL || process.env.ANTHROPIC_BASE_URL || "",
        apiKey,
        broker: process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR || ""
      }) + "\\n");
      ' #{claude_log} "$@"
    SH
    File.chmod(0o755, File.join(bin_dir, "claude"))

    token = Base64.strict_encode64(JSON.generate(
      "jwtToken" => "jwt-initial",
      "matrixUrl" => "http://127.0.0.1:#{port}",
      "controllerUrl" => "http://127.0.0.1:#{port}",
      "modelGatewayUrl" => "http://127.0.0.1:#{port}"
    ))
    env = {
      "PATH" => "#{bin_dir}:#{ENV.fetch("PATH")}",
      "LOONGSUITE_MANAGED_BIN_DIR" => managed_bin,
      "LOONGSUITE_MANAGED_CLAUDE_DIR" => managed_claude,
      "TEAMHARNESS_MANAGED_PROFILE" => profile
    }
    stdin, stdout, stderr, wait_thr = Open3.popen3(
      env,
      "node",
      worker_cli.to_s,
      "--state-dir", state_dir,
      "--work-dir", work_dir,
      "--plugin-dir", plugin_dir,
      "--bootstrap-token-file", write_bootstrap_token(tmp, token),
      "--plugin-install-scope", "global",
      "--model-config-mode", "managed-global"
    )
    stdin.close
    pid = wait_thr.pid
    launcher = File.join(managed_claude, "launcher.json")
    shim = File.join(managed_bin, "claude")
    wait_until("global launcher should be written") { File.exist?(launcher) && File.exist?(shim) }
    wait_until("global profile PATH block should be written") { File.exist?(profile) && File.read(profile).include?("LoongSuite AgentTeams managed PATH") }

    stdout_call, stderr_call, status_call = Open3.capture3(
      {
        "PATH" => "#{managed_bin}:#{bin_dir}:#{ENV.fetch("PATH")}",
        "LOONGSUITE_MANAGED_BIN_DIR" => managed_bin,
        "LOONGSUITE_MANAGED_CLAUDE_DIR" => managed_claude,
        "TEAMHARNESS_MANAGED_PROFILE" => profile
      },
      "claude",
      "hello",
      "--model",
      "user-model"
    )
    assert(status_call.success?, "managed shim call should succeed: #{stderr_call}\n#{stdout_call}")
    first_call = JSON.parse(File.readlines(claude_log).last)
    assert(first_call["argv"].include?("--plugin-dir"), "global shim should append managed plugin dir")
    assert(first_call["argv"].include?("--settings"), "global shim should pass managed settings")
    assert(first_call["argv"].include?("qwen3.7-max"), "global shim should replace --model with managed model")
    assert(!first_call["argv"].include?("user-model"), "global shim should remove user supplied model")
    assert(first_call["model"] == "qwen3.7-max", "global shim should inject managed model settings")
    assert(first_call["baseUrl"].start_with?("http://127.0.0.1:"), "global shim should inject local model proxy base URL settings")
    assert(first_call["apiKey"] != "gateway-key", "global shim should not expose raw gateway key")
    assert(first_call["broker"].end_with?(".teamharness/credential-broker.json"), "global shim should expose broker descriptor")

    descriptor = JSON.parse(File.read(launcher))
    descriptor["workerPid"] = 999_999_999
    File.write(launcher, "#{JSON.generate(descriptor)}\n")
    stdout_call, stderr_call, status_call = Open3.capture3(
      {
        "PATH" => "#{managed_bin}:#{bin_dir}:#{ENV.fetch("PATH")}",
        "LOONGSUITE_MANAGED_BIN_DIR" => managed_bin,
        "LOONGSUITE_MANAGED_CLAUDE_DIR" => managed_claude,
        "TEAMHARNESS_MANAGED_PROFILE" => profile
      },
      "claude",
      "fallback"
    )
    assert(status_call.success?, "stale global shim fallback should succeed: #{stderr_call}\n#{stdout_call}")
    fallback_call = JSON.parse(File.readlines(claude_log).last)
    assert(!fallback_call["argv"].include?("--plugin-dir"), "stale global shim should fallback without managed plugin")
    assert(fallback_call["model"].empty?, "stale global shim should fallback without managed model env")
    assert(!File.exist?(launcher), "stale global shim should remove launcher descriptor")
    assert(!File.exist?(shim), "stale global shim should remove shim")
    assert(!File.exist?(profile) || !File.read(profile).include?("LoongSuite AgentTeams managed PATH"), "stale global shim should remove profile PATH block")

    stop_process(pid)
    pid = nil
    stdout.read
    stderr.read
  ensure
    stop_process(pid) if pid
    server.shutdown
    thread.join
  end
end
