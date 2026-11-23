import assert from "node:assert/strict";
import test from "node:test";
import { copyFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

process.env.AGENTTEAMS_PLUGIN_DIR = fileURLToPath(new URL("../../../../teamharness/", import.meta.url));
const capture = mkdtempSync(join(tmpdir(), "openclaw-task-capture-"));
const modulePath = join(capture, "native-task.mjs");
copyFileSync(new URL("../../../../teamharness/adapters/openclaw/native-task.mjs", import.meta.url), modulePath);
test.after(() => rmSync(capture, { recursive: true, force: true }));
const { registerNativeTask } = await import(pathToFileURL(modulePath).href);

function install(invoke) {
  const hooks = new Map();
  let factory;
  registerNativeTask({
    registerTool(value) {
      assert.equal(value.contextVersion, 2);
      factory = (ctx) => value.create({ assertInvocationCurrent() {}, ...ctx });
    },
    on(name, callback) { hooks.set(name, callback); },
  }, { invoke });
  return { hooks, factory };
}

test("loads the public role schema and rejects calls without a trusted native frame", async () => {
  const plugin = install(() => assert.fail("unbound Task operation executed"));
  const tool = plugin.factory({ sessionId: "own-fixture" });
  assert.equal(tool.name, "task");
  assert(tool.parameters.properties.action.enum.includes("submit"));
  assert(!("_nativeSource" in tool.parameters.properties));
  await assert.rejects(tool.execute("call", { action: "submit", taskId: "guessed" }), /Trusted Task turn/);
});

test("passes the separate native context and injects the verified receipt into native tool output", async () => {
  const frame = { context: { role: "worker", execution: "t1" } };
  const calls = [];
  let checked = 0;
  const plugin = install(async (request) => {
    calls.push(request);
    return { result: { task: { id: "t1", status: "submitted" } }, fact: "Verified notification queued", phaseComplete: true };
  });
  const tool = plugin.factory({ sessionId: "fixture", toolBindings: { agentteamsTask: frame }, assertInvocationCurrent() { checked++; } });
  const args = { action: "submit", taskId: "t1", summary: "done" };
  const result = await tool.execute("call", args);
  assert.equal(calls[0].context, frame.context);
  assert.equal(calls[0].arguments, args);
  assert.equal(result.content[0].text, "Verified notification queued");
  assert.equal(checked, 1);
  await assert.rejects(tool.execute("duplicate", args), /notification queued/);
  assert.equal(calls.length, 1);
});

test("keeps refreshed Human bodies out of tool output and retains the pre-model checkpoint fence", async () => {
  const refreshed = [{ eventId: "$new", body: "Unique synthetic amendment 7591" }];
  const frame = { context: { role: "manager" }, inputCheckpointAvailable: true };
  const plugin = install(async () => ({
    result: { inputs: refreshed }, fact: "Interpret the refreshed Human inputs",
    contextUpdate: { inputRefreshPending: true }, requiresInputCheckpoint: true, refreshedInputs: refreshed,
  }));
  const result = await plugin.factory({ sessionId: "fixture", toolBindings: { agentteamsTask: frame } }).execute("call", {});
  assert.equal(frame.context.inputRefreshPending, true);
  assert.deepEqual(frame.pendingInputs, refreshed);
  assert(!JSON.stringify(result).includes("Unique synthetic amendment 7591"));
});

test("returns pre-call validation for correction but holds unconfirmed CLI effects", async () => {
  const frame = { context: {} };
  let effect = "none";
  let calls = 0;
  const plugin = install(async () => { calls++; return { ok: false, effect, message: "fixture rejected" }; });
  const tool = plugin.factory({ sessionId: "fixture", toolBindings: { agentteamsTask: frame } });
  assert.equal((await tool.execute("validation", {})).isError, true);
  assert(!frame.unresolvedEffects);
  effect = "unknown";
  await assert.rejects(tool.execute("unconfirmed", {}), /unconfirmed/);
  await assert.rejects(tool.execute("no-replay", {}), /verification/);
  assert.equal(calls, 2);
});

test("injects prepared review context and narrows coordination to the public Task tool", async () => {
  const plugin = install(() => assert.fail("unexpected Task call"));
  const frame = { context: {}, prepared: { review: ["t1", "s1"], fact: "Inspect this exact candidate", context: { id: "t1" } } };
  const result = await plugin.hooks.get("before_prompt_build")({}, { channelContext: { chat: { agentteamsTask: frame } } });
  assert.deepEqual(result.toolsAllow, ["task"]);
  assert(result.prependSystemContext.includes('"id":"t1"'));
  assert(result.prependSystemContext.includes("Inspect this exact candidate"));
});

test("binds private shell temporary storage through the native env hook without global changes", () => {
  const plugin = install(() => assert.fail("unexpected Task call"));
  const previous = process.env.TMPDIR;
  for (const id of ["a", "b", "c"]) {
    const frame = { context: {}, prepared: { context: { scratchDirectory: `/fixture/tasks/${id}/workspace/.scratch` } } };
    const result = plugin.hooks.get("resolve_exec_env")({}, { channelContext: { chat: { agentteamsTask: frame } } });
    assert.deepEqual(result, { TMPDIR: `/fixture/tasks/${id}/workspace/.scratch` });
  }
  assert.equal(process.env.TMPDIR, previous);
  assert.equal(plugin.hooks.get("resolve_exec_env")({}, {}), undefined);
});

test("blocks deprecated Task tools only in a bound Task turn and clears its own hook state", () => {
  const plugin = install(() => assert.fail("unexpected Task call"));
  plugin.factory({ sessionId: "fixture", toolBindings: { agentteamsTask: { context: {} } } });
  assert(plugin.hooks.get("before_tool_call")({ toolName: "mcp__teamharness__taskflow" }, { sessionId: "fixture" }).block);
  assert.equal(plugin.hooks.get("before_tool_call")({ toolName: "read" }, { sessionId: "fixture" }), undefined);
  plugin.hooks.get("agent_end")({}, { sessionId: "fixture" });
  assert.equal(plugin.hooks.get("before_tool_call")({ toolName: "mcp__teamharness__taskflow" }, { sessionId: "fixture" }), undefined);
});
