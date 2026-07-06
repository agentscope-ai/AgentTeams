#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
build_script = repo_root / "plugins/teamharness/remote/openclaw/scripts/build-openclaw-local-bundle.rb"
loongsuite_template = repo_root / "plugins/teamharness/remote/openclaw/loongsuite/agents.d/agentteams-openclaw-local-runtime.json"

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

Dir.mktmpdir("teamharness-openclaw-bundle-test-") do |tmp|
  assert_file(loongsuite_template)
  template = JSON.parse(loongsuite_template.read)
  assert(template.fetch("localWorkerRuntime") == "openclaw", "LoongSuite template runtime must be openclaw")
  assert(
    template.dig("pluginProbe", "source", "tarball") == "$PILOT_DATA/plugins/agentteams-openclaw-local-runtime-0.0.2.tar.gz",
    "LoongSuite template must point at the versioned OpenClaw local bundle tarball"
  )

  out_dir = Pathname.new(tmp) / "out"
  unpack_dir = Pathname.new(tmp) / "unpack"
  run!({ "OUT_DIR" => out_dir.to_s }, "ruby", build_script.to_s)
  tarball = out_dir / "agentteams-openclaw-local-runtime-0.0.2.tar.gz"
  assert_file(tarball)
  unpack_dir.mkpath
  run!({}, "tar", "-xzf", tarball.to_s, "-C", unpack_dir.to_s)

  root = Dir.glob((unpack_dir / "teamharness-openclaw-local-*").to_s).map { |path| Pathname.new(path) }.first
  fail!("bundle root not found") unless root&.directory?

  assert_file(root / "openclaw-worker/dist/cli.js")
  loongsuite_plugin = root / "loongsuite-js/opentelemetry-instrumentation-openclaw"
  assert_file(loongsuite_plugin / "openclaw.plugin.json")
  assert_file(loongsuite_plugin / "package.json")
  assert_file(loongsuite_plugin / "package-lock.json")
  assert_file(loongsuite_plugin / "dist/index.js")
  assert(!(loongsuite_plugin / "src").exist?, "LoongSuite OpenClaw plugin source must not be bundled")
  assert(!(loongsuite_plugin / "test").exist?, "LoongSuite OpenClaw plugin tests must not be bundled")
  loongsuite_package = JSON.parse((loongsuite_plugin / "package.json").read)
  assert(
    loongsuite_package.dig("openclaw", "extensions") == ["./dist/index.js"],
    "LoongSuite OpenClaw plugin package must point at the built runtime entry"
  )
  assert_file(root / "teamharness-openclaw-assets/mcp/server.js")
  assert_file(root / "node-runtime-core/mcp/server.js")
  assert_file(root / "teamharness-openclaw-assets/plugin.yaml")
  assert_file(root / "teamharness-openclaw-assets/prompts/team/TEAMS.md")
  assert_file(root / "teamharness-openclaw-assets/skills/find-skills/SKILL.md")
  assert_file(root / "node-runtime-core/package.json")
  assert_file(root / "node-runtime-core/package-lock.json")
  assert_directory(root / "node-runtime-core/node_modules/markdown-it")
  assert_directory(loongsuite_plugin / "node_modules/@opentelemetry")
  assert_directory(loongsuite_plugin / "node_modules/@loongsuite/opentelemetry-util-genai")
  assert_directory(root / "loongsuite-js/node_modules/@opentelemetry")
  assert_file(root / "loongsuite-js/.teamharness-source-digest")
  assert_file(root / "loongsuite-js/opentelemetry-util-genai/dist/index.js")
  assert(!(root / "loongsuite-js/opentelemetry-util-genai/src").exist?, "LoongSuite GenAI util source must not be bundled")
  assert(!(root / "loongsuite-js/opentelemetry-util-genai/test").exist?, "LoongSuite GenAI util tests must not be bundled")
  assert_file(root / "scripts/worker-entrypoint.sh")
  assert_file(root / "worker.manifest.json")

  assert(!(root / "teamharness-claude-plugin").exist?, "OpenClaw bundle must not contain Claude plugin dir")
  assert(!(root / "teamharness-openclaw-assets/.mcp.json").exist?, "OpenClaw bundle must not ship Claude .mcp.json")
  assert(!(root / "teamharness-openclaw-assets/hooks").exist?, "OpenClaw bundle must not ship Claude hooks")

  manifest = JSON.parse((root / "worker.manifest.json").read)
  assert(manifest.fetch("runtime") == "openclaw", "worker manifest runtime must be openclaw")
  assert(manifest.fetch("name") == "teamharness-openclaw-worker", "worker manifest name mismatch")
  assert(manifest.fetch("env").key?("TEAMHARNESS_OPENCLAW_ASSETS_DIR"), "manifest must expose OpenClaw assets dir")
  assert(manifest.fetch("env").fetch("TEAMHARNESS_OPENCLAW_COMMAND") == "${instance:openclawCommand}", "worker manifest must pass optional OpenClaw command through env")

  entrypoint = (root / "scripts/worker-entrypoint.sh").read
  assert(entrypoint.include?("PILOT_NODE_BIN"), "worker-entrypoint must use PILOT_NODE_BIN")
  assert(entrypoint.include?("openclaw-worker/dist/cli.js"), "worker-entrypoint must start OpenClaw node cli.js")
  assert(entrypoint.include?("TEAMHARNESS_OPENCLAW_ASSETS_DIR"), "entrypoint must expose OpenClaw assets")

  markdown_check = <<~JS
    const matrix = require(#{(root / "node-runtime-core/worker/matrix.js").to_s.inspect});
    const html = matrix.matrixFormattedBody("# Report\\n\\n| 项目 | 状态 |\\n|---|---|\\n| runtime | **OpenClaw** |\\n\\n1. done");
    if (!html.includes("<h1>Report</h1>")) throw new Error("markdown-it heading render missing");
    if (!html.includes("<table>")) throw new Error("markdown-it table render missing");
    if (!html.includes("<strong>OpenClaw</strong>")) throw new Error("markdown-it inline render missing");
    if (!html.includes("<ol>")) throw new Error("markdown-it ordered list render missing");
  JS
  run!({}, "node", "-e", markdown_check)

  loongsuite_import_check = <<~JS
    import(#{(loongsuite_plugin / "dist/index.js").to_s.inspect});
  JS
  run!({}, "node", "-e", loongsuite_import_check)

  all_text = Dir.glob((root / "**/*").to_s, File::FNM_DOTMATCH)
    .select { |path| File.file?(path) }
    .reject { |path| path.end_with?(".tar.gz") }
    .reject { |path| path.include?("/node_modules/") }
    .map { |path| File.binread(path) rescue "" }
    .join("\n")
  forbidden = ["claude", "Claude", "python3", "PYTHONPATH", "server.py", "pyproject.toml", "oss2", "markdown-it-py", "mc mirror", "mc alias"]
  forbidden.each do |needle|
    assert(!all_text.include?(needle), "bundle runtime must not contain #{needle.inspect}")
  end
end
