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

def normalized(text)
  text.gsub(/\s+/, " ")
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
assert(team_prompt.include?("Request Modes"), "team prompt must define request modes")
assert(team_prompt.include?("Direct Reply"), "team prompt must allow ordinary direct replies")
assert(!team_prompt.include?("Lightweight Action"), "team prompt must not keep the old lightweight action mode")
assert(team_prompt.include?("Quick Task"), "team prompt must define quick task mode")
assert(team_prompt.include?("Project Work"), "team prompt must reserve project/task flow for project work")
assert(team_prompt.include?("Request Modes And Standard Flow"), "team prompt must merge request modes and standard flow")
assert(!team_prompt.include?("Project Wake-Up Recovery"), "team prompt must not keep project wake-up as a standalone chapter")
assert(normalized(team_prompt).include?("resolve project context"), "team prompt must resolve project context before result handling")
assert(team_prompt.include?("create_quick_project"), "team prompt quick task flow must index project-management quick project steps")
assert(team_prompt.include?("task request message"), "team prompt must route task-room handoff as a task request message")
assert(team_prompt.include?("roomflow create_task_room"), "team prompt must create task rooms through roomflow")
assert(!team_prompt.include?("Load `teamharness-team-coordination` if the boundary is unclear"), "team prompt quick task flow must not re-run mode selection")
assert(normalized(team_prompt).include?("Do not call `delegate_task` for Quick Task"), "team prompt quick task flow must not delegate after create_quick_project")
assert(team_prompt.include?("create_project") && team_prompt.include?("plan_dag") && team_prompt.include?("plan_loop"), "team prompt project work flow must route planning to project-management")
assert(team_prompt.include?("Rooms and Channels"), "team prompt must define rooms and channels")
assert(team_prompt.include?("Source channel / requester room"), "team prompt must define source/requester channels")
assert(team_prompt.include?("DingTalk") && team_prompt.include?("DM or Team room"), "team prompt must describe requester/source intake channels")
assert(team_prompt.include?("Task room: a Matrix room"), "team prompt must define Matrix task rooms")
assert(team_prompt.include?("TASK：<projectId>"), "team prompt must define the project-id task-room name marker")
assert(team_prompt.include?("roomflow describe_room"), "team prompt must use roomflow describe_room for Matrix room identity")
assert(team_prompt.include?("name") && team_prompt.include?("topic") && team_prompt.include?("tags"), "team prompt must classify Matrix rooms from Matrix state")
assert(team_prompt.include?("Project/Task marker"), "team prompt must define task rooms by Project/Task markers")
assert(normalized(team_prompt).include?("teamharness-communication` to send the task request message"), "team prompt must route requester/source project handoff to communication skill")
assert(team_prompt.include?("teamharness-roomflow"), "team prompt must route room classification and creation to roomflow skill")
assert(team_prompt.include?("DingTalk") && team_prompt.include?("external channel"), "team prompt must use one requester/source model across source channels")
assert(team_prompt.include?("Stop in the source session") && team_prompt.include?("delegate_task"), "team prompt must forbid source-session project/task execution")
assert(team_prompt.include?("check_task") && team_prompt.include?("accept_task_result"), "team prompt must route submitted results through check and acceptance skills")
assert(team_prompt.include?("task result path") && team_prompt.include?("resolve project"), "team prompt must resolve project state for result events")
assert(team_prompt.include?("replyRoute"), "team prompt must route requester reports through project state")
assert(!team_prompt.include?("Event Resume Flow"), "team prompt must not expose a standalone event resume flow")
assert(!team_prompt.include?("check_active_tasks"), "team prompt must not mention cancelled hook recovery checks")
assert(team_prompt.include?("DAG") && team_prompt.include?("Loop"), "team prompt must retain DAG and Loop project modes")
assert(team_prompt.include?("shared/tasks/{task-id}/"), "team prompt must define task workspace paths")
assert(team_prompt.include?("non-overridable"), "team prompt must make credential safety non-overridable")
assert(team_prompt.include?("authorization headers"), "team prompt must forbid credential disclosure")
assert(team_prompt.include?("Team Worker Coordination"), "team prompt must define TeamHarness Worker coordination")
assert(team_prompt.include?("Workers in the team roster"), "team prompt must make TeamHarness Worker roster authoritative")
assert(team_prompt.include?("chat_with_agent"), "team prompt must mention internal agent tools")
assert(team_prompt.include?("Do not use internal agent tools"), "team prompt must forbid internal agent tools for Worker coordination")
assert(team_prompt.include?("Matrix Mentions"), "team prompt must define Matrix mention rules")
assert(team_prompt.include?("m.mentions") && team_prompt.include?("full member Matrix user id"), "team prompt must require resolvable Matrix mentions")
assert(team_prompt.include?("wrong") && team_prompt.include?("@name"), "team prompt must reject wrong Matrix @ names")
assert(team_prompt.include?("Direct Reply And NO_REPLY"), "team prompt must define direct reply and NO_REPLY rules")
assert(team_prompt.include?("Long Matrix Messages"), "team prompt must limit long Matrix messages")
assert(team_prompt.include?("Artifacts And Documents"), "team prompt must define artifact publication")
assert(team_prompt.include?("artifact publish_file"), "team prompt must use artifact publish_file for artifacts")
assert(team_prompt.include?("only for Matrix rooms") && team_prompt.include?("non-Matrix requester channels"), "team prompt must not imply artifact publish_file works for external requester channels")
assert(team_prompt.include?("Leader Cross-Room Communication"), "team prompt must define leader cross-room communication")
assert(team_prompt.include?("teamharness-communication") && team_prompt.include?("message-tool payloads"), "team prompt must route detailed cross-room messaging to communication skill")

worker_prompt = read(plugin_root / agent_prompts.fetch("worker"))
assert(worker_prompt.include?("NO_REPLY") && worker_prompt.include?("ping-pong"), "worker prompt must suppress ping-pong")
assert(worker_prompt.include?("Direct Checks"), "worker prompt must define direct checks")
assert(worker_prompt.include?("Do not use taskflow"), "worker prompt must avoid taskflow for direct checks")
remote_prompt = read(plugin_root / agent_prompts.fetch("remoteMember"))
assert(
  remote_prompt.include?("not") && remote_prompt.include?("AgentTeams-managed Worker"),
  "remote prompt must define remote worker boundary"
)
assert(remote_prompt.include?("current room/session"), "remote prompt must avoid message tool for current-session reports")
assert(remote_prompt.include?("must leave"), "remote prompt must scope message tool to cross-session routing")
leader_prompt = read(plugin_root / agent_prompts.fetch("leader"))
assert(leader_prompt.include?("Do not treat a worker completion message as automatic project acceptance"), "leader prompt must require acceptance")
assert(leader_prompt.include?("Request Intake"), "leader prompt must define request intake")
assert(leader_prompt.include?("Do not create a project"), "leader prompt must avoid project state for direct replies")
assert(leader_prompt.include?("PROJECT_REQUESTED") && leader_prompt.include?("requester/source session"), "leader prompt must route source project work through task-room self-trigger")
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
    "communication" => %w[leader worker manager remote-member],
    "file-sharing" => %w[leader worker manager remote-member],
    "roomflow" => %w[leader],
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
roomflow_skill = read(team_skill_dir / "roomflow/SKILL.md")
coordination_skill = read(team_skill_dir / "team-coordination/SKILL.md")
project_skill = read(team_skill_dir / "project-management/SKILL.md")
delegation_skill = read(team_skill_dir / "task-delegation/SKILL.md")
execution_skill = read(team_skill_dir / "task-execution/SKILL.md")
project_skill_text = normalized(project_skill)
delegation_skill_text = normalized(delegation_skill)
assert(communication_skill.include?("ordinary direct replies"), "communication skill must cover ordinary direct replies")
assert(communication_skill.include?("lightweight one-off"), "communication skill must cover lightweight one-off routing")
assert(communication_skill.include?("current room/session"), "communication skill must avoid message tool for current-session replies")
assert(communication_skill.include?("cross-session"), "communication skill must cover cross-session message routing")
assert(communication_skill.include?("mentionSender"), "communication skill must document DingTalk sender mentions for requester reports")
assert(communication_skill.include?("Task room") && !communication_skill.include?("Team Room"), "communication skill must use task-room terminology")
assert(communication_skill.include?("Task Room Request Message"), "communication skill must define task-room request message shape")
assert(communication_skill.include?("PROJECT_REQUESTED: <short task or project title>"), "communication skill must give PROJECT_REQUESTED task-room request example")
assert(communication_skill.include?('"target": "room:!task-room:matrix.local"'), "communication skill PROJECT_REQUESTED example must use string Matrix room target")
assert(communication_skill.include?("@prod-observe-oncall-bot:at-cn-rpg4um9o601"), "communication skill PROJECT_REQUESTED example must use a full Matrix user id")
assert(!communication_skill.include?('"agent": "leader"') && !communication_skill.include?('"agentId": "leader"'), "communication skill PROJECT_REQUESTED example must not use role names as trigger identity")
assert(communication_skill.include?("Do not use role names") && communication_skill.include?("default"), "communication skill must reject role/workspace names for trigger identity")
assert(communication_skill.include?("Requester Report Message"), "communication skill must define requester report delivery")
assert(communication_skill.include?("teamharness-project-management") && communication_skill.include?("<project-management requester report markdown>"), "communication skill must defer requester report content to project-management")
assert(communication_skill.include?("Do not use Matrix mention syntax") && communication_skill.include?("descriptive owners"), "communication skill must prevent requester-report display mentions")
assert(normalized(communication_skill).include?("do not describe workspace files as chat attachments") && communication_skill.include?("platform object-storage view"), "communication skill must describe external-channel artifact paths without fake file attachments")
assert(communication_skill.include?("return to `teamharness-project-management`") && communication_skill.include?("mark_requester_report_sent"), "communication skill must leave requester report state updates to project-management")
assert(communication_skill.include?("replyRoute") && communication_skill.include?("targetSession"), "communication skill must show requester report delivery payload")
assert(communication_skill.include?("Quick Task or Project Work handoff"), "communication skill self-trigger must cover Quick Task and Project Work")
assert(roomflow_skill.include?("roomflow describe_room"), "roomflow skill must document room description")
assert(roomflow_skill.include?("roomflow create_task_room"), "roomflow skill must document task-room creation")
assert(roomflow_skill.include?("TASK：<projectId>"), "roomflow skill must document project-id task-room name normalization")
assert(normalized(roomflow_skill).include?("Different projects from the same DingTalk group or same person still get different task rooms"), "roomflow skill must document project-scoped task-room reuse")
assert(coordination_skill.include?("Choose Loop"), "coordination skill must retain Loop mode")
assert(coordination_skill.include?("Do not pre-expand repeated Loop rounds"), "coordination skill must avoid flattening Loop into a large DAG")
assert(project_skill.include?("Quick Task") && project_skill.include?("Project Work"), "project skill must cover Quick Task and Project Work")
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
assert(project_skill.include?("`teamharness-roomflow` owns task-room creation"), "project skill must leave room setup details to roomflow")
assert(!project_skill.include?("roomBindingScope: \"sender\""), "project skill must not duplicate roomflow sender binding details")
assert(project_skill.include?("DingTalk group, Matrix DM, or other source room") && project_skill.include?("sourceRoomId") && project_skill.include?("replyRoute"), "project skill must persist requester/source routes")
assert(project_skill.include?("PROJECT_REQUESTED") && project_skill_text.include?("do not call `create_project` in the requester/source session"), "project skill must move source project creation into the task room")
assert(project_skill.include?("Do not call this in the requester/source session"), "project skill quick task creation must run in the task room")
assert(project_skill.include?("Project Status Reports"), "project skill must include requester status report template")
assert(project_skill.include?("owner or executor cells are descriptive text") && project_skill.include?("Do not prefix them with `@`"), "project report template must avoid display-only Matrix mentions")
assert(project_skill.include?("After the project state changes"), "project skill must require requester visibility after accepted state changes")
assert(project_skill.include?("publishArtifacts: true") && project_skill.include?("parentEventId"), "project skill must document explicit project artifact publish after requester report")
assert(project_skill.include?("non-Matrix channel") && project_skill.include?("do not claim that a file was attached") && project_skill.include?("platform object-storage view"), "project requester report template must avoid fake external-channel file attachment claims")
assert(project_skill.include?("Task room coordination") && project_skill.include?("do not count as the requester report"), "project skill must not count task-room coordination as requester reporting")
assert(!project_skill.include?("Loop planning, pause/resume/complete"), "project skill must not mark Loop planning as future")
assert(delegation_skill.include?("ready project node"), "delegation skill must be scoped to ready project nodes")
assert(delegation_skill.include?("ordinary conversation"), "delegation skill must not turn ordinary conversation into tasks")
assert(delegation_skill.include?("For Quick Task") && delegation_skill.include?("do not call\n`delegate_task` again"), "delegation skill must keep Quick Task assignment message separate from delegate_task")
assert(delegation_skill.include?("normal current-session reply") && delegation_skill.include?("Do not use the `message` tool"), "delegation skill must keep same-room assignment out of message tool")
assert(delegation_skill.include?("`teamharness-roomflow` owns task-room creation"), "delegation skill must leave room setup details to roomflow")
assert(!delegation_skill.include?("roomBindingScope: \"sender\""), "delegation skill must not duplicate roomflow sender binding details")
assert(delegation_skill_text.include?("Do not fall back to the requester/source session"), "delegation skill must keep Project Work assignment inside the task room")
assert(communication_skill.include?("matrix:!roomid:domain"), "communication skill must support legacy Matrix requester routing")
assert(communication_skill.include?("Matrix DM requester reports") && communication_skill.include?("targetSession"), "communication skill must document Matrix DM reply routes")
assert(communication_skill.include?("requester report") && communication_skill.include?("mandatory"), "communication skill must require requester reports after accepted state changes")
assert(communication_skill.include?("PROJECT_REQUESTED Self Trigger") && communication_skill.include?("matrix_self_trigger"), "communication skill must document PROJECT_REQUESTED trigger result")
assert(communication_skill.include?("same-agent Quick Task or Project Work handoff"), "communication skill must own PROJECT_REQUESTED handoff details")
assert(leader_prompt.include?("same runtime\nMatrix account") && leader_prompt.include?("current Matrix user id"), "leader prompt must describe self-trigger identity as the runtime Matrix account")
assert(leader_prompt.include?("role names such as `leader`") && leader_prompt.include?("workspace names such as\n`default`"), "leader prompt must reject role/workspace names for self-trigger identity")
assert(normalized(communication_skill).include?("recorded requester route is exactly"), "communication skill must own requester route exclusion rules")
assert(execution_skill.include?("Do not use this skill or taskflow"), "execution skill must exclude direct checks")
assert(execution_skill.include?("meta.json"), "execution skill must use CoPaw meta.json state")

server = manifest.fetch("mcp").fetch("servers").fetch(0)
assert(server.fetch("id") == "teamharness", "MCP server id must be teamharness")
assert(server.fetch("transport") == "stdio", "TeamHarness MCP must use stdio")
assert(
  server.fetch("tools") == %w[health message roomflow filesync artifact projectflow taskflow],
  "unexpected MCP tools: #{server.fetch("tools").inspect}"
)

assert(!manifest.key?("hooks"), "TeamHarness v0.1 must not define runtime-neutral top-level hooks")

adapters = manifest.fetch("adapters").to_h { |adapter| [adapter.fetch("id"), adapter.fetch("path")] }
assert(adapters == { "qwenpaw" => "adapters/qwenpaw" }, "unexpected adapters: #{adapters.inspect}")

plugin_files = Dir.glob((plugin_root / "**/*").to_s, File::FNM_DOTMATCH)
  .map { |path| Pathname.new(path) }
  .select(&:file?)
  .reject { |path| path.to_s.start_with?((plugin_root / "remote").to_s + "/") }
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
