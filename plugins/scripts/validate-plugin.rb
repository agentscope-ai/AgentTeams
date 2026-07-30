#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
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
fail!("metadata.version must be semver") unless version.match?(/\A\d+\.\d+\.\d+\z/)

schema_path = plugins_root / "schemas/plugin.schema.json"
assert_file(schema_path)
JSON.parse(schema_path.read)

package = manifest.fetch("package") { fail!("package is required") }
includes = package.fetch("include") { fail!("package.include is required") }
fail!("package.include must be an array") unless includes.is_a?(Array)
includes.each { |entry| fail!("package include missing: #{plugin_root / entry}") unless (plugin_root / entry).exist? }

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
