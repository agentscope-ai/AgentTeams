#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
matrix_core = repo_root / "plugins/teamharness/remote/node-runtime-core/worker/matrix.js"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

script = <<~JS
  const http = require("http");
  const fs = require("fs");
  const os = require("os");
  const path = require("path");
  const assert = require("assert");
  const matrix = require(#{matrix_core.to_s.inspect});

  function listen(server) {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => resolve(server.address().port));
    });
  }

  async function readBody(req) {
    const chunks = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    return Buffer.concat(chunks).toString("utf8");
  }

  (async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-matrix-"));
    const requests = [];
    let syncResponse = null;
    let syncFailuresRemaining = 0;
    const server = http.createServer(async (req, res) => {
      const body = await readBody(req);
      requests.push({ method: req.method, url: req.url, auth: req.headers.authorization || "", body });
      if (req.method === "GET" && req.url.startsWith("/_matrix/client/v3/sync?")) {
        if (syncFailuresRemaining > 0) {
          syncFailuresRemaining -= 1;
          res.writeHead(503, { "content-type": "text/plain" });
          res.end("temporary matrix outage");
          return;
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(syncResponse || { next_batch: "empty", rooms: { join: {} } }));
        return;
      }
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ event_id: `$${requests.length}` }));
    });
    const port = await listen(server);

    try {
      const edge = {
        matrixHomeserver: `http://127.0.0.1:${port}`,
        workerName: "worker-01",
        runtimeName: "worker-01"
      };
      const runtimeState = {
        runtime: {
          matrix: { accessToken: "matrix-token", teamRoomId: "!team:example" },
          member: {
            name: "worker-01",
            runtimeName: "worker-01",
            matrixUserId: "@worker-01:example",
            personalRoomId: "!personal:example"
          },
          team: { roomId: "!team:example" }
        }
      };

      const personalEvent = { type: "m.room.message", sender: "@user:example", content: { body: "hello" } };
      assert.strictEqual(matrix.shouldHandleEvent(personalEvent, "!personal:example", edge, runtimeState), true);
      assert.strictEqual(matrix.shouldHandleEvent(personalEvent, "!team:example", edge, runtimeState), false);
      const mentionEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "请处理", "m.mentions": { user_ids: ["@worker-01:example"] } }
      };
      assert.strictEqual(matrix.shouldHandleEvent(mentionEvent, "!team:example", edge, runtimeState), true);
      assert.strictEqual(matrix.shouldHandleEvent(mentionEvent, "!external:example", edge, runtimeState), true);
      assert.strictEqual(matrix.shouldHandleEvent(personalEvent, "!external:example", edge, runtimeState), false);
      const roomMentionEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "全员检查", "m.mentions": { room: true } }
      };
      assert.strictEqual(matrix.shouldHandleEvent(roomMentionEvent, "!team:example", edge, runtimeState), true);
      const formattedMentionEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: {
          body: "worker-01 请处理",
          format: "org.matrix.custom.html",
          formatted_body: '<a href="https://matrix.to/#/%40worker-01%3Aexample">worker-01</a> 请处理'
        }
      };
      assert.strictEqual(matrix.shouldHandleEvent(formattedMentionEvent, "!team:example", edge, runtimeState), true);
      const fullMxidMentionEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "@worker-01:example 请处理" }
      };
      assert.strictEqual(matrix.shouldHandleEvent(fullMxidMentionEvent, "!team:example", edge, runtimeState), true);
      const plainNameOnly = { type: "m.room.message", sender: "@user:example", content: { body: "worker-01: 你好" } };
      assert.strictEqual(matrix.shouldHandleEvent(plainNameOnly, "!team:example", edge, runtimeState), false);
      const wrongMention = {
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "请处理", "m.mentions": { user_ids: ["@someone-else:example"] } }
      };
      assert.strictEqual(matrix.shouldHandleEvent(wrongMention, "!team:example", edge, runtimeState), false);
      const similarMention = { type: "m.room.message", sender: "@user:example", content: { body: "worker-012: 你好" } };
      assert.strictEqual(matrix.shouldHandleEvent(similarMention, "!team:example", edge, runtimeState), false);
      const selfEvent = { type: "m.room.message", sender: "@worker-01:example", content: { body: "worker-01: hi" } };
      assert.strictEqual(matrix.shouldHandleEvent(selfEvent, "!team:example", edge, runtimeState), false);

      const threadEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "worker-01 reply", "m.relates_to": { rel_type: "m.thread", event_id: "$root" } }
      };
      assert.strictEqual(matrix.isMatrixThreadEvent(threadEvent), true);
      assert.strictEqual(matrix.shouldHandleEvent(threadEvent, "!team:example", edge, runtimeState), false);
      const replaceEvent = {
        type: "m.room.message",
        sender: "@user:example",
        content: {
          body: "* worker-01 reply",
          "m.mentions": { user_ids: ["@worker-01:example"] },
          "m.relates_to": { rel_type: "m.replace", event_id: "$old" },
          "m.new_content": { msgtype: "m.text", body: "worker-01 reply" }
        }
      };
      assert.strictEqual(matrix.isMatrixReplaceEvent(replaceEvent), true);
      assert.strictEqual(matrix.shouldHandleEvent(replaceEvent, "!team:example", edge, runtimeState), true);
      const selfReplaceEvent = {
        type: "m.room.message",
        sender: "@worker-01:example",
        content: {
          body: "* final reply",
          "m.relates_to": { rel_type: "m.replace", event_id: "$placeholder" },
          "m.new_content": { msgtype: "m.text", body: "final reply" }
        }
      };
      assert.strictEqual(matrix.shouldHandleEvent(selfReplaceEvent, "!team:example", edge, runtimeState), false);
      assert.strictEqual(matrix.isNoReplyText("NO_REPLY\\n"), true);
      assert.strictEqual(matrix.isNoReplyText("NO_REPLY with context"), false);
      assert.strictEqual(matrix.matrixControlCommand({ content: { body: "/stop" } }), "/stop");
      assert.strictEqual(matrix.matrixControlCommand({ content: { body: " /clear\\n" } }), "/clear");
      assert.strictEqual(matrix.matrixControlCommand({ content: { body: "/stop now" } }), "");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "/stop", "m.mentions": { user_ids: [] } }
      }, "!team:example", edge, runtimeState), "");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "@worker-01 /stop", "m.mentions": { user_ids: ["@worker-01:example"] } }
      }, "!team:example", edge, runtimeState), "/stop");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "/clear @worker-01", "m.mentions": { user_ids: ["@worker-01:example"] } }
      }, "!team:example", edge, runtimeState), "/clear");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "@worker-01:example /stop" }
      }, "!team:example", edge, runtimeState), "/stop");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "@worker-01 /clear", "m.mentions": { user_ids: ["@worker-01:example"] } }
      }, "!external:example", edge, runtimeState), "/clear");
      assert.strictEqual(matrix.matrixControlCommand({
        type: "m.room.message",
        sender: "@user:example",
        content: { body: "@worker-01 /stop now", "m.mentions": { user_ids: ["@worker-01:example"] } }
      }, "!team:example", edge, runtimeState), "");
      assert.deepStrictEqual(matrix.messagesAfter([{ event_id: "$1" }, { event_id: "$2" }, { event_id: "$3" }], "$1"), [{ event_id: "$2" }, { event_id: "$3" }]);

      fs.writeFileSync(path.join(tmp, "matrix-state.json"), JSON.stringify({
        nextBatch: "s0",
        matrixCursors: { "!team:example": "$1" },
        claudeSessions: { "!team:example": "claude-old" }
      }));
      const loaded = matrix.loadMatrixState({ runtime: "claude-code", stateDir: tmp });
      assert.strictEqual(loaded.matrixSyncToken, "s0");
      assert.strictEqual(loaded.runtimeSessions["claude-code"]["!team:example"], "claude-old");
      loaded.matrixSyncToken = "s1";
      loaded.claudeSessions["!personal:example"] = "claude-new";
      loaded.bootstrapPending = true;
      matrix.writeMatrixState({ runtime: "claude-code", stateDir: tmp }, loaded);
      const written = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(written.nextBatch, "s1");
      assert.strictEqual(written.runtimeSessions["claude-code"]["!personal:example"], "claude-new");
      assert.strictEqual(written.bootstrapPending, true);
      assert.strictEqual(matrix.loadMatrixState({ runtime: "claude-code", stateDir: tmp }).bootstrapPending, true);

      await matrix.matrixJoin(edge, runtimeState, "!team:example");
      await matrix.matrixTyping(edge, runtimeState, "!team:example", true);
      const placeholder = await matrix.matrixSendMessage(edge, runtimeState, "!team:example", "处理中...", {});
      assert.strictEqual(placeholder.event_id, "$3");
      await matrix.matrixSendMessage(edge, runtimeState, "!team:example", "最终回复", { replaceEventId: "$3" });
      const editRequest = requests[3];
      const editBody = JSON.parse(editRequest.body);
      assert.strictEqual(editBody.body, "* 最终回复");
      assert.strictEqual(editBody["m.relates_to"].rel_type, "m.replace");
      assert.strictEqual(editBody["m.new_content"].body, "最终回复");
      assert(editBody.formatted_body.includes("* 最终回复"), "edit fallback html should include Matrix edit marker");
      await matrix.matrixSendMessage(edge, runtimeState, "!team:example", "@leader:example 请验收", {});
      const mentionBody = JSON.parse(requests[4].body);
      assert.deepStrictEqual(mentionBody["m.mentions"], { user_ids: ["@leader:example"] });
      assert(mentionBody.formatted_body.includes("https://matrix.to/#/%40leader%3Aexample"), "outgoing full MXID should render as matrix.to mention");
      assert(requests.every(item => item.auth === "Bearer matrix-token"), "Matrix requests should use runtime access token");

      const tableHtml = matrix.matrixFormattedBody([
        "| 项目 | 状态 |",
        "|------|------|",
        "| 团队 | `zhuoguang-test` |",
        "| Broker | **已连接** |"
      ].join("\\n"));
      assert(tableHtml.includes("<table>"), "markdown table should render as HTML table");
      assert(tableHtml.includes("<th>项目</th>"), "markdown table should render header cells");
      assert(tableHtml.includes("<td><code>zhuoguang-test</code></td>"), "markdown table should render inline code in cells");
      assert(tableHtml.includes("<td><strong>已连接</strong></td>"), "markdown table should render bold text in cells");

      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": { "!team:example": "old-session" } }
      });
      syncResponse = {
        next_batch: "s2",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  { type: "m.room.message", event_id: "$ctx", sender: "@helper:example", content: { body: "处理中..." } },
                  {
                    type: "m.room.message",
                    event_id: "$ctx-edit",
                    sender: "@helper:example",
                    content: {
                      body: "* context before trigger",
                      "m.relates_to": { rel_type: "m.replace", event_id: "$ctx" },
                      "m.new_content": { msgtype: "m.text", body: "context before trigger" }
                    }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$hit",
                    sender: "@user:example",
                    content: { body: "run task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      const loopArgs = { runtime: "core-test", stateDir: tmp, intervalSeconds: 0 };
      let prepared = 0;
      let handled = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        prepareRuntimeForTask: async () => {
          prepared += 1;
        },
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        sessionForRoom: (sessions, roomId) => sessions[roomId] || "",
        storeSessionForRoom: (sessions, roomId, _args, sessionId) => {
          sessions[roomId] = sessionId;
        },
        dropSessionForRoom: (sessions, roomId) => {
          delete sessions[roomId];
        },
        runForEvent: async (_args, _edge, _runtimeState, roomId, event, roomHistory, sessionId) => {
          handled += 1;
          assert.strictEqual(roomId, "!team:example");
          assert.strictEqual(event.event_id, "$hit");
          assert.strictEqual(sessionId, "old-session");
          assert.deepStrictEqual(roomHistory.map(item => item.body), ["context before trigger"]);
          assert.deepStrictEqual(roomHistory.map(item => item.eventId), ["$ctx"]);
          assert.deepStrictEqual(roomHistory.map(item => item.replacedByEventId), ["$ctx-edit"]);
          return { text: "loop final reply", sessionId: "new-session" };
        },
        failurePrefix: "Runtime failed: "
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      assert.strictEqual(prepared, 1);
      assert.strictEqual(handled, 1);
      const loopState = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(loopState.matrixSyncToken, "s2");
      assert.strictEqual(loopState.matrixCursors["!team:example"], "$hit");
      assert.strictEqual(loopState.runtimeSessions["core-test"]["!team:example"], "new-session");
      const loopEdit = requests.map(item => {
        try {
          return JSON.parse(item.body || "{}");
        } catch {
          return {};
        }
      }).reverse().find(body => body["m.relates_to"] && body["m.relates_to"].rel_type === "m.replace" && body["m.new_content"]);
      assert(loopEdit, "loop should edit the core-owned placeholder");
      assert.strictEqual(loopEdit["m.new_content"].body, "loop final reply");

      requests.length = 0;
      const reconnectDir = path.join(tmp, "reconnect");
      fs.mkdirSync(reconnectDir, { recursive: true });
      matrix.writeMatrixState({ runtime: "core-test", stateDir: reconnectDir }, {
        matrixSyncToken: "resume-token",
        matrixCursors: {},
        runtimeSessions: { "core-test": {} }
      });
      syncFailuresRemaining = 1;
      syncResponse = {
        next_batch: "reconnected-token",
        rooms: {
          join: {
            "!personal:example": {
              timeline: {
                events: [
                  { type: "m.room.message", event_id: "$after-reconnect", sender: "@user:example", content: { body: "after reconnect" } }
                ]
              }
            }
          }
        }
      };
      let reconnectedHandled = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop({ runtime: "core-test", stateDir: reconnectDir, intervalSeconds: 0, matrixReconnectBaseMs: 1, matrixReconnectMaxMs: 1 }, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        runForEvent: async (_args, _edge, _runtimeState, roomId, event) => {
          reconnectedHandled += 1;
          assert.strictEqual(roomId, "!personal:example");
          assert.strictEqual(event.event_id, "$after-reconnect");
          return { text: "reconnected reply" };
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      assert.strictEqual(reconnectedHandled, 1, "Matrix sync should retry after a transient failure");
      const reconnectSyncs = requests.filter(item => item.method === "GET" && item.url.startsWith("/_matrix/client/v3/sync?"));
      assert.strictEqual(reconnectSyncs.length, 2, "Matrix loop should issue another sync request after retry delay");
      assert(reconnectSyncs.every(item => item.url.includes("since=resume-token")), "retry should keep the previous Matrix sync token");
      const reconnectState = JSON.parse(fs.readFileSync(path.join(reconnectDir, "matrix-state.json"), "utf8"));
      assert.strictEqual(reconnectState.matrixSyncToken, "reconnected-token");
      const reconnectStatus = JSON.parse(fs.readFileSync(path.join(reconnectDir, "status.json"), "utf8"));
      assert.strictEqual(reconnectStatus.reason, "MatrixReconnected");
      assert.strictEqual(reconnectStatus.failureCount, 1);

      requests.length = 0;
      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": { "!external:example": "external-session" } }
      });
      syncResponse = {
        next_batch: "s-external",
        rooms: {
          join: {
            "!external:example": {
              timeline: {
                events: [
                  { type: "m.room.message", event_id: "$external-ctx", sender: "@helper:example", content: { body: "external context before trigger" } },
                  {
                    type: "m.room.message",
                    event_id: "$external-hit",
                    sender: "@user:example",
                    content: { body: "external task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        sessionForRoom: (sessions, roomId) => sessions[roomId] || "",
        storeSessionForRoom: (sessions, roomId, _args, sessionId) => {
          sessions[roomId] = sessionId;
        },
        runForEvent: async (_args, _edge, _runtimeState, roomId, event, roomHistory, sessionId) => {
          assert.strictEqual(roomId, "!external:example");
          assert.strictEqual(event.event_id, "$external-hit");
          assert.strictEqual(sessionId, "external-session");
          assert.deepStrictEqual(roomHistory.map(item => item.body), ["external context before trigger"]);
          return { text: "external final reply", sessionId: "external-new-session" };
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      const externalState = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(externalState.matrixSyncToken, "s-external");
      assert.strictEqual(externalState.matrixCursors["!external:example"], "$external-hit");
      assert.strictEqual(externalState.runtimeSessions["core-test"]["!external:example"], "external-new-session");

      requests.length = 0;
      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": {} }
      });
      syncResponse = {
        next_batch: "s3",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$noreply",
                    sender: "@user:example",
                    content: { body: "ack only", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        runForEvent: async () => ({ text: "NO_REPLY" })
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      const noReplyBodies = requests.map(item => {
        try {
          return JSON.parse(item.body || "{}");
        } catch {
          return {};
        }
      });
      const noReplyEdit = noReplyBodies.find(body => body["m.relates_to"] && body["m.relates_to"].rel_type === "m.replace" && body["m.new_content"]);
      assert(noReplyEdit, "NO_REPLY should edit the placeholder");
      assert.strictEqual(noReplyEdit["m.new_content"].body, "已处理");
      assert(!noReplyBodies.some(body => body.reason === "NO_REPLY"), "NO_REPLY should not redact the placeholder");
      assert(!noReplyBodies.some(body => body["m.new_content"] && body["m.new_content"].body === "NO_REPLY"), "NO_REPLY must not be sent as a Matrix reply");

      requests.length = 0;
      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": { "!team:example": "old-session" } }
      });
      syncResponse = {
        next_batch: "s4",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  { type: "m.room.message", event_id: "$clear-ctx", sender: "@user:example", content: { body: "context before clear" } },
                  {
                    type: "m.room.message",
                    event_id: "$clear",
                    sender: "@user:example",
                    content: { body: "/clear", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      let clearHandled = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        dropSessionForRoom: sessions => {
          delete sessions["!team:example"];
        },
        runForEvent: async () => {
          clearHandled += 1;
          return { text: "should not run" };
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      assert.strictEqual(clearHandled, 0, "/clear must not be delivered to runtime");
      const clearState = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(clearState.runtimeSessions["core-test"]["!team:example"], undefined);
      const clearBodies = requests.map(item => {
        try {
          return JSON.parse(item.body || "{}");
        } catch {
          return {};
        }
      });
      assert(clearBodies.some(body => body.body === "已停止当前任务并清空上下文，下一次消息会开启新会话。"), "/clear should send an acknowledgement");

      requests.length = 0;
      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": { "!team:example": "old-session" } }
      });
      syncResponse = {
        next_batch: "s5",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$long-run",
                    sender: "@user:example",
                    content: { body: "long task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$stop",
                    sender: "@user:example",
                    content: { body: "/stop", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      let stopDropCount = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        sessionForRoom: (sessions, roomId) => sessions[roomId] || "",
        storeSessionForRoom: (sessions, roomId, _args, sessionId) => {
          sessions[roomId] = sessionId;
        },
        dropSessionForRoom: (sessions, roomId) => {
          stopDropCount += 1;
          delete sessions[roomId];
        },
        runForEvent: async (_args, _edge, _runtimeState, _roomId, _event, _roomHistory, _sessionId, eventContext) => {
          if (eventContext.abortSignal.aborted) {
            return { text: "should not be sent", sessionId: "bad-session" };
          }
          return new Promise(resolve => {
            eventContext.abortSignal.addEventListener("abort", () => {
              resolve({ text: "should not be sent", sessionId: "bad-session" });
            }, { once: true });
          });
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      const stopState = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(stopDropCount, 0, "/stop must preserve the runtime session");
      assert.strictEqual(stopState.runtimeSessions["core-test"]["!team:example"], "old-session");
      const stopBodies = requests.map(item => {
        try {
          return JSON.parse(item.body || "{}");
        } catch {
          return {};
        }
      });
      assert(stopBodies.some(body => body.body === "已停止当前任务，保留当前会话。"), "/stop should send an acknowledgement");
      assert(!stopBodies.some(body => body["m.new_content"] && body["m.new_content"].body === "should not be sent"), "aborted task result must not be delivered");

      requests.length = 0;
      matrix.writeMatrixState({ runtime: "core-test", stateDir: tmp }, {
        matrixSyncToken: "",
        matrixCursors: {},
        runtimeSessions: { "core-test": { "!team:example": "old-session" } }
      });
      syncResponse = {
        next_batch: "s6",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$clear-long-run",
                    sender: "@user:example",
                    content: { body: "long task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$clear-active",
                    sender: "@user:example",
                    content: { body: "/clear", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      let activeClearDropCount = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop(loopArgs, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        sessionForRoom: (sessions, roomId) => sessions[roomId] || "",
        storeSessionForRoom: (sessions, roomId, _args, sessionId) => {
          sessions[roomId] = sessionId;
        },
        dropSessionForRoom: (sessions, roomId) => {
          activeClearDropCount += 1;
          delete sessions[roomId];
        },
        runForEvent: async (_args, _edge, _runtimeState, _roomId, _event, _roomHistory, _sessionId, eventContext) => {
          if (eventContext.abortSignal.aborted) {
            return { text: "should not be sent", sessionId: "bad-session" };
          }
          return new Promise(resolve => {
            eventContext.abortSignal.addEventListener("abort", () => {
              resolve({ text: "should not be sent", sessionId: "bad-session" });
            }, { once: true });
          });
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      const activeClearState = JSON.parse(fs.readFileSync(path.join(tmp, "matrix-state.json"), "utf8"));
      assert.strictEqual(activeClearDropCount, 1, "/clear should rotate the runtime session once");
      assert.strictEqual(activeClearState.runtimeSessions["core-test"]["!team:example"], undefined);
      const activeClearBodies = requests.map(item => {
        try {
          return JSON.parse(item.body || "{}");
        } catch {
          return {};
        }
      });
      assert(activeClearBodies.some(body => body.body === "已停止当前任务并清空上下文，下一次消息会开启新会话。"), "/clear should send an acknowledgement");
      assert(!activeClearBodies.some(body => body["m.new_content"] && body["m.new_content"].body === "should not be sent"), "cleared task result must not be delivered");

      requests.length = 0;
      const bootstrapDir = path.join(tmp, "bootstrap");
      fs.mkdirSync(bootstrapDir, { recursive: true });
      syncResponse = {
        next_batch: "bootstrap-token",
        rooms: {
          join: {
            "!team:example": {
              timeline: {
                events: [
                  {
                    type: "m.room.message",
                    event_id: "$bootstrap-hit",
                    sender: "@user:example",
                    content: { body: "historical task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  },
                  {
                    type: "m.room.message",
                    event_id: "$bootstrap-clear",
                    sender: "@user:example",
                    content: { body: "@worker-01 /clear", "m.mentions": { user_ids: ["@worker-01:example"] } }
                  }
                ]
              }
            }
          }
        }
      };
      let bootstrapHandled = 0;
      process.env.TEAMHARNESS_MATRIX_ONCE = "1";
      await matrix.runMatrixLoop({ runtime: "core-test", stateDir: bootstrapDir, intervalSeconds: 0 }, edge, JSON.parse(JSON.stringify(runtimeState)), {
        sessionStore: state => {
          state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
          return state.runtimeSessions["core-test"];
        },
        runForEvent: async () => {
          bootstrapHandled += 1;
          return { text: "should not run" };
        }
      });
      delete process.env.TEAMHARNESS_MATRIX_ONCE;
      assert.strictEqual(bootstrapHandled, 0, "first sync should bootstrap without replaying historical tasks or controls");
      const bootstrapState = JSON.parse(fs.readFileSync(path.join(bootstrapDir, "matrix-state.json"), "utf8"));
      assert.strictEqual(bootstrapState.matrixSyncToken, "bootstrap-token");
      assert(bootstrapState.seenEventIds.includes("$bootstrap-hit"));
      assert(!bootstrapState.rooms["!team:example"].pendingTurn, "bootstrap trigger must not create pending turn");

      async function runConcurrencyCase(maxConcurrentTasks, runtimeOverride) {
        requests.length = 0;
        const concurrencyDir = path.join(tmp, `concurrency-${maxConcurrentTasks}`);
        fs.mkdirSync(concurrencyDir, { recursive: true });
        matrix.writeMatrixState({ runtime: "core-test", stateDir: concurrencyDir }, {
          matrixSyncToken: "",
          matrixCursors: {},
          runtimeSessions: { "core-test": {} }
        });
        syncResponse = {
          next_batch: `concurrency-${maxConcurrentTasks}`,
          rooms: {
            join: {
              "!personal:example": {
                timeline: {
                  events: [
                    { type: "m.room.message", event_id: `$personal-${maxConcurrentTasks}`, sender: "@user:example", content: { body: "personal task" } }
                  ]
                }
              },
              "!team:example": {
                timeline: {
                  events: [
                    {
                      type: "m.room.message",
                      event_id: `$team-${maxConcurrentTasks}`,
                      sender: "@user:example",
                      content: { body: "team task", "m.mentions": { user_ids: ["@worker-01:example"] } }
                    }
                  ]
                }
              }
            }
          }
        };
        let active = 0;
        let maxActive = 0;
        const order = [];
        process.env.TEAMHARNESS_MATRIX_ONCE = "1";
        await matrix.runMatrixLoop({ runtime: "core-test", stateDir: concurrencyDir, intervalSeconds: 0, maxConcurrentTasks }, edge, JSON.parse(JSON.stringify(runtimeOverride || runtimeState)), {
          sessionStore: state => {
            state.runtimeSessions["core-test"] = state.runtimeSessions["core-test"] || {};
            return state.runtimeSessions["core-test"];
          },
          runForEvent: async (_args, _edge, _runtimeState, roomId) => {
            active += 1;
            maxActive = Math.max(maxActive, active);
            order.push(roomId);
            await new Promise(resolve => setTimeout(resolve, 25));
            active -= 1;
            return { text: `done ${roomId}` };
          }
        });
        delete process.env.TEAMHARNESS_MATRIX_ONCE;
        return { maxActive, order };
      }
      const serial = await runConcurrencyCase(1);
      assert.strictEqual(serial.maxActive, 1, "maxConcurrentTasks=1 should serialize rooms");
      assert.deepStrictEqual(serial.order.sort(), ["!personal:example", "!team:example"].sort());
      const parallel = await runConcurrencyCase(2);
      assert.strictEqual(parallel.maxActive, 2, "maxConcurrentTasks=2 should run two rooms concurrently");
      const runtimeLimitedState = JSON.parse(JSON.stringify(runtimeState));
      runtimeLimitedState.runtime.desired = { scheduler: { maxConcurrentTasks: 1 } };
      const runtimeLimited = await runConcurrencyCase(undefined, runtimeLimitedState);
      assert.strictEqual(runtimeLimited.maxActive, 1, "runtime.yaml desired.scheduler.maxConcurrentTasks should apply when CLI/env is unset");

      console.log(JSON.stringify({ ok: true, requests: requests.length }));
    } finally {
      await new Promise(resolve => server.close(resolve));
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  })().catch(error => {
    console.error(error.stack || error.message);
    process.exit(1);
  });
JS

stdout, stderr, status = Open3.capture3("node", "-e", script)
fail!("matrix core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("matrix core test did not report ok") unless result["ok"] == true
