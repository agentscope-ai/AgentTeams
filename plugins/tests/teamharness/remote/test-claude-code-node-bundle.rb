#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
build_script = repo_root / "plugins/teamharness/remote/claude-code/scripts/build-claude-local-bundle.rb"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert(condition, message)
  fail!(message) unless condition
end

def assert_file(path)
  assert(path.file?, "missing file: #{path}")
end

def assert_directory(path)
  assert(path.directory?, "missing directory: #{path}")
end

def run!(env, *command)
  stdout, stderr, status = Open3.capture3(env, *command)
  fail!("command failed: #{command.join(" ")}\n#{stderr}\n#{stdout}") unless status.success?
  stdout
end

Dir.mktmpdir("teamharness-node-bundle-test-") do |tmp|
  out_dir = Pathname.new(tmp) / "out"
  unpack_dir = Pathname.new(tmp) / "unpack"
  run!({ "OUT_DIR" => out_dir.to_s }, "ruby", build_script.to_s)
  tarball = out_dir / "agentteams-claude-code-local-runtime-0.0.2.tar.gz"
  assert_file(tarball)
  unpack_dir.mkpath
  run!({}, "tar", "-xzf", tarball.to_s, "-C", unpack_dir.to_s)

  root = Dir.glob((unpack_dir / "teamharness-claude-local-*").to_s).map { |path| Pathname.new(path) }.first
  fail!("bundle root not found") unless root&.directory?

  assert_file(root / "claude-code-worker/dist/cli.js")
  assert_file(root / "teamharness-claude-plugin/teamharness-assets/mcp/server.js")
  assert_file(root / "node-runtime-core/mcp/server.js")
  assert_file(root / "node-runtime-core/package.json")
  assert_file(root / "node-runtime-core/package-lock.json")
  assert_directory(root / "node-runtime-core/node_modules/markdown-it")
  assert_file(root / "teamharness-claude-plugin/hooks/team-context.js")
  assert_file(root / "teamharness-claude-plugin/hooks/credential-guard.js")
  assert_file(root / "teamharness-claude-plugin/hooks/output-sanitizer.js")
  assert_file(root / "scripts/worker-entrypoint.sh")
  assert_file(root / "worker.manifest.json")

  mcp = JSON.parse((root / "teamharness-claude-plugin/.mcp.json").read)
  teamharness_mcp = mcp.fetch("mcpServers").fetch("teamharness")
  command = teamharness_mcp.fetch("args").join(" ")
  assert(command.include?("TEAMHARNESS_NODE_BIN"), ".mcp.json must use TEAMHARNESS_NODE_BIN")
  assert(command.include?("server.js"), ".mcp.json must start server.js")
  assert(!teamharness_mcp.fetch("env", {}).key?("TEAMHARNESS_NODE_BIN"), ".mcp.json must not override inherited TEAMHARNESS_NODE_BIN")

  entrypoint = (root / "scripts/worker-entrypoint.sh").read
  assert(entrypoint.include?("PILOT_NODE_BIN"), "worker-entrypoint must use PILOT_NODE_BIN")
  assert(entrypoint.include?("claude-code-worker/dist/cli.js"), "worker-entrypoint must start node cli.js")
  manifest = JSON.parse((root / "worker.manifest.json").read)
  manifest_env = manifest.fetch("env")
  assert(manifest_env.fetch("TEAMHARNESS_CLAUDE_COMMAND") == "${instance:claudeCommand}", "worker manifest must pass optional Claude command through env")
  assert(manifest_env.fetch("TEAMHARNESS_CLAUDE_PERMISSION_MODE") == "${instance:claudePermissionMode}", "worker manifest must pass optional Claude permission mode through env")

  markdown_check = <<~JS
    const matrix = require(#{(root / "node-runtime-core/worker/matrix.js").to_s.inspect});
    const html = matrix.matrixFormattedBody("# Report\\n\\n| 项目 | 状态 |\\n|---|---|\\n| runtime | **Claude Code** |\\n\\n1. done");
    if (!html.includes("<h1>Report</h1>")) throw new Error("markdown-it heading render missing");
    if (!html.includes("<table>")) throw new Error("markdown-it table render missing");
    if (!html.includes("<strong>Claude Code</strong>")) throw new Error("markdown-it inline render missing");
    if (!html.includes("<ol>")) throw new Error("markdown-it ordered list render missing");
  JS
  run!({}, "node", "-e", markdown_check)

  all_text = Dir.glob((root / "**/*").to_s, File::FNM_DOTMATCH)
    .select { |path| File.file?(path) }
    .reject { |path| path.end_with?(".tar.gz") }
    .reject { |path| path.include?("/node_modules/") }
    .map { |path| File.binread(path) rescue "" }
    .join("\n")
  forbidden = ["python3", "PYTHONPATH", "server.py", "pyproject.toml", "oss2"]
  forbidden.each do |needle|
    assert(!all_text.include?(needle), "bundle runtime must not contain #{needle.inspect}")
  end
end
