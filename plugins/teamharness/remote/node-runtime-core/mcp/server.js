#!/usr/bin/env node
"use strict";

const fs = require("fs");
const crypto = require("crypto");
const path = require("path");

const DEFAULT_OPTIONS = {
  runtime: "remote-node",
  serverName: "teamharness-remote-node",
  version: "0.1.0-node",
  healthDescription: "Check TeamHarness MCP server availability and remote-managed Node runtime wiring."
};

let runtimeOptions = { ...DEFAULT_OPTIONS };

const MATRIX_ROOM_RE = /^![^:\s]+:[^\s]+$/;
const MATRIX_USER_RE = /@[A-Za-z0-9._=\-/+]+:[A-Za-z0-9.\-]+/g;
const SHORT_MATRIX_MENTION_RE = /(?<![A-Za-z0-9._=\-/+])@([A-Za-z0-9._=\-/+]+)(?![A-Za-z0-9._=\-/+]*:)/g;
const LOW_INFORMATION_ACKS = new Set(["ok", "好的", "收到", "已收到", "done", "thanks", "谢谢"]);
const MESSAGE_TOOL_BLOCKED_ROLES = new Set(["worker", "remote-member"]);
const ROOMFLOW_LEADER_ACTIONS = new Set(["create_task_room", "archive_room"]);
const PROJECTFLOW_LEADER_ACTIONS = new Set([
  "create_project",
  "create_quick_project",
  "accept_task_result",
  "mark_requester_report_sent",
  "plan_dag",
  "plan_loop",
  "record_loop_iteration",
  "pause_project",
  "resume_project",
  "complete_project"
]);

const TOOL_SCHEMAS = {
  health: {
    description: DEFAULT_OPTIONS.healthDescription,
    inputSchema: { type: "object", properties: {}, additionalProperties: true }
  },
  message: {
    description: "Send a cross-room TeamHarness message. Do not use for normal replies in the current room.",
    inputSchema: {
      type: "object",
      properties: {
        action: { type: "string", enum: ["send"] },
        channel: { type: "string", enum: ["matrix"] },
        target: { type: "string" },
        roomId: { type: "string" },
        text: { type: "string" },
        message: { type: "string" },
        body: { type: "string" },
        dryRun: { type: "boolean" }
      },
      additionalProperties: true
    }
  },
  roomflow: {
    description: "Manage TeamHarness task rooms.",
    inputSchema: {
      type: "object",
      properties: {
        action: { type: "string", enum: ["describe_room", "create_task_room", "list_rooms", "archive_room"] },
        payload: { type: "object" },
        taskId: { type: "string" },
        projectId: { type: "string" },
        name: { type: "string" },
        roomId: { type: "string" },
        sessionId: { type: "string" },
        target: { type: "string" },
        invite: { type: ["array", "string"] },
        dryRun: { type: "boolean" }
      },
      additionalProperties: true
    }
  },
  filesync: {
    description: "List, stat, pull, or push TeamHarness shared artifacts through worker-provided storage credentials.",
    inputSchema: {
      type: "object",
      properties: {
        action: { type: "string", enum: ["list", "stat", "pull", "push"] },
        path: { type: "string" },
        kind: { type: "string", enum: ["shared", "global-shared", "member"] },
        dryRun: { type: "boolean" }
      },
      required: ["action"],
      additionalProperties: true
    }
  },
  projectflow: {
    description: "Manage TeamHarness project state files.",
    inputSchema: {
      type: "object",
      properties: {
        action: {
          type: "string",
          enum: [
            "create_project",
            "create_quick_project",
            "resolve_project",
            "accept_task_result",
            "mark_requester_report_sent",
            "plan_dag",
            "plan_loop",
            "ready_nodes",
            "ready_loop_nodes",
            "record_loop_iteration",
            "pause_project",
            "resume_project",
            "complete_project"
          ]
        },
        payload: { type: "object" },
        projectId: { type: "string" },
        taskId: { type: "string" },
        workspaceDir: { type: "string" }
      },
      additionalProperties: true
    }
  },
  taskflow: {
    description: "Manage TeamHarness task lifecycle state files.",
    inputSchema: {
      type: "object",
      properties: {
        action: { type: "string", enum: ["delegate_task", "ack_task", "submit_task", "check_task"] },
        payload: { type: "object" },
        taskId: { type: "string" },
        projectId: { type: "string" },
        role: { type: "string" },
        workspaceDir: { type: "string" }
      },
      additionalProperties: true
    }
  }
};

function findDescriptor() {
  const explicit = (process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR || "").trim();
  if (explicit && fs.existsSync(explicit)) {
    return explicit;
  }
  let current = process.cwd();
  for (;;) {
    const candidate = path.join(current, ".teamharness", "credential-broker.json");
    if (fs.existsSync(candidate)) {
      return candidate;
    }
    const parent = path.dirname(current);
    if (parent === current) {
      return "";
    }
    current = parent;
  }
}

function brokerDescriptor() {
  const descriptorPath = findDescriptor();
  if (!descriptorPath) {
    return null;
  }
  try {
    const descriptor = JSON.parse(fs.readFileSync(descriptorPath, "utf8"));
    const endpoint = String(descriptor.endpoint || "").replace(/\/+$/, "");
    const tokenFile = String(descriptor.tokenFile || "");
    const token = tokenFile ? fs.readFileSync(tokenFile, "utf8").trim() : "";
    if (!endpoint || !token) {
      return null;
    }
    return { endpoint, token, descriptorPath };
  } catch {
    return null;
  }
}

async function brokerGet(pathname) {
  const descriptor = brokerDescriptor();
  if (!descriptor) {
    throw new Error("credential broker unavailable");
  }
  const response = await fetch(`${descriptor.endpoint}${pathname}`, {
    headers: { Authorization: `Bearer ${descriptor.token}`, Accept: "application/json" }
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`broker ${pathname} failed: ${response.status} ${text}`);
  }
  return text.trim() ? JSON.parse(text) : {};
}

function stripEndpoint(endpoint) {
  return String(endpoint || "").trim().replace(/\/+$/, "");
}

function encodeObjectKey(key) {
  return String(key || "")
    .split("/")
    .map(part => encodeURIComponent(part))
    .join("/");
}

function ossRequestUrl(endpoint, bucket, objectKey, query) {
  const parsed = new URL(stripEndpoint(endpoint));
  const pathStyle = parsed.hostname === "localhost" || /^\d+\.\d+\.\d+\.\d+$/.test(parsed.hostname);
  if (pathStyle) {
    parsed.pathname = `/${bucket}${objectKey ? `/${encodeObjectKey(objectKey)}` : "/"}`;
  } else if (parsed.hostname.split(".")[0] === bucket) {
    parsed.pathname = objectKey ? `/${encodeObjectKey(objectKey)}` : "/";
  } else {
    parsed.hostname = `${bucket}.${parsed.hostname}`;
    parsed.pathname = objectKey ? `/${encodeObjectKey(objectKey)}` : "/";
  }
  parsed.search = query || "";
  return parsed;
}

function canonicalizedOssResource(bucket, objectKey) {
  return `/${bucket}${objectKey ? `/${objectKey}` : "/"}`;
}

function ossAuthHeaders(method, storage, bucket, objectKey) {
  const date = new Date().toUTCString();
  const securityToken = String(storage.securityToken || storage.security_token || "");
  const canonicalHeaders = securityToken ? `x-oss-security-token:${securityToken}\n` : "";
  const stringToSign = `${method}\n\n\n${date}\n${canonicalHeaders}${canonicalizedOssResource(bucket, objectKey)}`;
  const signature = crypto
    .createHmac("sha1", String(storage.accessKeySecret || storage.access_key_secret || ""))
    .update(stringToSign)
    .digest("base64");
  const headers = {
    Date: date,
    Authorization: `OSS ${String(storage.accessKeyId || storage.access_key_id || "")}:${signature}`
  };
  if (securityToken) {
    headers["x-oss-security-token"] = securityToken;
  }
  return headers;
}

async function ossFetch(method, storage, objectKey, options) {
  const bucket = String(storage.bucket || storage.oss_bucket || "").trim();
  const endpoint = stripEndpoint(storage.endpoint || storage.oss_endpoint || "");
  if (!bucket || !endpoint) {
    throw new Error("storage credential missing endpoint or bucket");
  }
  const query = options && options.query ? options.query : "";
  const body = options && options.body;
  const url = ossRequestUrl(endpoint, bucket, objectKey, query);
  const response = await fetch(url, {
    method,
    headers: ossAuthHeaders(method, storage, bucket, objectKey),
    body
  });
  const data = Buffer.from(await response.arrayBuffer());
  if (!response.ok) {
    throw new Error(`${method} ${url} failed: ${response.status} ${data.toString("utf8")}`);
  }
  return data;
}

async function storageCredential() {
  return brokerGet("/v1/credentials/storage");
}

function trimSlashes(value) {
  return String(value || "").replace(/^\/+|\/+$/g, "");
}

function resolveFilesyncPath(args, context) {
  const requestedPath = trimSlashes(args.path || "");
  const storage = context.storage && typeof context.storage === "object" ? context.storage : {};
  let kind = String(args.kind || "").trim();
  let relative = requestedPath;
  if (!kind) {
    if (requestedPath === "global-shared" || requestedPath.startsWith("global-shared/")) {
      kind = "global-shared";
      relative = requestedPath.replace(/^global-shared\/?/, "");
    } else if (requestedPath === "member" || requestedPath.startsWith("member/")) {
      kind = "member";
      relative = requestedPath.replace(/^member\/?/, "");
    } else {
      kind = "shared";
      relative = requestedPath.replace(/^shared\/?/, "");
    }
  }
  const prefix =
    kind === "global-shared"
      ? storage.globalSharedPrefix || "shared"
      : kind === "member"
        ? storage.memberPrefix || ""
        : storage.sharedPrefix || "shared";
  const remotePath = [trimSlashes(prefix), trimSlashes(relative)].filter(Boolean).join("/");
  const workspaceDir = path.resolve(String(args.workspaceDir || process.cwd()));
  const localPath = path.resolve(workspaceDir, requestedPath || ".");
  return { kind, requestedPath, relative, remotePath, localPath };
}

function payloadArgs(args) {
  const source = args && typeof args === "object" ? args : {};
  let payload = {};
  if (source.payload && typeof source.payload === "object" && !Array.isArray(source.payload)) {
    payload = { ...source.payload };
  } else if (typeof source.payload === "string" && source.payload.trim()) {
    try {
      const decoded = JSON.parse(source.payload);
      if (decoded && typeof decoded === "object" && !Array.isArray(decoded)) payload = decoded;
    } catch {
      payload = {};
    }
  }
  const aliases = {
    projectId: ["projectId", "project_id"],
    taskId: ["taskId", "task_id"],
    roomId: ["roomId", "room_id"],
    sourceRoomId: ["sourceRoomId", "source_room_id"],
    assignedTo: ["assignedTo", "assigned_to"]
  };
  for (const [canonical, keys] of Object.entries(aliases)) {
    if (keys.some(key => payload[key])) continue;
    for (const key of keys) {
      if (source[key] !== undefined) {
        payload[canonical] = source[key];
        break;
      }
    }
  }
  for (const key of [
    "spec",
    "status",
    "summary",
    "title",
    "name",
    "source",
    "requester",
    "topic",
    "admin",
    "invite",
    "replyRoute",
    "reply_route",
    "accepted",
    "resultStatus",
    "result_status",
    "reason",
    "goal",
    "stopCondition",
    "stop_condition",
    "iterationTemplate",
    "iteration_template",
    "maxIterations",
    "max_iterations",
    "currentIteration",
    "current_iteration",
    "iteration",
    "decision",
    "nextAction",
    "next_action",
    "sentAt",
    "sent_at"
  ]) {
    if (payload[key] === undefined && source[key] !== undefined) payload[key] = source[key];
  }
  if (payload.deliverables === undefined) {
    if (source.deliverables !== undefined) payload.deliverables = source.deliverables;
    else if (source.deliverable !== undefined) payload.deliverables = [source.deliverable];
  }
  if (payload.tasks === undefined && source.tasks !== undefined) payload.tasks = source.tasks;
  return payload;
}

function safeId(value, field) {
  const text = String(value || "").trim();
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(text)) {
    throw new Error(`${field} must be a safe id`);
  }
  return text;
}

function workspaceDir(args) {
  return path.resolve(String(args && args.workspaceDir || process.cwd()));
}

function taskDir(args, taskId) {
  return path.join(workspaceDir(args), "shared", "tasks", taskId);
}

function taskStatePath(args, taskId) {
  return path.join(taskDir(args, taskId), "meta.json");
}

function projectStatePath(args, projectId) {
  return path.join(workspaceDir(args), "shared", "projects", projectId, "meta.json");
}

function readJsonFile(file, fallback) {
  if (!fs.existsSync(file)) return { ...(fallback || {}) };
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function writeJsonFile(file, data) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, `${JSON.stringify(data, null, 2)}\n`, "utf8");
}

function normalizeRole(role) {
  const text = String(role || "").trim();
  if (text === "worker") return "remote-member";
  return text || "remote-member";
}

async function currentRole(args) {
  const context = await runtimeContext();
  return normalizeRole(context.role || process.env.AGENTTEAMS_ROLE || "remote-member");
}

function roleBlockedFromMessage(role) {
  return MESSAGE_TOOL_BLOCKED_ROLES.has(normalizeRole(role));
}

function xmlUnescape(text) {
  return String(text || "")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, "\"")
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, "&");
}

async function listOssKeys(storage, prefix) {
  const xml = (await ossFetch("GET", storage, "", { query: `?prefix=${encodeURIComponent(prefix)}` })).toString("utf8");
  return Array.from(xml.matchAll(/<Key>([^<]+)<\/Key>/g)).map(match => xmlUnescape(match[1]));
}

function walkFiles(root) {
  if (!fs.existsSync(root)) return [];
  const stat = fs.statSync(root);
  if (stat.isFile()) return [root];
  const files = [];
  for (const entry of fs.readdirSync(root)) {
    const full = path.join(root, entry);
    const itemStat = fs.statSync(full);
    if (itemStat.isDirectory()) files.push(...walkFiles(full));
    else if (itemStat.isFile()) files.push(full);
  }
  return files;
}

function excludedRelativePath(relativePath, exclude) {
  const normalized = relativePath.split(path.sep).join("/");
  return (exclude || []).some(item => {
    const rule = String(item || "").replace(/^\/+/, "");
    if (!rule) return false;
    if (rule.endsWith("/")) return normalized.startsWith(rule);
    return normalized === rule;
  });
}

async function resolvedTaskPath(args, taskId) {
  const context = await runtimeContext();
  return resolveFilesyncPath({ ...args, path: `shared/tasks/${taskId}` }, context);
}

async function pullTask(args, taskId) {
  try {
    const storage = await storageCredential();
    const resolved = await resolvedTaskPath(args, taskId);
    const prefix = `${trimSlashes(resolved.remotePath)}/`;
    const keys = await listOssKeys(storage, prefix);
    for (const key of keys) {
      const relative = key.slice(prefix.length);
      if (!relative) continue;
      const data = await ossFetch("GET", storage, key);
      const local = path.join(resolved.localPath, ...relative.split("/"));
      fs.mkdirSync(path.dirname(local), { recursive: true });
      fs.writeFileSync(local, data);
    }
    return keys.length > 0;
  } catch {
    return false;
  }
}

async function pushTask(args, taskId, exclude) {
  try {
    const storage = await storageCredential();
    const resolved = await resolvedTaskPath(args, taskId);
    const root = resolved.localPath;
    let count = 0;
    for (const file of walkFiles(root)) {
      const relative = path.relative(root, file);
      if (excludedRelativePath(relative, exclude)) continue;
      const remoteKey = `${trimSlashes(resolved.remotePath)}/${relative.split(path.sep).join("/")}`;
      await ossFetch("PUT", storage, remoteKey, { body: fs.readFileSync(file) });
      count += 1;
    }
    return count > 0;
  } catch {
    return false;
  }
}

function loadTask(args, taskId) {
  const task = readJsonFile(taskStatePath(args, taskId));
  if (!task.task_id && !task.taskId) throw new Error("task not found");
  if (!task.task_id) task.task_id = String(task.taskId);
  return task;
}

function firstText(...values) {
  for (const value of values) {
    const text = String(value || "").trim();
    if (text) return text;
  }
  return "";
}

function utcTimestamp() {
  return new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
}

function projectTaskForMeta(args, task) {
  const projectId = firstText(task.project_id, task.projectId);
  const taskId = firstText(task.task_id, task.taskId);
  if (!projectId || !taskId) return {};
  const project = readJsonFile(projectStatePath(args, projectId));
  const tasks = Array.isArray(project.tasks) ? [...project.tasks] : [];
  if (project.loop && Array.isArray(project.loop.tasks)) tasks.push(...project.loop.tasks);
  return tasks.find(item => item && typeof item === "object" && firstText(item.task_id, item.taskId) === taskId) || {};
}

function ensureConsoleTaskMeta(args, task) {
  const projectTask = projectTaskForMeta(args, task);
  task.task_id = firstText(task.task_id, task.taskId);
  task.project_id = firstText(task.project_id, task.projectId);
  task.room_id = firstText(task.room_id, task.roomId);
  task.spec_path = firstText(task.spec_path, task.specPath);
  const sourceRoomId = firstText(task.source_room_id, task.sourceRoomId, projectTask.source_room_id);
  if (sourceRoomId) task.source_room_id = sourceRoomId;
  task.task_title = firstText(task.task_title, task.taskTitle, projectTask.title, task.title, task.task_id);
  task.assigned_to = firstText(task.assigned_to, task.assignedTo, projectTask.assigned_to, projectTask.assignedTo);
  task.assigned_at = firstText(task.assigned_at, task.assignedAt, task.created_at, task.createdAt) || utcTimestamp();
  for (const [snakeKey, camelKey] of [
    ["acknowledged_by_role", "acknowledgedByRole"],
    ["result_status", "resultStatus"],
    ["result_path", "resultPath"],
    ["submitted_by_role", "submittedByRole"]
  ]) {
    const value = firstText(task[snakeKey], task[camelKey]);
    if (value) task[snakeKey] = value;
  }
  for (const key of [
    "taskId",
    "projectId",
    "roomId",
    "specPath",
    "sourceRoomId",
    "taskTitle",
    "assignedTo",
    "assignedAt",
    "createdAt",
    "acknowledgedByRole",
    "resultStatus",
    "resultPath",
    "submittedByRole"
  ]) {
    delete task[key];
  }
}

function writeTask(args, task) {
  ensureConsoleTaskMeta(args, task);
  writeJsonFile(taskStatePath(args, task.task_id), task);
}

function updateProjectTask(args, projectId, taskId, updates) {
  if (!projectId) return;
  const file = projectStatePath(args, projectId);
  const project = readJsonFile(file);
  if (!project.project_id && !project.projectId) return;
  let changed = false;
  const updateList = list => {
    if (!Array.isArray(list)) return;
    for (const item of list) {
      if (item && typeof item === "object" && item.task_id === taskId) {
        Object.assign(item, updates);
        changed = true;
      }
    }
  };
  updateList(project.tasks);
  if (project.loop && typeof project.loop === "object") updateList(project.loop.tasks);
  if (changed) writeJsonFile(file, project);
}

function parseTaskResult(file) {
  const result = { status: "", summary: "", deliverables: [] };
  const errors = [];
  let inDeliverables = false;
  let hasDeliverables = false;
  if (!fs.existsSync(file)) return { result: {}, errors: ["missing result.md"] };
  for (const line of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
    const stripped = line.trim();
    const statusMatch = stripped.match(/^(?:-\s*)?Status:\s*`?([^`]+?)`?\s*$/i);
    if (statusMatch) {
      result.status = statusMatch[1].trim();
      continue;
    }
    const summaryMatch = stripped.match(/^(?:-\s*)?Summary:\s*(.+?)\s*$/i);
    if (summaryMatch) {
      result.summary = summaryMatch[1].trim();
      continue;
    }
    if (["## deliverables", "deliverables:"].includes(stripped.toLowerCase())) {
      inDeliverables = true;
      hasDeliverables = true;
      continue;
    }
    if (inDeliverables && stripped.startsWith("- ")) {
      const item = stripped.slice(2).trim().replace(/^`|`$/g, "");
      if (item) result.deliverables.push(item);
    }
  }
  if (!result.status) errors.push("missing result status");
  else if (!["SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "FAILED", "PARTIAL"].includes(result.status)) {
    errors.push(`invalid result status: ${result.status}`);
  }
  if (!result.summary) errors.push("missing result summary");
  if (!hasDeliverables) errors.push("missing deliverables section");
  return { result, errors };
}

function slugify(value, fallback) {
  const text = String(value || "")
    .trim()
    .toLowerCase()
    .replace(/[^A-Za-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return text || fallback;
}

function projectTimestamp() {
  const now = new Date();
  const pad = value => String(value).padStart(2, "0");
  return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
}

function uniqueProjectId(args, baseId) {
  const projectId = safeId(baseId, "projectId");
  if (!fs.existsSync(projectStatePath(args, projectId))) return projectId;
  for (let index = 1; index < 1000; index += 1) {
    const candidate = safeId(`${baseId}-${String(index).padStart(2, "0")}`, "projectId");
    if (!fs.existsSync(projectStatePath(args, candidate))) return candidate;
  }
  throw new Error(`cannot allocate unique project id for: ${baseId}`);
}

function projectIdFromPayload(args, payload) {
  const explicit = payload.projectId || payload.project_id;
  if (explicit) {
    const projectId = safeId(explicit, "projectId");
    if (fs.existsSync(projectStatePath(args, projectId))) throw new Error(`project already exists: ${projectId}`);
    return projectId;
  }
  const title = String(payload.title || payload.name || "project");
  return uniqueProjectId(args, `${slugify(title, "project")}-${projectTimestamp()}`);
}

function projectDir(args, projectId) {
  return path.join(workspaceDir(args), "shared", "projects", projectId);
}

function normalizeReplyRoute(raw) {
  const route = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
  const channel = String(route.channel || "").trim();
  const targetUser = String(route.targetUser || route.target_user || route.userId || route.user_id || "").trim();
  const targetSession = String(route.targetSession || route.target_session || route.sessionId || route.session_id || "").trim();
  return channel && targetUser && targetSession
    ? { channel, target_user: targetUser, target_session: targetSession }
    : {};
}

function replyRouteFromRequester(requester) {
  const text = String(requester || "").trim();
  if (!text.startsWith("dingtalk:")) return {};
  const parts = text.split(":", 3);
  return parts.length === 3 && parts[1] && parts[2]
    ? { channel: "dingtalk", target_user: parts[1], target_session: parts[2] }
    : {};
}

function sourceRoomIdFromPayload(payload, replyRoute) {
  const explicit = String(payload.sourceRoomId || payload.source_room_id || "").trim();
  if (explicit) return explicit;
  const route = replyRoute && typeof replyRoute === "object" ? replyRoute : {};
  const channel = String(route.channel || payload.source || "").trim().toLowerCase();
  if (channel && channel !== "matrix") return String(route.target_session || "").trim();
  const requesterRoute = replyRouteFromRequester(payload.requester);
  return requesterRoute.channel && requesterRoute.channel !== "matrix"
    ? String(requesterRoute.target_session || "").trim()
    : "";
}

function writeProjectPlan(args, project) {
  const projectId = project.project_id || project.projectId;
  const lines = [
    `# ${project.title || projectId}`,
    "",
    `- Project ID: \`${projectId}\``,
    `- Status: \`${project.status}\``
  ];
  const planType = String(project.plan_type || "").trim();
  if (planType) lines.push(`- Plan Type: \`${planType}\``);
  if (project.requester) lines.push(`- Requester: \`${project.requester}\``);
  if (project.source_room_id) lines.push(`- Source Room ID: \`${project.source_room_id}\``);
  if (project.reply_route && typeof project.reply_route === "object") {
    const route = project.reply_route;
    if (route.channel && route.target_user && route.target_session) {
      lines.push(`- Reply Route: \`${route.channel}/${route.target_user}/${route.target_session}\``);
    }
  }
  const loop = project.loop && typeof project.loop === "object" ? project.loop : {};
  const tasks = planType === "loop"
    ? (Array.isArray(loop.tasks) ? loop.tasks : [])
    : (Array.isArray(project.tasks) ? project.tasks : []);
  if (planType === "loop") {
    lines.push(
      `- Loop Goal: ${loop.goal || ""}`,
      `- Stop Condition: ${loop.stop_condition || ""}`,
      `- Current Iteration: \`${loop.current_iteration || 0}\` / \`${loop.max_iterations || 0}\``,
      `- Loop Status: \`${loop.status || "running"}\``,
      "",
      "## Iteration Template",
      "",
      String(loop.iteration_template || "")
    );
  }
  lines.push("", "## Tasks");
  for (const task of tasks) {
    const deps = Array.isArray(task.depends_on) && task.depends_on.length ? task.depends_on.join(", ") : "none";
    lines.push(`- \`${task.task_id}\` ${task.title || ""} -> ${task.assigned_to || "unassigned"}; deps: ${deps}; status: ${task.status}`);
  }
  if (planType === "loop" && Array.isArray(loop.history) && loop.history.length) {
    lines.push("", "## Loop History");
    for (const item of loop.history) {
      if (item && typeof item === "object") {
        let detail = `- Iteration ${item.iteration}: \`${item.decision}\` - ${item.summary || ""}`;
        if (item.next_action) detail += ` Next: ${item.next_action}`;
        lines.push(detail);
      } else {
        lines.push(`- ${item}`);
      }
    }
  }
  const file = path.join(projectDir(args, projectId), "plan.md");
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, `${lines.join("\n")}\n`, "utf8");
}

function normalizeTask(raw, previous) {
  const item = raw && typeof raw === "object" ? raw : {};
  const prior = previous || {};
  const taskId = safeId(item.taskId || item.task_id, "taskId");
  let status = String(item.status || prior.status || "planned");
  if (status === "pending") status = "planned";
  return {
    task_id: taskId,
    title: String(item.title || prior.title || taskId),
    assigned_to: String(item.assignedTo || item.assigned_to || prior.assigned_to || ""),
    depends_on: (item.dependsOn || item.depends_on || prior.depends_on || []).map(dep => String(dep)),
    status
  };
}

function validateTaskGraph(tasks) {
  const seen = new Set();
  const ids = new Set();
  for (const task of tasks) {
    if (seen.has(task.task_id)) throw new Error(`duplicate task id: ${task.task_id}`);
    seen.add(task.task_id);
    ids.add(task.task_id);
  }
  for (const task of tasks) {
    for (const dep of task.depends_on || []) {
      if (!ids.has(dep)) throw new Error(`task ${task.task_id} depends on unknown task: ${dep}`);
    }
  }
  const visiting = new Set();
  const visited = new Set();
  const depsById = Object.fromEntries(tasks.map(task => [task.task_id, task.depends_on || []]));
  const visit = (taskId, pathStack) => {
    if (visited.has(taskId)) return;
    if (visiting.has(taskId)) throw new Error(`task dependency cycle detected: ${[...pathStack, taskId].join(" -> ")}`);
    visiting.add(taskId);
    for (const dep of depsById[taskId] || []) visit(dep, [...pathStack, taskId]);
    visiting.delete(taskId);
    visited.add(taskId);
  };
  for (const taskId of Object.keys(depsById)) visit(taskId, []);
}

function positiveInt(value, field) {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isInteger(parsed)) throw new Error(`${field} must be an integer`);
  if (parsed < 1) throw new Error(`${field} must be greater than zero`);
  return parsed;
}

function nonNegativeInt(value, field) {
  const parsed = Number.parseInt(value, 10);
  if (!Number.isInteger(parsed)) throw new Error(`${field} must be an integer`);
  if (parsed < 0) throw new Error(`${field} must be zero or greater`);
  return parsed;
}

function safeLoopStatus(value) {
  const status = String(value || "running").trim();
  const allowed = new Set(["running", "waiting_user", "completed", "blocked"]);
  if (!allowed.has(status)) throw new Error(`status must be one of: ${Array.from(allowed).sort().join(", ")}`);
  return status;
}

function safeLoopDecision(value) {
  const decision = String(value || "").trim();
  const allowed = new Set(["continue", "replan", "ask_user", "stop_success", "stop_blocked"]);
  if (!allowed.has(decision)) throw new Error(`decision must be one of: ${Array.from(allowed).sort().join(", ")}`);
  return decision;
}

function readyNodes(project) {
  if (project.plan_type === "loop") throw new Error(`project plan is not a DAG: ${project.project_id}`);
  if (String(project.status || "active") !== "active") return [];
  const tasks = Array.isArray(project.tasks) ? project.tasks : [];
  const statusById = Object.fromEntries(tasks.map(task => [task.task_id, task.status]));
  return tasks.filter(task =>
    ["planned", "assigned"].includes(task.status)
    && (task.depends_on || []).every(dep => statusById[dep] === "completed")
  );
}

function readyLoopNodes(project) {
  if (String(project.status || "active") !== "active") return [];
  const loop = project.loop && typeof project.loop === "object" ? project.loop : null;
  if (!loop) throw new Error(`project has no loop plan: ${project.project_id}`);
  if (["completed", "blocked", "waiting_user"].includes(String(loop.status || "running"))) return [];
  const tasks = Array.isArray(loop.tasks) ? loop.tasks : [];
  const statusById = Object.fromEntries(tasks.map(task => [task.task_id, task.status]));
  return tasks.filter(task =>
    ["planned", "assigned"].includes(task.status)
    && (task.depends_on || []).every(dep => statusById[dep] === "completed")
  );
}

function acceptedNodeStatus(resultStatus) {
  const status = String(resultStatus || "SUCCESS").trim();
  if (["SUCCESS", "SUCCESS_WITH_NOTES"].includes(status)) return "completed";
  if (status === "REVISION_NEEDED") return "revision";
  if (["BLOCKED", "INTERRUPTED"].includes(status)) return "blocked";
  throw new Error(`unsupported result status: ${status}`);
}

function payloadBool(value, fallback) {
  if (value === undefined || value === null) return fallback;
  if (typeof value === "boolean") return value;
  const text = String(value).trim().toLowerCase();
  if (["true", "1", "yes", "y", "accepted"].includes(text)) return true;
  if (["false", "0", "no", "n", "rejected"].includes(text)) return false;
  return fallback;
}

function resolveProject(args, payload) {
  let task = {};
  let projectIdValue = payload.projectId || payload.project_id;
  const taskIdValue = payload.taskId || payload.task_id;
  if (taskIdValue) {
    const taskId = safeId(taskIdValue, "taskId");
    task = loadTask(args, taskId);
    projectIdValue = task.project_id;
  }
  if (!projectIdValue) throw new Error("projectId or taskId is required");
  const projectId = safeId(projectIdValue, "projectId");
  const project = readJsonFile(projectStatePath(args, projectId));
  if (!project.project_id && !project.projectId) throw new Error("project not found");
  if (!project.project_id) project.project_id = String(project.projectId);
  const planType = String(project.plan_type || "dag");
  const ready = planType === "loop" ? readyLoopNodes(project) : readyNodes(project);
  const result = {
    ok: true,
    tool: "projectflow",
    action: "resolve_project",
    project,
    planType,
    replyRoute: project.reply_route,
    sourceRoomId: project.source_room_id || task.source_room_id || null,
    readyNodes: ready
  };
  if (task.task_id) result.task = task;
  return result;
}

function matrixTarget(target) {
  let raw = String(target || "").trim();
  if (raw.startsWith("matrix:")) raw = raw.slice("matrix:".length);
  if (raw.startsWith("room:")) {
    const roomId = raw.slice("room:".length).trim();
    if (MATRIX_ROOM_RE.test(roomId)) return { kind: "room", id: roomId };
  }
  if (raw.startsWith("!") && MATRIX_ROOM_RE.test(raw)) return { kind: "room", id: raw };
  if (raw.startsWith("user:") || raw.startsWith("@")) {
    return { kind: "user", id: raw.startsWith("user:") ? raw.slice("user:".length) : raw };
  }
  throw new Error("target must be a Matrix room target such as room:!room:domain");
}

function matrixRoomDomain(roomId) {
  return String(roomId || "").includes(":") ? String(roomId).split(":", 2)[1] : "";
}

function matrixMentions(text, roomId) {
  const mentions = String(text || "").match(MATRIX_USER_RE) || [];
  const domain = matrixRoomDomain(roomId);
  if (domain) {
    for (const match of String(text || "").matchAll(SHORT_MATRIX_MENTION_RE)) {
      mentions.push(`@${match[1]}:${domain}`);
    }
  }
  return Array.from(new Set(mentions));
}

function compactWithoutMentions(text, mentions) {
  let without = String(text || "").replace(MATRIX_USER_RE, "");
  for (const mxid of mentions) {
    const local = mxid.split(":", 1)[0];
    without = without.replace(new RegExp(`(^|[^A-Za-z0-9._=\\-/+])${local.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}($|[^A-Za-z0-9._=\\-/+])`, "g"), " ");
  }
  return (without.match(/[0-9A-Za-z\u4e00-\u9fff]+/g) || []).join("").toLowerCase();
}

function pingPongError(text, mentions) {
  if (!mentions.length) return "";
  const compact = compactWithoutMentions(text, mentions);
  return (!compact || LOW_INFORMATION_ACKS.has(compact))
    ? "message blocked: low-information mention acknowledgements can create ping-pong loops"
    : "";
}

function escapeHtml(text) {
  return String(text || "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function matrixFormattedBody(text, mentions) {
  let body = escapeHtml(text).replace(/\n/g, "<br>\n");
  for (const mxid of mentions) {
    const encoded = encodeURIComponent(mxid);
    const local = mxid.split(":", 1)[0];
    const display = escapeHtml(local.replace(/^@/, "") || mxid);
    const anchor = `<a href="https://matrix.to/#/${encoded}">${display}</a>`;
    const escapedMxid = escapeHtml(mxid);
    if (body.includes(escapedMxid)) body = body.replace(escapedMxid, anchor);
    else body = body.replace(escapeHtml(local), anchor);
  }
  return body;
}

function matrixContent(text, mentions) {
  const content = {
    msgtype: "m.text",
    body: text,
    format: "org.matrix.custom.html",
    formatted_body: matrixFormattedBody(text, mentions)
  };
  if (mentions.length) content["m.mentions"] = { user_ids: mentions };
  return content;
}

async function matrixCredentials() {
  const broker = await brokerGet("/v1/credentials/matrix");
  const homeserver = stripEndpoint(broker.homeserver || broker.matrixHomeserver || "");
  const accessToken = String(broker.accessToken || broker.access_token || "");
  if (!homeserver || !accessToken) throw new Error("credential broker matrix credentials missing homeserver or accessToken");
  return { homeserver, accessToken, userId: String(broker.userId || broker.user_id || "") };
}

async function matrixApi(method, requestPath, body) {
  const credentials = await matrixCredentials();
  const response = await fetch(`${credentials.homeserver}${requestPath}`, {
    method,
    headers: {
      Authorization: `Bearer ${credentials.accessToken}`,
      "Content-Type": "application/json",
      Accept: "application/json"
    },
    body: body === undefined ? undefined : JSON.stringify(body)
  });
  const text = await response.text();
  let data = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = {};
    }
  }
  if (!response.ok) {
    const error = new Error(`Matrix API error: HTTP ${response.status}: ${text.slice(0, 200)}`);
    error.status = response.status;
    error.body = text;
    throw error;
  }
  return data;
}

async function matrixApiOptional(method, requestPath, body) {
  try {
    return { ok: true, data: await matrixApi(method, requestPath, body) };
  } catch (error) {
    return {
      ok: false,
      status: error.status || 0,
      error: String(error.message || error)
    };
  }
}

function roomIdFromPayload(args, payload) {
  const raw = payload.roomId
    || payload.room_id
    || payload.target
    || payload.sessionId
    || payload.session_id
    || args?.roomId
    || args?.room_id
    || args?.target
    || args?.sessionId
    || "";
  const target = matrixTarget(raw);
  if (target.kind !== "room") throw new Error("describe_room requires a Matrix room target");
  return target.id;
}

function roomClassification(roomId, state) {
  const name = String(state.name?.name || state.name?.content?.name || "").trim();
  const topic = String(state.topic?.topic || state.topic?.content?.topic || "").trim();
  if (/^TASK[:：]/i.test(name) || /(^|\b)(taskId|projectId|TeamHarness Task|TASK[:：])/i.test(topic)) {
    return "task_room";
  }
  return "source_or_team_room";
}

async function describeMatrixRoom(args, payload) {
  const roomId = roomIdFromPayload(args, payload);
  const base = {
    ok: true,
    tool: "roomflow",
    action: "describe_room",
    roomId,
    target: `room:${roomId}`
  };
  if (args && args.dryRun) return { ...base, dryRun: true, classification: "unknown" };

  const name = await matrixApiOptional("GET", `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/state/m.room.name/`);
  const topic = await matrixApiOptional("GET", `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/state/m.room.topic/`);
  const state = {
    name: name.ok ? name.data : {},
    topic: topic.ok ? topic.data : {}
  };
  return {
    ...base,
    classification: roomClassification(roomId, state),
    name: String(state.name.name || "").trim(),
    topic: String(state.topic.topic || "").trim(),
    state
  };
}

function stringList(value) {
  if (value === undefined || value === null) return [];
  if (typeof value === "string") {
    const text = value.trim();
    if (!text) return [];
    try {
      return stringList(JSON.parse(text));
    } catch {
      return text.split(",").map(item => item.trim()).filter(Boolean);
    }
  }
  if (Array.isArray(value)) return value.map(item => String(item).trim()).filter(Boolean);
  return [];
}

function canonicalRoomId(value) {
  const text = String(value || "").trim();
  return text.startsWith("room:") ? text.slice("room:".length).trim() : text;
}

function externalRequesterChannel(project) {
  const route = project.reply_route && typeof project.reply_route === "object" ? project.reply_route : {};
  let channel = String(route.channel || project.source || "").trim().toLowerCase();
  if (channel && channel !== "matrix") return channel;
  const requesterRoute = replyRouteFromRequester(project.requester);
  channel = String(requesterRoute.channel || "").trim().toLowerCase();
  return channel && channel !== "matrix" ? channel : "";
}

async function runtimeTeamRoomId() {
  const context = await runtimeContext();
  return String(context.teamRoomId || context.team_room_id || "").trim();
}

async function validateAssignmentRoom(project, roomId) {
  const channel = externalRequesterChannel(project);
  if (!channel) return;
  const teamRoom = await runtimeTeamRoomId();
  if (!teamRoom) return;
  if (canonicalRoomId(roomId) === canonicalRoomId(teamRoom)) {
    throw new Error(`${channel} requester tasks require a dedicated task room; call roomflow create_task_room and pass its roomId`);
  }
}

async function matrixRoomMembers(roomId) {
  const data = await matrixApi("GET", `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/members`);
  const members = [];
  for (const event of Array.isArray(data.chunk) ? data.chunk : []) {
    const userId = String(event.state_key || "").trim();
    const membership = String(event.content?.membership || "").trim();
    if (userId && ["join", "invite"].includes(membership)) members.push(userId);
  }
  return members;
}

async function ensureMatrixRoomMembers(roomId, invite) {
  const current = new Set(await matrixRoomMembers(roomId));
  for (const userId of invite) {
    if (userId && !current.has(userId)) {
      await matrixApi("POST", `/_matrix/client/v3/rooms/${encodeURIComponent(roomId)}/invite`, { user_id: userId });
    }
  }
}

function sourceRoomBindingPath(args, sourceRoomKey, source) {
  const digest = crypto.createHash("sha256").update(sourceRoomKey).digest("hex").slice(0, 16);
  return path.join(workspaceDir(args), "shared", "roomflow", "source-rooms", `${source}-${digest}.json`);
}

function roomflowSourceRoomBinding(args, payload) {
  const source = String(payload.source || "").trim().toLowerCase();
  if (!source || source === "matrix") return {};
  const sourceRoomId = String(payload.sourceRoomId || payload.source_room_id || "").trim();
  if (!sourceRoomId) throw new Error("sourceRoomId is required for non-Matrix source task rooms");
  const sourceRoomKey = `${source}:${sourceRoomId}`;
  const file = sourceRoomBindingPath(args, sourceRoomKey, source);
  return { source, sourceRoomId, sourceRoomKey, file, record: readJsonFile(file) };
}

function boundRoomId(binding) {
  const record = binding.record && typeof binding.record === "object" ? binding.record : {};
  return String(record.roomId || record.room_id || "").trim();
}

function writeRoomflowSourceRoomBinding(binding, roomId, base) {
  if (!binding.file) return;
  const now = new Date().toISOString();
  const record = {
    ...(binding.record && typeof binding.record === "object" ? binding.record : {}),
    source: binding.source,
    sourceRoomId: binding.sourceRoomId,
    sourceRoomKey: binding.sourceRoomKey,
    roomId,
    target: `room:${roomId}`,
    updatedAt: now,
    taskId: base.taskId,
    name: base.name
  };
  if (!record.createdAt) record.createdAt = now;
  writeJsonFile(binding.file, record);
}

async function runtimeContext() {
  try {
    return await brokerGet("/v1/runtime/context");
  } catch {
    return {};
  }
}

async function visibleTools() {
  const context = await runtimeContext();
  const role = normalizeRole(context.role || process.env.AGENTTEAMS_ROLE || "remote-member");
  return Object.entries(TOOL_SCHEMAS)
    .filter(([name]) => !(name === "message" && roleBlockedFromMessage(role)))
    .map(([name, schema]) => ({ name, ...schema }));
}

function textResult(payload) {
  return {
    content: [
      {
        type: "text",
        text: typeof payload === "string" ? payload : JSON.stringify(payload, null, 2)
      }
    ]
  };
}

async function callTool(name, args) {
  if (name === "health") {
    const descriptor = brokerDescriptor();
    const context = await runtimeContext();
    return textResult({
      ok: true,
      tool: "health",
      runtime: runtimeOptions.runtime,
      broker: Boolean(descriptor),
      context
    });
  }
  if (name === "message") {
    const role = await currentRole(args || {});
    if (roleBlockedFromMessage(role)) {
      return textResult({
        ok: false,
        tool: "message",
        error: "forbidden_tool",
        message: "message tool is not available to worker roles"
      });
    }
    const action = String(args && args.action || "send");
    const channel = String(args && args.channel || "matrix");
    if (action !== "send") return textResult({ ok: false, tool: "message", error: `unsupported action: ${action}` });
    if (channel !== "matrix") {
      return textResult({ ok: false, tool: "message", error: "only matrix channel is implemented in the Node MCP server" });
    }
    const text = String(args && (args.message || args.text || args.body) || "");
    let target;
    try {
      target = matrixTarget(args && (args.target || args.room_id || args.roomId) || "");
    } catch (error) {
      return textResult({ ok: false, tool: "message", error: error.message });
    }
    if (target.kind !== "room") return textResult({ ok: false, tool: "message", error: "Matrix user targets are not supported yet" });
    const mentions = matrixMentions(text, target.id);
    const blocked = pingPongError(text, mentions);
    if (blocked) return textResult({ ok: false, tool: "message", error: blocked });
    const content = matrixContent(text, mentions);
    const base = {
      ok: true,
      tool: "message",
      action: "send",
      channel: "matrix",
      target: `room:${target.id}`,
      targetKind: "room",
      mentions,
      content
    };
    if (args && args.dryRun) return textResult({ ...base, dryRun: true });
    try {
      const data = await matrixApi(
        "PUT",
        `/_matrix/client/v3/rooms/${encodeURIComponent(target.id)}/send/m.room.message/teamharness-${process.pid}-${Date.now()}`,
        content
      );
      return textResult({ ...base, messageId: data.event_id });
    } catch (error) {
      return textResult({ ok: false, tool: "message", error: error.message });
    }
  }
  if (name === "roomflow") {
    const action = String(args && args.action || "create_task_room");
    const payload = payloadArgs(args || {});
    const role = await currentRole(args || {});
    try {
      if (ROOMFLOW_LEADER_ACTIONS.has(action) && role !== "leader") throw new Error(`${action} requires leader role`);
      if (action === "describe_room") return textResult(await describeMatrixRoom(args || {}, payload));
      if (action === "create_task_room") {
        const taskId = safeId(payload.taskId || payload.projectId, "taskId");
        const roomName = String(payload.name || payload.title || "").trim();
        if (!roomName) throw new Error("name is required");
        const source = String(payload.source || "").trim();
        const topic = String(payload.topic || `Task room for ${taskId}${source ? ` [source: ${source}]` : ""}`);
        const invite = stringList(payload.invite !== undefined ? payload.invite : args?.invite);
        const admin = String(payload.admin || payload.adminUser || payload.admin_user || "").trim();
        if (admin && !invite.includes(admin)) invite.push(admin);
        const powerUsers = {};
        if (admin) powerUsers[admin] = 100;
        const body = {
          name: roomName,
          topic,
          invite,
          preset: "trusted_private_chat"
        };
        if (Object.keys(powerUsers).length > 0) {
          body.power_level_content_override = { users: powerUsers };
        }
        const binding = roomflowSourceRoomBinding(args || {}, payload);
        const base = {
          ok: true,
          tool: "roomflow",
          action,
          taskId,
          name: roomName,
          source,
          topic,
          invite,
          content: body
        };
        if (binding.sourceRoomKey) {
          base.sourceRoomKey = binding.sourceRoomKey;
          base.sourceRoomId = binding.sourceRoomId;
        }
        const existingRoomId = boundRoomId(binding);
        if (existingRoomId) {
          base.roomId = existingRoomId;
          base.target = `room:${existingRoomId}`;
          base.reused = true;
          if (args && args.dryRun) return textResult({ ...base, dryRun: true });
          await ensureMatrixRoomMembers(existingRoomId, invite);
          return textResult(base);
        }
        if (args && args.dryRun) return textResult({ ...base, dryRun: true });
        const data = await matrixApi("POST", "/_matrix/client/v3/createRoom", body);
        const roomId = String(data.room_id || "");
        if (!roomId) throw new Error("Matrix createRoom response missing room_id");
        base.roomId = roomId;
        base.target = `room:${roomId}`;
        writeRoomflowSourceRoomBinding(binding, roomId, base);
        return textResult(base);
      }
      if (action === "list_rooms") {
        if (args && args.dryRun) return textResult({ ok: true, tool: "roomflow", action, dryRun: true });
        const data = await matrixApi("GET", "/_matrix/client/v3/joined_rooms");
        const rooms = Array.isArray(data.joined_rooms) ? data.joined_rooms : [];
        return textResult({ ok: true, tool: "roomflow", action, rooms, count: rooms.length });
      }
      if (action === "archive_room") {
        const target = matrixTarget(payload.roomId || payload.room_id || args?.target || "");
        if (target.kind !== "room") throw new Error("archive_room requires a Matrix room target");
        const base = { ok: true, tool: "roomflow", action, roomId: target.id, target: `room:${target.id}` };
        if (args && args.dryRun) return textResult({ ...base, dryRun: true });
        await matrixApi("POST", `/_matrix/client/v3/rooms/${encodeURIComponent(target.id)}/leave`, {});
        return textResult({ ...base, archived: true });
      }
    } catch (error) {
      return textResult({ ok: false, tool: "roomflow", action, error: error.message });
    }
    return textResult({ ok: false, tool: "roomflow", action, error: `unsupported action: ${action}` });
  }
  if (name === "filesync") {
    const action = String(args && args.action || "");
    const context = await runtimeContext();
    const resolved = resolveFilesyncPath(args || {}, context);
    if (resolved.kind === "global-shared" && action === "push") {
      return textResult({ ok: false, tool: "filesync", action, error: "global-shared is read-only for TeamHarness filesync" });
    }
    if (args && args.dryRun) {
      return textResult({
        ok: true,
        tool: "filesync",
        action,
        kind: resolved.kind,
        path: resolved.requestedPath,
        localPath: resolved.localPath,
        remotePath: resolved.remotePath,
        dryRun: true,
        command: null
      });
    }
    const storage = await storageCredential();
    if (action === "list") {
      const query = `?prefix=${encodeURIComponent(resolved.remotePath)}`;
      const xml = (await ossFetch("GET", storage, "", { query })).toString("utf8");
      const keys = Array.from(xml.matchAll(/<Key>([^<]+)<\/Key>/g)).map(match => xmlUnescape(match[1]));
      return textResult({ ok: true, tool: "filesync", action, ...resolved, keys });
    }
    if (action === "pull" || action === "stat") {
      const data = await ossFetch("GET", storage, resolved.remotePath);
      if (action === "pull") {
        fs.mkdirSync(path.dirname(resolved.localPath), { recursive: true });
        fs.writeFileSync(resolved.localPath, data);
      }
      return textResult({ ok: true, tool: "filesync", action, ...resolved, bytes: data.length });
    }
    if (action === "push") {
      const data = fs.readFileSync(resolved.localPath);
      await ossFetch("PUT", storage, resolved.remotePath, { body: data });
      return textResult({ ok: true, tool: "filesync", action, ...resolved, bytes: data.length });
    }
    return textResult({ ok: false, tool: "filesync", action, error: "unsupported filesync action" });
  }
  if (name === "projectflow") {
    const action = String(args && args.action || "").trim();
    const payload = payloadArgs(args || {});
    const role = await currentRole(args || {});
    try {
      if (PROJECTFLOW_LEADER_ACTIONS.has(action) && role !== "leader") throw new Error(`${action} requires leader role`);
      if (action === "create_project") {
        const projectId = projectIdFromPayload(args || {}, payload);
        const project = {
          project_id: projectId,
          title: String(payload.title || projectId),
          source: String(payload.source || ""),
          requester: String(payload.requester || ""),
          status: "active",
          tasks: []
        };
        let replyRoute = normalizeReplyRoute(payload.replyRoute || payload.reply_route);
        if (!replyRoute.channel) replyRoute = replyRouteFromRequester(project.requester);
        if (replyRoute.channel) project.reply_route = replyRoute;
        const sourceRoomId = sourceRoomIdFromPayload(payload, replyRoute);
        if (sourceRoomId) project.source_room_id = sourceRoomId;
        writeJsonFile(projectStatePath(args || {}, projectId), project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project });
      }
      if (action === "create_quick_project") {
        const projectId = projectIdFromPayload(args || {}, payload);
        const title = String(payload.title || projectId);
        const assignedTo = String(payload.assignedTo || payload.assigned_to || "").trim();
        if (!assignedTo) throw new Error("assignedTo is required");
        const roomId = String(payload.roomId || payload.room_id || "").trim();
        if (!roomId) throw new Error("roomId is required");
        const spec = String(payload.spec || "").trim();
        if (!spec) throw new Error("spec is required");
        const taskId = safeId(payload.taskId || payload.task_id || `${projectId}-01`, "taskId");
        if (!taskId.startsWith(`${projectId}-`)) throw new Error("taskId must belong to projectId");
        if (fs.existsSync(taskStatePath(args || {}, taskId))) throw new Error(`task already exists: ${taskId}`);
        const taskNode = { task_id: taskId, title, assigned_to: assignedTo, depends_on: [], status: "assigned" };
        const project = {
          project_id: projectId,
          title,
          source: String(payload.source || ""),
          requester: String(payload.requester || ""),
          status: "active",
          mode: "quick",
          plan_type: "dag",
          tasks: [taskNode]
        };
        let replyRoute = normalizeReplyRoute(payload.replyRoute || payload.reply_route);
        if (!replyRoute.channel) replyRoute = replyRouteFromRequester(project.requester);
        if (replyRoute.channel) project.reply_route = replyRoute;
        const sourceRoomId = sourceRoomIdFromPayload(payload, replyRoute);
        if (sourceRoomId) {
          project.source_room_id = sourceRoomId;
          taskNode.source_room_id = sourceRoomId;
        }
        await validateAssignmentRoom(project, roomId);
        writeJsonFile(projectStatePath(args || {}, projectId), project);
        writeProjectPlan(args || {}, project);
        const root = taskDir(args || {}, taskId);
        fs.mkdirSync(root, { recursive: true });
        fs.writeFileSync(path.join(root, "spec.md"), `${spec}\n`, "utf8");
        const task = {
          task_id: taskId,
          project_id: projectId,
          room_id: roomId,
          status: "assigned",
          assigned_to: assignedTo,
          spec_path: `shared/tasks/${taskId}/spec.md`
        };
        if (sourceRoomId) task.source_room_id = sourceRoomId;
        writeTask(args || {}, task);
        return textResult({ ok: true, tool: "projectflow", action, project, task, synced: await pushTask(args || {}, taskId) });
      }
      if (action === "resolve_project") return textResult(resolveProject(args || {}, payload));
      if (action === "accept_task_result") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const taskId = safeId(payload.taskId || payload.task_id, "taskId");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        const resultStatusValue = payload.resultStatus || payload.result_status;
        const accepted = payloadBool(payload.accepted, true);
        let nodeStatus = acceptedNodeStatus(resultStatusValue);
        let resultStatus = String(resultStatusValue || "SUCCESS");
        if (!accepted && nodeStatus === "completed") {
          resultStatus = "REVISION_NEEDED";
          nodeStatus = "revision";
        }
        let changed = false;
        for (const list of [project.tasks, project.loop?.tasks]) {
          if (!Array.isArray(list)) continue;
          for (const task of list) {
            if (task.task_id === taskId) {
              task.status = nodeStatus;
              changed = true;
              break;
            }
          }
        }
        if (!changed) throw new Error("task not found in project plan");
        if (nodeStatus === "completed") {
          project.requester_report = {
            pending: true,
            reason: "task_result_accepted",
            task_id: taskId,
            result_status: resultStatus,
            summary: String(payload.summary || ""),
            report_path: `shared/projects/${projectId}/result.md`
          };
        } else if (project.requester_report?.task_id === taskId) {
          project.requester_report.pending = false;
          project.requester_report.reason = `task_result_${nodeStatus}`;
        }
        writeJsonFile(projectStatePath(args || {}, projectId), project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project, taskId, nodeStatus, accepted: nodeStatus === "completed" });
      }
      if (action === "mark_requester_report_sent") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        const report = project.requester_report && typeof project.requester_report === "object" ? project.requester_report : {};
        report.pending = false;
        report.sent_at = String(payload.sentAt || payload.sent_at || new Date().toISOString());
        project.requester_report = report;
        writeJsonFile(projectStatePath(args || {}, projectId), project);
        return textResult({ ok: true, tool: "projectflow", action, project });
      }
      if (action === "plan_dag") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const statePath = projectStatePath(args || {}, projectId);
        const project = readJsonFile(statePath, { project_id: projectId, title: projectId, status: "active", tasks: [] });
        const previous = Object.fromEntries((Array.isArray(project.tasks) ? project.tasks : []).map(task => [task.task_id, task]));
        if (!Array.isArray(payload.tasks)) throw new Error("tasks must be a list");
        const plannedTasks = payload.tasks.filter(item => item && typeof item === "object").map(item => normalizeTask(item, previous[item.taskId || item.task_id]));
        validateTaskGraph(plannedTasks);
        project.tasks = plannedTasks;
        project.plan_type = "dag";
        writeJsonFile(statePath, project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project, readyNodes: readyNodes(project) });
      }
      if (action === "plan_loop") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const statePath = projectStatePath(args || {}, projectId);
        const project = readJsonFile(statePath, { project_id: projectId, title: projectId, status: "active", tasks: [] });
        const previousLoop = project.loop && typeof project.loop === "object" ? project.loop : {};
        const previous = Object.fromEntries((Array.isArray(previousLoop.tasks) ? previousLoop.tasks : []).map(task => [task.task_id, task]));
        const rawTasks = payload.tasks || [];
        if (!Array.isArray(rawTasks)) throw new Error("tasks must be a list");
        const maxIterations = positiveInt(payload.maxIterations || payload.max_iterations, "maxIterations");
        const currentIteration = nonNegativeInt(payload.currentIteration || payload.current_iteration || previousLoop.current_iteration || 0, "currentIteration");
        if (currentIteration > maxIterations) throw new Error("currentIteration cannot exceed maxIterations");
        const plannedTasks = rawTasks.filter(item => item && typeof item === "object").map(item => normalizeTask(item, previous[item.taskId || item.task_id]));
        validateTaskGraph(plannedTasks);
        const loop = {
          goal: String(payload.goal || previousLoop.goal || "").trim(),
          stop_condition: String(payload.stopCondition || payload.stop_condition || previousLoop.stop_condition || "").trim(),
          iteration_template: String(payload.iterationTemplate || payload.iteration_template || previousLoop.iteration_template || "").trim(),
          max_iterations: maxIterations,
          current_iteration: currentIteration,
          status: safeLoopStatus(payload.status || previousLoop.status || "running"),
          tasks: plannedTasks,
          history: Array.isArray(previousLoop.history) ? previousLoop.history : []
        };
        if (!loop.goal) throw new Error("goal is required");
        if (!loop.stop_condition) throw new Error("stopCondition is required");
        if (!loop.iteration_template) throw new Error("iterationTemplate is required");
        project.plan_type = "loop";
        project.loop = loop;
        writeJsonFile(statePath, project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project, loop, readyLoopNodes: readyLoopNodes(project) });
      }
      if (action === "ready_nodes") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        return textResult({ ok: true, tool: "projectflow", action, project, readyNodes: readyNodes(project) });
      }
      if (action === "ready_loop_nodes") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        return textResult({ ok: true, tool: "projectflow", action, project, loop: project.loop || {}, readyLoopNodes: readyLoopNodes(project) });
      }
      if (action === "record_loop_iteration") {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        const loop = project.loop && typeof project.loop === "object" ? project.loop : null;
        if (!loop) throw new Error(`project has no loop plan: ${projectId}`);
        const iteration = positiveInt(payload.iteration, "iteration");
        const maxIterations = positiveInt(loop.max_iterations, "maxIterations");
        if (iteration > maxIterations) throw new Error("iteration cannot exceed maxIterations");
        const decision = safeLoopDecision(payload.decision);
        loop.status = {
          continue: "running",
          replan: "running",
          ask_user: "waiting_user",
          stop_success: "completed",
          stop_blocked: "blocked"
        }[decision];
        loop.current_iteration = Math.max(nonNegativeInt(loop.current_iteration || 0, "currentIteration"), iteration);
        loop.history = Array.isArray(loop.history) ? loop.history : [];
        loop.history.push({
          iteration,
          decision,
          summary: String(payload.summary || "").trim(),
          next_action: String(payload.nextAction || payload.next_action || "").trim()
        });
        project.plan_type = "loop";
        project.loop = loop;
        writeJsonFile(projectStatePath(args || {}, projectId), project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project, loop, readyLoopNodes: readyLoopNodes(project) });
      }
      if (["pause_project", "resume_project", "complete_project"].includes(action)) {
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const statePath = projectStatePath(args || {}, projectId);
        const project = readJsonFile(statePath);
        if (!project.project_id && !project.projectId) throw new Error("project not found");
        if (action === "pause_project") project.status = "paused";
        else if (action === "resume_project") project.status = "active";
        else {
          project.status = "completed";
          if (project.loop && typeof project.loop === "object") project.loop.status = "completed";
        }
        writeJsonFile(statePath, project);
        writeProjectPlan(args || {}, project);
        return textResult({ ok: true, tool: "projectflow", action, project });
      }
    } catch (error) {
      return textResult({ ok: false, tool: "projectflow", action, error: error.message });
    }
    return textResult({ ok: false, tool: "projectflow", action, error: `unsupported action: ${action}` });
  }
  if (name === "taskflow") {
    const action = String(args && args.action || "").trim();
    const payload = payloadArgs(args || {});
    const role = await currentRole(args || {});
    try {
      if (action === "delegate_task") {
        if (role !== "leader") throw new Error("delegate_task requires leader role");
        const projectId = safeId(payload.projectId || payload.project_id, "projectId");
        const taskId = safeId(payload.taskId || payload.task_id, "taskId");
        const roomId = String(payload.roomId || payload.room_id || "").trim();
        if (!roomId) throw new Error("roomId is required");
        const project = readJsonFile(projectStatePath(args || {}, projectId));
        await validateAssignmentRoom(project, roomId);
        let assignedTo = String(payload.assignedTo || payload.assigned_to || "").trim();
        if (!assignedTo) {
          const taskLists = [
            ...(Array.isArray(project.tasks) ? project.tasks : []),
            ...(Array.isArray(project.loop?.tasks) ? project.loop.tasks : [])
          ];
          const planned = taskLists.find(item => item && item.task_id === taskId);
          assignedTo = String(planned?.assigned_to || "").trim();
        }
        let sourceRoomId = String(payload.sourceRoomId || payload.source_room_id || "").trim();
        if (!sourceRoomId) sourceRoomId = String(project.source_room_id || "").trim();
        const taskRoot = taskDir(args || {}, taskId);
        fs.mkdirSync(taskRoot, { recursive: true });
        const spec = String(payload.spec || "");
        fs.writeFileSync(path.join(taskRoot, "spec.md"), spec ? `${spec}\n` : "", "utf8");
        const task = {
          task_id: taskId,
          project_id: projectId,
          room_id: roomId,
          status: "assigned",
          spec_path: `shared/tasks/${taskId}/spec.md`
        };
        if (assignedTo) task.assigned_to = assignedTo;
        if (sourceRoomId) task.source_room_id = sourceRoomId;
        writeTask(args || {}, task);
        const projectUpdates = { status: "assigned" };
        if (assignedTo) projectUpdates.assigned_to = assignedTo;
        if (sourceRoomId) projectUpdates.source_room_id = sourceRoomId;
        updateProjectTask(args || {}, projectId, taskId, projectUpdates);
        return textResult({ ok: true, tool: "taskflow", action, task, synced: await pushTask(args || {}, taskId) });
      }

      if (action === "ack_task") {
        if (!["remote-member", "worker"].includes(role)) throw new Error("ack_task requires worker or remote-member role");
        const taskId = safeId(payload.taskId || payload.task_id, "taskId");
        const pulled = await pullTask(args || {}, taskId);
        const task = loadTask(args || {}, taskId);
        task.status = "in_progress";
        task.acknowledged_by_role = role;
        writeTask(args || {}, task);
        updateProjectTask(args || {}, task.project_id || "", taskId, { status: "in_progress" });
        const specPath = path.join(taskDir(args || {}, taskId), "spec.md");
        const spec = fs.existsSync(specPath) ? fs.readFileSync(specPath, "utf8") : "";
        return textResult({
          ok: true,
          tool: "taskflow",
          action,
          task,
          spec,
          pulled,
          synced: await pushTask(args || {}, taskId, ["spec.md", "base/"])
        });
      }

      if (action === "submit_task") {
        if (!["remote-member", "worker"].includes(role)) throw new Error("submit_task requires worker or remote-member role");
        const taskId = safeId(payload.taskId || payload.task_id, "taskId");
        const task = loadTask(args || {}, taskId);
        const summary = String(payload.summary || "");
        const status = String(payload.status || "SUCCESS");
        const deliverables = payload.deliverables || [];
        if (!Array.isArray(deliverables)) throw new Error("deliverables must be a list");
        const taskRoot = taskDir(args || {}, taskId);
        fs.mkdirSync(taskRoot, { recursive: true });
        const resultLines = [
          "# Task Result",
          "",
          `- Status: \`${status}\``,
          `- Summary: ${summary}`,
          "",
          "## Deliverables",
          ...deliverables.map(item => `- \`${item}\``)
        ];
        fs.writeFileSync(path.join(taskRoot, "result.md"), `${resultLines.join("\n")}\n`, "utf8");
        Object.assign(task, {
          status: "submitted",
          result_status: status,
          summary,
          deliverables: deliverables.map(item => String(item)),
          result_path: `shared/tasks/${taskId}/result.md`,
          submitted_by_role: role
        });
        writeTask(args || {}, task);
        updateProjectTask(args || {}, task.project_id || "", taskId, { status: "submitted" });
        return textResult({
          ok: true,
          tool: "taskflow",
          action,
          task,
          synced: await pushTask(args || {}, taskId, ["spec.md", "base/"])
        });
      }

      if (action === "check_task") {
        if (role !== "leader") throw new Error("check_task requires leader role");
        const taskId = safeId(payload.taskId || payload.task_id, "taskId");
        const pulled = await pullTask(args || {}, taskId);
        const task = loadTask(args || {}, taskId);
        const { result, errors } = parseTaskResult(path.join(taskDir(args || {}, taskId), "result.md"));
        const effective = task.status === "submitted" && errors.length === 0;
        return textResult({
          ok: true,
          tool: "taskflow",
          action,
          task,
          result,
          validationErrors: errors,
          effective,
          pulled
        });
      }
    } catch (error) {
      return textResult({ ok: false, tool: "taskflow", action, error: error.message });
    }
    return textResult({ ok: false, tool: "taskflow", action, error: `unsupported action: ${action}` });
  }
  return textResult({
    ok: false,
    tool: name,
    error: "tool is declared but not implemented in the Node skeleton yet"
  });
}

async function handle(request) {
  if (request.method === "initialize") {
    return {
      protocolVersion: "2024-11-05",
      capabilities: { tools: {} },
      serverInfo: { name: runtimeOptions.serverName, version: runtimeOptions.version }
    };
  }
  if (request.method === "tools/list") {
    return { tools: await visibleTools() };
  }
  if (request.method === "tools/call") {
    const params = request.params || {};
    return callTool(String(params.name || ""), params.arguments || {});
  }
  return {};
}

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

let buffer = Buffer.alloc(0);
function readFrames(chunk) {
  buffer = Buffer.concat([buffer, chunk]);
  const frames = [];
  for (;;) {
    const headerEnd = buffer.indexOf("\r\n\r\n");
    if (headerEnd === -1) {
      const newline = buffer.indexOf("\n");
      if (newline === -1) {
        return frames;
      }
      const line = buffer.subarray(0, newline).toString("utf8").trim();
      buffer = buffer.subarray(newline + 1);
      if (line) {
        frames.push(line);
      }
      continue;
    }
    const header = buffer.subarray(0, headerEnd).toString("utf8");
    const match = header.match(/content-length:\s*(\d+)/i);
    if (!match) {
      buffer = buffer.subarray(headerEnd + 4);
      continue;
    }
    const length = Number(match[1]);
    const start = headerEnd + 4;
    const end = start + length;
    if (buffer.length < end) {
      return frames;
    }
    frames.push(buffer.subarray(start, end).toString("utf8"));
    buffer = buffer.subarray(end);
  }
}

function configure(options) {
  runtimeOptions = {
    ...DEFAULT_OPTIONS,
    ...(options || {})
  };
  TOOL_SCHEMAS.health.description = runtimeOptions.healthDescription;
  return runtimeOptions;
}

function runMcpServer(options) {
  configure(options);
  process.stdin.on("data", chunk => {
    for (const frame of readFrames(chunk)) {
      let request;
      try {
        request = JSON.parse(frame);
      } catch (error) {
        send({ jsonrpc: "2.0", id: null, error: { code: -32700, message: error.message } });
        continue;
      }
      if (!Object.prototype.hasOwnProperty.call(request, "id")) {
        continue;
      }
      handle(request)
        .then(result => send({ jsonrpc: "2.0", id: request.id, result }))
        .catch(error => send({ jsonrpc: "2.0", id: request.id, error: { code: -32000, message: error.message } }));
    }
  });
}

module.exports = {
  configure,
  runMcpServer,
  handle,
  readFrames
};

if (require.main === module) {
  runMcpServer();
}
