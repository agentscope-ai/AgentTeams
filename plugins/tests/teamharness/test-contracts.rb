#!/usr/bin/env ruby
# frozen_string_literal: true

require "pathname"
require "yaml"

repo_root = Pathname.new(__dir__).join("../../..").expand_path
plugin_root = repo_root / "plugins/teamharness"
manifest_path = plugin_root / "plugin.yaml"
boundary_doc = repo_root / "docs/teamharness-boundary-and-contracts.md"

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

def read(path)
  path.read(encoding: "utf-8")
end

def skill_frontmatter(path)
  text = read(path)
  match = text.match(/\A---\n(.*?)\n---\n/m)
  fail!("skill missing YAML front matter: #{path}") unless match
  YAML.safe_load(match[1]) || {}
end

manifest = YAML.load_file(manifest_path)

assert_file(boundary_doc)
doc = read(boundary_doc)
[
  "TeamHarness v0.1",
  "does not own worker lifecycle",
  "desired.agentPackage",
  "HiClaw AgentSpec package"
].each do |needle|
  assert(doc.include?(needle), "boundary doc must describe #{needle.inspect}")
end

assert(manifest.dig("metadata", "name") == "teamharness", "metadata.name must be teamharness")

prompts = manifest.fetch("prompts")
assert_file(plugin_root / prompts.fetch("team"))
agent_prompts = prompts.fetch("agent")
expected_agent_prompts = {
  "leader" => "prompts/agent/leader.md",
  "worker" => "prompts/agent/worker.md",
  "remoteMember" => "prompts/agent/remote-member.md"
}
assert(agent_prompts == expected_agent_prompts, "unexpected agent prompts: #{agent_prompts.inspect}")
manager_prompts = prompts.fetch("manager")
expected_manager_prompts = {
  "agents" => "prompts/manager/AGENTS.md",
  "tools" => "prompts/manager/TOOLS.md",
  "heartbeat" => "prompts/manager/HEARTBEAT.md"
}
assert(manager_prompts == expected_manager_prompts, "unexpected manager prompts: #{manager_prompts.inspect}")

[
  prompts.fetch("team"),
  *agent_prompts.values,
  *manager_prompts.values
].each { |path| assert_file(plugin_root / path) }

team_prompt = read(plugin_root / prompts.fetch("team"))
assert(team_prompt.include?("stable collaboration"), "team prompt must define stable collaboration rules")
assert(team_prompt.include?("not a status database"), "team prompt must reject live status storage")
assert(team_prompt.include?("Request Modes"), "team prompt must define request modes")
assert(team_prompt.include?("Direct Reply"), "team prompt must allow ordinary direct replies")
assert(!team_prompt.include?("Lightweight Action"), "team prompt must not keep the old lightweight action mode")
assert(team_prompt.include?("Quick Task"), "team prompt must define quick task mode")
assert(team_prompt.include?("Project Work"), "team prompt must reserve project/task flow for project work")
assert(team_prompt.include?("Standard Flow Index"), "team prompt must provide a standard flow index")
assert(team_prompt.include?("| Role | Purpose | Skill | Skill step |"), "team prompt flow index must be role-based")
assert(team_prompt.include?("Quick Task Flow"), "team prompt must define quick task flow")
assert(team_prompt.include?("Project Work Flow"), "team prompt must define project work flow")
assert(team_prompt.include?("restore project state"), "team prompt must restore project state before creating or planning work")
assert(team_prompt.include?("Create Quick Project"), "team prompt quick task flow must index project-management quick project steps")
assert(team_prompt.include?("assignment room"), "team prompt must include assignment-room handoff")
assert(team_prompt.include?("Acknowledge") && team_prompt.include?("Completion Message"), "team prompt must index Worker execution steps")
assert(team_prompt.include?("project requester source and reply route"), "team prompt must route requester reports through project state")
assert(!team_prompt.include?("Event Resume Flow"), "team prompt must not expose a standalone event resume flow")
assert(!team_prompt.include?("check_active_tasks"), "team prompt must not mention cancelled hook recovery checks")
assert(team_prompt.include?("DAG") && team_prompt.include?("Loop"), "team prompt must retain DAG and Loop project modes")
assert(team_prompt.include?("shared/tasks/{task-id}/"), "team prompt must define task workspace paths")
assert(team_prompt.include?("non-overridable"), "team prompt must make credential safety non-overridable")
assert(team_prompt.include?("authorization headers"), "team prompt must forbid credential disclosure")
assert(team_prompt.include?("Runtime Agent Boundary"), "team prompt must define runtime-agent boundary")
assert(team_prompt.include?("TeamHarness roster is the authority"), "team prompt must make TeamHarness roster authoritative")
assert(team_prompt.include?("chat_with_agent"), "team prompt must mention runtime-native agent tools")
assert(team_prompt.include?("is a team Worker"), "team prompt must reject runtime-native agents as Workers")

worker_prompt = read(plugin_root / agent_prompts.fetch("worker"))
assert(worker_prompt.include?("NO_REPLY") && worker_prompt.include?("ping-pong"), "worker prompt must suppress ping-pong")
assert(worker_prompt.include?("Direct Checks"), "worker prompt must define direct checks")
assert(worker_prompt.include?("Do not use taskflow"), "worker prompt must avoid taskflow for direct checks")
remote_prompt = read(plugin_root / agent_prompts.fetch("remoteMember"))
assert(
  remote_prompt.include?("not") && remote_prompt.include?("HiClaw-managed Worker"),
  "remote prompt must define remote worker boundary"
)
assert(remote_prompt.include?("current room/session"), "remote prompt must avoid message tool for current-session reports")
assert(remote_prompt.include?("must leave"), "remote prompt must scope message tool to cross-session routing")
leader_prompt = read(plugin_root / agent_prompts.fetch("leader"))
assert(leader_prompt.include?("Do not treat a worker completion message as automatic project acceptance"), "leader prompt must require acceptance")
assert(leader_prompt.include?("Request Intake"), "leader prompt must define request intake")
assert(leader_prompt.include?("Do not create a project"), "leader prompt must avoid project state for direct replies")
manager_prompt = read(plugin_root / manager_prompts.fetch("agents"))
assert(manager_prompt.include?("control-plane"), "manager prompt must state the control-plane boundary")

skills = manifest.fetch("skills")
assert(!skills.key?("manager"), "manager control-plane skills are not part of TeamHarness v0.1 manifest")
expected_skills = {
  "agent" => {
    "mcporter" => %w[leader worker manager remote-member],
    "find-skills" => %w[leader worker manager remote-member]
  },
  "team" => {
    "organization" => %w[leader worker manager remote-member],
    "communication" => %w[leader worker manager remote-member],
    "file-sharing" => %w[leader worker manager remote-member],
    "team-coordination" => %w[leader],
    "project-management" => %w[leader],
    "task-delegation" => %w[leader],
    "task-execution" => %w[worker remote-member]
  }
}
assert(skills.keys.sort == expected_skills.keys.sort, "unexpected skill groups: #{skills.keys.inspect}")
expected_skills.each do |group, expected|
  entries = skills.fetch(group)
  actual = entries.to_h { |entry| [entry.fetch("id"), Array(entry.fetch("roles"))] }
  assert(actual == expected, "unexpected #{group} skills: #{actual.inspect}")
  entries.each do |entry|
    assert(!entry.fetch("id").start_with?("teamharness-"), "skill id must be unprefixed: #{entry.fetch("id")}")
    skill_path = plugin_root / entry.fetch("path") / "SKILL.md"
    assert_file(skill_path)
    frontmatter = skill_frontmatter(skill_path)
    expected_name = group == "agent" ? entry.fetch("id") : "teamharness-#{entry.fetch("id")}"
    assert(frontmatter.fetch("name", nil) == expected_name, "skill #{entry.fetch("id")} front matter name must be #{expected_name}")
    description = frontmatter.fetch("description", "").to_s.strip
    assert(!description.empty?, "skill #{entry.fetch("id")} front matter description must be present")
  end
end

team_skill_dir = plugin_root / "skills/team"
communication_skill = read(team_skill_dir / "communication/SKILL.md")
coordination_skill = read(team_skill_dir / "team-coordination/SKILL.md")
project_skill = read(team_skill_dir / "project-management/SKILL.md")
delegation_skill = read(team_skill_dir / "task-delegation/SKILL.md")
execution_skill = read(team_skill_dir / "task-execution/SKILL.md")
assert(communication_skill.include?("ordinary direct replies"), "communication skill must cover ordinary direct replies")
assert(communication_skill.include?("lightweight one-off"), "communication skill must cover lightweight one-off routing")
assert(communication_skill.include?("current room/session"), "communication skill must avoid message tool for current-session replies")
assert(communication_skill.include?("cross-session"), "communication skill must cover cross-session message routing")
assert(coordination_skill.include?("Choose Loop"), "coordination skill must retain Loop mode")
assert(coordination_skill.include?("Do not pre-expand repeated Loop rounds"), "coordination skill must avoid flattening Loop into a large DAG")
assert(project_skill.include?("Project Work"), "project skill must be scoped to Project Work")
assert(project_skill.include?("ordinary direct replies"), "project skill must exclude ordinary direct replies")
assert(project_skill.include?("meta.json"), "project skill must use CoPaw meta.json state")
assert(project_skill.include?("resolve_project"), "project skill must document project context resume")
assert(project_skill.include?("accept_task_result"), "project skill must document explicit task result acceptance")
assert(!project_skill.include?("check_active_tasks"), "project skill must not document cancelled hook recovery checks")
assert(!project_skill.include?("Hook Recovery Checks"), "project skill must not expose cancelled hook recovery checks")
assert(project_skill.include?("mark_requester_report_sent"), "project skill must document requester report clearing")
assert(project_skill.include?("plan_loop"), "project skill must document Loop planning")
assert(project_skill.include?("ready_loop_nodes"), "project skill must document Loop ready-node resolution")
assert(project_skill.include?("record_loop_iteration"), "project skill must document Loop iteration recording")
assert(project_skill.include?("Project Status Reports"), "project skill must include requester status report template")
assert(project_skill.include?("After the project state changes"), "project skill must require requester visibility after accepted state changes")
assert(project_skill.include?("Team Room coordination") && project_skill.include?("do not count as the requester report"), "project skill must not count team-room coordination as requester reporting")
assert(!project_skill.include?("Loop planning, pause/resume/complete"), "project skill must not mark Loop planning as future")
assert(delegation_skill.include?("ready project node"), "delegation skill must be scoped to ready project nodes")
assert(delegation_skill.include?("ordinary conversation"), "delegation skill must not turn ordinary conversation into tasks")
assert(communication_skill.include?("matrix:!roomid:domain"), "communication skill must support legacy Matrix requester routing")
assert(communication_skill.include?("requester report") && communication_skill.include?("mandatory"), "communication skill must require requester reports after accepted state changes")
assert(execution_skill.include?("Do not use this skill or taskflow"), "execution skill must exclude direct checks")
assert(execution_skill.include?("meta.json"), "execution skill must use CoPaw meta.json state")

server = manifest.fetch("mcp").fetch("servers").fetch(0)
assert(server.fetch("id") == "teamharness", "MCP server id must be teamharness")
assert(server.fetch("transport") == "stdio", "TeamHarness MCP must use stdio")
assert(
  server.fetch("tools") == %w[health message roomflow filesync projectflow taskflow],
  "unexpected MCP tools: #{server.fetch("tools").inspect}"
)

assert(!manifest.key?("hooks"), "TeamHarness v0.1 must not define runtime-neutral top-level hooks")

adapters = manifest.fetch("adapters").to_h { |adapter| [adapter.fetch("id"), adapter.fetch("path")] }
assert(adapters == { "qwenpaw" => "adapters/qwenpaw", "claude-code" => "adapters/claude-code" }, "unexpected adapters: #{adapters.inspect}")

plugin_files = Dir.glob((plugin_root / "**/*").to_s, File::FNM_DOTMATCH)
  .map { |path| Pathname.new(path) }
  .select(&:file?)
  .reject { |path| path.extname == ".pyc" }

combined = plugin_files.map { |path| read(path) }.join("\n")
[
  "AGENTTEAM_",
  "teamharness_periodic_sync",
  "AGENTTEAM_SYNC_INTERVAL_SECONDS",
  "/api/agentteam",
  "agentteam-qwenpaw",
  "project.json",
  "task.json"
].each do |needle|
  assert(!combined.include?(needle), "forbidden legacy/runtime ownership marker #{needle.inspect}")
end

puts "ok: teamharness contracts"
