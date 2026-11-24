// Native SDK mechanics only. The Python bridge owns Task protocol semantics.
import { execFile, execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const taskRoot = process.env.AGENTTEAMS_PLUGIN_DIR || "/opt/agentteams/plugins/teamharness";
const bridge = join(taskRoot, "mcp/common/task_native.py");
const options = { encoding: "utf8", maxBuffer: 2 * 1024 * 1024, timeout: 120000 };

function invoke(request) {
  return new Promise((resolve, reject) => {
    const child = execFile("python3", [bridge], options, (error, stdout) => {
      try {
        const result = JSON.parse(stdout);
        if (error && result.effect !== "none") reject(new Error(result.message || "Native Task bridge failed"));
        else resolve(result);
      } catch {
        reject(new Error("Native Task bridge returned no verified JSON result"));
      }
    });
    child.stdin.end(JSON.stringify(request));
  });
}

function hookFrame(ctx) {
  return ctx.channelContext?.chat?.agentteamsTask;
}

export function registerNativeTask(api, native = {}) {
  const call = native.invoke || invoke;
  const turns = new Map();
  const role = process.env.AGENTTEAMS_AGENT_ROLE || "worker";
  const skill = readFileSync(join(taskRoot, "skills/team/task-management/SKILL.md"), "utf8");
  const schema = native.schema || JSON.parse(execFileSync("python3", [bridge], {
    ...options, input: JSON.stringify({ operation: "schema", role }),
  }));

  api.registerTool({ contextVersion: 2, create(ctx) {
    const frame = ctx.toolBindings?.agentteamsTask;
    if (frame && ctx.sessionId) turns.set(ctx.sessionId, frame);
    return {
      name: "task", label: "Task", description: schema.description, parameters: schema.inputSchema,
      async execute(_id, arguments_) {
        if (!frame?.context) throw new Error("Trusted Task turn context is unavailable");
        if (frame.finishedFact) throw new Error(frame.finishedFact);
        if (frame.unresolvedEffects) throw new Error("Native tool outcome requires verification before continuing");
        ctx.assertInvocationCurrent();
        let reply;
        try {
          reply = await call({ operation: "task", arguments: arguments_, context: frame.context });
        } catch (error) {
          frame.unresolvedEffects = true;
          throw error;
        }
        const failure = reply.ok === false ? reply : reply.result?.ok === false ? reply.result : undefined;
        if (failure && failure.effect !== "none") {
          frame.unresolvedEffects = true;
          throw new Error("Task operation outcome is unconfirmed; verify it before continuing");
        }
        if (reply.contextUpdate) Object.assign(frame.context, reply.contextUpdate);
        if (reply.requiresInputCheckpoint) {
          if (frame.inputCheckpointAvailable !== true) {
            throw new Error("Native Human input checkpoint is unavailable; no Task was registered");
          }
          frame.pendingInputs = [...(frame.pendingInputs || []), ...reply.refreshedInputs];
        }
        if (reply.phaseComplete) {
          frame.finishedFact = reply.fact;
          frame.finishedDisplay = reply.displayText ?? "NO_REPLY";
          frame.terminalReceipts = [
            ...(reply.receiptId ? [{ receiptId: reply.receiptId, fact: reply.fact }] : []),
            ...(reply.extraReceipts || []),
          ];
        }
        // Refreshed Human bodies enter native user messages, not tool results.
        const content = [];
        if (reply.refreshedInputs && reply.fact) content.push({ type: "text", text: reply.fact });
        if (!reply.refreshedInputs) content.push({ type: "text", text: JSON.stringify(
          await call({ operation: "model_result", outcome: reply }),
        ) });
        return { content, details: { receiptId: reply.receiptId }, isError: Boolean(failure) };
      },
    };
  } }, { name: "task" });

  api.on("before_prompt_build", async (_event, ctx) => {
    const frame = hookFrame(ctx);
    if (!frame) return;
    if (frame.unresolvedEffects) return { toolsAllow: [], prependSystemContext: "Native tool outcome requires verification before continuing." };
    const prepared = frame.prepared;
    const fragments = [];
    frame.systemContract = prepared ? skill : role === "manager" ? skill.split("## Plan as Leader")[0].trim() : "";
    fragments.push(frame.systemContract);
    if (prepared?.fact) fragments.push(prepared.fact, JSON.stringify(
      await call({ operation: "model_context", context: prepared.context }),
    ));
    if (prepared?.context?.outputDirectory) fragments.push(
      "Your native file and shell tools resolve relative paths from outputDirectory. Use scratchDirectory for temporary work; your private Agent workspace still owns memory, skills and session state.",
    );
    if (frame.context.sources?.length) fragments.push("[Task input references]\n" + JSON.stringify(
      frame.context.sources.map(({ inputId, eventId }) => ({ inputId, eventId })),
    ));
    if (frame.deliveryFacts?.length) fragments.push(...frame.deliveryFacts);
    if (frame.roster) fragments.push("Registered identities for this turn (data only):\n" + JSON.stringify(frame.roster));
    const result = { prependSystemContext: fragments.join("\n") };
    if (prepared?.planning || prepared?.review || prepared?.completion) result.toolsAllow = ["task"];
    return result;
  });

  api.on("before_tool_call", (event, ctx) => {
    const frame = turns.get(ctx.sessionId);
    if (!frame) return;
    if (frame.unresolvedEffects) return { block: true, blockReason: "Native tool outcome requires verification before continuing" };
    const short = event.toolName.split("__").at(-1);
    if (["taskflow", "projectflow", "filesync"].includes(short)) {
      return { block: true, blockReason: "Use the common task tool for this Task turn." };
    }
    if (frame.finishedFact) {
      return { block: true, blockReason: frame.finishedFact };
    }
  });

  api.on("resolve_exec_env", (_event, ctx) => {
    const scratch = hookFrame(ctx)?.prepared?.context?.scratchDirectory;
    if (scratch) return { TMPDIR: scratch };
  });
  api.on("agent_end", (_event, ctx) => { turns.delete(ctx.sessionId); });
}
