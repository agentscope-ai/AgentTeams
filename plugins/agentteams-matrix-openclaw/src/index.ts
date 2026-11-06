/**
 * OpenClaw plugin entry for AgentTeams Matrix (channel `agentteams-matrix`).
 *
 * The gateway loads this module (see `openclaw.plugin.json`), injects the
 * runtime through `setRuntime`, and starts the channel's accounts via
 * `gateway.startAccount`.
 *
 * `defineChannelPluginEntry` comes from the public SDK subpath
 * `openclaw/plugin-sdk/channel-core` (defined at `src/plugin-sdk/core.ts:513`
 * in OpenClaw 2026.10.9; `scripts/verify-sdk.mjs` asserts the runtime import
 * before the operator gates run).
 */

import { defineChannelPluginEntry } from "openclaw/plugin-sdk/channel-core";
import type { OpenClawPluginDefinition } from "openclaw/plugin-sdk/core";
import { openClawMatrixPlugin, setRuntime } from "./plugin.js";

// The explicit annotation keeps the emitted declaration nameable: the
// inferred entry type reaches into the SDK's internal schema modules, which
// `tsc --declaration` cannot reference portably (TS2742).
const entry: OpenClawPluginDefinition = defineChannelPluginEntry({
  id: "agentteams-matrix",
  name: "AgentTeams Matrix",
  description: "Matrix channel bridged by the AgentTeams sidecar",
  plugin: openClawMatrixPlugin,
  setRuntime: (rt) => setRuntime(rt),
});

export default entry;
