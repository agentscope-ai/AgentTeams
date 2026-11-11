#!/usr/bin/env ruby
# frozen_string_literal: true

require "open3"
require "pathname"
require "yaml"

manifest_path = Pathname.new(ARGV[0] || "plugins/teamharness/plugin.yaml").expand_path
plugin_root = manifest_path.dirname
plugins_root = plugin_root.parent

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

def read_yaml(path)
  YAML.load_file(path)
rescue Psych::SyntaxError => e
  fail!("invalid yaml #{path}: #{e.message}")
end

assert_file(manifest_path)
manifest = read_yaml(manifest_path)

fail!("apiVersion must match existing plugin manifest API version agentteams.agentteam/v1alpha1") unless manifest["apiVersion"] == "agentteams.agentteam/v1alpha1"
fail!("kind must be AgentTeamPlugin") unless manifest["kind"] == "AgentTeamPlugin"

metadata = manifest.fetch("metadata") { fail!("metadata is required") }
name = metadata["name"].to_s
version = metadata["version"].to_s
fail!("metadata.name is required") if name.empty?
fail!("build metadata is not supported for plugin releases") if version.include?("+")
fail!("metadata.version must be semver") unless version.match?(/\A\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?\z/)

# Real Draft 2020-12 schema validation: validate_manifest.py applies the
# shared form rule (adapters + requires => bundle.schema.json, otherwise
# plugin.schema.json) and reports every violation verbatim.
schemas_dir = plugins_root / "schemas"
assert_file(schemas_dir / "plugin.schema.json")
assert_file(schemas_dir / "bundle.schema.json")
stdout, stderr, status = Open3.capture3(
  "python3",
  (Pathname.new(__dir__) / "validate_manifest.py").to_s,
  manifest_path.to_s,
  schemas_dir.to_s
)
fail!("schema validation failed:\n#{stdout}#{stderr}") unless status.success?

package = manifest.fetch("package") { fail!("package is required") }
includes = package.fetch("include") { fail!("package.include is required") }
fail!("package.include must be an array") unless includes.is_a?(Array)
includes.each { |entry| fail!("package include missing: #{plugin_root / entry}") unless (plugin_root / entry).exist? }

if manifest.key?("entry")
  entry_requires = manifest.fetch("requires") { fail!("requires is required for entry plugins") }
  fail!("requires must be a map") unless entry_requires.is_a?(Hash)
  fail!("requires.qwenpaw is required and must be non-empty") if entry_requires["qwenpaw"].to_s.empty?

  entry = manifest["entry"]
  fail!("entry must be a map") unless entry.is_a?(Hash)
  backend = entry["backend"].to_s
  fail!("entry.backend is required and must be non-empty") if backend.empty?
  frontend = entry["frontend"].to_s
  fail!("entry.frontend is required and must be non-empty") if frontend.empty?
  assert_file(plugin_root / backend)
  assert_file(plugin_root / frontend)

  fail!("runtime is required and must be non-empty") if manifest["runtime"].to_s.empty?

  puts "ok: #{name} #{version}"
  exit 0
elsif manifest.key?("adapters") && !%w[prompts skills mcp].any? { |key| manifest.key?(key) }
  bundle_requires = manifest.fetch("requires") { fail!("requires is required for bundle plugins") }
  fail!("requires must be a map") unless bundle_requires.is_a?(Hash)
  %w[agentteams contracts tool_schema].each do |key|
    value = bundle_requires[key].to_s
    fail!("requires.#{key} is required and must be non-empty") if value.empty?
  end
  bundle_adapters = manifest["adapters"]
  fail!("adapters must be an array") unless bundle_adapters.is_a?(Array)
  fail!("bundle plugins must declare at least one adapter") if bundle_adapters.empty?
else
  if manifest.key?("requires")
    requires = manifest["requires"]
    fail!("requires must be a map") unless requires.is_a?(Hash)
    %w[agentteams contracts tool_schema].each do |key|
      value = requires[key].to_s
      fail!("requires.#{key} is required and must be non-empty") if value.empty?
    end
  end

  prompts = manifest.fetch("prompts") { fail!("prompts is required") }
  assert_file(plugin_root / prompts.fetch("team") { fail!("prompts.team is required") })
  agent_prompts = prompts.fetch("agent") { fail!("prompts.agent is required") }
  fail!("prompts.agent must be a map") unless agent_prompts.is_a?(Hash)
  agent_prompts.each_value { |path| assert_file(plugin_root / path) }
  manager_prompts = prompts.fetch("manager") { fail!("prompts.manager is required") }
  fail!("prompts.manager must be a map") unless manager_prompts.is_a?(Hash)
  manager_prompts.each_value { |path| assert_file(plugin_root / path) }

  skill_ids = []
  skills = manifest.fetch("skills") { fail!("skills is required") }
  fail!("skills must be a map") unless skills.is_a?(Hash)
  skills.each do |group, entries|
    fail!("skills.#{group} must be an array") unless entries.is_a?(Array)
    entries.each do |entry|
      id = entry.fetch("id") { fail!("skill id is required in #{group}") }
      path = plugin_root / entry.fetch("path") { fail!("skill path is required for #{id}") }
      assert_dir(path)
      assert_file(path / "SKILL.md")
      skill_ids << id
    end
  end
  duplicates = skill_ids.group_by { |id| id }.select { |_id, values| values.size > 1 }.keys
  fail!("duplicate skill ids: #{duplicates.join(', ')}") unless duplicates.empty?

  mcp = manifest.fetch("mcp") { fail!("mcp is required") }
  servers = mcp.fetch("servers") { fail!("mcp.servers is required") }
  fail!("mcp.servers must be an array") unless servers.is_a?(Array)
  servers.each do |server|
    server_id = server.fetch("id") { fail!("mcp server id is required") }
    server.fetch("args") { fail!("mcp server #{server_id} args are required") }.each do |arg|
      assert_file(plugin_root / arg) if arg.end_with?(".py")
    end
  end
end

fail!("top-level hooks are not part of TeamHarness plugin contract; put runtime hooks under adapters") if manifest.key?("hooks")

Array(manifest["adapters"]).each do |adapter|
  id = adapter.fetch("id") { fail!("adapter id is required") }
  path = plugin_root / adapter.fetch("path") { fail!("adapter path is required for #{id}") }
  assert_dir(path)
  assert_file(path / "README.md")
end

assert_file(plugin_root / "scripts/install.sh")
assert_file(plugin_root / "scripts/uninstall.sh")

puts "ok: #{name} #{version}"
