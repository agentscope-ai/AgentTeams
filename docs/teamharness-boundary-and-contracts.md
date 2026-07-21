# TeamHarness v0.1 Boundary And Contracts

This document defines the functional boundary for the current TeamHarness
plugin package.

TeamHarness v0.1 is the runtime-neutral team collaboration base. It packages
stable prompts, team skills, MCP tools, lifecycle scripts, and runtime adapter
entrypoints. It does not own worker lifecycle, controller reconciliation,
worker desired-state apply loop, runtime hook behavior, or
periodic workspace persistence.

## Responsibilities

TeamHarness owns:

- Stable team collaboration prompts and role prompts.
- Team collaboration skills for organization, communication, shared files,
  projects, task delegation, and task execution.
- Explicit MCP tools for team messages, shared files, project flow, task flow,
  and plugin health.
- A single plugin tarball installed through the AgentTeams `agt` CLI by
  default and compatible with LoongSuite `plugin-probe` for local runtimes.

TeamHarness does not own:

- Controller generation of `agents/{runtimeName}/runtime/runtime.yaml`.
- Worker process lifecycle, pod restart, or runtime process supervision.
- Worker desired-state parsing, polling, apply, or diagnostics.
- Runtime-neutral top-level hooks.
- Runtime hook trigger contracts, payload formats, and enforcement behavior.
- Credential access enforcement that depends on runtime-specific file or tool
  guard support.
- AgentSpec package download, apply, rollback, or update inside the worker desired-state apply loop.
- Periodic workspace push/pull loops.
- Direct QwenPaw or Claude Code runtime mutation in the base package.
- Secret value storage.

## Contract Relationships

Controller to runtime:

- The controller writes non-secret desired state and team facts to
  `agents/{runtimeName}/runtime/runtime.yaml`.
- Secrets stay in environment variables, mounted files, or service account
  tokens.

Runtime worker to TeamHarness:

- The worker installs or exposes TeamHarness assets for the selected runtime.
- The worker owns the desired-state apply loop, including runtime config polling and AgentSpec package application.
- The worker may call TeamHarness MCP tools, but TeamHarness does not poll CR
  state or object storage by itself.

Runtime adapter to TeamHarness:

- The adapter maps TeamHarness prompts, skills, and MCP into a concrete runtime.
- Runtime-specific hooks live under the adapter implementation, for example
  `adapters/qwenpaw/hooks/`, when that runtime integration phase defines them.
- Runtime config consumption belongs to the worker/runtime adapter layer, not to
  the TeamHarness plugin package.
- The adapter should consume controller-written runtime config facts instead of
  querying the `agt` CLI for team or member identity.

TeamHarness plugin package to AgentSpec package:

- The TeamHarness plugin package is runtime infrastructure.
- `desired.agentPackage` in `runtime.yaml` is an AgentTeams AgentSpec package and
  belongs to the worker desired-state apply path.
- Updating an AgentSpec package must not be modeled as updating the TeamHarness
  plugin package.

## Standard Asset Set

Prompts:

- `prompts/team/TEAMS.md`
- `prompts/agent/leader.md`
- `prompts/agent/worker.md`
- `prompts/agent/remote-member.md`
- `prompts/manager/AGENTS.md`
- `prompts/manager/TOOLS.md`
- `prompts/manager/HEARTBEAT.md`

Skills:

- Agent skills: `mcporter`, `find-skills`.
- Team skills: `organization`, `communication`, `file-sharing`,
  `team-coordination`, `project-management`, `task-delegation`,
  `task-execution`.

MCP tools:

- `health`
- `message`
- `filesync`
- `projectflow`
- `taskflow`
