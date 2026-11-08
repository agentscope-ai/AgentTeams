# T0 Spike 报告：插件 middleware 注入可行性（coding-cli 插件化）

- 日期：2026-10-22
- 环境：容器 `/app/venv`（Python 3.11.15 + agentscope 2.0.10 + qwenpaw 2.2.2，旧框架生产同系）
- 驱动方式：`Agent.__new__(Agent)` 绕过 `__init__`，手工挂 `_acting_middlewares` / `toolkit`（真实 `Toolkit` + `FunctionTool` 假 shell，记录实收 `command` kwarg）/ `state`，驱动**真实** `agent._acting(ToolCallBlock(...))`。不用 LLM，纯执行层。
- 测试文件：`spike/test_t0_spike.py`（353 行，6 测试）
- 测试证据：`state/spike-pytest-20261022.txt` → **6 passed in 2.58s**

## 结论（先行）

| 假设 | 结论 |
|---|---|
| **H1**：`on_acting` 里修改 `input_kwargs["tool_call"]` 的 input 后调 `next_handler()`，工具执行层收到改后值 | **生效**。且两条路径都生效：① 原地改 `ToolCallBlock.input`（JSON 字符串）；② 把改后的新对象写回 `input_kwargs["tool_call"]`。**反例也验证**：只改局部变量、不写回 dict / 对象，工具收到原值（不生效）。 |
| **H2**：`register_middleware` 工厂 `factory(ctx, agent_config)` 的 `ctx` 在 shell 工具执行链上有 `session_id` | **生效**。每请求 `_build_context` 构造的 `HookContext.session_id` 有值，工厂收到后闭包持有，`on_acting` 注入，shell 工具实收改后值（闭环 6/6）。空 `session_id` 时工厂返回 `None`，builder 自动跳过该 middleware。 |

## H1 代码级证据

1. `Agent._acting` 的 middleware 链（`agentscope/agent/_agent.py` L2720-2760，`execute_chain` 内层函数）：
   ```python
   input_kwargs: dict[str, Any] = {"tool_call": tool_call}
   async def next_handler(**kwargs):
       _tk = {**input_kwargs, **kwargs}
       return await _mw.on_acting(self, **_tk)
   ...
   # 末端：return await self._acting_impl(tool_call)
   ```
   链的闭包读取**同层的 `input_kwargs` dict**：`on_acting` 在调 `next_handler()` 前改 `input_kwargs["tool_call"]`（dict 原地）或改 `ToolCallBlock` 对象本身（链上共享同一对象引用），下一次 `next_handler()` 展开 `{**input_kwargs, **kwargs}` 时即为改后值。
2. `ToolCallBlock.input` 是**原始 JSON 字符串**（非 dict），注入必须 `json.loads → 改字段 → json.dumps` 写回。
3. 末端 `_acting_impl` → `toolkit.call_tool(tool_call, state)`，`call_tool` 在调用时 `_json_loads_with_repair(tool_call.input)` 解析 → 函数工具收到最终 JSON。中间没有任何环节重新从别处取 input，故改后值必然送达。

spike 三组对偶测试（`test_t0_spike.py::test_h1_*`）：
- 原地 `tc.input = json.dumps(updated)` → 假 shell 实收 `command = "echo P2-session-<uuid> ..."` → **生效**；
- `input_kwargs["tool_call"] = tc.model_copy(update={...})` → 同样生效；
- 只把 `json.loads` 结果存局部变量改完不写回 → 假 shell 实收原 command → **不生效**（对照组，证明测试本身能区分生效/不生效）。

## H2 代码级证据

1. `qwenpaw/runtime/runtime.py` `_build_context`（L568-584）：每请求构造 `HookContext(session_id=request.session_id, ...)`（agent 组装前，per-request）。
2. `qwenpaw/runtime/builder.py` `_build_middlewares`（L1418-1422）：`reg.factory(ctx, agent_config)` —— 插件经 `PluginRegistry`（单例）注册的工厂在此被调用，收到的就是上面那个带 `session_id` 的 `ctx`；返回 `None` 的注册被跳过（L1424-1427）。
3. spike 用真实 `HookContext` + 真实 `PluginRegistry` 单例 `register_middleware` + 真实 `AgentBuilder._build_middlewares` 复现该路径（`test_h2_factory_receives_session_id`）：工厂收到的 `ctx.session_id == "spike-session-abc123"`，且中间件实例被 builder 采用。
4. 闭环（`test_h2_closed_loop_injects_session`）：session_id → ctx → 工厂持有 → `on_acting` 注入 → 假 shell 实收改后 command，与 H1 的注入形态（json.loads→改→json.dumps 写回）完全一致。

## 对 P2（会话注入落码）的实现建议

- 采用「`json.loads(input_kwargs["tool_call"].input)` → 若已是 shell 类工具且 `command` 未含目标变量 → 拼接 `export QWENPAW_SESSION_ID='...'` 前缀（幂等：已含则跳过）→ `json.dumps` 原地写回 `tc.input`」——即 spike 策略 A（对象引用传递，最贴近设计文档 §5 的形态）。
- 工厂内 `if not ctx.session_id: return None`（与 spike `test_h2_empty_session_factory_returns_none` 行为一致，空 session 不装中间件）。
- 工具白名单只认 shell 类工具（按 `tool_call.name` / 工具类型判断），避免误改其他工具的 input。

## P3-a 增补：新框架（#7565 @0db9a8e）双跑实测（2026-10-22）

**新框架差异点（实测）**：
1. `AgentBuilder._build_middlewares` 返回的列表里，插件中间件被包进 `qwenpaw.runtime.plugin_middleware.PluginMiddlewareGuard`（`guard.middleware` = 内层实例，钩子经 guard 转发；guard 负责插件钩子故障隔离 + 流回收）。因此任何「按类型挑出插件实例」的断言/驱动必须先解包，否则会误判"中间件没被采用"。
2. 任务态恢复与 `PluginLifecycle`（`load / unload / reload / update_config`）实测：`load` 后 router/hooks/middleware 各 1 条；`reload` 后仍各 1 条（无重复注册）；`unload(UNLOAD)` 后全部清零且不删插件源码目录。
3. 文件态 dispatch 任务（`<state_dir>/dispatch/<32hex>.json`，task_id 必须匹配 `^[0-9a-f]{32}$`）在 reload 后由新实例可读 —— D5「状态全落文件」跨重载成立。
4. `register_shutdown_hook` 在新框架已标记 deprecated（建议 `api.effect` / runtime ledger）；本插件保留交集 API，警告不影响功能。

**双跑结果**：旧框架（venv 2.2.2）**31 passed, 1 skipped**（新框架专属 lifecycle 测试自动 skip）；新框架（PYTHONPATH=#7565 树）**32 passed**。环境依赖：新树需 `typesafe_sdk`（PyPI 0.7.5）。
