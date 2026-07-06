# TeamHarness Remote Node Runtime Core

This directory contains shared protocol code for remote-managed local Node.js runtimes.

The core owns TeamHarness remote-management mechanics that must stay consistent across runtime adapters:

- MCP server tools and broker-backed file sync.
- Common LoongSuite-injected worker argument parsing.
- Local model proxy for OpenAI-compatible and Anthropic-compatible calls.
- Bootstrap token validation, controller edge-token exchange, STS refresh, heartbeat.
- OSS object operations and `runtime.yaml` loading/snapshot redaction.
- Matrix sync loop and helper functions for state files, trigger checks, send/edit/join/typing.
- AgentSpec package view sync back into the worker OSS workspace view.
- Local credential broker endpoints for runtime context and Matrix/model/storage/skill-registry credentials.
- Shared prompt assembly for runtime metadata, managed model, room history, and current Matrix message.
- Worker lifecycle orchestration for bootstrap, command waiting, initial runtime load, heartbeat, cleanup, periodic tasks, and Matrix loop handoff.

Runtime adapters still own runtime-specific behavior:

- Runtime binary/settings/hooks/session handling.
- Runtime workspace/config/session handling.
- Agent execution and final response parsing.
- Runtime-specific package materialization into the local workspace.

The core must not import concrete adapter modules. Adapters may load core modules from the source tree during tests or from the bundled `node-runtime-core/` directory at runtime.

Node dependencies are declared in `package.json`. Local bundle builders run `npm ci --omit=dev` in the staged `node-runtime-core/` directory so remote workers run from the shipped `node_modules` without requiring users to install dependencies.
