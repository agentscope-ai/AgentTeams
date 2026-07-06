#!/usr/bin/env node
"use strict";

const fs = require("fs");
const crypto = require("crypto");
const zlib = require("zlib");
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

const DEFAULT_STATE_DIR = ".agentteams/runtime/openclaw";
const LOONGSUITE_OPENCLAW_TRACE_PLUGIN_ID = "opentelemetry-instrumentation-openclaw";

function parseArgs(argv) {
  return coreArgs.parseRuntimeArgs(argv, {
    runtime: "openclaw",
    defaultStateDir: DEFAULT_STATE_DIR,
    pluginEnvVar: "TEAMHARNESS_OPENCLAW_ASSETS_DIR",
    pluginArgAliases: ["--plugin-dir", "--assets-dir"],
    commandName: "openclawCommand",
    commandEnvVars: ["TEAMHARNESS_OPENCLAW_COMMAND", "OPENCLAW_BIN"],
    commandDefault: "openclaw",
    commandArg: "--openclaw-command",
    commandSearchPaths: [
      process.env.HOME ? path.join(process.env.HOME, ".local/bin/openclaw") : "",
      "/usr/local/bin/openclaw"
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
  const workspace = currentWorkspaceDir(args);
  if (workspace) {
    removeFileQuietly(path.join(workspace, ".teamharness", "credential-broker.json"));
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

function shortHash(value, length) {
  return crypto.createHash("sha256").update(String(value || "")).digest("base64url").slice(0, length || 12);
}

function openclawSessionStoreKey(roomId) {
  return String(roomId || "");
}

function openclawWorkerIdentity(args, runtimeState, edge) {
  const member = runtimeState?.runtime?.member || {};
  return args.instanceId || member.name || member.runtimeName || edge?.workerName || path.resolve(args.workDir || ".");
}

function newOpenClawRuntimeSessionId(roomId, args, runtimeState, edge) {
  const workerHash = shortHash(openclawWorkerIdentity(args, runtimeState, edge), 12);
  const roomHash = shortHash(roomId, 12);
  const epoch = `${Date.now().toString(36)}${crypto.randomBytes(3).toString("base64url")}`;
  return `oc_${workerHash}_${roomHash}_${epoch}`;
}

function openclawSessionForRoom(sessions, roomId, args, runtimeState, edge) {
  const key = openclawSessionStoreKey(roomId);
  const current = sessions[key];
  if (typeof current === "string" && current.startsWith("oc_")) return current;
  if (current && typeof current === "object" && typeof current.sessionId === "string" && current.sessionId.startsWith("oc_")) {
    return current.sessionId;
  }
  sessions[key] = newOpenClawRuntimeSessionId(roomId, args, runtimeState, edge);
  return sessions[key];
}

function storeOpenClawSessionForRoom(sessions, roomId, args, sessionId) {
  if (sessionId && String(sessionId).startsWith("oc_")) {
    sessions[openclawSessionStoreKey(roomId)] = sessionId;
  }
}

function dropOpenClawSessionForRoom(sessions, roomId, args, runtimeState, edge) {
  sessions[openclawSessionStoreKey(roomId)] = newOpenClawRuntimeSessionId(roomId, args, runtimeState, edge);
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

function managedLlmConfig(edge, runtimeState) {
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
  if (args.modelProxy) {
    refreshOpenClawConfig(args, edge, runtimeState);
  }
  if (runtimeState.brokerDescriptor) {
    writeCredentialBrokerDescriptor(args, runtimeState.brokerDescriptor);
  }
  applyGlobalIntegrations(args, edge, runtimeState);
  if (previousDigest && nextPackage && previousDigest !== nextPackage.digest) {
    runtimeState.sessionResetAt = new Date().toISOString();
    writeStatus(args, "Running", "AgentPackageChanged", "AgentSpec package changed; future OpenClaw sessions use the new package", {
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
    const externalAttrs = buffer.readUInt32LE(offset + 38);
    const localHeaderOffset = buffer.readUInt32LE(offset + 42);
    const entryName = buffer.subarray(offset + 46, offset + 46 + fileNameLength).toString("utf8");
    offset += 46 + fileNameLength + extraLength + commentLength;

    const unixMode = (externalAttrs >>> 16) & 0xffff;
    if ((unixMode & 0o170000) === 0o120000) {
      throw new Error(`unsafe zip symlink entry: ${entryName}`);
    }
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
    } else if (entry.isSymbolicLink()) {
      mkdirp(path.dirname(dst));
      removeFileQuietly(dst);
      fs.symlinkSync(fs.readlinkSync(src), dst);
    } else if (entry.isFile()) {
      mkdirp(path.dirname(dst));
      fs.copyFileSync(src, dst);
    }
  }
}

function chownTreeQuietly(target) {
  if (typeof process.getuid !== "function" || typeof process.getgid !== "function") return;
  const uid = process.getuid();
  const gid = process.getgid();
  const apply = file => {
    try {
      const stat = fs.lstatSync(file);
      if (stat.isDirectory()) {
        for (const entry of fs.readdirSync(file)) {
          apply(path.join(file, entry));
        }
      }
      if (stat.isSymbolicLink() && typeof fs.lchownSync === "function") {
        fs.lchownSync(file, uid, gid);
      } else {
        fs.chownSync(file, uid, gid);
      }
    } catch (_) {
      // Best effort only; OpenClaw will still surface a clear diagnostic if ownership is rejected.
    }
  };
  if (target && fs.existsSync(target)) apply(target);
}

function normalizeLoongSuiteOpenClawPackage(pluginDir) {
  const packagePath = path.join(pluginDir, "package.json");
  const runtimeEntry = path.join(pluginDir, "dist", "index.js");
  if (!fs.existsSync(packagePath) || !fs.existsSync(runtimeEntry)) return;
  try {
    const payload = JSON.parse(fs.readFileSync(packagePath, "utf8"));
    const extensions = payload.openclaw && Array.isArray(payload.openclaw.extensions)
      ? payload.openclaw.extensions
      : [];
    if (extensions.length === 1 && extensions[0] === "./dist/index.js") return;
    payload.openclaw = { extensions: ["./dist/index.js"] };
    fs.writeFileSync(packagePath, JSON.stringify(payload, null, 2) + "\n");
  } catch (_) {
    // Keep startup resilient; plugin validation will report malformed package metadata.
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

function readTextIfExists(file) {
  if (!file || !fs.existsSync(file) || !fs.statSync(file).isFile()) return "";
  return fs.readFileSync(file, "utf8").trim();
}

function basePluginDir(args) {
  const base = args.basePluginDir || args.pluginDir || process.env.TEAMHARNESS_OPENCLAW_ASSETS_DIR || "";
  if (!base) return "";
  args.basePluginDir = base;
  return base;
}

function loongSuiteOpenClawTracePluginDir(args) {
  const explicit = process.env.TEAMHARNESS_OPENCLAW_TRACE_PLUGIN_DIR || "";
  if (explicit && fs.existsSync(path.join(explicit, "openclaw.plugin.json"))) return explicit;

  const persistentRoot = path.join(args.stateDir, "openclaw", "plugins", "loongsuite-js");
  const persistentPlugin = path.join(persistentRoot, "opentelemetry-instrumentation-openclaw");
  const base = basePluginDir(args);
  const sourceRoot = base ? path.join(path.dirname(base), "loongsuite-js") : "";
  const sourcePlugin = sourceRoot ? path.join(sourceRoot, "opentelemetry-instrumentation-openclaw") : "";
  if (sourceRoot && fs.existsSync(path.join(sourcePlugin, "openclaw.plugin.json"))) {
    const sourceDigest = readTextIfExists(path.join(sourceRoot, ".teamharness-source-digest")) || directoryDigest(sourceRoot);
    const persistentDigest = readTextIfExists(path.join(persistentRoot, ".teamharness-source-digest"));
    if (!fs.existsSync(path.join(persistentPlugin, "openclaw.plugin.json")) || sourceDigest !== persistentDigest) {
      rmrf(persistentRoot);
      copyDir(sourceRoot, persistentRoot);
      if (sourceDigest) {
        fs.writeFileSync(path.join(persistentRoot, ".teamharness-source-digest"), `${sourceDigest}\n`);
      }
    }
    if (fs.existsSync(path.join(persistentPlugin, "openclaw.plugin.json"))) {
      normalizeLoongSuiteOpenClawPackage(persistentPlugin);
      chownTreeQuietly(persistentRoot);
      return persistentPlugin;
    }
  }
  if (fs.existsSync(path.join(persistentPlugin, "openclaw.plugin.json"))) {
    normalizeLoongSuiteOpenClawPackage(persistentPlugin);
    chownTreeQuietly(persistentRoot);
    return persistentPlugin;
  }
  return "";
}

function loongSuiteOpenClawTracePluginLoadPath(traceDir) {
  return traceDir;
}

function expandHomePath(file, env) {
  if (!file) return "";
  const sourceEnv = env || process.env;
  let expanded = String(file || "");
  expanded = expanded.replace(/\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?/g, (match, name) => {
    const value = sourceEnv[name] || process.env[name] || "";
    return value || match;
  });
  if (expanded === "~") return sourceEnv.HOME || process.env.HOME || "";
  if (expanded.startsWith("~/")) return path.join(sourceEnv.HOME || process.env.HOME || "", expanded.slice(2));
  return expanded;
}

function plainObject(value) {
  return Boolean(value && typeof value === "object" && !Array.isArray(value));
}

function cloneJsonValue(value) {
  if (value === undefined) return undefined;
  return JSON.parse(JSON.stringify(value));
}

function stringValue(value) {
  const text = String(value || "").trim();
  return text || "";
}

function readJsonFileIfExists(file, env) {
  const target = expandHomePath(file, env);
  if (!target || !fs.existsSync(target) || !fs.statSync(target).isFile()) return null;
  try {
    return JSON.parse(fs.readFileSync(target, "utf8"));
  } catch {
    return null;
  }
}

function buildOpenClawConfigDiscoveryEnv(args, includeShellProfile) {
  const options = {
    runtime: "openclaw",
    runtimeState: { args },
    roleFallback: "worker"
  };
  const env = includeShellProfile
    ? coreEnv.buildNativeConfigRuntimeEnv(process.env, options)
    : coreEnv.buildManagedRuntimeEnv(process.env, options);
  const workspace = currentWorkspaceDir(args);
  if (workspace) {
    env.OPENCLAW_WORKSPACE_DIR = workspace;
  }
  return env;
}

function openclawCommand(args) {
  return args.openclawCommand || process.env.TEAMHARNESS_OPENCLAW_COMMAND || process.env.OPENCLAW_BIN || "openclaw";
}

function resolveOpenClawConfigFile(args, env) {
  const result = spawnSync(openclawCommand(args), ["config", "file"], {
    cwd: args.workDir || process.cwd(),
    env,
    encoding: "utf8",
    timeout: 5000
  });
  if (result.error || result.status !== 0) return "";
  const stdout = String(result.stdout || "").trim();
  if (!stdout) return "";
  return stdout.split(/\r?\n/).map(line => line.trim()).filter(Boolean).pop() || "";
}

function readOpenClawConfigViaConfigFile(args, env) {
  const configPath = resolveOpenClawConfigFile(args, env);
  if (!configPath) return null;
  return readJsonFileIfExists(configPath, env);
}

function openclawUserConfigCandidates(args, envs) {
  const generatedConfig = path.resolve(openclawConfigPath(args));
  const candidates = [];
  const pushCandidate = (file, env) => {
    const clean = stringValue(file);
    if (!clean) return;
    const resolved = path.resolve(expandHomePath(clean, env));
    if (!resolved || resolved === generatedConfig || candidates.includes(resolved)) return;
    candidates.push(resolved);
  };

  pushCandidate(process.env.TEAMHARNESS_OPENCLAW_USER_CONFIG_PATH);
  const sources = Array.isArray(envs) && envs.length ? envs : [process.env];
  for (const env of sources) {
    pushCandidate(env.TEAMHARNESS_OPENCLAW_USER_CONFIG_PATH, env);
    pushCandidate(env.OPENCLAW_CONFIG_PATH, env);

    const userStateDir = stringValue(env.OPENCLAW_STATE_DIR);
    if (userStateDir) {
      const stateDir = expandHomePath(userStateDir, env);
      pushCandidate(path.join(stateDir, "openclaw.json"), env);
      pushCandidate(path.join(stateDir, "clawdbot.json"), env);
    }

    const userHome = expandHomePath(stringValue(env.OPENCLAW_HOME || env.HOME), env);
    if (userHome) {
      for (const dir of [path.join(userHome, ".openclaw"), path.join(userHome, ".clawdbot")]) {
        pushCandidate(path.join(dir, "openclaw.json"), env);
        pushCandidate(path.join(dir, "clawdbot.json"), env);
      }
    }
  }
  return candidates;
}

function readUserOpenClawConfig(args) {
  const currentEnv = buildOpenClawConfigDiscoveryEnv(args, false);
  const currentEnvConfig = readOpenClawConfigViaConfigFile(args, currentEnv);
  if (plainObject(currentEnvConfig)) return currentEnvConfig;

  const shellEnv = buildOpenClawConfigDiscoveryEnv(args, true);
  const shellEnvConfig = readOpenClawConfigViaConfigFile(args, shellEnv);
  if (plainObject(shellEnvConfig)) return shellEnvConfig;

  for (const candidate of openclawUserConfigCandidates(args, [currentEnv, shellEnv])) {
    const payload = readJsonFileIfExists(candidate);
    if (plainObject(payload)) return payload;
  }
  return null;
}

function inheritUserOpenClawModelConfig(config, args, agentId) {
  const userConfig = readUserOpenClawConfig(args);
  if (!plainObject(userConfig)) return;

  for (const key of ["models", "auth", "secrets"]) {
    if (plainObject(userConfig[key])) {
      config[key] = cloneJsonValue(userConfig[key]);
    }
  }

  const userAgents = plainObject(userConfig.agents) ? userConfig.agents : {};
  const userDefaults = plainObject(userAgents.defaults) ? userAgents.defaults : {};
  for (const key of ["model", "models"]) {
    if (Object.prototype.hasOwnProperty.call(userDefaults, key)) {
      config.agents.defaults[key] = cloneJsonValue(userDefaults[key]);
    }
  }

  const userAgent = Array.isArray(userAgents.list)
    ? userAgents.list.find(agent => plainObject(agent) && String(agent.id || "").trim() === agentId) ||
      userAgents.list.find(agent => plainObject(agent) && agent.default === true)
    : null;
  if (!plainObject(userAgent)) return;
  for (const key of ["model", "models"]) {
    if (Object.prototype.hasOwnProperty.call(userAgent, key)) {
      config.agents.list[0][key] = cloneJsonValue(userAgent[key]);
    }
  }
}

function extractArmsProject(endpoint) {
  try {
    return new URL(endpoint).hostname.split(".")[0] || "";
  } catch {
    return "";
  }
}

function loongSuiteOpenClawTraceEnv() {
  const env = {};
  for (const key of [
    "ARMS_OTLP_ENDPOINT",
    "ARMS_LICENSE_KEY",
    "ARMS_PROJECT",
    "ARMS_CMS_WORKSPACE",
    "ARMS_SERVICE_NAME",
    "ARMS_TRACE_DEBUG"
  ]) {
    if (process.env[key]) env[key] = process.env[key];
  }

  const configPath = process.env.AGENT_DATA_COLLECTION_CONFIG ||
    path.join(process.env.HOME || "", ".loongsuite-pilot", "config.json");
  const config = readJsonFileIfExists(configPath);
  if (!config || config.collectTrace === false) return env;

  const serviceNamePrefix = stringValue(process.env.LOONGSUITE_PILOT_SERVICE_NAME_PREFIX || config.serviceNamePrefix || "loongsuite-pilot");
  const openclawServiceName = `${serviceNamePrefix}-openclaw`;
  const otlpTrace = config.otlpTrace && typeof config.otlpTrace === "object" ? config.otlpTrace : null;
  const cms = config.cms && typeof config.cms === "object" ? config.cms : null;

  const otlpEndpoint = stringValue(process.env.LOONGSUITE_PILOT_OTLP_ENDPOINT || otlpTrace?.endpoint || "");
  if (otlpEndpoint) {
    env.ARMS_OTLP_ENDPOINT = env.ARMS_OTLP_ENDPOINT || otlpEndpoint;
    env.ARMS_SERVICE_NAME = env.ARMS_SERVICE_NAME || stringValue(otlpTrace?.serviceName || openclawServiceName);
    const headers = otlpTrace?.headers && typeof otlpTrace.headers === "object" ? otlpTrace.headers : {};
    env.ARMS_LICENSE_KEY = env.ARMS_LICENSE_KEY || stringValue(headers["x-arms-license-key"] || "");
    env.ARMS_PROJECT = env.ARMS_PROJECT || stringValue(headers["x-arms-project"] || "");
    env.ARMS_CMS_WORKSPACE = env.ARMS_CMS_WORKSPACE || stringValue(headers["x-cms-workspace"] || "");
    if (otlpTrace?.debug === true && !env.ARMS_TRACE_DEBUG) env.ARMS_TRACE_DEBUG = "true";
    return Object.fromEntries(Object.entries(env).filter(([, value]) => stringValue(value)));
  }

  const cmsEndpoint = stringValue(process.env.LOONGSUITE_PILOT_CMS_ENDPOINT || cms?.endpoint || "");
  const cmsLicense = stringValue(process.env.LOONGSUITE_PILOT_CMS_LICENSE_KEY || cms?.licenseKey || "");
  if (cmsEndpoint && cmsLicense) {
    env.ARMS_OTLP_ENDPOINT = env.ARMS_OTLP_ENDPOINT || cmsEndpoint;
    env.ARMS_LICENSE_KEY = env.ARMS_LICENSE_KEY || cmsLicense;
    env.ARMS_PROJECT = env.ARMS_PROJECT || extractArmsProject(cmsEndpoint);
    env.ARMS_CMS_WORKSPACE = env.ARMS_CMS_WORKSPACE || stringValue(process.env.LOONGSUITE_PILOT_CMS_WORKSPACE || cms?.workspace || "");
    env.ARMS_SERVICE_NAME = env.ARMS_SERVICE_NAME || openclawServiceName;
    if (cms?.debug === true && !env.ARMS_TRACE_DEBUG) env.ARMS_TRACE_DEBUG = "true";
  }

  return Object.fromEntries(Object.entries(env).filter(([, value]) => stringValue(value)));
}

function withOtelResourceAttribute(env, key, value) {
  const cleanKey = stringValue(key);
  const cleanValue = stringValue(value);
  if (!cleanKey || !cleanValue) return env;
  const entries = [];
  let replaced = false;
  for (const raw of String(env.OTEL_RESOURCE_ATTRIBUTES || "").split(",")) {
    const item = raw.trim();
    if (!item) continue;
    const index = item.indexOf("=");
    if (index <= 0) continue;
    const currentKey = item.slice(0, index).trim();
    if (currentKey === cleanKey) {
      entries.push(`${cleanKey}=${cleanValue}`);
      replaced = true;
    } else {
      entries.push(item);
    }
  }
  if (!replaced) entries.push(`${cleanKey}=${cleanValue}`);
  env.OTEL_RESOURCE_ATTRIBUTES = entries.join(",");
  return env;
}

function workspaceRoot(args) {
  return path.join(args.stateDir, "openclaw", "workspace");
}

function currentWorkspaceDir(args) {
  return path.join(workspaceRoot(args), "current");
}

function openclawConfigPath(args) {
  return path.join(args.stateDir, "openclaw", "config", "openclaw.json");
}

function openclawStateDir(args) {
  return path.join(args.stateDir, "openclaw", "state");
}

function brokerDescriptorPath(args) {
  return path.join(currentWorkspaceDir(args), ".teamharness", "credential-broker.json");
}

function normalizedRuntimeRole(role) {
  const normalized = String(role || "worker").toLowerCase().replace(/_/g, "-");
  if (["leader", "team-leader", "teamleader"].includes(normalized)) return "leader";
  if (["remote-member", "remote-member-worker"].includes(normalized)) return "remote-member";
  return "worker";
}

function parseRoleList(value) {
  return String(value || "")
    .replace(/^\[/, "")
    .replace(/\]$/, "")
    .split(",")
    .map(item => item.trim().replace(/^['"]|['"]$/g, ""))
    .filter(Boolean)
    .map(normalizedRuntimeRole);
}

function loadSkillManifest(args) {
  const base = basePluginDir(args);
  const file = path.join(base, "plugin.yaml");
  if (!base || !fs.existsSync(file)) return [];
  const entries = [];
  let current = null;
  for (const line of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
    const id = line.match(/^\s*-\s+id:\s*([A-Za-z0-9_.-]+)/);
    if (id) {
      current = { id: id[1], path: "", roles: [] };
      entries.push(current);
      continue;
    }
    if (!current) continue;
    const entryPath = line.match(/^\s+path:\s*(\S+)/);
    if (entryPath) {
      current.path = entryPath[1].replace(/^['"]|['"]$/g, "");
      continue;
    }
    const roles = line.match(/^\s+roles:\s*(\[.*\])\s*$/);
    if (roles) {
      current.roles = parseRoleList(roles[1]);
    }
  }
  return entries.filter(entry => entry.id && entry.path);
}

function copyAllowedBaseSkills(args, skillsRoot, role) {
  const base = basePluginDir(args);
  const allowlist = [];
  for (const entry of loadSkillManifest(args)) {
    if (entry.roles.length && !entry.roles.includes(role)) {
      continue;
    }
    const source = path.join(base, entry.path);
    if (!fs.existsSync(path.join(source, "SKILL.md"))) {
      continue;
    }
    const target = path.join(skillsRoot, entry.id);
    rmrf(target);
    copyDir(source, target);
    allowlist.push(entry.id);
  }
  return allowlist;
}

function copyPackageSkills(packageDir, skillsRoot) {
  const allowlist = [];
  if (!packageDir) return allowlist;
  for (const skillDir of findSkillDirs(packageDir)) {
    const skillId = path.basename(skillDir);
    if (!/^[\w.-]+$/.test(skillId)) continue;
    const target = path.join(skillsRoot, skillId);
    rmrf(target);
    copyDir(skillDir, target);
    allowlist.push(skillId);
  }
  return allowlist;
}

function packageContextFile(packageDir, name) {
  if (!packageDir) return "";
  return [
    path.join(packageDir, "config", name),
    path.join(packageDir, name)
  ].find(candidate => fs.existsSync(candidate) && fs.statSync(candidate).isFile()) || "";
}

function writeOpenClawWorkspace(args, runtimeState, packageDir, packageState) {
  const base = basePluginDir(args);
  if (!base || !fs.existsSync(base)) {
    throw new Error("OpenClaw assets dir is required to build managed workspace");
  }
  const runtime = runtimeState.runtime || {};
  const member = runtime.member || {};
  const role = normalizedRuntimeRole(member.role);
  const root = workspaceRoot(args);
  const current = currentWorkspaceDir(args);
  const tmp = path.join(root, `.current.tmp-${crypto.randomUUID()}`);
  const old = path.join(root, `.current.old-${crypto.randomUUID()}`);
  const skillsRoot = path.join(tmp, "skills");
  mkdirp(skillsRoot);

  const agentSections = [
    readTextIfExists(path.join(base, "prompts", "team", "TEAMS.md")),
    readTextIfExists(path.join(base, "prompts", "agent", `${role}.md`)),
    readTextIfExists(packageContextFile(packageDir, "AGENTS.md")),
    corePrompt.managedRuntimeContextBlock(runtimeState)
  ].filter(Boolean);
  fs.writeFileSync(path.join(tmp, "AGENTS.md"), `${agentSections.join("\n\n")}\n`);

  const soul = readTextIfExists(packageContextFile(packageDir, "SOUL.md"));
  fs.writeFileSync(path.join(tmp, "SOUL.md"), soul ? `${soul}\n` : "");

  const skillIds = new Set(copyAllowedBaseSkills(args, skillsRoot, role));
  for (const id of copyPackageSkills(packageDir, skillsRoot)) {
    skillIds.add(id);
  }
  const skillAllowlist = Array.from(skillIds).sort();

  mkdirp(path.join(tmp, ".teamharness"));
  writeJson(path.join(tmp, ".teamharness", "agent-package.json"), {
    status: packageState.status || "base",
    ref: packageState.ref || "",
    digest: packageState.digest || "",
    objectKey: packageState.objectKey || "",
    packageRoot: packageState.packageRoot || "",
    baseAssetsDigest: packageState.basePluginDigest || "",
    skills: skillAllowlist,
    updatedAt: new Date().toISOString()
  });

  mkdirp(root);
  if (fs.existsSync(current)) {
    fs.renameSync(current, old);
  }
  fs.renameSync(tmp, current);
  rmrf(old);

  args.openclawWorkspaceDir = current;
  args.openclawConfigPath = openclawConfigPath(args);
  args.openclawStateDir = openclawStateDir(args);
  runtimeState.skillAllowlist = skillAllowlist;
  runtimeState.openclawWorkspaceDir = current;
  return { workspaceDir: current, skillAllowlist };
}

function refreshOpenClawConfig(args, edge, runtimeState) {
  const base = basePluginDir(args);
  const runtime = runtimeState.runtime || {};
  const member = runtime.member || {};
  const desired = runtime.desired || {};
  const model = desired.model || {};
  const modelId = String(model.model || runtimeLlmConfig(edge, runtimeState).model || "default").trim();
  const workspace = currentWorkspaceDir(args);
  const nodeBin = process.env.TEAMHARNESS_NODE_BIN || process.execPath || "node";
  const bundledMcpServer = path.join(base, "mcp", "server.js");
  const sourceMcpServer = path.join(path.dirname(base), "node-mcp", "server.js");
  const mcpServer = fs.existsSync(bundledMcpServer) ? bundledMcpServer : sourceMcpServer;
  const proxyBaseUrl = args.modelProxy?.endpoint ? `${args.modelProxy.endpoint}/v1` : "";
  const agentId = String(member.name || member.runtimeName || edge.workerName || "worker").trim();
  const traceDir = loongSuiteOpenClawTracePluginDir(args);
  const traceLoadPath = loongSuiteOpenClawTracePluginLoadPath(traceDir);
  const traceEnv = loongSuiteOpenClawTraceEnv();
  const traceEnabled = Boolean(traceDir && traceEnv.ARMS_OTLP_ENDPOINT);
  const useNativeModelConfig = usesNativeModelConfig(args);
  const agentDefaults = {
    skipBootstrap: true,
    workspace
  };
  const agentConfig = {
    id: agentId,
    workspace,
    skills: runtimeState.skillAllowlist || []
  };
  if (!useNativeModelConfig) {
    agentDefaults.model = `teamharness/${modelId}`;
    agentConfig.model = `teamharness/${modelId}`;
  }

  mkdirp(openclawStateDir(args));
  const config = {
    agents: {
      defaults: agentDefaults,
      list: [agentConfig]
    },
    mcp: {
      servers: {
        teamharness: {
          command: nodeBin,
          args: [mcpServer],
          env: {
            TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR: brokerDescriptorPath(args),
            TEAMHARNESS_NODE_BIN: nodeBin
          },
          cwd: workspace
        }
      }
    },
    skills: {
      load: {
        extraDirs: []
      }
    },
    plugins: {
      load: {
        paths: traceLoadPath ? [traceLoadPath] : []
      },
      allow: [LOONGSUITE_OPENCLAW_TRACE_PLUGIN_ID],
      entries: {
        [LOONGSUITE_OPENCLAW_TRACE_PLUGIN_ID]: {
          enabled: traceEnabled,
          hooks: {
            allowConversationAccess: true
          },
          config: {
            batchSize: 10,
            flushIntervalMs: 5000
          }
        }
      }
    },
    tools: {
      profile: "coding",
      sandbox: {
        tools: {
          alsoAllow: ["bundle-mcp"]
        }
      }
    }
  };
  if (!useNativeModelConfig) {
    config.models = {
      providers: {
        teamharness: {
          baseUrl: proxyBaseUrl,
          apiKey: "teamharness-local-proxy",
          api: "openai-completions",
          agentRuntime: { id: "openclaw" },
          models: [
            {
              id: modelId,
              name: modelId
            }
          ]
        }
      }
    };
  } else {
    inheritUserOpenClawModelConfig(config, args, agentId);
  }
  writeJson(openclawConfigPath(args), config);
  args.openclawConfigPath = openclawConfigPath(args);
  args.openclawStateDir = openclawStateDir(args);
  args.openclawWorkspaceDir = workspace;
  return args.openclawConfigPath;
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
    const state = {
      status: "base",
      ref: "",
      digest: "",
      basePluginDigest: baseDigest,
      updatedAt: new Date().toISOString()
    };
    const workspace = writeOpenClawWorkspace(args, runtimeState, null, state);
    state.workspaceDir = workspace.workspaceDir;
    state.overlayDir = workspace.workspaceDir;
    state.skillAllowlist = workspace.skillAllowlist;
    writeJson(agentPackageStatePath(args), state);
    runtimeState.agentPackage = state;
    logEvent("info", "agent_package_base_applied", {
      runtime: args.runtime,
      workspaceDir: state.workspaceDir
    });
    return state;
  }
  if (!String(ref).startsWith("oss://")) {
    const state = {
      status: "skipped",
      errorCode: "UnsupportedAgentPackageRef",
      ref,
      digest: "",
      basePluginDigest: baseDigest,
      updatedAt: new Date().toISOString()
    };
    const workspace = writeOpenClawWorkspace(args, runtimeState, null, state);
    state.workspaceDir = workspace.workspaceDir;
    state.overlayDir = workspace.workspaceDir;
    state.skillAllowlist = workspace.skillAllowlist;
    writeJson(agentPackageStatePath(args), state);
    runtimeState.agentPackage = state;
    writeStatus(args, "Degraded", "UnsupportedAgentPackageRef", `unsupported agent package ref: ${ref}`);
    logEvent("warn", "agent_package_ref_unsupported", { runtime: args.runtime, ref });
    return state;
  }
  const source = await ensureAgentPackageDownloaded(args, runtimeState, sts, ref, previous, baseDigest);
  const state = {
    status: "applied",
    ref,
    objectKey: source.objectKey,
    digest: source.digest,
    basePluginDigest: source.basePluginDigest,
    packageRoot: source.packageRoot,
    viewSync: source.viewSync,
    updatedAt: new Date().toISOString()
  };
  const workspace = writeOpenClawWorkspace(args, runtimeState, source.packageRoot, state);
  state.workspaceDir = workspace.workspaceDir;
  state.overlayDir = workspace.workspaceDir;
  state.skillAllowlist = workspace.skillAllowlist;
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
    workspaceDir: state.workspaceDir
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
  const normalized = String(role || "worker").toLowerCase().replace(/_/g, "-");
  if (["worker", "team-worker"].includes(normalized)) return "worker";
  if (["leader", "team-leader", "teamleader"].includes(normalized)) return "leader";
  return "remote-member";
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

function openclawEnv(edge, runtimeState) {
  const args = runtimeState.args || {};
  const buildRuntimeEnv = usesNativeModelConfig(args) ? coreEnv.buildNativeConfigRuntimeEnv : coreEnv.buildManagedRuntimeEnv;
  const env = buildRuntimeEnv(process.env, {
    runtime: "openclaw",
    edge,
    runtimeState,
    roleFallback: "worker",
    brokerDescriptor: brokerDescriptorPath(args)
  });
  env.OPENCLAW_CONFIG_PATH = openclawConfigPath(args);
  env.OPENCLAW_STATE_DIR = openclawStateDir(args);
  env.OPENCLAW_WORKSPACE_DIR = currentWorkspaceDir(args);
  Object.assign(env, loongSuiteOpenClawTraceEnv());
  withOtelResourceAttribute(env, "agentteams.worker.name", env.AGENTTEAMS_WORKER_NAME);
  return env;
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

function managedOpenClawDir() {
  return process.env.LOONGSUITE_MANAGED_OPENCLAW_DIR ||
    path.join(process.env.HOME || "", ".loongsuite-pilot", "openclaw");
}

function stripManagedPathBlock(text) {
  const pattern = new RegExp(`\\n?${PATH_BLOCK_BEGIN.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\n[\\s\\S]*?\\n${PATH_BLOCK_END.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\n?`, "g");
  const cleaned = String(text || "").replace(pattern, "\n").trimEnd();
  return cleaned ? `${cleaned}\n` : "";
}

function removeManagedPathBlock(profile) {
  if (!profile || !fs.existsSync(profile)) return;
  fs.writeFileSync(profile, stripManagedPathBlock(fs.readFileSync(profile, "utf8")));
}

function launcherDescriptorPath() {
  return path.join(managedOpenClawDir(), "launcher.json");
}

function cleanupGlobalIntegrations(args) {
  const descriptorPath = launcherDescriptorPath();
  const heartbeatPath = path.join(args.stateDir, "heartbeat");
  try {
    const descriptor = readJson(descriptorPath, {});
    if (descriptor.owner === "agentteams") {
      removeManagedPathBlock(descriptor.profilePath);
      for (const file of [descriptorPath, descriptor.shimPath, descriptor.heartbeatFile]) {
        if (file) removeFileQuietly(String(file));
      }
    }
  } catch {
    // Best effort.
  }
  removeFileQuietly(heartbeatPath);
}

function applyGlobalIntegrations(args, edge, runtimeState) {
  const pluginScope = scopeValue(args.pluginInstallScope);
  const modelScope = selectedModelConfigMode(args);
  cleanupGlobalIntegrations(args);
  if (pluginScope !== "local" || modelScope === "managed-global") {
    writeStatus(args, "Running", "OpenClawGlobalScopeIgnored", "OpenClaw global scope is not supported in phase 1; using local worker-managed context", {
      pluginInstallScope: pluginScope,
      modelConfigMode: modelScope
    });
  }
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
    if (!block || typeof block !== "object" || !["tool_use", "toolCall"].includes(block.type)) continue;
    const name = String(block.name || block.toolName || "").trim();
    if (name) {
      summaries.push(`tool_use: \`${name}\`${formatToolInput(block)}`);
    }
  }
  return summaries;
}

function isOpenClawApiErrorText(value) {
  const text = String(value || "").toLowerCase();
  return text.includes("api error") || text.includes("api_error") || text.includes("invalid api key") || text.includes("无效的api key");
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

function eventAssistantText(event) {
  if (!event || typeof event !== "object") return "";
  const message = event.message || event;
  if (String(message?.role || "").trim() !== "assistant") return "";
  const blocks = Array.isArray(message.content) ? message.content : [];
  const texts = blocks
    .map(block => block && block.type === "text" && typeof block.text === "string" ? block.text.trim() : "")
    .filter(Boolean);
  if (texts.length) return texts.join("\n").trim();
  return typeof message.text === "string" ? message.text.trim() : "";
}

function extractOpenClawText(line, collector) {
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
      collector.result = openclawResultText(event) || collector.result || "";
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
          if (isOpenClawApiErrorText(text)) {
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

function openclawSessionKeyForRoom(roomId, args, runtimeState, edge) {
  return newOpenClawRuntimeSessionId(roomId, args, runtimeState, edge);
}

function payloadText(payload) {
  if (!payload || typeof payload !== "object") return "";
  const parts = [];
  if (typeof payload.text === "string" && payload.text.trim()) parts.push(payload.text.trim());
  if (typeof payload.mediaUrl === "string" && payload.mediaUrl.trim()) parts.push(payload.mediaUrl.trim());
  if (Array.isArray(payload.mediaUrls)) {
    for (const url of payload.mediaUrls) {
      if (typeof url === "string" && url.trim()) parts.push(url.trim());
    }
  }
  return parts.join("\n");
}

function openclawResultText(event) {
  if (!event || typeof event !== "object") return "";
  for (const key of ["text", "message"]) {
    if (typeof event[key] === "string" && event[key].trim()) return event[key].trim();
  }
  const result = event.result && typeof event.result === "object" ? event.result : event;
  if (typeof event.result === "string" && event.result.trim()) return event.result.trim();
  const payloads = Array.isArray(result.payloads) ? result.payloads : [];
  const parts = payloads.map(payloadText).filter(Boolean);
  if (parts.length) return parts.join("\n\n");
  if (typeof result.summary === "string" && result.summary.trim()) return result.summary.trim();
  if (typeof event.summary === "string" && event.summary.trim()) return event.summary.trim();
  if (typeof result.status === "string" && result.status.trim() && !/^(ok|success)$/i.test(result.status.trim())) {
    return result.status.trim();
  }
  return "";
}

function extractOpenClawFinalText(stdout) {
  const text = String(stdout || "").trim();
  if (!text) return "";
  let parsed;
  try {
    parsed = JSON.parse(text);
  } catch {
    const jsonLine = text.split(/\r?\n/).reverse().find(line => line.trim().startsWith("{") && line.trim().endsWith("}"));
    if (!jsonLine) {
      return text;
    }
    try {
      parsed = JSON.parse(jsonLine);
    } catch {
      return text;
    }
  }
  const finalText = openclawResultText(parsed);
  if (finalText) return finalText;
  return parsed ? "" : text;
}

function parseOpenClawJson(stdout) {
  const text = String(stdout || "").trim();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    const jsonLine = text.split(/\r?\n/).reverse().find(line => line.trim().startsWith("{") && line.trim().endsWith("}"));
    if (!jsonLine) return null;
    try {
      return JSON.parse(jsonLine);
    } catch {
      return null;
    }
  }
}

function openclawResultEnvelope(parsed) {
  if (!parsed || typeof parsed !== "object") return {};
  return parsed.result && typeof parsed.result === "object" ? parsed.result : parsed;
}

function openclawSessionFileFromStdout(stdout) {
  const parsed = parseOpenClawJson(stdout);
  const result = openclawResultEnvelope(parsed);
  const sessionFile = result?.meta?.agentMeta?.sessionFile || parsed?.meta?.agentMeta?.sessionFile || "";
  return typeof sessionFile === "string" ? sessionFile.trim() : "";
}

function sessionEventTimestampMs(event) {
  const timestamp = event?.timestamp;
  if (typeof timestamp === "number" && Number.isFinite(timestamp)) return timestamp > 1e12 ? timestamp : timestamp * 1000;
  if (typeof timestamp !== "string" || !timestamp.trim()) return 0;
  const parsed = Date.parse(timestamp);
  return Number.isFinite(parsed) ? parsed : 0;
}

function toolResultText(event) {
  const message = event?.message || event;
  const blocks = Array.isArray(message?.content) ? message.content : [];
  const text = blocks
    .map(block => block && typeof block.text === "string" ? block.text.trim() : "")
    .filter(Boolean)
    .join("\n\n")
    .trim();
  return text || (typeof message?.text === "string" ? message.text.trim() : "");
}

function formatToolResult(event) {
  const text = toolResultText(event);
  if (!text) return "";
  try {
    return `\n\n\`\`\`json\n${truncateMatrixThreadText(JSON.stringify(sanitizeToolInput(JSON.parse(text), 0), null, 2), 2000)}\n\`\`\``;
  } catch {
    return `\n\n${truncateMatrixThreadText(text, 2000)}`;
  }
}

function openclawSessionEventSummaries(event) {
  if (!event || typeof event !== "object") return [];
  if (event.type === "thinking_level_change") {
    const level = String(event.thinkingLevel || "").trim();
    return level && level !== "off" ? [`Thinking level: \`${level}\``] : [];
  }

  const message = event.message || event;
  const role = String(message?.role || "").trim();
  if (role === "toolResult") {
    const name = String(message.toolName || message.name || "").trim();
    return name ? [`tool_result: \`${name}\`${formatToolResult(event)}`] : [];
  }
  if (role !== "assistant") return [];

  const summaries = [];
  for (const block of assistantContentBlocks(event)) {
    if (!block || typeof block !== "object") continue;
    const blockType = String(block.type || "");
    if (["thinking", "reasoning", "reasoning_content"].includes(blockType)) {
      const text = String(block.summary || block.text || block.content || "").trim();
      if (text) summaries.push(`Thinking:\n\n${truncateMatrixThreadText(text)}`);
    } else if (["tool_use", "toolCall"].includes(blockType)) {
      const name = String(block.name || block.toolName || "").trim();
      if (name) summaries.push(`tool_use: \`${name}\`${formatToolInput(block)}`);
    }
  }
  return summaries;
}

function readOpenClawSessionSummaries(args, sessionFile, sinceMs) {
  const file = String(sessionFile || "").trim();
  if (!file) return [];
  const resolved = path.resolve(file);
  const stateRoot = path.resolve(openclawStateDir(args));
  if (!resolved.startsWith(`${stateRoot}${path.sep}`) || !fs.existsSync(resolved)) return [];
  const summaries = [];
  for (const line of fs.readFileSync(resolved, "utf8").split(/\r?\n/)) {
    if (!line.trim()) continue;
    try {
      const event = JSON.parse(line);
      const timestampMs = sessionEventTimestampMs(event);
      if (sinceMs && !timestampMs) continue;
      if (timestampMs && sinceMs && timestampMs < sinceMs - 1000) continue;
      summaries.push(...openclawSessionEventSummaries(event));
    } catch {
      continue;
    }
  }
  return summaries;
}

function readOpenClawSessionFinalText(args, sessionFile, sinceMs) {
  const file = String(sessionFile || "").trim();
  if (!file) return "";
  const resolved = path.resolve(file);
  const stateRoot = path.resolve(openclawStateDir(args));
  if (!resolved.startsWith(`${stateRoot}${path.sep}`) || !fs.existsSync(resolved)) return "";
  let finalText = "";
  for (const line of fs.readFileSync(resolved, "utf8").split(/\r?\n/)) {
    if (!line.trim()) continue;
    try {
      const event = JSON.parse(line);
      const timestampMs = sessionEventTimestampMs(event);
      if (timestampMs && sinceMs && timestampMs < sinceMs - 1000) continue;
      const text = eventAssistantText(event);
      if (text) finalText = text;
    } catch {
      continue;
    }
  }
  return finalText;
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

async function runOpenClawForEvent(args, edge, runtimeState, roomId, event, roomHistory, sessionId, eventContext) {
  if (eventContext?.abortSignal?.aborted) throw abortError();
  const member = runtimeState.runtime?.member || {};
  const model = runtimeState.runtime?.desired?.model?.model || runtimeLlmConfig(edge, runtimeState).model || "default";
  const agentId = String(member.name || member.runtimeName || edge.workerName || "worker").trim();
  const prompt = eventContext?.turn
    ? buildPromptForTurn(args, eventContext.turn, runtimeState)
    : buildPrompt(args, event, roomHistory, runtimeState, roomId);
  const command = [
    args.openclawCommand,
    "agent",
    "--local",
    "--json",
    "--agent",
    agentId,
    "--session-key",
    sessionId || openclawSessionKeyForRoom(roomId, args, runtimeState, edge),
    "--message",
    prompt
  ];
  if (!usesNativeModelConfig(args)) {
    command.splice(command.length - 2, 0, "--model", `teamharness/${model}`);
  }
  const child = spawn(command[0], command.slice(1), {
    cwd: args.workDir,
    env: openclawEnv(edge, runtimeState),
    stdio: ["ignore", "pipe", "pipe"]
  });
  const unbindAbort = bindAbortSignal(child, eventContext?.abortSignal);
  let stderr = "";
  let stdout = "";
  let stdoutBuffer = "";
  const collector = { parts: [], result: "", sessionId: "", apiErrorText: "" };
  const streamSends = [];
  const sentThreadSummaries = new Set();
  const startedAtMs = Date.now();
  const handleStdoutLine = line => {
    stdout += `${line}\n`;
    const streamEvent = extractOpenClawText(line, collector);
    if (!streamEvent) return;
    for (const summary of [...visibleAssistantSummaries(streamEvent), ...toolUseSummaries(streamEvent)]) {
      sentThreadSummaries.add(summary);
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
    stderr += String(chunk);
  });
  const code = await new Promise(resolve => child.on("close", resolve));
  unbindAbort();
  if (eventContext?.abortSignal?.aborted) throw abortError();
  if (stdoutBuffer.trim()) {
    handleStdoutLine(stdoutBuffer);
  }
  await Promise.all(streamSends);
  if (collector.apiErrorText) {
    stderr = collector.apiErrorText;
  }
  const sessionFile = openclawSessionFileFromStdout(stdout);
  if (code === 0) {
    for (const summary of readOpenClawSessionSummaries(args, sessionFile, startedAtMs)) {
      if (sentThreadSummaries.has(summary)) continue;
      sentThreadSummaries.add(summary);
      await matrixSendMessage(edge, runtimeState, roomId, summary, {
        msgtype: "m.notice",
        threadRootEventId: eventContext?.placeholderEventId
      }).catch(() => {});
    }
  }
  const parsedStdout = parseOpenClawJson(stdout);
  const stdoutFallbackText = parsedStdout ? "" : collector.parts.join("\n").trim();
  const finalText = code === 0
    ? (collector.result || extractOpenClawFinalText(stdout) || readOpenClawSessionFinalText(args, sessionFile, startedAtMs) || stdoutFallbackText || stderr.trim() || "OpenClaw completed without a text payload")
    : `OpenClaw failed with exit code ${code}: ${String(stderr || stdout || collector.apiErrorText || "").slice(0, 500)}`;
  return {
    text: finalText,
    sessionId: "",
    msgtype: code === 0 ? "m.text" : "m.notice",
    formatted: code === 0
  };
}

async function runOpenClawForTurn(args, edge, runtimeState, turn) {
  const event = eventFromTurn(turn);
  return runOpenClawForEvent(args, edge, runtimeState, turn.roomId, event, turn.history || [], turn.sessionId || "", {
    placeholderEventId: turn.placeholderEventId || "",
    abortSignal: turn.abortSignal,
    turn
  });
}

async function matrixLoop(args, edge, runtimeState) {
  return coreMatrix.runMatrixLoop(args, edge, runtimeState, {
    prepareRuntimeForTask,
    runForTurn: runOpenClawForTurn,
    sessionStore: matrixState => matrixState.openclawSessions,
    sessionForRoom: openclawSessionForRoom,
    storeSessionForRoom: storeOpenClawSessionForRoom,
    dropSessionForRoom: dropOpenClawSessionForRoom,
    failurePrefix: "OpenClaw 执行失败："
  });
}

async function startBroker(args, edge, sts, runtimeState) {
  return coreBroker.startBroker(args, edge, sts, runtimeState, {
    runtime: "openclaw",
    modelCredentialsEnabled: !usesNativeModelConfig(args),
    modelConfig: managedLlmConfig,
    writeDescriptor: writeCredentialBrokerDescriptor,
    refreshMcpConfig: refreshPluginMcpConfig,
    clearMcpNeedsAuthCache: clearPluginMcpNeedsAuthCache
  });
}

function writeCredentialBrokerDescriptor(args, descriptor) {
  const payload = { ...descriptor, updatedAt: new Date().toISOString() };
  const descriptorFile = brokerDescriptorPath(args);
  writeJson(descriptorFile, payload);
  return descriptorFile;
}

function refreshRuntimePluginWiring(args) {
  if (args.runtimeState?.brokerDescriptor) {
    writeCredentialBrokerDescriptor(args, args.runtimeState.brokerDescriptor);
  }
  if (args.runtimeState?.edge) {
    refreshOpenClawConfig(args, args.runtimeState.edge, args.runtimeState);
  }
}

function refreshPluginMcpConfig(args) {
  return args;
}

function clearPluginMcpNeedsAuthCache() {
  return undefined;
}

async function main() {
  return coreLifecycle.runWorkerMain(process.argv.slice(2), {
    parseArgs,
    command: args => args.openclawCommand,
    commandMissingReason: "OpenClawBinaryNotFound",
    commandMissingMessage: args => `OpenClaw command not found: ${args.openclawCommand}`,
    afterRuntimeLoaded: args => {
      basePluginDir(args);
    },
    applyAgentPackage,
    startModelProxy,
    startBroker,
    afterBrokerStarted: (args, edge, runtimeState) => {
      refreshOpenClawConfig(args, edge, runtimeState);
    },
    applyGlobalIntegrations,
    cleanupBrokerFiles,
    cleanupGlobalIntegrations,
    startRemotePeriodicTasks,
    matrixLoop,
    readyMessage: "Node openclaw worker is running"
  });
}

function formatErrorForLog(error) {
  const message = error?.message || String(error);
  const cause = error?.cause;
  if (!cause || typeof cause !== "object") {
    return message;
  }
  const details = [cause.code, cause.hostname, cause.address, cause.port ? `port=${cause.port}` : ""]
    .filter(Boolean)
    .join(" ");
  return details ? `${message} (${details})` : message;
}

module.exports = {
  openclawSessionStoreKey,
  newOpenClawRuntimeSessionId,
  openclawSessionForRoom,
  storeOpenClawSessionForRoom,
  dropOpenClawSessionForRoom,
  currentMessageFromTurn,
  eventFromTurn
};

if (require.main === module) {
  main().catch(error => {
    console.error(`openclaw-worker: ${formatErrorForLog(error)}`);
    process.exitCode = 1;
  });
}
