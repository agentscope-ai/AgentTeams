#!/usr/bin/env node
"use strict";

const HISTORY_CONTEXT_MARKER = "[Chat messages since your last reply - for context]";
const CURRENT_MESSAGE_MARKER = "[Current message - respond to this]";

function runtimeModel(runtimeState) {
  const runtime = runtimeState && runtimeState.runtime ? runtimeState.runtime : {};
  const desired = runtime.desired || {};
  return desired.model || {};
}

function appendMetadata(lines, metadata) {
  for (const item of metadata || []) {
    lines.push(`${item.label}: ${item.value || ""}`);
  }
}

function formatHistoryItem(item) {
  const sender = String(item?.sender || "").trim();
  const body = String(item?.body || "").trim();
  if (!body) return "";
  let line = sender ? `${sender}: ${body}` : body;
  const eventId = String(item?.eventId || item?.event_id || "").trim();
  if (eventId) line += ` [id:${eventId}]`;
  return line;
}

function buildStableRuntimePrompt(options) {
  const opts = options || {};
  const lines = Array.isArray(opts.introLines) ? [...opts.introLines] : [];
  appendMetadata(lines, opts.metadata);
  for (const line of opts.guidanceLines || []) {
    lines.push(line);
  }

  const model = opts.model || runtimeModel(opts.runtimeState);
  if (model.model) {
    lines.push(`Managed model: ${model.model}`);
  }
  if (opts.workspace) {
    lines.push(`Workspace: ${opts.workspace}`);
  }
  const contextSections = (opts.contextSections || []).filter(Boolean);
  if (contextSections.length) {
    lines.push("");
    lines.push(opts.contextTitle || "Runtime context:");
    lines.push(...contextSections);
  }
  return `${lines.join("\n").replace(/\s+$/, "")}\n`;
}

function buildMatrixTurnPrompt(options) {
  const opts = options || {};
  const lines = [];
  appendMetadata(lines, opts.metadata || opts.turnMetadata);
  if (opts.taskPathHint) {
    lines.push(`Task path: ${opts.taskPathHint}`);
  }

  const roomHistory = Array.isArray(opts.roomHistory) ? opts.roomHistory : [];
  const historyLines = roomHistory.slice(-20).map(formatHistoryItem).filter(Boolean);
  if (historyLines.length) {
    if (lines.length) lines.push("");
    lines.push(HISTORY_CONTEXT_MARKER);
    lines.push(...historyLines);
    lines.push("");
  } else if (lines.length) {
    lines.push("");
  }
  lines.push(CURRENT_MESSAGE_MARKER);
  lines.push(opts.currentMessage || "");
  return `${lines.join("\n").replace(/\s+$/, "")}\n`;
}

function buildRuntimePrompt(options) {
  const stable = buildStableRuntimePrompt(options).trimEnd();
  const turn = buildMatrixTurnPrompt(options).trimEnd();
  if (stable && turn) return `${stable}\n\n${turn}\n`;
  if (stable) return `${stable}\n`;
  return `${turn}\n`;
}

function extractTaskPathHint(text) {
  const match = String(text || "").match(/(shared\/tasks\/[A-Za-z0-9_.:/@-]+)/);
  return match ? match[1] : "";
}

function runtimeTeamMetadata(runtimeState) {
  const runtime = runtimeState?.runtime || {};
  const team = runtime.team || {};
  const member = runtime.member || {};
  const metadata = [];
  const teamName = runtimeState?.edge?.teamName || team.name || team.teamName;
  if (teamName) metadata.push({ label: "Team", value: teamName });
  if (member.name) metadata.push({ label: "Member", value: member.name });
  if (member.runtimeName) metadata.push({ label: "Runtime name", value: member.runtimeName });
  if (member.role) metadata.push({ label: "Role", value: member.role });
  if (member.runtime) metadata.push({ label: "Runtime", value: member.runtime });
  if (member.matrixUserId) metadata.push({ label: "Matrix user", value: member.matrixUserId });
  if (member.personalRoomId) metadata.push({ label: "Personal room", value: member.personalRoomId });
  const teamRoom = runtime.matrix?.teamRoomId || team.teamRoomId || team.roomId;
  if (teamRoom) metadata.push({ label: "Team room", value: teamRoom });
  return metadata;
}

function matrixTurnMetadata(options) {
  const opts = options || {};
  const event = opts.event || {};
  const metadata = [];
  if (opts.roomId) metadata.push({ label: "Room id", value: opts.roomId });
  if (event.event_id) metadata.push({ label: "Room event id", value: event.event_id });
  if (event.sender) metadata.push({ label: "Room sender", value: event.sender });
  return metadata;
}

function managedRuntimeContextBlock(runtimeState, options) {
  const opts = options || {};
  const facts = runtimeTeamMetadata(runtimeState);
  if (!facts.length) return "";
  const start = opts.startMarker || "<!-- BEGIN TEAMHARNESS RUNTIME CONTEXT -->";
  const end = opts.endMarker || "<!-- END TEAMHARNESS RUNTIME CONTEXT -->";
  const lines = [
    start,
    "## Runtime Team Context",
    ""
  ];
  for (const item of facts) {
    lines.push(`- ${item.label}: ${item.value}`);
  }
  lines.push("");
  lines.push("Do not write secrets, credentials, or live task status into this file.");
  lines.push(end);
  return lines.join("\n");
}

module.exports = {
  HISTORY_CONTEXT_MARKER,
  CURRENT_MESSAGE_MARKER,
  buildStableRuntimePrompt,
  buildMatrixTurnPrompt,
  buildRuntimePrompt,
  extractTaskPathHint,
  runtimeTeamMetadata,
  matrixTurnMetadata,
  managedRuntimeContextBlock
};
