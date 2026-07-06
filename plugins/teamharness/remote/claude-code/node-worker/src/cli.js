#!/usr/bin/env node
"use strict";

const fs = require("fs");
const crypto = require("crypto");
const zlib = require("zlib");
const os = require("os");
const path = require("path");
const { spawn, spawnSync } = require("child_process");

function loadCoreWorkerModule(moduleName) {
  const explicit = process.env.TEAMHARNESS_NODE_RUNTIME_CORE_DIR;
  const modulePath = `${moduleName}.js`;
  const candidates = [
    explicit ? path.join(explicit, "worker", modulePath) : "",
    path.join(__dirname, "../../../node-runtime-core/worker", modulePath),
    path.join(__dirname, "../../node-runtime-core/worker", modulePath)
  ].filter(Boolean);
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch (error) {
      if (error.code !== "MODULE_NOT_FOUND" || !String(error.message || "").includes(candidate)) {
        throw error;
      }
    }
  }
  throw new Error(`TeamHarness node-runtime-core ${moduleName} not found from ${__dirname}`);
}

const coreStatus = loadCoreWorkerModule("status");
const coreController = loadCoreWorkerModule("controller");
const coreStorageOss = loadCoreWorkerModule("storage-oss");
const coreRuntimeConfig = loadCoreWorkerModule("runtime-config");
const coreMatrix = loadCoreWorkerModule("matrix");
const corePackageView = loadCoreWorkerModule("package-view");
const coreBroker = loadCoreWorkerModule("broker");
const coreArgs = loadCoreWorkerModule("args");
const corePrompt = loadCoreWorkerModule("prompt");
const coreLifecycle = loadCoreWorkerModule("lifecycle");
const coreEnv = loadCoreWorkerModule("env");
const { logEvent } = loadCoreWorkerModule("log");
const { startModelProxy: startCoreModelProxy } = loadCoreWorkerModule("model-proxy");

const DEFAULT_STATE_DIR = ".agentteams/runtime/claude-code";
const MIN_CLAUDE_CODE_SETTINGS_VERSION = [2, 1, 141];

function parseArgs(argv) {
  return coreArgs.parseRuntimeArgs(argv, {
    runtime: "claude-code",
    defaultStateDir: DEFAULT_STATE_DIR,
    pluginEnvVar: "TEAMHARNESS_CLAUDE_PLUGIN_DIR",
    commandName: "claudeCommand",
    commandEnvVars: ["TEAMHARNESS_CLAUDE_COMMAND"],
    commandDefault: "claude",
    commandArg: "--claude-command",
    commandSearchPaths: [
      process.env.HOME ? path.join(process.env.HOME, ".local/bin/claude") : "",
      "/usr/local/bin/claude"
    ]
  });
}

function mkdirp(dir) {
  return coreStatus.mkdirp(dir);
}

function writeJson(file, payload, mode) {
  return coreStatus.writeJson(file, payload, mode);
}

function writeStatus(args, phase, reason, message, extra) {
  return coreStatus.writeStatus(args, phase, reason, message, extra);
}

function removeFileQuietly(file) {
  return coreStatus.removeFileQuietly(file);
}

function cleanupLegacySensitiveState(args) {
  return coreStatus.cleanupLegacySensitiveState(args);
}

function cleanupBrokerFiles(args) {
  removeFileQuietly(path.join(args.stateDir, "credential-token"));
  removeFileQuietly(path.join(args.stateDir, "credential-broker.json"));
  removeFileQuietly(path.join(args.stateDir, "mcp-runtime.json"));
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (pluginDir) {
    removeFileQuietly(path.join(pluginDir, ".teamharness", "credential-broker.json"));
  }
}

function readJson(file, fallback) {
  return coreStatus.readJson(file, fallback);
}

function positiveIntervalSeconds(value, fallback) {
  return coreStatus.positiveIntervalSeconds(value, fallback);
}

function stsRefreshRequired(sts, nowSeconds) {
  return coreStatus.stsRefreshRequired(sts, nowSeconds);
}

async function postJson(baseUrl, requestPath, payload, headers) {
  return coreController.postJson(baseUrl, requestPath, payload, headers);
}

async function postEmpty(baseUrl, requestPath, headers) {
  return coreController.postEmpty(baseUrl, requestPath, headers);
}

function parseScalar(value) {
  return coreRuntimeConfig.parseScalar(value);
}

function parseRuntimeYaml(text) {
  return coreRuntimeConfig.parseRuntimeYaml(text);
}

function runtimeObjectKey(workerName) {
  return coreRuntimeConfig.runtimeObjectKey(workerName);
}

function personalRoomId(runtime) {
  return coreRuntimeConfig.personalRoomId(runtime);
}

function matrixRooms(runtime) {
  return coreRuntimeConfig.matrixRooms(runtime);
}

function escapeRegExp(value) {
  return coreMatrix.escapeRegExp(value);
}

function containsNameMention(body, name) {
  return coreMatrix.containsNameMention(body, name);
}

function isMatrixThreadEvent(event) {
  return coreMatrix.isMatrixThreadEvent(event);
}

function messagesAfter(messages, lastEventId) {
  return coreMatrix.messagesAfter(messages, lastEventId);
}

function claudeSessionKey(roomId, args) {
  const workDir = path.resolve(args.workDir || ".");
  const digest = crypto.createHash("sha256").update(workDir).digest("hex").slice(0, 16);
  return `${roomId}::workdir:${digest}`;
}

function claudeSessionForRoom(sessions, roomId, args) {
  return sessions[claudeSessionKey(roomId, args)] || sessions[roomId] || "";
}

function storeClaudeSessionForRoom(sessions, roomId, args, sessionId) {
  if (sessionId) {
    sessions[claudeSessionKey(roomId, args)] = sessionId;
  }
}

function dropClaudeSessionForRoom(sessions, roomId, args) {
  delete sessions[claudeSessionKey(roomId, args)];
  delete sessions[roomId];
}

function loadMatrixState(args) {
  return coreMatrix.loadMatrixState(args);
}

function writeMatrixState(args, state) {
  return coreMatrix.writeMatrixState(args, state);
}

function combineGatewayUrl(modelGatewayUrl, runtimeGatewayUrl) {
  const gateway = String(modelGatewayUrl || "").trim();
  const runtime = String(runtimeGatewayUrl || "").trim();
  if (!gateway) {
    return runtime;
  }
  if (!runtime) {
    return gateway;
  }
  try {
    const gatewayUrl = new URL(gateway);
    const runtimeUrl = new URL(runtime);
    gatewayUrl.pathname = runtimeUrl.pathname || gatewayUrl.pathname;
    gatewayUrl.search = runtimeUrl.search;
    return gatewayUrl.toString().replace(/\/+$/, "");
  } catch {
    return runtime || gateway;
  }
}

function openaiChatUrl(baseUrl) {
  const base = String(baseUrl || "").trim().replace(/\/+$/, "");
  if (!base) return "";
  if (base.endsWith("/v1/chat/completions") || base.endsWith("/chat/completions")) return base;
  if (base.endsWith("/v1")) return `${base}/chat/completions`;
  return `${base}/v1/chat/completions`;
}

function runtimeLlmConfig(edge, runtimeState) {
  const model = runtimeState.runtime?.desired?.model || {};
  return {
    model: String(model.model || "").trim(),
    baseUrl: combineGatewayUrl(edge.modelGatewayUrl, model.gatewayUrl),
    apiKey: String(model.gatewayKey || "").trim()
  };
}

function claudeManagedLlmConfig(edge, runtimeState) {
  const raw = runtimeLlmConfig(edge, runtimeState);
  const proxy = runtimeState.args?.modelProxy;
  if (proxy?.endpoint && proxy?.token) {
    return { ...raw, baseUrl: proxy.endpoint, apiKey: proxy.token };
  }
  return raw;
}

async function startModelProxy(args, edge, runtimeState) {
  if (usesNativeModelConfig(args)) return null;
  return startCoreModelProxy(args, edge, runtimeState);
}

function stripEndpoint(endpoint) {
  return coreStorageOss.stripEndpoint(endpoint);
}

function ossPublicEndpointFallback(endpoint) {
  return coreStorageOss.ossPublicEndpointFallback(endpoint);
}

function brokerOssEndpoint(sts) {
  return coreStorageOss.brokerOssEndpoint(sts);
}

function encodeObjectKey(key) {
  return coreStorageOss.encodeObjectKey(key);
}

function ossRequestUrl(endpoint, bucket, objectKey, query) {
  return coreStorageOss.ossRequestUrl(endpoint, bucket, objectKey, query);
}

function canonicalizedOssResource(bucket, objectKey, queryKeys) {
  return coreStorageOss.canonicalizedOssResource(bucket, objectKey, queryKeys);
}

function ossAuthHeaders(method, sts, bucket, objectKey, queryKeys) {
  return coreStorageOss.ossAuthHeaders(method, sts, bucket, objectKey, queryKeys);
}

async function ossFetch(method, sts, objectKey, options) {
  return coreStorageOss.ossFetch(method, sts, objectKey, options);
}

async function ossGet(sts, objectKey) {
  return coreStorageOss.ossGet(sts, objectKey);
}

async function ossPut(sts, objectKey, content) {
  return coreStorageOss.ossPut(sts, objectKey, content);
}

async function ossDelete(sts, objectKey) {
  return coreStorageOss.ossDelete(sts, objectKey);
}

async function ossList(sts, prefix) {
  return coreStorageOss.ossList(sts, prefix);
}

async function exchangeEdgeToken(args, bootstrap) {
  return coreController.exchangeEdgeToken(args, bootstrap);
}

async function requestSts(args, edge) {
  return coreController.requestSts(args, edge);
}

function runtimeStateSnapshot(runtimeState) {
  return coreRuntimeConfig.runtimeStateSnapshot(runtimeState);
}

async function loadRuntimeConfig(args, edge, sts) {
  return coreRuntimeConfig.loadRuntimeConfig(args, edge, sts);
}

function updateRuntimeState(target, source) {
  return coreRuntimeConfig.updateRuntimeState(target, source);
}

function runtimeRefreshDue(args, runtimeState) {
  return coreRuntimeConfig.runtimeRefreshDue(args, runtimeState);
}

async function refreshRemoteRuntime(args, edge, runtimeState, options) {
  return coreRuntimeConfig.refreshRemoteRuntime(args, edge, runtimeState, options);
}

async function prepareRuntimeForTask(args, edge, runtimeState) {
  const previousDigest = appliedAgentPackageDigest(runtimeState.agentPackage);
  await refreshRemoteRuntime(args, edge, runtimeState);
  const nextPackage = await applyAgentPackage(args, runtimeState, runtimeState.sts);
  applyGlobalIntegrations(args, edge, runtimeState);
  if (previousDigest && nextPackage && previousDigest !== nextPackage.digest) {
    runtimeState.sessionResetAt = new Date().toISOString();
    writeStatus(args, "Running", "AgentPackageChanged", "AgentSpec package changed; future Claude sessions use the new package", {
      previousPackageDigest: previousDigest,
      packageDigest: nextPackage.digest
    });
  }
  return runtimeState;
}

function startRemotePeriodicTasks(args, edge, runtimeState) {
  const timers = [];
  const runtimeInterval = positiveIntervalSeconds(args.runtimeRefreshIntervalSeconds, 60);
  if (runtimeInterval > 0) {
    const timer = setInterval(() => {
      refreshRemoteRuntime(args, edge, runtimeState)
        .then(() => stageAgentPackage(args, runtimeState, runtimeState.sts).catch(error => {
          writeStatus(args, "Degraded", "AgentPackageStageFailed", error.message);
        }))
        .catch(error => {
          writeStatus(args, "Degraded", "RuntimeRefreshFailed", error.message);
        });
    }, runtimeInterval * 1000);
    timer.unref();
    timers.push(timer);
  }
  const heartbeatInterval = positiveIntervalSeconds(args.heartbeatIntervalSeconds, 30);
  if (heartbeatInterval > 0) {
    const timer = setInterval(() => {
      writeLauncherHeartbeat(args);
      reportHeartbeat(args, edge).catch(error => {
        writeStatus(args, "Degraded", "HeartbeatFailed", error.message);
      });
    }, heartbeatInterval * 1000);
    timer.unref();
    timers.push(timer);
  }
  return () => timers.forEach(timer => clearInterval(timer));
}

function rmrf(target) {
  fs.rmSync(target, { recursive: true, force: true });
}

function safeJoin(root, entryName) {
  const target = path.resolve(root, entryName);
  const rootPath = path.resolve(root);
  if (target !== rootPath && !target.startsWith(`${rootPath}${path.sep}`)) {
    throw new Error(`unsafe zip entry path: ${entryName}`);
  }
  return target;
}

function findEndOfCentralDirectory(buffer) {
  const min = Math.max(0, buffer.length - 0xffff - 22);
  for (let offset = buffer.length - 22; offset >= min; offset -= 1) {
    if (buffer.readUInt32LE(offset) === 0x06054b50) {
      return offset;
    }
  }
  throw new Error("invalid zip: end of central directory not found");
}

function extractZip(buffer, destDir) {
  rmrf(destDir);
  mkdirp(destDir);
  const eocd = findEndOfCentralDirectory(buffer);
  const entries = buffer.readUInt16LE(eocd + 10);
  const centralOffset = buffer.readUInt32LE(eocd + 16);
  let offset = centralOffset;
  for (let index = 0; index < entries; index += 1) {
    if (buffer.readUInt32LE(offset) !== 0x02014b50) {
      throw new Error("invalid zip: central directory entry expected");
    }
    const method = buffer.readUInt16LE(offset + 10);
    const compressedSize = buffer.readUInt32LE(offset + 20);
    const fileNameLength = buffer.readUInt16LE(offset + 28);
    const extraLength = buffer.readUInt16LE(offset + 30);
    const commentLength = buffer.readUInt16LE(offset + 32);
    const localHeaderOffset = buffer.readUInt32LE(offset + 42);
    const entryName = buffer.subarray(offset + 46, offset + 46 + fileNameLength).toString("utf8");
    offset += 46 + fileNameLength + extraLength + commentLength;

    if (!entryName || entryName.endsWith("/")) {
      mkdirp(safeJoin(destDir, entryName || "."));
      continue;
    }
    if (buffer.readUInt32LE(localHeaderOffset) !== 0x04034b50) {
      throw new Error(`invalid zip local header for ${entryName}`);
    }
    const localNameLength = buffer.readUInt16LE(localHeaderOffset + 26);
    const localExtraLength = buffer.readUInt16LE(localHeaderOffset + 28);
    const dataStart = localHeaderOffset + 30 + localNameLength + localExtraLength;
    const compressed = buffer.subarray(dataStart, dataStart + compressedSize);
    let data;
    if (method === 0) {
      data = compressed;
    } else if (method === 8) {
      data = zlib.inflateRawSync(compressed);
    } else {
      throw new Error(`unsupported zip compression method ${method} for ${entryName}`);
    }
    const target = safeJoin(destDir, entryName);
    mkdirp(path.dirname(target));
    fs.writeFileSync(target, data);
  }
}

function copyDir(source, target) {
  mkdirp(target);
  for (const entry of fs.readdirSync(source, { withFileTypes: true })) {
    const src = path.join(source, entry.name);
    const dst = path.join(target, entry.name);
    if (entry.isDirectory()) {
      copyDir(src, dst);
    } else if (entry.isFile()) {
      mkdirp(path.dirname(dst));
      fs.copyFileSync(src, dst);
    }
  }
}

function directoryDigest(root) {
  if (!root || !fs.existsSync(root) || !fs.statSync(root).isDirectory()) {
    return "";
  }
  const entries = [];
  const walk = dir => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      const rel = path.relative(root, full).split(path.sep).join("/");
      entries.push({ full, rel, isDir: entry.isDirectory(), isFile: entry.isFile() });
      if (entry.isDirectory()) {
        walk(full);
      }
    }
  };
  walk(root);
  const digest = crypto.createHash("sha256");
  for (const entry of entries.sort((left, right) => left.rel.localeCompare(right.rel))) {
    if (entry.isDir) {
      digest.update(`dir:${entry.rel}\n`);
    } else if (entry.isFile) {
      digest.update(`file:${entry.rel}\n`);
      digest.update(fs.readFileSync(entry.full));
      digest.update("\n");
    }
  }
  return `sha256:${digest.digest("hex")}`;
}

function findSkillDirs(root) {
  const result = [];
  const walk = dir => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const target = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (fs.existsSync(path.join(target, "SKILL.md"))) {
          result.push(target);
        } else {
          walk(target);
        }
      }
    }
  };
  if (fs.existsSync(root)) {
    walk(root);
  }
  return result;
}

function canonicalRuntimeRole(role) {
  const normalized = String(role || "worker").trim().toLowerCase().replace(/_/g, "-");
  if (["leader", "team-leader", "teamleader"].includes(normalized)) return "leader";
  if (["remote-member", "remote-member-worker"].includes(normalized)) return "remote-member";
  if (normalized === "manager") return "manager";
  return "worker";
}

function parseRoleList(value) {
  return String(value || "")
    .replace(/^\[/, "")
    .replace(/\]$/, "")
    .split(",")
    .map(item => item.trim().replace(/^['"]|['"]$/g, ""))
    .filter(Boolean)
    .map(canonicalRuntimeRole);
}

function loadRuntimeSkillManifest(pluginDir) {
  const manifestPath = path.join(pluginDir, "teamharness-assets", "plugin.yaml");
  if (!fs.existsSync(manifestPath)) return [];
  const entries = [];
  let current = null;
  for (const line of fs.readFileSync(manifestPath, "utf8").split(/\r?\n/)) {
    const id = line.match(/^\s*-\s+id:\s*([A-Za-z0-9_.-]+)/);
    if (id) {
      current = { id: id[1], roles: [] };
      entries.push(current);
      continue;
    }
    if (!current) continue;
    const roles = line.match(/^\s+roles:\s*(\[.*\])\s*$/);
    if (roles) current.roles = parseRoleList(roles[1]);
  }
  return entries.filter(entry => entry.id);
}

function filterRuntimePluginSkills(overlayDir, role) {
  const entries = loadRuntimeSkillManifest(overlayDir);
  if (!entries.length) return;
  const currentRole = canonicalRuntimeRole(role);
  const known = new Set();
  const allowed = new Set();
  for (const entry of entries) {
    known.add(entry.id);
    if (!entry.roles.length || entry.roles.includes(currentRole)) {
      allowed.add(entry.id);
    }
  }
  const skillsDir = path.join(overlayDir, "skills");
  if (!fs.existsSync(skillsDir)) return;
  for (const entry of fs.readdirSync(skillsDir, { withFileTypes: true })) {
    if (entry.name.startsWith(".") || !known.has(entry.name) || allowed.has(entry.name)) continue;
    rmrf(path.join(skillsDir, entry.name));
  }
}

function copyAgentPackageIntoPlugin(packageDir, pluginDir) {
  const contextDir = path.join(pluginDir, ".teamharness", "runtime-context");
  mkdirp(contextDir);
  for (const name of ["AGENTS.md", "SOUL.md"]) {
    const candidates = [
      path.join(packageDir, "config", name),
      path.join(packageDir, name)
    ];
    const source = candidates.find(candidate => fs.existsSync(candidate));
    if (source) {
      fs.copyFileSync(source, path.join(contextDir, name));
    }
  }
  const skillsRoot = path.join(pluginDir, "skills");
  for (const skillDir of findSkillDirs(packageDir)) {
    const skillId = path.basename(skillDir);
    if (!/^[\w.-]+$/.test(skillId)) {
      continue;
    }
    const target = path.join(skillsRoot, skillId);
    rmrf(target);
    copyDir(skillDir, target);
  }
}

function packageViewBasePrefixes(runtime) {
  return corePackageView.packageViewBasePrefixes(runtime);
}

async function syncPackageFileToView(sts, source, objectKey) {
  return corePackageView.syncPackageFileToView(sts, source, objectKey);
}

async function syncPackageSkillsToView(sts, packageDir, skillsPrefix) {
  return corePackageView.syncPackageSkillsToView(sts, packageDir, skillsPrefix);
}

async function syncAgentPackageView(args, runtimeState, packageState, packageDir, sts) {
  return corePackageView.syncAgentPackageView(args, runtimeState, packageState, packageDir, sts);
}

async function syncAgentPackageViewIfNeeded(args, runtimeState, state, packageDir, sts) {
  return corePackageView.syncAgentPackageViewIfNeeded(args, runtimeState, state, packageDir, sts);
}

function basePluginDir(args) {
  const base = args.basePluginDir || args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!base) return "";
  args.basePluginDir = base;
  return base;
}

function buildRuntimePluginOverlay(args, packageDir) {
  const base = basePluginDir(args);
  if (!base || !fs.existsSync(base)) {
    throw new Error("plugin dir is required to build runtime plugin overlay");
  }
  const runtimeRoot = path.join(args.stateDir, "runtime-plugin");
  const current = path.join(runtimeRoot, "current");
  const tmp = path.join(runtimeRoot, `.current.tmp-${crypto.randomUUID()}`);
  const old = path.join(runtimeRoot, `.current.old-${crypto.randomUUID()}`);
  mkdirp(runtimeRoot);
  copyDir(base, tmp);
  if (packageDir) {
    copyAgentPackageIntoPlugin(packageDir, tmp);
  }
  filterRuntimePluginSkills(tmp, args.runtimeState?.runtime?.member?.role || "remote-member");
  if (fs.existsSync(current)) {
    fs.renameSync(current, old);
  }
  fs.renameSync(tmp, current);
  rmrf(old);
  args.pluginDir = current;
  refreshRuntimePluginWiring(args);
  return current;
}

function agentPackageStatePath(args) {
  return path.join(args.stateDir, "agent-package-state.json");
}

function desiredAgentPackageRef(runtimeState) {
  return String(runtimeState.runtime?.desired?.agentPackage?.ref || "").trim();
}

function appliedAgentPackageDigest(state) {
  if (!state || typeof state !== "object") return "";
  if (state.status === "staged") return String(state.appliedDigest || "");
  return String(state.digest || "");
}

function canReusePackageRoot(state, ref) {
  return Boolean(
    state &&
      ["applied", "staged"].includes(String(state.status || "")) &&
      state.ref === ref &&
      state.packageRoot &&
      fs.existsSync(state.packageRoot)
  );
}

async function ensureAgentPackageDownloaded(args, runtimeState, sts, ref, previous, baseDigest) {
  const objectKey = String(ref).replace(/^oss:\/\//, "").replace(/^\/+/, "");
  if (canReusePackageRoot(previous, ref)) {
    logEvent("info", "agent_package_reused", {
      runtime: args.runtime,
      ref,
      digest: previous.digest || "",
      packageRoot: previous.packageRoot
    });
    return {
      ref,
      objectKey: previous.objectKey || objectKey,
      digest: previous.digest || "",
      basePluginDigest: baseDigest,
      packageRoot: previous.packageRoot,
      viewSync: previous.viewSync
    };
  }
  const zipBytes = await ossFetch("GET", sts, objectKey);
  const digest = crypto.createHash("sha256").update(zipBytes).digest("hex");
  const packageRoot = path.join(args.stateDir, "agent-package", "versions", digest);
  const marker = path.join(packageRoot, ".extracted");
  if (!fs.existsSync(marker)) {
    extractZip(zipBytes, packageRoot);
    fs.writeFileSync(marker, `${new Date().toISOString()}\n`);
  }
  logEvent("info", "agent_package_downloaded", {
    runtime: args.runtime,
    ref,
    objectKey,
    digest,
    bytes: zipBytes.length,
    packageRoot
  });
  return {
    ref,
    objectKey,
    digest,
    basePluginDigest: baseDigest,
    packageRoot,
    viewSync: undefined
  };
}

async function stageAgentPackage(args, runtimeState, sts) {
  const ref = desiredAgentPackageRef(runtimeState);
  if (!ref) {
    return runtimeState.agentPackage || readJson(agentPackageStatePath(args), {});
  }
  const previous = runtimeState.agentPackage || readJson(agentPackageStatePath(args), {});
  if (previous.status === "applied" && previous.ref === ref && previous.packageRoot && fs.existsSync(previous.packageRoot)) {
    return previous;
  }
  if (previous.status === "staged" && previous.ref === ref && previous.viewSync?.status === "synced") {
    return previous;
  }
  const baseDigest = directoryDigest(basePluginDir(args));
  if (!String(ref).startsWith("oss://")) {
    const state = {
      status: "skipped",
      errorCode: "UnsupportedAgentPackageRef",
      ref,
      digest: "",
      basePluginDigest: baseDigest,
      appliedDigest: appliedAgentPackageDigest(previous),
      updatedAt: new Date().toISOString()
    };
    writeJson(agentPackageStatePath(args), state);
    runtimeState.agentPackage = state;
    writeStatus(args, "Degraded", "UnsupportedAgentPackageRef", `unsupported agent package ref: ${ref}`);
    logEvent("warn", "agent_package_ref_unsupported", { runtime: args.runtime, ref });
    return state;
  }
  const state = {
    status: "staged",
    ...(await ensureAgentPackageDownloaded(args, runtimeState, sts, ref, previous, baseDigest)),
    appliedRef: previous.status === "applied" ? previous.ref : previous.appliedRef || "",
    appliedDigest: appliedAgentPackageDigest(previous),
    updatedAt: new Date().toISOString()
  };
  writeJson(agentPackageStatePath(args), state);
  runtimeState.agentPackage = state;
  const synced = await syncAgentPackageViewIfNeeded(args, runtimeState, state, state.packageRoot, sts);
  if (synced.viewSync?.status === "failed") {
    writeStatus(args, "Degraded", "AgentPackageViewSyncFailed", synced.viewSync.message || "AgentSpec package view sync failed", {
      packageRef: ref,
      packageDigest: synced.digest
    });
    logEvent("warn", "agent_package_view_sync_failed", {
      runtime: args.runtime,
      ref,
      digest: synced.digest,
      message: synced.viewSync.message || ""
    });
  } else {
    writeStatus(args, "Running", "AgentPackageStaged", "AgentSpec package staged; next Matrix task will apply it", {
      packageRef: ref,
      packageDigest: synced.digest
    });
    logEvent("info", "agent_package_staged", {
      runtime: args.runtime,
      ref,
      digest: synced.digest
    });
  }
  return synced;
}

async function applyAgentPackage(args, runtimeState, sts) {
  const ref = desiredAgentPackageRef(runtimeState);
  const previous = runtimeState.agentPackage || readJson(agentPackageStatePath(args), {});
  const baseDigest = directoryDigest(basePluginDir(args));
  if (!ref) {
    if (previous.status === "base" && previous.basePluginDigest === baseDigest && previous.overlayDir && fs.existsSync(previous.overlayDir)) {
      args.pluginDir = previous.overlayDir;
      refreshRuntimePluginWiring(args);
      runtimeState.agentPackage = previous;
      return previous;
    }
    const overlayDir = buildRuntimePluginOverlay(args, null);
    const state = {
      status: "base",
      ref: "",
      digest: "",
      basePluginDigest: baseDigest,
      overlayDir,
      updatedAt: new Date().toISOString()
    };
    writeJson(agentPackageStatePath(args), state);
    runtimeState.agentPackage = state;
    logEvent("info", "agent_package_base_applied", {
      runtime: args.runtime,
      overlayDir
    });
    return state;
  }
  if (!String(ref).startsWith("oss://")) {
    const overlayDir = buildRuntimePluginOverlay(args, null);
    const state = {
      status: "skipped",
      errorCode: "UnsupportedAgentPackageRef",
      ref,
      digest: "",
      basePluginDigest: baseDigest,
      overlayDir,
      updatedAt: new Date().toISOString()
    };
    writeJson(agentPackageStatePath(args), state);
    runtimeState.agentPackage = state;
    writeStatus(args, "Degraded", "UnsupportedAgentPackageRef", `unsupported agent package ref: ${ref}`);
    logEvent("warn", "agent_package_ref_unsupported", { runtime: args.runtime, ref });
    return state;
  }
  if (previous.status === "applied" && previous.ref === ref && previous.overlayDir && fs.existsSync(previous.overlayDir)) {
    if (previous.packageRoot && fs.existsSync(previous.packageRoot)) {
      if (previous.basePluginDigest === baseDigest) {
        args.pluginDir = previous.overlayDir;
        refreshRuntimePluginWiring(args);
        runtimeState.agentPackage = previous;
        logEvent("info", "agent_package_applied_reused", {
          runtime: args.runtime,
          ref,
          digest: previous.digest || "",
          overlayDir: previous.overlayDir
        });
        return syncAgentPackageViewIfNeeded(args, runtimeState, previous, previous.packageRoot, sts);
      }
      const overlayDir = buildRuntimePluginOverlay(args, previous.packageRoot);
      const state = {
        ...previous,
        basePluginDigest: baseDigest,
        overlayDir,
        updatedAt: new Date().toISOString()
      };
      writeJson(agentPackageStatePath(args), state);
      runtimeState.agentPackage = state;
      logEvent("info", "agent_package_overlay_rebuilt", {
        runtime: args.runtime,
        ref,
        digest: previous.digest || "",
        overlayDir
      });
      return syncAgentPackageViewIfNeeded(args, runtimeState, state, previous.packageRoot, sts);
    }
    args.pluginDir = previous.overlayDir;
    refreshRuntimePluginWiring(args);
    runtimeState.agentPackage = previous;
    logEvent("info", "agent_package_overlay_reused", {
      runtime: args.runtime,
      ref,
      overlayDir: previous.overlayDir
    });
    return previous;
  }
  const source = await ensureAgentPackageDownloaded(args, runtimeState, sts, ref, previous, baseDigest);
  const overlayDir = buildRuntimePluginOverlay(args, source.packageRoot);
  const state = {
    status: "applied",
    ref,
    objectKey: source.objectKey,
    digest: source.digest,
    basePluginDigest: source.basePluginDigest,
    packageRoot: source.packageRoot,
    viewSync: source.viewSync,
    overlayDir,
    updatedAt: new Date().toISOString()
  };
  writeJson(agentPackageStatePath(args), state);
  runtimeState.agentPackage = state;
  writeStatus(args, "Running", "AgentPackageReady", "AgentSpec package applied", {
    packageRef: ref,
    packageDigest: source.digest
  });
  logEvent("info", "agent_package_applied", {
    runtime: args.runtime,
    ref,
    digest: source.digest,
    overlayDir
  });
  return syncAgentPackageViewIfNeeded(args, runtimeState, state, source.packageRoot, sts);
}

async function reportHeartbeat(args, edge) {
  return coreController.reportHeartbeat(args, edge);
}

async function matrixRequest(method, homeserver, token, requestPath, body) {
  return coreMatrix.matrixRequest(method, homeserver, token, requestPath, body);
}

function txnId(prefix) {
  return coreMatrix.txnId(prefix);
}

function escapeHtml(value) {
  return coreMatrix.escapeHtml(value);
}

function renderInlineMarkdown(value) {
  return coreMatrix.renderInlineMarkdown(value);
}

function matrixFormattedBody(text) {
  return coreMatrix.matrixFormattedBody(text);
}

function matrixEditFallbackHtml(text) {
  return coreMatrix.matrixEditFallbackHtml(text);
}

async function matrixSendMessage(edge, runtimeState, roomId, body, options) {
  return coreMatrix.matrixSendMessage(edge, runtimeState, roomId, body, options);
}

async function matrixJoin(edge, runtimeState, roomId) {
  return coreMatrix.matrixJoin(edge, runtimeState, roomId);
}

async function matrixTyping(edge, runtimeState, roomId, typing) {
  return coreMatrix.matrixTyping(edge, runtimeState, roomId, typing);
}

function eventBody(event) {
  return coreMatrix.eventBody(event);
}

function shouldHandleEvent(event, roomId, edge, runtimeState) {
  return coreMatrix.shouldHandleEvent(event, roomId, edge, runtimeState);
}

function rolePromptName(role) {
  const normalized = String(role || "remote-member").toLowerCase().replace(/_/g, "-");
  if (["worker", "team-worker"].includes(normalized)) return "worker";
  if (["leader", "team-leader", "teamleader"].includes(normalized)) return "leader";
  return "remote-member";
}

function runtimeRolePrompt(args, runtimeState) {
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!pluginDir) return "";
  const role = runtimeState.runtime?.member?.role || "remote-member";
  return [
    path.join(pluginDir, "prompts", "team", "TEAMS.md"),
    path.join(pluginDir, "prompts", "agent", `${rolePromptName(role)}.md`)
  ]
    .map(promptPath => fs.existsSync(promptPath) ? fs.readFileSync(promptPath, "utf8").trim() : "")
    .filter(Boolean)
    .join("\n\n");
}

function runtimeContextPromptSections(args) {
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!pluginDir) {
    return [];
  }
  const contextDir = path.join(pluginDir, ".teamharness", "runtime-context");
  const sections = [];
  for (const [name, title] of [["AGENTS.md", "Agent instructions"], ["SOUL.md", "Soul"]]) {
    const target = path.join(contextDir, name);
    if (fs.existsSync(target)) {
      const text = fs.readFileSync(target, "utf8").trim();
      if (text) {
        sections.push(`${title}:\n${text}`);
      }
    }
  }
  return sections;
}

function stableRuntimeMetadata(runtimeState, roleFallback) {
  const runtime = runtimeState?.runtime || {};
  const member = runtime.member || {};
  const team = runtime.team || {};
  const metadata = [];
  const teamName = runtimeState?.edge?.teamName || team.name || team.teamName;
  if (teamName) metadata.push({ label: "Team", value: teamName });
  metadata.push({ label: "Member", value: member.name });
  metadata.push({ label: "Runtime name", value: member.runtimeName });
  metadata.push({ label: "Role", value: member.role || roleFallback });
  return metadata;
}

function buildStablePrompt(args, runtimeState) {
  const rolePrompt = runtimeRolePrompt(args, runtimeState);
  return corePrompt.buildStableRuntimePrompt({
    introLines: rolePrompt ? [rolePrompt, ""] : [
      "You are a remote-managed HiClaw TeamHarness Claude Code member.",
      ""
    ],
    metadata: stableRuntimeMetadata(runtimeState, "remote-member"),
    guidanceLines: [
      "Reply to the current Matrix room directly with the final useful answer.",
      "If you need TeamHarness MCP tools and they are still connecting, call WaitForMcpServers before reporting that MCP is unavailable."
    ],
    contextSections: runtimeContextPromptSections(args),
    runtimeState
  });
}

function buildPrompt(args, event, roomHistory, runtimeState, roomId) {
  const currentMessage = eventBody(event);
  return corePrompt.buildMatrixTurnPrompt({
    metadata: corePrompt.matrixTurnMetadata({ event, roomId }),
    taskPathHint: corePrompt.extractTaskPathHint(currentMessage),
    roomHistory,
    currentMessage
  });
}

function currentMessageFromTurn(turn) {
  const messages = Array.isArray(turn?.messages) ? turn.messages : [];
  if (messages.length <= 1) return String(messages[0]?.body || "");
  return messages.map((message, index) => {
    const sender = String(message?.sender || "").trim();
    const body = String(message?.body || "").trim();
    return sender ? `[${index + 1}] ${sender}: ${body}` : `[${index + 1}] ${body}`;
  }).join("\n\n");
}

function eventFromTurn(turn) {
  const messages = Array.isArray(turn?.messages) ? turn.messages : [];
  const last = messages[messages.length - 1] || {};
  return {
    type: last.type || "m.room.message",
    event_id: last.eventId || last.event_id || turn?.turnId || "",
    sender: last.sender || "",
    origin_server_ts: last.originServerTs || last.origin_server_ts || 0,
    content: {
      ...(last.content && typeof last.content === "object" ? last.content : {}),
      body: currentMessageFromTurn(turn)
    }
  };
}

function buildPromptForTurn(args, turn, runtimeState) {
  const event = eventFromTurn(turn);
  const currentMessage = eventBody(event);
  return corePrompt.buildMatrixTurnPrompt({
    metadata: corePrompt.matrixTurnMetadata({ event, roomId: turn.roomId }),
    taskPathHint: corePrompt.extractTaskPathHint(currentMessage),
    roomHistory: turn.history || [],
    currentMessage
  });
}

function llmEnv(edge, runtimeState) {
  const args = runtimeState.args || {};
  const nativeModelConfig = usesNativeModelConfig(args);
  const buildRuntimeEnv = nativeModelConfig ? coreEnv.buildNativeConfigRuntimeEnv : coreEnv.buildManagedRuntimeEnv;
  const env = buildRuntimeEnv(process.env, {
    runtime: "claude-code",
    edge,
    runtimeState,
    roleFallback: "remote-member",
    brokerDescriptor: process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR || ""
  });
  if (!nativeModelConfig) {
    for (const key of [
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
      "ANTHROPIC_BASE_URL",
      "ANTHROPIC_DEFAULT_HAIKU_MODEL",
      "ANTHROPIC_DEFAULT_OPUS_MODEL",
      "ANTHROPIC_DEFAULT_SONNET_MODEL",
      "ANTHROPIC_MODEL",
      "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"
    ]) {
      delete env[key];
    }
    env.CLAUDE_CONFIG_DIR = managedClaudeConfigDir(args).toString();
  }
  return env;
}

function managedClaudeConfigDir(args) {
  return path.join(args.stateDir || DEFAULT_STATE_DIR, "claude-config");
}

function managedPromptDir(args) {
  return path.join(args.stateDir || DEFAULT_STATE_DIR, "managed");
}

function writeManagedClaudeSystemPrompt(args, runtimeState) {
  requireClaudeSettingsSupport(args);
  const promptDir = managedPromptDir(args);
  mkdirp(promptDir);
  const promptPath = path.join(promptDir, "claude-system-prompt.md");
  fs.writeFileSync(promptPath, buildStablePrompt(args, runtimeState), { mode: 0o600 });
  return promptPath;
}

function writeExecutable(file, content) {
  mkdirp(path.dirname(file));
  fs.writeFileSync(file, content, { mode: 0o700 });
}

function writeManagedModelKeyHelper(args, pluginDir) {
  const descriptorPath = path.join(pluginDir, ".teamharness", "credential-broker.json");
  const helperPath = path.join(managedClaudeConfigDir(args), "model-key-helper.js");
  writeExecutable(helperPath, `#!/usr/bin/env node
"use strict";
const fs = require("fs");
const http = require("http");
const https = require("https");
const descriptor = JSON.parse(fs.readFileSync(${JSON.stringify(descriptorPath)}, "utf8"));
const token = fs.readFileSync(String(descriptor.tokenFile), "utf8").trim();
const url = String(descriptor.endpoint).replace(/\\/+$/, "") + "/v1/credentials/model";
const client = url.startsWith("https:") ? https : http;
const request = client.request(url, {
  method: "GET",
  timeout: 5000,
  headers: {Accept: "application/json", Authorization: "Bearer " + token}
}, response => {
  const chunks = [];
  response.on("data", chunk => chunks.push(chunk));
  response.on("end", () => {
    const body = Buffer.concat(chunks).toString("utf8");
    if (response.statusCode < 200 || response.statusCode >= 300) {
      console.error(body || "model credential request failed");
      process.exit(1);
    }
    const payload = JSON.parse(body || "{}");
    process.stdout.write(String(payload.apiKey || "") + "\\n");
  });
});
request.on("timeout", () => request.destroy(new Error("model credential request timed out")));
request.on("error", error => {
  console.error(error && error.message ? error.message : String(error));
  process.exit(1);
});
request.end();
`);
  return helperPath;
}

function loongsuitePilotDataDir() {
  return process.env.LOONGSUITE_PILOT_DATA_DIR || path.join(os.homedir(), ".loongsuite-pilot");
}

function loongsuiteClaudeTraceHooks() {
  const hookPath = path.join(loongsuitePilotDataDir(), "hooks", "claude-code-loongsuite-pilot-hook.sh");
  if (!fs.existsSync(hookPath)) return {};
  const hookFor = subcommand => [{
    matcher: "*",
    hooks: [{
      type: "command",
      command: `${shellQuote(hookPath)} ${subcommand}`
    }]
  }];
  return {
    hooks: {
      Stop: hookFor("stop"),
      SubagentStart: hookFor("subagent-start"),
      SubagentStop: hookFor("subagent-stop")
    }
  };
}

function writeManagedClaudeSettings(args, edge, runtimeState, pluginDir) {
  const llm = claudeManagedLlmConfig(edge, runtimeState);
  if (!llm.model || !llm.baseUrl || !llm.apiKey) return "";
  requireClaudeSettingsSupport(args);
  const configDir = managedClaudeConfigDir(args);
  mkdirp(configDir);
  const helperPath = writeManagedModelKeyHelper(args, pluginDir);
  const settingsPath = path.join(configDir, "settings.json");
  writeJson(settingsPath, {
    apiKeyHelper: helperPath,
    env: {
      ANTHROPIC_BASE_URL: llm.baseUrl,
      ANTHROPIC_MODEL: llm.model,
      ANTHROPIC_DEFAULT_SONNET_MODEL: llm.model,
      ANTHROPIC_DEFAULT_OPUS_MODEL: llm.model,
      ANTHROPIC_DEFAULT_HAIKU_MODEL: llm.model,
      CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST: "1"
    },
    ...loongsuiteClaudeTraceHooks()
  });
  return settingsPath;
}

const PATH_BLOCK_BEGIN = "# >>> LoongSuite AgentTeams managed PATH >>>";
const PATH_BLOCK_END = "# <<< LoongSuite AgentTeams managed PATH <<<";

function scopeValue(value) {
  const normalized = String(value || "local").trim().toLowerCase();
  if (!["local", "global"].includes(normalized)) {
    throw new Error(`scope must be local or global, got ${value}`);
  }
  return normalized;
}

function modelScopeValue(value) {
  const normalized = String(value || "native-config").trim().toLowerCase();
  if (!["managed-runtime", "managed-global", "native-config"].includes(normalized)) {
    throw new Error(`model config mode must be managed-runtime, managed-global, or native-config, got ${value}`);
  }
  return normalized;
}

function selectedModelConfigMode(args) {
  return modelScopeValue(args.modelConfigMode);
}

function usesNativeModelConfig(args) {
  return selectedModelConfigMode(args) === "native-config";
}

function shellQuote(value) {
  return `'${String(value).replace(/'/g, "'\\''")}'`;
}

function managedBinDir() {
  return process.env.LOONGSUITE_MANAGED_BIN_DIR || path.join(os.homedir(), ".loongsuite-pilot", "bin");
}

function managedClaudeDir() {
  return process.env.LOONGSUITE_MANAGED_CLAUDE_DIR || path.join(os.homedir(), ".loongsuite-pilot", "claude-code");
}

function managedProfilePath() {
  if (process.env.TEAMHARNESS_MANAGED_PROFILE) return process.env.TEAMHARNESS_MANAGED_PROFILE;
  const shell = process.env.SHELL || "";
  if (shell.endsWith("zsh")) return path.join(os.homedir(), ".zshrc");
  if (shell.endsWith("bash")) return path.join(os.homedir(), ".bashrc");
  return path.join(os.homedir(), ".profile");
}

function stripManagedPathBlock(text) {
  const pattern = new RegExp(`\\n?${PATH_BLOCK_BEGIN.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\n[\\s\\S]*?\\n${PATH_BLOCK_END.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\n?`, "g");
  const cleaned = String(text || "").replace(pattern, "\n").trimEnd();
  return cleaned ? `${cleaned}\n` : "";
}

function writeManagedPathBlock() {
  const profile = managedProfilePath();
  let current = "";
  try {
    current = fs.readFileSync(profile, "utf8");
  } catch {
    current = "";
  }
  const block = [
    PATH_BLOCK_BEGIN,
    `export PATH=${shellQuote(managedBinDir())}:$PATH`,
    PATH_BLOCK_END,
    ""
  ].join("\n");
  mkdirp(path.dirname(profile));
  fs.writeFileSync(profile, `${stripManagedPathBlock(current)}${block}`);
  return profile;
}

function removeManagedPathBlock(profile) {
  if (!profile || !fs.existsSync(profile)) return;
  fs.writeFileSync(profile, stripManagedPathBlock(fs.readFileSync(profile, "utf8")));
}

function launcherShimPath() {
  return path.join(managedBinDir(), "claude");
}

function launcherDescriptorPath() {
  return path.join(managedClaudeDir(), "launcher.json");
}

function samePath(left, right) {
  try {
    return fs.realpathSync(left) === fs.realpathSync(right);
  } catch {
    return path.resolve(left) === path.resolve(right);
  }
}

function resolveClaudeCommand(args) {
  const command = args.claudeCommand || "claude";
  const exclude = launcherShimPath();
  const candidates = [];
  if (command.includes(path.sep)) {
    candidates.push(command);
  } else {
    const result = spawnSync("which", ["-a", command], { encoding: "utf8" });
    if (result.status === 0) {
      candidates.push(...String(result.stdout || "").split(/\r?\n/).filter(Boolean));
    }
  }
  for (const candidate of candidates) {
    if (candidate && fs.existsSync(candidate) && !samePath(candidate, exclude)) {
      return candidate;
    }
  }
  throw new Error(`Claude Code command not found: ${command}`);
}

function parseSemver(value) {
  const match = String(value || "").match(/\b(\d+)\.(\d+)\.(\d+)\b/);
  return match ? [Number(match[1]), Number(match[2]), Number(match[3])] : null;
}

function semverLessThan(left, right) {
  for (let index = 0; index < 3; index += 1) {
    if (left[index] !== right[index]) {
      return left[index] < right[index];
    }
  }
  return false;
}

function requireClaudeSettingsSupport(args) {
  if (args.claudeSettingsChecked) return;
  const command = resolveClaudeCommand(args);
  const versionResult = spawnSync(command, ["--version"], { encoding: "utf8", timeout: 10000 });
  const helpResult = spawnSync(command, ["--help"], { encoding: "utf8", timeout: 10000 });
  if (versionResult.error || helpResult.error) {
    const error = versionResult.error || helpResult.error;
    const message = `failed to inspect Claude Code command ${command}: ${error.message}`;
    writeStatus(args, "Failed", "UnsupportedClaudeCode", message);
    throw new Error(message);
  }
  const version = parseSemver(`${versionResult.stdout || ""}\n${versionResult.stderr || ""}`);
  if (version && semverLessThan(version, MIN_CLAUDE_CODE_SETTINGS_VERSION)) {
    const minimum = MIN_CLAUDE_CODE_SETTINGS_VERSION.join(".");
    const message = `Claude Code ${minimum}+ is required for managed --settings/apiKeyHelper injection`;
    writeStatus(args, "Failed", "UnsupportedClaudeCode", message);
    throw new Error(message);
  }
  const helpText = `${helpResult.stdout || ""}\n${helpResult.stderr || ""}`;
  if (helpResult.status !== 0 || !helpText.includes("--settings")) {
    const message = "Claude Code command does not support --settings; please upgrade Claude Code";
    writeStatus(args, "Failed", "UnsupportedClaudeCode", message);
    throw new Error(message);
  }
  if (!helpText.includes("--append-system-prompt-file") && !helpText.includes("--append-system-prompt[-file]")) {
    const message = "Claude Code command does not support --append-system-prompt-file; please upgrade Claude Code";
    writeStatus(args, "Failed", "UnsupportedClaudeCode", message);
    throw new Error(message);
  }
  args.claudeSettingsChecked = true;
}

function cleanupGlobalIntegrations(args) {
  const descriptorPath = launcherDescriptorPath();
  const heartbeatPath = path.join(args.stateDir, "heartbeat");
  let owned = false;
  let shimPath = "";
  try {
    const descriptor = readJson(descriptorPath, {});
    if (descriptor.owner === "agentteams") {
      owned = true;
      shimPath = String(descriptor.shimPath || "");
      removeManagedPathBlock(descriptor.profilePath);
    }
  } catch {
    // Best effort.
  }
  for (const file of [owned ? descriptorPath : "", owned ? shimPath : "", heartbeatPath]) {
    if (!file) continue;
    removeFileQuietly(file);
  }
}

function writeLauncherShim(realClaudePath, descriptorPath) {
  writeExecutable(launcherShimPath(), `#!/usr/bin/env node
"use strict";
const fs = require("fs");
const path = require("path");
const {spawnSync} = require("child_process");
const realClaude = ${JSON.stringify(realClaudePath)};
const descriptorPath = ${JSON.stringify(descriptorPath)};
const PATH_BLOCK_BEGIN = ${JSON.stringify(PATH_BLOCK_BEGIN)};
const PATH_BLOCK_END = ${JSON.stringify(PATH_BLOCK_END)};
function readJson(file) { return JSON.parse(fs.readFileSync(file, "utf8")); }
function workerAlive(pid) {
  try { process.kill(Number(pid), 0); return true; } catch { return false; }
}
function pluginDirAlreadyPresent(argv, pluginDir) {
  for (let i = 0; i < argv.length; i += 1) {
    let value = "";
    if (argv[i] === "--plugin-dir" && argv[i + 1]) value = argv[i + 1];
    if (argv[i].startsWith("--plugin-dir=")) value = argv[i].slice("--plugin-dir=".length);
    if (value && require("path").resolve(value) === require("path").resolve(pluginDir)) return true;
  }
  return false;
}
function stripManagedPathBlock(text) {
  const escape = value => String(value).replace(/[.*+?^\\x24{}()|[\\]\\\\]/g, "\\\\$&");
  const pattern = new RegExp("\\\\n?" + escape(PATH_BLOCK_BEGIN) + "\\\\n[\\\\s\\\\S]*?\\\\n" + escape(PATH_BLOCK_END) + "\\\\n?", "g");
  const cleaned = String(text || "").replace(pattern, "\\n").trimEnd();
  return cleaned ? cleaned + "\\n" : "";
}
function cleanupProfile(profile) {
  if (!profile || !fs.existsSync(profile)) return;
  try { fs.writeFileSync(profile, stripManagedPathBlock(fs.readFileSync(profile, "utf8"))); } catch {}
}
function cleanupStaleLauncher(descriptor) {
  cleanupProfile(descriptor.profilePath);
  for (const file of [descriptor.heartbeatFile, descriptorPath, descriptor.shimPath]) {
    if (!file) continue;
    try { fs.rmSync(file, {force: true}); } catch {}
  }
}
function runReal(argv, env) {
  const result = spawnSync(realClaude, argv, {stdio: "inherit", env});
  process.exit(result.status === null ? 1 : result.status);
}
function fallbackReal(descriptor, cleanup) {
  if (cleanup && descriptor && typeof descriptor === "object") cleanupStaleLauncher(descriptor);
  runReal(process.argv.slice(2), process.env);
}
function managedArgs(argv, model, pluginDir, settingsPath) {
  const result = [];
  for (let i = 0; i < argv.length; i += 1) {
    if (model && argv[i] === "--model") { i += 1; continue; }
    if (model && argv[i].startsWith("--model=")) continue;
    if (settingsPath && argv[i] === "--settings") { i += 1; continue; }
    if (settingsPath && argv[i].startsWith("--settings=")) continue;
    result.push(argv[i]);
  }
  if (pluginDir && !pluginDirAlreadyPresent(argv, pluginDir)) result.push("--plugin-dir", pluginDir);
  if (settingsPath) result.push("--settings", settingsPath);
  if (model) result.push("--model", model);
  return result;
}
function fetchModel(brokerDescriptorPath) {
  const descriptor = readJson(brokerDescriptorPath);
  const token = fs.readFileSync(String(descriptor.tokenFile), "utf8").trim();
  const url = String(descriptor.endpoint).replace(/\\/+$/, "") + "/v1/credentials/model";
  const script = 'const http=require("http"),https=require("https");const url=process.argv[1],token=process.argv[2];const c=url.startsWith("https:")?https:http;const r=c.request(url,{method:"GET",timeout:5000,headers:{Accept:"application/json",Authorization:"Bearer "+token}},res=>{const a=[];res.on("data",d=>a.push(d));res.on("end",()=>{if(res.statusCode<200||res.statusCode>=300)process.exit(2);process.stdout.write(Buffer.concat(a).toString("utf8"))})});r.on("error",()=>process.exit(3));r.end();';
  const result = spawnSync(process.execPath, ["-e", script, url, token], {encoding: "utf8"});
  if (result.status !== 0) return null;
  const payload = JSON.parse(result.stdout || "{}");
  if (!payload.model || !payload.baseUrl || !payload.apiKey) return null;
  return payload;
}
function writePrivateFile(file, content, mode) {
  fs.mkdirSync(path.dirname(file), {recursive: true});
  fs.writeFileSync(file, content, {mode});
  try { fs.chmodSync(file, mode); } catch {}
}
function writeModelKeyHelper(helperPath, brokerDescriptorPath) {
  const content = [
    "#!/usr/bin/env node",
    "\\"use strict\\";",
    "const fs = require(\\"fs\\");",
    "const http = require(\\"http\\");",
    "const https = require(\\"https\\");",
    "const descriptor = JSON.parse(fs.readFileSync(" + JSON.stringify(String(brokerDescriptorPath)) + ", \\"utf8\\"));",
    "const token = fs.readFileSync(String(descriptor.tokenFile), \\"utf8\\").trim();",
    "const url = String(descriptor.endpoint).replace(/\\\\/+$/, \\"\\") + \\"/v1/credentials/model\\";",
    "const client = url.startsWith(\\"https:\\") ? https : http;",
    "const request = client.request(url, {",
    "  method: \\"GET\\",",
    "  timeout: 5000,",
    "  headers: {Accept: \\"application/json\\", Authorization: \\"Bearer \\" + token}",
    "}, response => {",
    "  const chunks = [];",
    "  response.on(\\"data\\", chunk => chunks.push(chunk));",
    "  response.on(\\"end\\", () => {",
    "    const body = Buffer.concat(chunks).toString(\\"utf8\\");",
    "    if (response.statusCode < 200 || response.statusCode >= 300) {",
    "      console.error(body || \\"model credential request failed\\");",
    "      process.exit(1);",
    "    }",
    "    const payload = JSON.parse(body || \\"{}\\");",
    "    process.stdout.write(String(payload.apiKey || \\"\\") + \\"\\\\n\\");",
    "  });",
    "});",
    "request.on(\\"timeout\\", () => request.destroy(new Error(\\"model credential request timed out\\")));",
    "request.on(\\"error\\", error => {",
    "  console.error(error && error.message ? error.message : String(error));",
    "  process.exit(1);",
    "});",
    "request.end();"
  ].join("\\n") + "\\n";
  writePrivateFile(helperPath, content, 0o700);
}
function writeManagedSettings(configDir, brokerDescriptorPath, settings) {
  if (!configDir || !settings || !settings.model || !settings.baseUrl) return "";
  fs.mkdirSync(configDir, {recursive: true});
  const helperPath = path.join(configDir, "model-key-helper.js");
  writeModelKeyHelper(helperPath, brokerDescriptorPath);
  const settingsPath = path.join(configDir, "settings.json");
  let existing = {};
  try { existing = readJson(settingsPath); } catch {}
  const next = {
    ...existing,
    apiKeyHelper: helperPath,
    env: {
      ANTHROPIC_BASE_URL: settings.baseUrl,
      ANTHROPIC_MODEL: settings.model,
      ANTHROPIC_DEFAULT_SONNET_MODEL: settings.model,
      ANTHROPIC_DEFAULT_OPUS_MODEL: settings.model,
      ANTHROPIC_DEFAULT_HAIKU_MODEL: settings.model,
      CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST: "1"
    }
  };
  writePrivateFile(settingsPath, JSON.stringify(next, null, 2), 0o600);
  return settingsPath;
}
function clearManagedModelEnv(env) {
  for (const key of [
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL",
    "ANTHROPIC_DEFAULT_OPUS_MODEL",
    "ANTHROPIC_DEFAULT_SONNET_MODEL",
    "ANTHROPIC_MODEL",
    "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"
  ]) {
    delete env[key];
  }
}
try {
  const descriptor = readJson(descriptorPath);
  if (descriptor.owner !== "agentteams" || descriptor.mode !== "env-shim") fallbackReal(descriptor, false);
  if (descriptor.pluginDir && !fs.existsSync(descriptor.pluginDir)) fallbackReal(descriptor, true);
  if (!workerAlive(descriptor.workerPid)) fallbackReal(descriptor, true);
  if (descriptor.validUntilEpoch && Date.now() / 1000 > Number(descriptor.validUntilEpoch)) fallbackReal(descriptor, true);
  if (descriptor.heartbeatFile && fs.existsSync(descriptor.heartbeatFile) && Date.now() - fs.statSync(descriptor.heartbeatFile).mtimeMs > 300000) fallbackReal(descriptor, true);
  if (!descriptor.brokerDescriptor || !fs.existsSync(descriptor.brokerDescriptor)) fallbackReal(descriptor, true);
  const env = {...process.env};
  if (descriptor.configDir) {
    fs.mkdirSync(descriptor.configDir, {recursive: true});
    env.CLAUDE_CONFIG_DIR = descriptor.configDir;
  }
  let model = "";
  let settingsPath = "";
  if (descriptor.modelInjection) {
    const settings = fetchModel(descriptor.brokerDescriptor);
    if (settings) {
      clearManagedModelEnv(env);
      env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR = descriptor.brokerDescriptor;
      settingsPath = writeManagedSettings(descriptor.configDir, descriptor.brokerDescriptor, settings);
      model = settings.model;
    }
  }
  const result = spawnSync(realClaude, managedArgs(process.argv.slice(2), model, descriptor.pluginDir || "", settingsPath), {stdio: "inherit", env});
  process.exit(result.status === null ? 1 : result.status);
} catch {
  runReal(process.argv.slice(2), process.env);
}
`);
}

function writeLauncherHeartbeat(args) {
  mkdirp(args.stateDir);
  fs.writeFileSync(path.join(args.stateDir, "heartbeat"), `${Math.floor(Date.now() / 1000)}\n`, { mode: 0o600 });
  const descriptorPath = launcherDescriptorPath();
  const descriptor = readJson(descriptorPath, null);
  if (descriptor && descriptor.owner === "agentteams") {
    descriptor.validUntilEpoch = Math.floor(Date.now() / 1000) + 300;
    descriptor.updatedAt = new Date().toISOString();
    writeJson(descriptorPath, descriptor);
  }
}

function applyGlobalIntegrations(args, edge, runtimeState) {
  const pluginScope = scopeValue(args.pluginInstallScope);
  const modelScope = selectedModelConfigMode(args);
  if (pluginScope === "local" && modelScope !== "managed-global") {
    cleanupGlobalIntegrations(args);
    return;
  }
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!pluginDir || !fs.existsSync(pluginDir)) {
    throw new Error("plugin dir is required for global Claude Code launcher injection");
  }
  if (modelScope === "managed-global") {
    const llm = claudeManagedLlmConfig(edge, runtimeState);
    if (!llm.model || !llm.baseUrl || !llm.apiKey) {
      throw new Error("desired.model model/gatewayUrl/gatewayKey is required for managed-global model config");
    }
  }
  const descriptorPath = launcherDescriptorPath();
  const realClaudePath = resolveClaudeCommand(args);
  const brokerDescriptor = path.join(pluginDir, ".teamharness", "credential-broker.json");
  mkdirp(managedBinDir());
  mkdirp(managedClaudeDir());
  writeLauncherShim(realClaudePath, descriptorPath);
  const profilePath = writeManagedPathBlock();
  writeLauncherHeartbeat(args);
  writeJson(descriptorPath, {
    version: 1,
    owner: "agentteams",
    mode: "env-shim",
    realClaudePath,
    shimPath: launcherShimPath(),
    brokerDescriptor,
    modelInjection: modelScope === "managed-global",
    pluginDir: pluginScope === "global" ? pluginDir : "",
    workerPid: process.pid,
    heartbeatFile: path.join(args.stateDir, "heartbeat"),
    configDir: managedClaudeConfigDir(args),
    profilePath,
    validUntilEpoch: Math.floor(Date.now() / 1000) + 300,
    instanceId: args.instanceId || "",
    updatedAt: new Date().toISOString()
  });
  writeStatus(args, "Running", "ClaudeLauncherGlobalReady", "Claude Code launcher state ready");
}

function truncateMatrixThreadText(text, limit) {
  const value = String(text || "");
  const max = limit || 2000;
  if (value.length <= max) return value;
  return `${value.slice(0, max - 20).trimEnd()}\n...(truncated)`;
}

const SENSITIVE_TOOL_INPUT_KEY = /(?:token|secret|password|credential|authorization|api[_-]?key|access[_-]?key|refresh[_-]?token)/i;

function sanitizeToolInput(value, depth) {
  if (depth > 4) return "...";
  if (value === null || value === undefined) return value;
  if (typeof value === "string") return truncateMatrixThreadText(value, 500);
  if (typeof value !== "object") return value;
  if (Array.isArray(value)) {
    const items = value.slice(0, 20).map(item => sanitizeToolInput(item, depth + 1));
    if (value.length > 20) items.push("...(truncated)");
    return items;
  }
  const entries = Object.entries(value);
  const result = {};
  for (const [key, item] of entries.slice(0, 30)) {
    result[key] = SENSITIVE_TOOL_INPUT_KEY.test(key) ? "[REDACTED]" : sanitizeToolInput(item, depth + 1);
  }
  if (entries.length > 30) result.__truncated__ = true;
  return result;
}

function toolInputFromBlock(block) {
  const input = block.input ?? block.arguments ?? block.tool_input;
  if (typeof input === "string") {
    try {
      return JSON.parse(input);
    } catch {
      return input;
    }
  }
  return input;
}

function formatToolInput(block) {
  const input = toolInputFromBlock(block);
  if (input === null || input === undefined || input === "") return "";
  const sanitized = sanitizeToolInput(input, 0);
  const json = JSON.stringify(sanitized, null, 2);
  if (!json || json === "{}") return "";
  return `\n\n\`\`\`json\n${truncateMatrixThreadText(json, 2000)}\n\`\`\``;
}

function assistantContentBlocks(event) {
  const message = event && (event.message || event);
  return message && Array.isArray(message.content) ? message.content : [];
}

function visibleAssistantSummaries(event) {
  if (!event || event.type !== "assistant") return [];
  const summaries = [];
  for (const block of assistantContentBlocks(event)) {
    if (!block || typeof block !== "object") continue;
    const blockType = String(block.type || "");
    let text = "";
    if (["thinking", "reasoning", "reasoning_content"].includes(blockType)) {
      for (const key of ["summary", "text", "content"]) {
        if (typeof block[key] === "string" && block[key].trim()) {
          text = block[key].trim();
          break;
        }
      }
    } else if (blockType === "text" && typeof block.text === "string") {
      text = block.text.trim();
    }
    if (text) {
      summaries.push(`Thinking:\n\n${truncateMatrixThreadText(text)}`);
    }
  }
  return summaries;
}

function toolUseSummaries(event) {
  if (!event || event.type !== "assistant") return [];
  const summaries = [];
  for (const block of assistantContentBlocks(event)) {
    if (!block || typeof block !== "object" || block.type !== "tool_use") continue;
    const name = String(block.name || "").trim();
    if (name) {
      summaries.push(`tool_use: \`${name}\`${formatToolInput(block)}`);
    }
  }
  return summaries;
}

function isClaudeApiErrorText(value) {
  const text = String(value || "").toLowerCase();
  return text.includes("api error") || text.includes("api_error") || text.includes("invalid api key") || text.includes("无效的api key");
}

function isMissingClaudeSessionError(value) {
  const text = String(value || "").toLowerCase();
  return text.includes("no conversation found with session id")
    || text.includes("no conversation found with session");
}

function eventResultText(event) {
  if (!event || typeof event !== "object") return "";
  for (const key of ["result", "text", "message"]) {
    if (typeof event[key] === "string" && event[key].trim()) return event[key].trim();
  }
  const message = event.message || event;
  if (message && Array.isArray(message.content)) {
    const texts = message.content
      .map(block => block && typeof block.text === "string" ? block.text.trim() : "")
      .filter(Boolean);
    if (texts.length) return texts[texts.length - 1];
  }
  return "";
}

function extractClaudeText(line, collector) {
  const stripped = String(line || "").trim();
  if (!stripped) {
    return null;
  }
  try {
    const event = JSON.parse(stripped);
    if (event.isApiErrorMessage || event.api_error_status || event.apiErrorStatus) {
      collector.apiErrorText = eventResultText(event) || stripped;
      if (event.session_id) {
        collector.sessionId = String(event.session_id);
      }
      return null;
    }
    if (event.type === "result") {
      collector.result = String(event.result || event.text || event.message || collector.result || "");
      if (event.session_id) {
        collector.sessionId = String(event.session_id);
      }
      return event;
    }
    const message = event.message || event;
    if (message && Array.isArray(message.content)) {
      let sawApiErrorText = false;
      for (const block of message.content) {
        if (block && block.type === "text" && block.text) {
          const text = String(block.text);
          if (isClaudeApiErrorText(text)) {
            collector.apiErrorText = text;
            sawApiErrorText = true;
          } else {
            collector.parts.push(text);
          }
        }
      }
      if (sawApiErrorText) {
        return null;
      }
    }
    if (event.session_id) {
      collector.sessionId = String(event.session_id);
    }
    return event;
  } catch {
    collector.parts.push(stripped);
    return null;
  }
}

function claudePermissionMode() {
  const configured = process.env.TEAMHARNESS_CLAUDE_PERMISSION_MODE;
  if (configured) return configured;
  return "bypassPermissions";
}

function abortError() {
  const error = new Error("Matrix command stopped current session");
  error.name = "AbortError";
  return error;
}

function bindAbortSignal(child, signal) {
  if (!signal || typeof signal.addEventListener !== "function") return () => {};
  let forceKillTimer = null;
  const abort = () => {
    try {
      child.kill("SIGTERM");
    } catch (_) {
      // Best effort; the process may already have exited.
    }
    forceKillTimer = setTimeout(() => {
      try {
        child.kill("SIGKILL");
      } catch (_) {
        // Best effort.
      }
    }, 2000);
    if (typeof forceKillTimer.unref === "function") forceKillTimer.unref();
  };
  if (signal.aborted) abort();
  signal.addEventListener("abort", abort, { once: true });
  return () => {
    signal.removeEventListener("abort", abort);
    if (forceKillTimer) clearTimeout(forceKillTimer);
  };
}

async function runClaudeForEvent(args, edge, runtimeState, roomId, event, roomHistory, sessionId, eventContext) {
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!pluginDir) {
    throw new Error("plugin dir is required before running Claude Code");
  }
  const desiredModel = runtimeState.runtime?.desired?.model?.model;
  const baseCommand = [
    args.claudeCommand,
    "--plugin-dir",
    pluginDir,
    "--output-format",
    "stream-json",
    "--verbose"
  ];
  const permissionMode = claudePermissionMode();
  if (permissionMode) {
    baseCommand.push("--permission-mode", permissionMode);
  }
  baseCommand.push(
    "--allowedTools",
    process.env.TEAMHARNESS_CLAUDE_ALLOWED_TOOLS || [
      "Bash",
      "WaitForMcpServers",
      "mcp__plugin_teamharness-claude-code_teamharness__filesync",
      "mcp__plugin_teamharness-claude-code_teamharness__health",
      "mcp__plugin_teamharness-claude-code_teamharness__message",
      "mcp__plugin_teamharness-claude-code_teamharness__projectflow",
      "mcp__plugin_teamharness-claude-code_teamharness__roomflow",
      "mcp__plugin_teamharness-claude-code_teamharness__taskflow"
    ].join(",")
  );
  const useNativeModelConfig = usesNativeModelConfig(args);
  const settingsPath = useNativeModelConfig ? "" : writeManagedClaudeSettings(args, edge, runtimeState, pluginDir);
  if (settingsPath) {
    baseCommand.push("--settings", settingsPath);
  }
  if (!useNativeModelConfig && desiredModel) {
    baseCommand.push("--model", String(desiredModel));
  }
  const systemPromptPath = writeManagedClaudeSystemPrompt(args, runtimeState);
  baseCommand.push("--append-system-prompt-file", systemPromptPath);
  const prompt = eventContext?.turn
    ? buildPromptForTurn(args, eventContext.turn, runtimeState)
    : buildPrompt(args, event, roomHistory, runtimeState, roomId);

  const runOnce = async currentSessionId => {
    if (eventContext?.abortSignal?.aborted) throw abortError();
    const command = [...baseCommand];
    if (currentSessionId) {
      command.push("--resume", currentSessionId);
    }
    const child = spawn(command[0], command.slice(1), {
      cwd: args.workDir,
      env: llmEnv(edge, runtimeState),
      stdio: ["pipe", "pipe", "pipe"]
    });
    const unbindAbort = bindAbortSignal(child, eventContext?.abortSignal);
    child.stdin.end(prompt);
    const collector = { parts: [], result: "", sessionId: currentSessionId || "", apiErrorText: "" };
    let stderr = "";
    let stdoutBuffer = "";
    const streamSends = [];
    const handleStdoutLine = line => {
      const streamEvent = extractClaudeText(line, collector);
      if (!streamEvent) return;
      for (const summary of [...visibleAssistantSummaries(streamEvent), ...toolUseSummaries(streamEvent)]) {
        streamSends.push(matrixSendMessage(edge, runtimeState, roomId, summary, {
          msgtype: "m.notice",
          threadRootEventId: eventContext?.placeholderEventId
        }).catch(() => {}));
      }
    };
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", chunk => {
      stdoutBuffer += String(chunk);
      const lines = stdoutBuffer.split(/\r?\n/);
      stdoutBuffer = lines.pop() || "";
      for (const line of lines) {
        handleStdoutLine(line);
      }
    });
    child.stderr.on("data", chunk => {
      stderr += chunk;
    });
    let code = await new Promise(resolve => child.on("close", resolve));
    unbindAbort();
    if (eventContext?.abortSignal?.aborted) throw abortError();
    if (stdoutBuffer.trim()) {
      handleStdoutLine(stdoutBuffer);
    }
    await Promise.all(streamSends);
    if (collector.apiErrorText) {
      if (code === 0) code = 1;
      if (!isClaudeApiErrorText(stderr)) {
        stderr = collector.apiErrorText;
      }
    }
    const errorText = String(stderr || collector.apiErrorText || collector.result || collector.parts.join("\n") || "");
    const finalText = code === 0
      ? (collector.result || collector.parts.join("\n").trim() || stderr.trim() || `Claude Code exited with code ${code}`)
      : `Claude Code failed with exit code ${code}: ${errorText.slice(0, 500)}`;
    return {
      text: finalText,
      sessionId: collector.sessionId,
      msgtype: code === 0 ? "m.text" : "m.notice",
      formatted: code === 0,
      missingSession: Boolean(currentSessionId && code !== 0 && isMissingClaudeSessionError(errorText))
    };
  };

  const first = await runOnce(sessionId);
  if (!first.missingSession) {
    return first;
  }
  const retried = await runOnce("");
  return {
    ...retried,
    dropSession: true
  };
}

async function runClaudeForTurn(args, edge, runtimeState, turn) {
  const event = eventFromTurn(turn);
  return runClaudeForEvent(args, edge, runtimeState, turn.roomId, event, turn.history || [], turn.sessionId || "", {
    placeholderEventId: turn.placeholderEventId || "",
    abortSignal: turn.abortSignal,
    turn
  });
}

async function matrixLoop(args, edge, runtimeState) {
  return coreMatrix.runMatrixLoop(args, edge, runtimeState, {
    prepareRuntimeForTask,
    runForTurn: runClaudeForTurn,
    sessionStore: matrixState => matrixState.claudeSessions,
    sessionForRoom: claudeSessionForRoom,
    storeSessionForRoom: storeClaudeSessionForRoom,
    dropSessionForRoom: dropClaudeSessionForRoom,
    failurePrefix: "Claude Code 执行失败："
  });
}

async function startBroker(args, edge, sts, runtimeState) {
  return coreBroker.startBroker(args, edge, sts, runtimeState, {
    runtime: "claude-code",
    modelCredentialsEnabled: !usesNativeModelConfig(args),
    modelConfig: claudeManagedLlmConfig,
    writeDescriptor: writeCredentialBrokerDescriptor,
    refreshMcpConfig: refreshPluginMcpConfig,
    clearMcpNeedsAuthCache: clearPluginMcpNeedsAuthCache
  });
}

function writeCredentialBrokerDescriptor(args, descriptor) {
  const payload = { ...descriptor, updatedAt: new Date().toISOString() };
  const descriptorFile = path.join(args.stateDir, "credential-broker.json");
  writeJson(descriptorFile, payload);
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (pluginDir) {
    writeJson(path.join(pluginDir, ".teamharness", "credential-broker.json"), payload);
  }
  return descriptorFile;
}

function refreshRuntimePluginWiring(args) {
  refreshPluginMcpConfig(args);
  if (args.runtimeState?.brokerDescriptor) {
    writeCredentialBrokerDescriptor(args, args.runtimeState.brokerDescriptor);
  }
}

function refreshPluginMcpConfig(args) {
  const pluginDir = args.pluginDir || process.env.TEAMHARNESS_CLAUDE_PLUGIN_DIR || "";
  if (!pluginDir) {
    return;
  }
  const nodeBin = process.env.TEAMHARNESS_NODE_BIN || process.execPath || "node";
  const nodeRuntimeCoreDir = process.env.TEAMHARNESS_NODE_RUNTIME_CORE_DIR || "";
  writeJson(path.join(pluginDir, ".mcp.json"), {
    mcpServers: {
      teamharness: {
        command: nodeBin,
        args: [path.join(pluginDir, "teamharness-assets", "mcp", "server.js")],
        cwd: path.join(pluginDir, "teamharness-assets"),
        env: {
          PATH: process.env.PATH || "",
          TEAMHARNESS_NODE_RUNTIME_CORE_DIR: nodeRuntimeCoreDir
        }
      }
    }
  });
}

function clearPluginMcpNeedsAuthCache() {
  const file = path.join(os.homedir(), ".claude", "mcp-needs-auth-cache.json");
  try {
    if (!fs.existsSync(file)) {
      return;
    }
    const payload = JSON.parse(fs.readFileSync(file, "utf8") || "{}");
    delete payload["plugin:teamharness-claude-code:teamharness"];
    if (Object.keys(payload).length) {
      writeJson(file, payload);
    } else {
      removeFileQuietly(file);
    }
  } catch {
    // Cache cleanup is best-effort; Claude Code can recreate it if auth is truly required.
  }
}

async function main() {
  return coreLifecycle.runWorkerMain(process.argv.slice(2), {
    parseArgs,
    command: args => args.claudeCommand,
    commandMissingReason: "ClaudeCodeNotFound",
    commandMissingMessage: args => `Claude Code command not found: ${args.claudeCommand}`,
    afterRuntimeLoaded: args => {
      basePluginDir(args);
    },
    applyAgentPackage,
    startModelProxy,
    startBroker,
    applyGlobalIntegrations,
    cleanupBrokerFiles,
    cleanupGlobalIntegrations,
    startRemotePeriodicTasks,
    matrixLoop,
    readyMessage: "Node claude-code worker is running"
  });
}

main().catch(error => {
  console.error(`claude-code-worker: ${error.message}`);
  process.exitCode = 1;
});
