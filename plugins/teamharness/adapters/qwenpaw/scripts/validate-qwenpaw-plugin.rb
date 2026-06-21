#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "yaml"

abort("usage: validate-qwenpaw-plugin.rb <generated-plugin-dir>") if ARGV.empty?

package_dir = Pathname.new(ARGV[0]).expand_path

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

def assert_file(path)
  fail!("missing file: #{path}") unless path.file?
end

def assert_dir(path)
  fail!("missing directory: #{path}") unless path.directory?
end

assert_dir(package_dir)
plugin_json = package_dir / "plugin.json"
plugin_py = package_dir / "plugin.py"
asset_dir = package_dir / "teamharness"

assert_file(plugin_json)
assert_file(plugin_py)
assert_dir(asset_dir)

manifest = JSON.parse(plugin_json.read)
fail!("plugin id must be teamharness") unless manifest["id"] == "teamharness"
fail!("plugin type must be general") unless manifest["type"] == "general"
fail!("backend entry must be plugin.py") unless manifest.dig("entry", "backend") == "plugin.py"

features = manifest.dig("meta", "features") || []
fail!("qwenpaw plugin must not declare periodic-sync") if features.include?("periodic-sync")

assert_file(asset_dir / "plugin.yaml")
source_manifest = YAML.load_file(asset_dir / "plugin.yaml")
version = source_manifest.fetch("metadata").fetch("version")
fail!("plugin version mismatch") unless manifest["version"] == version

assert_file(asset_dir / "prompts/team/TEAMS.md")
assert_file(asset_dir / "prompts/agent/worker.md")
assert_file(asset_dir / "prompts/manager/AGENTS.md")
assert_file(asset_dir / "skills/team/communication/SKILL.md")
assert_file(asset_dir / "mcp/server.py")
fail!("top-level hooks must not be packaged for qwenpaw") if (asset_dir / "hooks").exist?

stdout, stderr, status = Open3.capture3("python3", "-m", "py_compile", plugin_py.to_s)
fail!("plugin.py syntax check failed: #{stderr}#{stdout}") unless status.success?

stdout, stderr, status = Open3.capture3("python3", "-m", "py_compile", (asset_dir / "mcp/server.py").to_s)
fail!("teamharness mcp syntax check failed: #{stderr}#{stdout}") unless status.success?

puts "ok: qwenpaw teamharness #{version}"
