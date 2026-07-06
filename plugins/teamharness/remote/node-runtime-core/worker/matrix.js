#!/usr/bin/env node
"use strict";

const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const { readJson, writeJson, writeStatus } = require("./status");
const { normalizeMaxConcurrentTasks } = require("./args");
const { logEvent } = require("./log");
const { teamRoomId, personalRoomId, matrixRooms } = require("./runtime-config");

const MATRIX_USER_RE = /@[A-Za-z0-9._=+/\-]+:[A-Za-z0-9.\-]+(?::\d+)?/g;

let markdownRenderer;
let markdownRendererLoaded = false;

function escapeRegExp(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function containsNameMention(body, name) {
  const value = String(name || "").trim();
  if (!value) return false;
  return new RegExp(`(^|[^\\w-])@?${escapeRegExp(value)}($|[^\\w-])`).test(body);
}

function isMatrixThreadEvent(event) {
  const relatesTo = event?.content && typeof event.content === "object" ? event.content["m.relates_to"] : null;
  return Boolean(relatesTo && typeof relatesTo === "object" && relatesTo.rel_type === "m.thread");
}

function isMatrixReplaceEvent(event) {
  const relatesTo = event?.content && typeof event.content === "object" ? event.content["m.relates_to"] : null;
  return Boolean(relatesTo && typeof relatesTo === "object" && relatesTo.rel_type === "m.replace");
}

function matrixReplaceTargetEventId(event) {
  const relatesTo = event?.content && typeof event.content === "object" ? event.content["m.relates_to"] : null;
  if (!relatesTo || typeof relatesTo !== "object" || relatesTo.rel_type !== "m.replace") return "";
  return String(relatesTo.event_id || "").trim();
}

function extractMatrixMentionsFromText(text) {
  return Array.from(new Set(String(text || "").match(MATRIX_USER_RE) || []));
}

function formattedBodyMentionsUser(formattedBody, userId) {
  const body = String(formattedBody || "");
  const value = String(userId || "").trim();
  if (!body || !value) return false;
  const escaped = escapeRegExp(value);
  if (new RegExp(`href=["']https://matrix\\.to/#/${escaped}["']`, "i").test(body)) return true;
  const encoded = escapeRegExp(encodeURIComponent(value));
  return new RegExp(`href=["']https://matrix\\.to/#/${encoded}["']`, "i").test(body);
}

function eventMentionsUser(event, userId) {
  const value = String(userId || "").trim();
  if (!value) return false;
  const content = event && event.content && typeof event.content === "object" ? event.content : {};
  const mentions = content["m.mentions"] && typeof content["m.mentions"] === "object" ? content["m.mentions"] : {};
  const userIds = Array.isArray(mentions.user_ids) ? mentions.user_ids : [];
  if (userIds.some(item => String(item || "") === value)) return true;
  if (mentions.room) return true;
  if (formattedBodyMentionsUser(content.formatted_body, value)) return true;
  return extractMatrixMentionsFromText(eventBody(event)).some(item => item.toLowerCase() === value.toLowerCase());
}

function messagesAfter(messages, lastEventId) {
  if (!lastEventId) return messages;
  const index = messages.findIndex(event => event.event_id === lastEventId);
  return index >= 0 ? messages.slice(index + 1) : messages;
}

function sessionBucketName(args) {
  const runtime = String(args.runtime || "").trim().toLowerCase();
  if (runtime === ["cla", "ude-code"].join("")) return ["cla", "udeSessions"].join("");
  if (runtime === "openclaw") return "openclawSessions";
  return "";
}

function normalizeRooms(value) {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    const rooms = {};
    for (const [roomId, room] of Object.entries(value)) {
      rooms[roomId] = {
        history: Array.isArray(room?.history) ? room.history : [],
        pendingTurn: room?.pendingTurn || null,
        runningTurn: room?.runningTurn || null,
        pausedByStop: Boolean(room?.pausedByStop)
      };
    }
    return rooms;
  }
  const rooms = {};
  if (Array.isArray(value)) {
    for (const roomId of value) {
      rooms[String(roomId)] = { history: [], pendingTurn: null, runningTurn: null, pausedByStop: false };
    }
  }
  return rooms;
}

function normalizeMatrixState(raw, args, hadFile) {
  const state = raw && typeof raw === "object" ? raw : {};
  const runtimeSessions = state.runtimeSessions && typeof state.runtimeSessions === "object" ? state.runtimeSessions : {};
  const codeRuntime = ["cla", "ude-code"].join("");
  const codeBucket = ["cla", "udeSessions"].join("");
  const codeSessions = state[codeBucket] && typeof state[codeBucket] === "object"
    ? state[codeBucket]
    : runtimeSessions[codeRuntime] || {};
  const openclawSessions = state.openclawSessions && typeof state.openclawSessions === "object"
    ? state.openclawSessions
    : runtimeSessions.openclaw || {};
  return {
    version: 2,
    matrixSyncToken: String(state.matrixSyncToken || state.nextBatch || ""),
    nextBatch: String(state.matrixSyncToken || state.nextBatch || ""),
    seenEventIds: Array.isArray(state.seenEventIds) ? state.seenEventIds.map(String).filter(Boolean) : [],
    matrixCursors: state.matrixCursors && typeof state.matrixCursors === "object" ? state.matrixCursors : {},
    rooms: normalizeRooms(state.rooms),
    scheduler: {
      pendingRoomQueue: Array.isArray(state.scheduler?.pendingRoomQueue)
        ? state.scheduler.pendingRoomQueue.map(String).filter(Boolean)
        : []
    },
    runtimeSessions: {
      ...runtimeSessions,
      [codeRuntime]: codeSessions,
      openclaw: openclawSessions
    },
    [codeBucket]: codeSessions,
    openclawSessions,
    bootstrapPending: Boolean(state.bootstrapPending) || !hadFile || Object.keys(state).length === 0
  };
}

function loadMatrixState(args) {
  const file = path.join(args.stateDir, "matrix-state.json");
  const hadFile = fs.existsSync(file);
  return normalizeMatrixState(readJson(file, {}), args, hadFile);
}

function writeMatrixState(args, state) {
  const runtimeSessions = state.runtimeSessions && typeof state.runtimeSessions === "object" ? state.runtimeSessions : {};
  const codeRuntime = ["cla", "ude-code"].join("");
  const codeBucket = ["cla", "udeSessions"].join("");
  const codeSessions = state[codeBucket] || runtimeSessions[codeRuntime] || {};
  const openclawSessions = state.openclawSessions || runtimeSessions.openclaw || {};
  const bucket = sessionBucketName(args);
  if (bucket && state[bucket]) {
    runtimeSessions[String(args.runtime)] = state[bucket];
  }
  writeJson(path.join(args.stateDir, "matrix-state.json"), {
    version: 2,
    matrixSyncToken: state.matrixSyncToken || "",
    nextBatch: state.matrixSyncToken || "",
    seenEventIds: Array.isArray(state.seenEventIds) ? state.seenEventIds : [],
    matrixCursors: state.matrixCursors || {},
    rooms: state.rooms && typeof state.rooms === "object" && !Array.isArray(state.rooms) ? state.rooms : {},
    scheduler: state.scheduler && typeof state.scheduler === "object" ? state.scheduler : { pendingRoomQueue: [] },
    bootstrapPending: Boolean(state.bootstrapPending),
    runtimeSessions: {
      ...runtimeSessions,
      [codeRuntime]: codeSessions,
      openclaw: openclawSessions
    },
    [codeBucket]: codeSessions,
    openclawSessions,
    updatedAt: new Date().toISOString()
  });
}

async function matrixRequest(method, homeserver, token, requestPath, body) {
  const url = new URL(requestPath, `${homeserver.replace(/\/+$/, "")}/`);
  const response = await fetch(url, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/json",
      "Content-Type": "application/json; charset=utf-8"
    },
    body: body === undefined ? undefined : JSON.stringify(body)
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`${method} ${url} failed: ${response.status} ${text}`);
  }
  return text.trim() ? JSON.parse(text) : {};
}

function txnId(prefix) {
  return `${prefix}-${Date.now()}-${crypto.randomBytes(4).toString("hex")}`;
}

function escapeHtml(value) {
  return String(value || "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function renderInlineMarkdown(value) {
  return escapeHtml(value)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
}

function splitMarkdownTableRow(line) {
  let text = String(line || "").trim();
  if (!text.includes("|")) return [];
  if (text.startsWith("|")) text = text.slice(1);
  if (text.endsWith("|")) text = text.slice(0, -1);
  return text.split("|").map(cell => cell.trim());
}

function isMarkdownTableSeparator(line) {
  const cells = splitMarkdownTableRow(line);
  return cells.length > 0 && cells.every(cell => /^:?-{3,}:?$/.test(cell));
}

function renderMarkdownTable(headerLine, separatorLine, bodyLines) {
  const headers = splitMarkdownTableRow(headerLine);
  const separator = splitMarkdownTableRow(separatorLine);
  const width = Math.max(headers.length, separator.length);
  const headerHtml = [];
  for (let index = 0; index < width; index += 1) {
    headerHtml.push(`<th>${renderInlineMarkdown(headers[index] || "")}</th>`);
  }
  const rows = [];
  for (const line of bodyLines) {
    const cells = splitMarkdownTableRow(line);
    const cellHtml = [];
    for (let index = 0; index < width; index += 1) {
      cellHtml.push(`<td>${renderInlineMarkdown(cells[index] || "")}</td>`);
    }
    rows.push(`<tr>${cellHtml.join("")}</tr>`);
  }
  const tbody = rows.length ? `<tbody>${rows.join("")}</tbody>` : "";
  return `<table><thead><tr>${headerHtml.join("")}</tr></thead>${tbody}</table>`;
}

function loadMarkdownRenderer() {
  if (markdownRendererLoaded) return markdownRenderer;
  markdownRendererLoaded = true;
  try {
    const MarkdownIt = require("markdown-it");
    const renderer = new MarkdownIt({
      html: false,
      linkify: true,
      breaks: true,
      typographer: false
    });
    renderer.enable("table");
    renderer.enable("strikethrough");
    markdownRenderer = renderer;
  } catch (_error) {
    markdownRenderer = null;
  }
  return markdownRenderer;
}

function fallbackMatrixFormattedBody(text) {
  const lines = String(text || "").split(/\r?\n/);
  const html = [];
  let inCode = false;
  let codeLines = [];
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.trim().startsWith("```")) {
      if (inCode) {
        html.push(`<pre><code>${escapeHtml(codeLines.join("\n"))}</code></pre>`);
        codeLines = [];
        inCode = false;
      } else {
        inCode = true;
      }
      continue;
    }
    if (inCode) {
      codeLines.push(line);
      continue;
    }
    if (splitMarkdownTableRow(line).length > 0 && i + 1 < lines.length && isMarkdownTableSeparator(lines[i + 1])) {
      const bodyLines = [];
      let j = i + 2;
      for (; j < lines.length; j += 1) {
        if (!lines[j].trim() || !lines[j].includes("|")) break;
        bodyLines.push(lines[j]);
      }
      html.push(renderMarkdownTable(line, lines[i + 1], bodyLines));
      i = j - 1;
      continue;
    }
    if (!line.trim()) {
      html.push("<br />");
    } else if (/^\s*[-*]\s+/.test(line)) {
      html.push(`${renderInlineMarkdown(line.replace(/^\s*[-*]\s+/, "• "))}<br />`);
    } else {
      html.push(`${renderInlineMarkdown(line)}<br />`);
    }
  }
  if (inCode) {
    html.push(`<pre><code>${escapeHtml(codeLines.join("\n"))}</code></pre>`);
  }
  return html.join("\n").replace(/(<br \/>\n?)+$/, "");
}

function matrixFormattedBody(text) {
  const renderer = loadMarkdownRenderer();
  if (renderer) {
    return renderer.render(String(text || "")).replace(/\n$/, "");
  }
  return fallbackMatrixFormattedBody(text);
}

function matrixEditFallbackHtml(text) {
  return `<p>* ${escapeHtml(text).replace(/\r?\n/g, "<br>\n")}</p>`;
}

function linkMatrixMentions(htmlBody, mentions) {
  let html = String(htmlBody || "");
  for (const mxid of mentions) {
    const encoded = encodeURIComponent(mxid);
    const display = escapeHtml(mxid.split(":")[0].replace(/^@/, "") || mxid);
    const anchor = `<a href="https://matrix.to/#/${encoded}">${display}</a>`;
    const escapedMxid = escapeHtml(mxid);
    if (html.includes(escapedMxid)) html = html.replace(escapedMxid, anchor);
    else if (!html.includes(anchor)) html = `${anchor} ${html}`.trim();
  }
  return html;
}

function applyMatrixMentions(content, text, runtimeState) {
  const selfId = String(runtimeState?.runtime?.member?.matrixUserId || "");
  const mentions = extractMatrixMentionsFromText(text).filter(mxid => mxid && mxid !== selfId);
  if (!mentions.length) return;
  content["m.mentions"] = { user_ids: mentions };
  if (content.formatted_body) {
    content.format = "org.matrix.custom.html";
    content.formatted_body = linkMatrixMentions(content.formatted_body, mentions);
  }
}

async function matrixSendMessage(edge, runtimeState, roomId, body, options) {
  const runtime = runtimeState.runtime || {};
  const token = runtime.matrix && runtime.matrix.accessToken;
  if (!token) {
    throw new Error("runtime.yaml missing matrix.accessToken");
  }
  const msgtype = options && options.msgtype ? options.msgtype : "m.text";
  const content = { msgtype, body };
  if (options && options.formatted !== false && (msgtype === "m.text" || msgtype === "m.notice")) {
    content.format = "org.matrix.custom.html";
    content.formatted_body = options.formattedBody || matrixFormattedBody(body);
  }
  applyMatrixMentions(content, body, runtimeState);
  if (options && options.threadRootEventId) {
    content["m.relates_to"] = {
      rel_type: "m.thread",
      event_id: options.threadRootEventId,
      is_falling_back: false
    };
  }
  if (options && options.replaceEventId) {
    content.body = `* ${body}`;
    const newContent = { msgtype, body };
    if (options.formatted !== false && (msgtype === "m.text" || msgtype === "m.notice")) {
      newContent.format = "org.matrix.custom.html";
      newContent.formatted_body = options.formattedBody || matrixFormattedBody(body);
      content.format = "org.matrix.custom.html";
      content.formatted_body = matrixEditFallbackHtml(body);
    }
    applyMatrixMentions(newContent, body, runtimeState);
    applyMatrixMentions(content, body, runtimeState);
    content["m.new_content"] = newContent;
    content["m.relates_to"] = {
      rel_type: "m.replace",
      event_id: options.replaceEventId
    };
  }
  const roomPath = encodeURIComponent(roomId);
  const eventType = encodeURIComponent("m.room.message");
  return matrixRequest(
    "PUT",
    edge.matrixHomeserver,
    token,
    `/_matrix/client/v3/rooms/${roomPath}/send/${eventType}/${txnId("m")}`,
    content
  );
}

async function matrixRedactMessage(edge, runtimeState, roomId, eventId, reason) {
  const runtime = runtimeState.runtime || {};
  const token = runtime.matrix && runtime.matrix.accessToken;
  if (!token || !eventId) return {};
  const body = reason ? { reason } : {};
  return matrixRequest(
    "PUT",
    edge.matrixHomeserver,
    token,
    `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/redact/${encodeURIComponent(eventId)}/${txnId("redact")}`,
    body
  );
}

async function matrixJoin(edge, runtimeState, roomId) {
  const token = runtimeState.runtime?.matrix?.accessToken;
  if (!token) {
    return;
  }
  await matrixRequest("POST", edge.matrixHomeserver, token, `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/join`, {});
}

async function matrixTyping(edge, runtimeState, roomId, typing) {
  const token = runtimeState.runtime?.matrix?.accessToken;
  const userId = runtimeState.runtime?.member?.matrixUserId;
  if (!token || !userId) {
    return;
  }
  await matrixRequest(
    "PUT",
    edge.matrixHomeserver,
    token,
    `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/typing/${encodeURIComponent(userId)}`,
    { typing: Boolean(typing), timeout: typing ? 30000 : 0 }
  );
}

function eventBody(event) {
  const content = event && event.content && typeof event.content === "object" ? event.content : {};
  const replacement = content["m.new_content"] && typeof content["m.new_content"] === "object" ? content["m.new_content"] : null;
  return String((replacement && replacement.body) || content.body || "");
}

function shouldHandleEvent(event, roomId, edge, runtimeState) {
  if (!event || event.type !== "m.room.message") return false;
  if (isMatrixThreadEvent(event)) return false;
  const runtime = runtimeState.runtime || {};
  const member = runtime.member || {};
  const sender = String(event.sender || "");
  if (sender && sender === String(member.matrixUserId || "")) return false;
  const body = eventBody(event);
  if (!body.trim()) return false;
  const personal = personalRoomId(runtime);
  if (personal && roomId === personal) return true;
  return eventMentionsUser(event, member.matrixUserId);
}

function wait(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function matrixSyncRetryDelayMs(args, failures) {
  const base = Number(args.matrixReconnectBaseMs);
  const max = Number(args.matrixReconnectMaxMs);
  const baseMs = Number.isFinite(base) && base >= 0 ? base : 1000;
  const maxMs = Number.isFinite(max) && max >= 0 ? max : 30000;
  return Math.min(maxMs, baseMs * (2 ** Math.min(Math.max(failures - 1, 0), 5)));
}

function isNoReplyText(text) {
  return String(text ?? "").trim() === "NO_REPLY";
}

function controlMentionNames(edge, runtimeState) {
  const runtime = runtimeState?.runtime || {};
  const member = runtime.member || {};
  const values = [
    member.name,
    member.runtimeName,
    member.matrixUserId,
    String(member.matrixUserId || "").split(":")[0],
    String(member.matrixUserId || "").split(":")[0].replace(/^@/, ""),
    edge?.workerName,
    edge?.runtimeName
  ];
  return Array.from(new Set(values.map(value => String(value || "").trim()).filter(Boolean)));
}

function stripCurrentWorkerMentionText(body, edge, runtimeState) {
  let text = String(body || "");
  const names = controlMentionNames(edge, runtimeState).sort((a, b) => b.length - a.length);
  for (const name of names) {
    const variants = (name.startsWith("@") ? [name, name.slice(1)] : [name, `@${name}`]).sort((a, b) => b.length - a.length);
    for (const variant of variants) {
      if (!variant) continue;
      text = text.replace(new RegExp(`(^|\\s)${escapeRegExp(variant)}(?=$|\\s|[,:;，：])`, "g"), " ");
    }
  }
  return text.replace(/[,:;，：]/g, " ").replace(/\s+/g, " ").trim();
}

function matrixControlCommand(event, roomId, edge, runtimeState) {
  const body = eventBody(event).trim();
  if (!roomId || !runtimeState) {
    if (body === "/stop" || body === "/clear") return body;
    return "";
  }
  const runtime = runtimeState.runtime || {};
  const personal = personalRoomId(runtime);
  if (personal && roomId === personal) {
    if (body === "/stop" || body === "/clear") return body;
    return "";
  }
  if (roomId !== personal && eventMentionsUser(event, runtime.member?.matrixUserId)) {
    const stripped = stripCurrentWorkerMentionText(body, edge, runtimeState);
    if (stripped === "/stop" || stripped === "/clear") return stripped;
  }
  return "";
}

function createTaskAbortController() {
  if (typeof AbortController === "function") return new AbortController();
  const signal = {
    aborted: false,
    addEventListener: () => {},
    removeEventListener: () => {}
  };
  return {
    signal,
    abort: () => {
      signal.aborted = true;
    }
  };
}

function sessionsFor(opts, matrixState) {
  return typeof opts.sessionStore === "function" ? opts.sessionStore(matrixState) : {};
}

function sessionForRoom(opts, sessions, roomId, args, runtimeState, edge) {
  return typeof opts.sessionForRoom === "function" ? opts.sessionForRoom(sessions, roomId, args, runtimeState, edge) : sessions[roomId] || "";
}

function storeSessionForRoom(opts, sessions, roomId, args, sessionId, runtimeState, edge) {
  if (typeof opts.storeSessionForRoom === "function") {
    opts.storeSessionForRoom(sessions, roomId, args, sessionId, runtimeState, edge);
  } else if (sessionId) {
    sessions[roomId] = sessionId;
  }
}

function dropSessionForRoom(opts, sessions, roomId, args, runtimeState, edge) {
  if (typeof opts.dropSessionForRoom === "function") {
    opts.dropSessionForRoom(sessions, roomId, args, runtimeState, edge);
  } else {
    delete sessions[roomId];
  }
}

function ensureRoomState(matrixState, roomId) {
  matrixState.rooms = matrixState.rooms && typeof matrixState.rooms === "object" && !Array.isArray(matrixState.rooms) ? matrixState.rooms : {};
  if (!matrixState.rooms[roomId]) {
    matrixState.rooms[roomId] = { history: [], pendingTurn: null, runningTurn: null, pausedByStop: false };
  }
  const room = matrixState.rooms[roomId];
  room.history = Array.isArray(room.history) ? room.history : [];
  room.pendingTurn = room.pendingTurn || null;
  room.runningTurn = room.runningTurn || null;
  room.pausedByStop = Boolean(room.pausedByStop);
  return room;
}

function enqueuePendingRoom(matrixState, roomId) {
  matrixState.scheduler = matrixState.scheduler && typeof matrixState.scheduler === "object" ? matrixState.scheduler : {};
  const queue = Array.isArray(matrixState.scheduler.pendingRoomQueue) ? matrixState.scheduler.pendingRoomQueue : [];
  if (!queue.includes(roomId)) queue.push(roomId);
  matrixState.scheduler.pendingRoomQueue = queue;
}

function trimSeenEventIds(matrixState, protectedIds) {
  const protectedSet = new Set((protectedIds || []).map(String).filter(Boolean));
  const seen = Array.isArray(matrixState.seenEventIds) ? matrixState.seenEventIds : [];
  const keep = [];
  for (let index = seen.length - 1; index >= 0; index -= 1) {
    const eventId = String(seen[index] || "");
    if (!eventId) continue;
    if (protectedSet.has(eventId) || keep.length < 200) keep.unshift(eventId);
  }
  matrixState.seenEventIds = Array.from(new Set(keep));
}

function addSeenEventId(matrixState, eventId, protectedIds) {
  const value = String(eventId || "");
  if (!value) return;
  const seen = Array.isArray(matrixState.seenEventIds) ? matrixState.seenEventIds : [];
  if (!seen.includes(value)) seen.push(value);
  matrixState.seenEventIds = seen;
  trimSeenEventIds(matrixState, protectedIds);
}

function turnId(prefix) {
  return `${prefix}-${Date.now().toString(36)}-${crypto.randomBytes(4).toString("hex")}`;
}

function messageFromEvent(event) {
  return {
    eventId: String(event?.event_id || ""),
    event_id: String(event?.event_id || ""),
    replacesEventId: matrixReplaceTargetEventId(event),
    sender: String(event?.sender || ""),
    body: eventBody(event),
    type: event?.type || "m.room.message",
    content: event?.content && typeof event.content === "object" ? event.content : { body: eventBody(event) },
    originServerTs: event?.origin_server_ts || event?.originServerTs || 0
  };
}

function appendRoomHistory(room, event) {
  const body = eventBody(event);
  const eventId = String(event?.event_id || "");
  const targetEventId = matrixReplaceTargetEventId(event);
  if (targetEventId) {
    const existing = room.history.find(item => item && (item.eventId === targetEventId || item.replacesEventId === targetEventId));
    if (existing) {
      existing.sender = event.sender || existing.sender || "";
      existing.body = body;
      existing.replacedByEventId = eventId;
      return;
    }
  }
  room.history.push({ sender: event.sender || "", body, eventId: targetEventId || eventId, replacesEventId: targetEventId || undefined });
  room.history = room.history.slice(-50);
}

function eventFromMessage(message) {
  const eventId = String(message?.eventId || message?.event_id || "");
  return {
    type: message?.type || "m.room.message",
    event_id: eventId,
    sender: String(message?.sender || ""),
    origin_server_ts: message?.originServerTs || message?.origin_server_ts || 0,
    content: message?.content && typeof message.content === "object" ? message.content : { body: String(message?.body || "") }
  };
}

function appendPendingMessage(matrixState, roomId, event) {
  const room = ensureRoomState(matrixState, roomId);
  const message = messageFromEvent(event);
  const now = new Date().toISOString();
  if (!room.pendingTurn) {
    room.pendingTurn = {
      turnId: turnId("turn"),
      eventIds: [],
      messages: [],
      history: room.history.slice(-50),
      createdAt: now,
      updatedAt: now
    };
    room.history = [];
  }
  if (message.eventId && !room.pendingTurn.eventIds.includes(message.eventId)) {
    room.pendingTurn.eventIds.push(message.eventId);
  }
  room.pendingTurn.messages.push(message);
  room.pendingTurn.updatedAt = now;
  room.pausedByStop = false;
  enqueuePendingRoom(matrixState, roomId);
}

function mergePendingTurns(restored, pending) {
  if (!pending) return restored;
  return {
    ...restored,
    eventIds: [...(restored.eventIds || []), ...(pending.eventIds || [])],
    messages: [...(restored.messages || []), ...(pending.messages || [])],
    history: [...(restored.history || []), ...(pending.history || [])].slice(-50),
    updatedAt: new Date().toISOString()
  };
}

function countRunningRooms(matrixState) {
  const rooms = matrixState.rooms && typeof matrixState.rooms === "object" ? matrixState.rooms : {};
  return Object.values(rooms).filter(room => room && room.runningTurn).length;
}

function resolveMaxConcurrentTasks(args, runtimeState) {
  const desired = runtimeState?.runtime?.desired?.scheduler?.maxConcurrentTasks;
  if (args.maxConcurrentTasks !== undefined && args.maxConcurrentTasks !== null) {
    return normalizeMaxConcurrentTasks(args.maxConcurrentTasks, 2);
  }
  return normalizeMaxConcurrentTasks(desired, 2);
}

function normalizeRunResult(result) {
  if (result && typeof result === "object" && !Array.isArray(result)) {
    return result;
  }
  return { sessionId: result || "" };
}

function createStateMutationQueue(args, matrixState) {
  let chain = Promise.resolve();
  return function withStateMutation(fn) {
    const run = chain.then(async () => {
      const result = await fn(matrixState);
      writeMatrixState(args, matrixState);
      return result;
    });
    chain = run.catch(() => {});
    return run;
  };
}

async function runTurnAdapter(opts, args, edge, runtimeState, turn) {
  if (typeof opts.runForTurn === "function") {
    return normalizeRunResult(await opts.runForTurn(args, edge, runtimeState, turn));
  }
  const messages = Array.isArray(turn.messages) ? turn.messages : [];
  const event = turn.event || eventFromMessage(messages[messages.length - 1] || {});
  return normalizeRunResult(await opts.runForEvent(args, edge, runtimeState, turn.roomId, event, turn.history || [], turn.sessionId || "", {
    placeholderEventId: turn.placeholderEventId || "",
    abortSignal: turn.abortSignal,
    turn
  }));
}

function recoverRoomState(matrixState, roomId, room, opts, args, runtimeState, edge) {
  const running = room.runningTurn;
  if (!running) {
    if (room.pendingTurn && !room.pausedByStop) enqueuePendingRoom(matrixState, roomId);
    return;
  }
  const status = String(running.status || "");
  if (["starting", "running", "finishing"].includes(status)) {
    const restored = {
      turnId: running.turnId || turnId("turn"),
      eventIds: Array.isArray(running.eventIds) ? running.eventIds : [],
      messages: Array.isArray(running.messages) ? running.messages : [],
      history: Array.isArray(running.history) ? running.history : [],
      sessionId: running.sessionId || "",
      placeholderEventId: running.placeholderEventId || "",
      attempt: Number(running.attempt || 0) + 1,
      createdAt: running.createdAt || running.startedAt || new Date().toISOString(),
      updatedAt: new Date().toISOString()
    };
    room.pendingTurn = mergePendingTurns(restored, room.pendingTurn);
    room.runningTurn = null;
    if (!room.pausedByStop) enqueuePendingRoom(matrixState, roomId);
    return;
  }
  if (status === "stopping") {
    room.runningTurn = null;
    room.pausedByStop = true;
    return;
  }
  if (status === "resetting") {
    const sessions = sessionsFor(opts, matrixState);
    dropSessionForRoom(opts, sessions, roomId, args, runtimeState, edge);
    room.runningTurn = null;
    room.pendingTurn = null;
    room.history = [];
    room.pausedByStop = false;
  }
}

async function runMatrixLoop(args, edge, runtimeState, options) {
  const opts = options || {};
  const initialRuntime = runtimeState.runtime || {};
  if (!initialRuntime.matrix?.accessToken) {
    writeStatus(args, "Degraded", "MatrixAccessTokenMissing", "runtime.yaml missing matrix.accessToken");
    logEvent("warn", "matrix_access_token_missing", { runtime: args.runtime });
    return;
  }
  const matrixState = runtimeState.matrixState || loadMatrixState(args);
  runtimeState.matrixState = matrixState;
  const withStateMutation = createStateMutationQueue(args, matrixState);
  const activeControllers = new Map();
  const activePromises = new Map();
  let draining = false;
  let drainAgain = false;
  let matrixSyncFailures = 0;

  await withStateMutation(state => {
    for (const roomId of matrixRooms(initialRuntime)) ensureRoomState(state, roomId);
    for (const [roomId, room] of Object.entries(state.rooms || {})) {
      recoverRoomState(state, roomId, ensureRoomState(state, roomId), opts, args, runtimeState, edge);
    }
  });
  logEvent("info", "matrix_loop_started", {
    runtime: args.runtime,
    workerName: edge.workerName,
    rooms: matrixRooms(initialRuntime),
    maxConcurrentTasks: resolveMaxConcurrentTasks(args, runtimeState),
    hasSyncToken: Boolean(matrixState.matrixSyncToken),
    bootstrapPending: Boolean(matrixState.bootstrapPending)
  });

  for (const roomId of matrixRooms(initialRuntime)) {
    await matrixJoin(edge, runtimeState, roomId)
      .then(() => logEvent("info", "matrix_room_joined", { runtime: args.runtime, roomId }))
      .catch(error => logEvent("warn", "matrix_room_join_failed", { runtime: args.runtime, roomId, error }));
  }

  async function applyControlCommand(command, roomId) {
    let abortController = null;
    await withStateMutation(state => {
      const room = ensureRoomState(state, roomId);
      const running = room.runningTurn;
      if (command === "/stop") {
        room.pausedByStop = true;
        if (running && running.status !== "finishing") {
          running.status = "stopping";
          abortController = activeControllers.get(roomId)?.controller || null;
        }
      } else if (command === "/clear") {
        room.pendingTurn = null;
        room.history = [];
        room.pausedByStop = false;
        const sessions = sessionsFor(opts, state);
        dropSessionForRoom(opts, sessions, roomId, args, runtimeState, edge);
        if (running) {
          if (running.status === "finishing") {
            running.dropSessionOnFinish = true;
            running.sessionDroppedOnFinish = true;
          } else {
            running.status = "resetting";
            abortController = activeControllers.get(roomId)?.controller || null;
          }
        }
      }
    });
    if (abortController) abortController.abort();
    logEvent("info", "matrix_control_command_applied", {
      runtime: args.runtime,
      roomId,
      command,
      abortedRunningTask: Boolean(abortController)
    });
    const body = command === "/stop"
      ? "已停止当前任务，保留当前会话。"
      : "已停止当前任务并清空上下文，下一次消息会开启新会话。";
    await matrixSendMessage(edge, runtimeState, roomId, body, { msgtype: "m.notice", formatted: false }).catch(() => {});
    writeStatus(args, "Running", command === "/stop" ? "MatrixTaskStopped" : "MatrixSessionCleared", body);
  }

  async function finishStoppedOrResetting(roomId, turnId) {
    await withStateMutation(state => {
      const room = ensureRoomState(state, roomId);
      const running = room.runningTurn;
      if (!running || running.turnId !== turnId) return;
      if (running.status === "stopping") {
        room.runningTurn = null;
        room.pausedByStop = true;
      } else if (running.status === "resetting") {
        room.runningTurn = null;
        room.pendingTurn = null;
        room.history = [];
        room.pausedByStop = false;
      }
    });
  }

  async function finishRoomTurn(roomId, turnId, result, error) {
    let finishing = null;
    await withStateMutation(state => {
      const room = ensureRoomState(state, roomId);
      const running = room.runningTurn;
      if (!running || running.turnId !== turnId) return;
      if (running.status === "stopping" || running.status === "resetting") return;
      running.status = "finishing";
      finishing = { ...running };
    });
    if (!finishing) {
      await finishStoppedOrResetting(roomId, turnId);
      return;
    }

    let sendFailed = false;
    const normalized = normalizeRunResult(result || {});
    const finalError = error && error.name !== "AbortError" ? error : null;
    try {
      if (finalError) {
        const body = `${opts.failurePrefix || "Runtime 执行失败："}${finalError.message}`;
        const sendOptions = finishing.placeholderEventId
          ? { msgtype: "m.notice", replaceEventId: finishing.placeholderEventId, formatted: false }
          : { msgtype: "m.notice", formatted: false };
        await matrixSendMessage(edge, runtimeState, roomId, body, sendOptions);
      } else {
        const hasText = Object.prototype.hasOwnProperty.call(normalized, "text") || Object.prototype.hasOwnProperty.call(normalized, "replyText");
        if (hasText) {
          const replyText = String(normalized.text ?? normalized.replyText ?? "");
          if (isNoReplyText(replyText)) {
            if (finishing.placeholderEventId) {
              await matrixSendMessage(edge, runtimeState, roomId, "已处理", {
                msgtype: "m.notice",
                replaceEventId: finishing.placeholderEventId,
                formatted: false
              });
            }
          } else {
            await matrixSendMessage(edge, runtimeState, roomId, replyText, {
              msgtype: normalized.msgtype || (normalized.notice ? "m.notice" : "m.text"),
              replaceEventId: finishing.placeholderEventId,
              formatted: normalized.formatted !== false
            });
          }
        }
      }
    } catch (_) {
      sendFailed = true;
    }
    logEvent(finalError || sendFailed ? "warn" : "info", "matrix_turn_finished", {
      runtime: args.runtime,
      roomId,
      turnId,
      eventIds: finishing.eventIds || [],
      status: finalError ? "failed" : (sendFailed ? "reply_failed" : "completed"),
      noReply: isNoReplyText(String(normalized.text ?? normalized.replyText ?? "")),
      hasReplyText: Object.prototype.hasOwnProperty.call(normalized, "text") || Object.prototype.hasOwnProperty.call(normalized, "replyText"),
      error: finalError || undefined
    });

    await withStateMutation(state => {
      const room = ensureRoomState(state, roomId);
      const running = room.runningTurn;
      if (!running || running.turnId !== turnId) return;
      const sessions = sessionsFor(opts, state);
      const nextSessionId = normalized.sessionId || normalized.nextSessionId || "";
      const resetAt = String(runtimeState.sessionResetAt || "");
      const packageResetDuringTurn = Boolean(resetAt && running.startedAt && resetAt > String(running.startedAt) && running.packageSessionResetAt !== resetAt);
      const forceDropOnly = Boolean(finalError || running.dropSessionOnFinish || sendFailed || packageResetDuringTurn);
      if ((forceDropOnly && !running.sessionDroppedOnFinish) || normalized.dropSession) {
        dropSessionForRoom(opts, sessions, roomId, args, runtimeState, edge);
      }
      if (!forceDropOnly && nextSessionId) {
        storeSessionForRoom(opts, sessions, roomId, args, nextSessionId, runtimeState, edge);
      }
      room.runningTurn = null;
      if (room.pendingTurn && !room.pausedByStop) enqueuePendingRoom(state, roomId);
    });
  }

  async function runRoomTurn(roomId, startedTurn) {
    const turnIdValue = startedTurn.turnId;
    const controller = createTaskAbortController();
    let placeholderEventId = startedTurn.placeholderEventId || "";
    logEvent("info", "matrix_turn_starting", {
      runtime: args.runtime,
      roomId,
      turnId: turnIdValue,
      eventIds: startedTurn.eventIds || [],
      attempt: startedTurn.attempt || 0,
      hasSession: Boolean(startedTurn.sessionId)
    });
    try {
      const sessionResetBefore = String(runtimeState.sessionResetAt || "");
      if (typeof opts.prepareRuntimeForTask === "function") {
        await opts.prepareRuntimeForTask(args, edge, runtimeState).catch(error => {
          writeStatus(args, "Degraded", "RuntimeRefreshFailed", error.message);
        });
      }
      if (String(runtimeState.sessionResetAt || "") && String(runtimeState.sessionResetAt || "") !== sessionResetBefore) {
        const resetAt = String(runtimeState.sessionResetAt || "");
        await withStateMutation(state => {
          const room = ensureRoomState(state, roomId);
          const running = room.runningTurn;
          if (!running || running.turnId !== turnIdValue || running.status !== "starting") return;
          const sessions = sessionsFor(opts, state);
          for (const key of Object.keys(sessions)) delete sessions[key];
          running.sessionId = sessionForRoom(opts, sessions, roomId, args, runtimeState, edge);
          running.packageSessionResetAt = resetAt;
        });
      }
      await matrixTyping(edge, runtimeState, roomId, true).catch(() => {});
      if (!placeholderEventId) {
        const placeholder = await matrixSendMessage(edge, runtimeState, roomId, "处理中...", { msgtype: "m.notice", formatted: false });
        placeholderEventId = placeholder.event_id || "";
      }

      let runnableTurn = null;
      await withStateMutation(state => {
        const room = ensureRoomState(state, roomId);
        const running = room.runningTurn;
        if (!running || running.turnId !== turnIdValue || running.status !== "starting") return;
        running.placeholderEventId = placeholderEventId;
        running.status = "running";
        activeControllers.set(roomId, { turnId: turnIdValue, controller });
        runnableTurn = { ...running, roomId, abortSignal: controller.signal };
      });
      if (!runnableTurn) {
        logEvent("warn", "matrix_turn_not_runnable", {
          runtime: args.runtime,
          roomId,
          turnId: turnIdValue
        });
        await finishStoppedOrResetting(roomId, turnIdValue);
        return;
      }
      if (controller.signal.aborted) {
        logEvent("info", "matrix_turn_aborted_before_run", {
          runtime: args.runtime,
          roomId,
          turnId: turnIdValue
        });
        await finishStoppedOrResetting(roomId, turnIdValue);
        return;
      }
      logEvent("info", "matrix_turn_running", {
        runtime: args.runtime,
        roomId,
        turnId: turnIdValue,
        eventIds: runnableTurn.eventIds || [],
        sessionId: runnableTurn.sessionId || ""
      });
      const result = await runTurnAdapter(opts, args, edge, runtimeState, runnableTurn);
      await finishRoomTurn(roomId, turnIdValue, result, null);
    } catch (error) {
      const stopped = controller.signal.aborted || error?.name === "AbortError";
      logEvent(stopped ? "info" : "error", stopped ? "matrix_turn_stopped" : "matrix_turn_failed", {
        runtime: args.runtime,
        roomId,
        turnId: turnIdValue,
        eventIds: startedTurn.eventIds || [],
        error: stopped ? undefined : error
      });
      if (stopped) {
        await finishStoppedOrResetting(roomId, turnIdValue);
      } else {
        await finishRoomTurn(roomId, turnIdValue, null, error);
      }
    } finally {
      await matrixTyping(edge, runtimeState, roomId, false).catch(() => {});
      const active = activeControllers.get(roomId);
      if (active && active.turnId === turnIdValue) activeControllers.delete(roomId);
      activePromises.delete(roomId);
      await drainScheduler().catch(error => {
        writeStatus(args, "Degraded", "MatrixSchedulerDrainFailed", error.message);
      });
    }
  }

  async function drainScheduler() {
    if (draining) {
      drainAgain = true;
      return;
    }
    draining = true;
    try {
      do {
        drainAgain = false;
        for (;;) {
          let start = null;
          await withStateMutation(state => {
            const maxConcurrentTasks = resolveMaxConcurrentTasks(args, runtimeState);
            if (countRunningRooms(state) >= maxConcurrentTasks) return;
            const queue = Array.isArray(state.scheduler?.pendingRoomQueue) ? state.scheduler.pendingRoomQueue : [];
            state.scheduler = state.scheduler && typeof state.scheduler === "object" ? state.scheduler : {};
            state.scheduler.pendingRoomQueue = [];
            while (queue.length) {
              const roomId = String(queue.shift() || "");
              if (!roomId) continue;
              const room = ensureRoomState(state, roomId);
              if (!room.pendingTurn || room.runningTurn || room.pausedByStop) continue;
              const sessions = sessionsFor(opts, state);
              const pending = room.pendingTurn;
              const sessionId = pending.sessionId || sessionForRoom(opts, sessions, roomId, args, runtimeState, edge);
              room.pendingTurn = null;
              room.runningTurn = {
                ...pending,
                sessionId,
                status: "starting",
                attempt: Number(pending.attempt || 0) + 1,
                startedAt: new Date().toISOString()
              };
              start = { roomId, turn: { ...room.runningTurn } };
              break;
            }
            for (const roomId of queue) {
              if (roomId && !state.scheduler.pendingRoomQueue.includes(roomId)) {
                state.scheduler.pendingRoomQueue.push(roomId);
              }
            }
          });
          if (!start) break;
          logEvent("info", "matrix_scheduler_dispatch", {
            runtime: args.runtime,
            roomId: start.roomId,
            turnId: start.turn.turnId,
            eventIds: start.turn.eventIds || [],
            activeRooms: activePromises.size,
            maxConcurrentTasks: resolveMaxConcurrentTasks(args, runtimeState)
          });
          const promise = runRoomTurn(start.roomId, start.turn);
          activePromises.set(start.roomId, promise);
        }
      } while (drainAgain);
    } finally {
      draining = false;
    }
  }

  async function processMatrixEvent(roomId, event, protectedIds, bootstrap) {
    const eventId = String(event.event_id || "");
    let controlToApply = "";
    let eventAction = "";
    await withStateMutation(state => {
      const runtime = runtimeState.runtime || {};
      const room = ensureRoomState(state, roomId);
      if (eventId && Array.isArray(state.seenEventIds) && state.seenEventIds.includes(eventId)) {
        state.matrixCursors[roomId] = eventId;
        return;
      }
      const body = eventBody(event);
      const currentPersonalRoomId = personalRoomId(runtime);
      const isGroupRoom = Boolean(!currentPersonalRoomId || roomId !== currentPersonalRoomId);
      if (!bootstrap && event && event.type === "m.room.message" && !isMatrixThreadEvent(event)) {
        const command = matrixControlCommand(event, roomId, edge, runtimeState);
        if (command) {
          controlToApply = command;
          eventAction = "control";
        } else if (shouldHandleEvent(event, roomId, edge, runtimeState)) {
          appendPendingMessage(state, roomId, event);
          eventAction = "queued";
        } else if (isGroupRoom && body.trim() && event.sender !== runtime.member?.matrixUserId) {
          appendRoomHistory(room, event);
        }
      } else if (bootstrap && isGroupRoom && body.trim() && event.sender !== runtime.member?.matrixUserId && !isMatrixThreadEvent(event)) {
        appendRoomHistory(room, event);
      }
      if (eventId) {
        state.matrixCursors[roomId] = eventId;
        addSeenEventId(state, eventId, protectedIds);
      }
    });

    if (eventAction) {
      logEvent("info", "matrix_event_processed", {
        runtime: args.runtime,
        roomId,
        eventId,
        sender: event.sender || "",
        action: eventAction,
        controlCommand: controlToApply,
        bootstrap: Boolean(bootstrap),
        isReplace: isMatrixReplaceEvent(event),
        isThread: isMatrixThreadEvent(event)
      });
    }
    if (controlToApply) await applyControlCommand(controlToApply, roomId);
  }

  async function waitForIdleOnce() {
    for (;;) {
      await drainScheduler();
      const promises = Array.from(activePromises.values());
      if (!promises.length) return;
      await Promise.allSettled(promises);
    }
  }

  for (;;) {
    const runtime = runtimeState.runtime || {};
    const token = runtime.matrix && runtime.matrix.accessToken;
    if (!token) {
      writeStatus(args, "Degraded", "MatrixAccessTokenMissing", "runtime.yaml missing matrix.accessToken");
      logEvent("warn", "matrix_access_token_missing", { runtime: args.runtime });
      await wait(Math.max(1, args.intervalSeconds) * 1000);
      continue;
    }
    const filter = encodeURIComponent(JSON.stringify({ room: { timeline: { limit: 50 } } }));
    const since = matrixState.matrixSyncToken || "";
    const query = since ? `timeout=30000&since=${encodeURIComponent(since)}&filter=${filter}` : `timeout=30000&filter=${filter}`;
    let sync;
    try {
      sync = await matrixRequest("GET", edge.matrixHomeserver, token, `/_matrix/client/v3/sync?${query}`);
      if (matrixSyncFailures > 0) {
        writeStatus(args, "Running", "MatrixReconnected", `Matrix sync recovered after ${matrixSyncFailures} failed attempt(s)`, {
          failureCount: matrixSyncFailures
        });
        logEvent("info", "matrix_sync_reconnected", {
          runtime: args.runtime,
          failureCount: matrixSyncFailures
        });
        matrixSyncFailures = 0;
      }
    } catch (error) {
      matrixSyncFailures += 1;
      const retryDelayMs = matrixSyncRetryDelayMs(args, matrixSyncFailures);
      writeStatus(args, "Degraded", "MatrixSyncFailed", `${error.message}; retrying in ${Math.round(retryDelayMs / 1000)}s`, {
        failureCount: matrixSyncFailures,
        retryDelayMs
      });
      logEvent("warn", "matrix_sync_failed", {
        runtime: args.runtime,
        failureCount: matrixSyncFailures,
        retryDelayMs,
        error
      });
      await wait(retryDelayMs);
      continue;
    }
    const bootstrap = Boolean(matrixState.bootstrapPending);

    const invites = sync.rooms && sync.rooms.invite ? sync.rooms.invite : {};
    for (const roomId of Object.keys(invites)) {
      await matrixJoin(edge, runtimeState, roomId).catch(error => {
        writeStatus(args, "Degraded", "MatrixInviteJoinFailed", error.message);
        logEvent("warn", "matrix_invite_join_failed", {
          runtime: args.runtime,
          roomId,
          error
        });
      });
      await withStateMutation(state => ensureRoomState(state, roomId));
    }

    const joined = sync.rooms && sync.rooms.join ? sync.rooms.join : {};
    const protectedIds = [];
    for (const room of Object.values(joined)) {
      const events = room.timeline && Array.isArray(room.timeline.events) ? room.timeline.events : [];
      for (const event of events) {
        if (event?.event_id) protectedIds.push(String(event.event_id));
      }
    }
    for (const [roomId, room] of Object.entries(joined)) {
      await withStateMutation(state => {
        for (const configuredRoom of matrixRooms(runtime)) ensureRoomState(state, configuredRoom);
        ensureRoomState(state, roomId);
      });
      const events = room.timeline && Array.isArray(room.timeline.events) ? room.timeline.events : [];
      const hasSyncCheckpoint = Boolean(since);
      const lastEventId = String(matrixState.matrixCursors[roomId] || "");
      let unread = hasSyncCheckpoint || bootstrap ? events : messagesAfter(events, lastEventId);
      if (!hasSyncCheckpoint && !lastEventId && !bootstrap) {
        unread = unread.slice(-50);
      }
      for (const event of unread) {
        await processMatrixEvent(roomId, event, protectedIds, bootstrap);
      }
    }
    await withStateMutation(state => {
      state.matrixSyncToken = sync.next_batch || since;
      state.nextBatch = state.matrixSyncToken;
      state.bootstrapPending = false;
      trimSeenEventIds(state, []);
    });
    await drainScheduler();
    if (process.env.TEAMHARNESS_MATRIX_ONCE === "1" || args.once) {
      await waitForIdleOnce();
      return;
    }
  }
}

module.exports = {
  escapeRegExp,
  containsNameMention,
  isMatrixThreadEvent,
  isMatrixReplaceEvent,
  eventMentionsUser,
  messagesAfter,
  loadMatrixState,
  writeMatrixState,
  matrixRequest,
  txnId,
  escapeHtml,
  renderInlineMarkdown,
  matrixFormattedBody,
  matrixEditFallbackHtml,
  matrixSendMessage,
  matrixRedactMessage,
  matrixJoin,
  matrixTyping,
  eventBody,
  shouldHandleEvent,
  isNoReplyText,
  matrixSyncRetryDelayMs,
  stripCurrentWorkerMentionText,
  matrixControlCommand,
  createStateMutationQueue,
  resolveMaxConcurrentTasks,
  runMatrixLoop
};
