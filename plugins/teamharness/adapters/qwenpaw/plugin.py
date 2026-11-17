"""TeamHarness integration implemented with QwenPaw 2 public plugin APIs."""

from __future__ import annotations

import importlib.util
import asyncio
import json
from contextvars import ContextVar
from contextlib import nullcontext
import os
from pathlib import Path
import re
import shlex
from typing import Any, AsyncGenerator, Callable
import urllib.request
import urllib.parse


PLUGIN_DIR = Path(__file__).resolve().parent
_requester_route: ContextVar[dict[str, Any] | None] = ContextVar("teamharness_requester_route", default=None)
_coordinator_route: ContextVar[dict[str, Any] | None] = ContextVar("teamharness_coordinator_route", default=None)
_source_event: ContextVar[str] = ContextVar("teamharness_source_event", default="")
_execution_task: ContextVar[str] = ContextVar("teamharness_execution_task", default="")
_review_task: ContextVar[tuple[str, str] | None] = ContextVar("teamharness_review_task", default=None)
_ack_scope: ContextVar[tuple | None] = ContextVar("teamharness_ack_scope", default=None)
_approval_status_records: dict[str, dict[str, Any]] = {}
ASSET_DIR = PLUGIN_DIR / "teamharness"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent


def _team_prompt(_agent: Any) -> str:
    if os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
        # The Harness binds the role prompt as MANAGER.md in the native agent.
        return ""
    path = ASSET_DIR / "prompts" / "team" / "TEAMS.md"
    try:
        return path.read_text(encoding="utf-8").strip()
    except OSError:
        return ""


def _skill_dir() -> Path:
    if os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
        return ASSET_DIR / "skills/manager"
    return ASSET_DIR / "qwenpaw-skills"


def _sanitizer_rules() -> list[str]:
    raw = os.getenv("AGENTTEAMS_OUTPUT_SANITIZE_KEYWORDS", "")
    return [value.strip() for value in raw.split(",") if value.strip()]


def _sanitize(value: Any) -> None:
    rules = _sanitizer_rules()
    if not rules:
        return
    if isinstance(value, dict):
        for key, item in list(value.items()):
            if isinstance(item, str):
                for rule in rules:
                    item = re.sub(
                        re.escape(rule),
                        "[REDACTED]",
                        item,
                        flags=re.IGNORECASE,
                    )
                value[key] = item
            else:
                _sanitize(item)
        return
    if isinstance(value, list):
        for item in value:
            _sanitize(item)
        return
    for attr in ("content", "output", "text"):
        if hasattr(value, attr):
            item = getattr(value, attr)
            if isinstance(item, str):
                for rule in rules:
                    item = re.sub(
                        re.escape(rule),
                        "[REDACTED]",
                        item,
                        flags=re.IGNORECASE,
                    )
                setattr(value, attr, item)
            else:
                _sanitize(item)


def _sanitizer_factory(_ctx: Any, _agent_config: Any):
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    class TeamHarnessSanitizer(MiddlewareBase):
        _submitted_task = ""
        _accepted_task = ""
        _task_scope = None

        async def _allow_task_output_change(self, command: str, cwd: str | None) -> bool:
            if not self._task_scope:
                self._task_scope = _ack_scope.get()
            if not self._task_scope:
                return False
            task_id, root, assignee, delegation = self._task_scope
            try:
                from qwenpaw_worker.cleanup_policy import is_private_cleanup
                plain = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
                base = Path(cwd) if cwd else None
                words = shlex.split(plain)
                pattern_cleanup = False
                if words and words[0] in {"mv", "/bin/mv", "/usr/bin/mv"}:
                    if len(words) != 3 or re.search(r"[;&|<>`$\n*?(){}\[\]]", plain) or not all(Path(p).is_absolute() for p in words[1:]):
                        return False
                elif not is_private_cleanup(plain, root, None, base):
                    if (not words or words[0] not in {"rm", "/bin/rm", "/usr/bin/rm"}
                            or re.search(r"[;&|<>`$\n(){}]", plain)
                            or not any(re.search(r"[*?\[]", word) for word in words[1:])
                            or any(word.startswith("-") and word not in {"-r", "-f", "-rf", "-fr", "--recursive", "--force", "--"} for word in words[1:])):
                        return False
                    pattern_cleanup = True
                for word in words[1:]:
                    if word.startswith("-"):
                        continue
                    target = Path(word)
                    if pattern_cleanup and (word.endswith("/") or re.search(r"[*?\[]", str(target.parent))):
                        return False
                    # Do not interpret relative paths against a guessed cwd.
                    if not target.is_absolute() and base is None:
                        return False
                    target = (target if target.is_absolute() else base / target).resolve()
                    relative = target.relative_to(root)
                    if not relative.parts or relative.parts[0] in {"meta.json", "spec.md", "result.md", "base", "inputs"}:
                        return False
                    if pattern_cleanup and (len(relative.parts) < 2 or relative.parts[0] not in {"workspace", "progress"}):
                        return False
                current = await asyncio.to_thread(_controller_json, "tasks/" + urllib.parse.quote(task_id, safe="") + "/meta")
                return (current.get("task_id") == task_id and current.get("status") == "in_progress"
                        and current.get("assigned_to") == assignee and current.get("notifyTxn") == delegation)
            except (ImportError, OSError, ValueError, KeyError):
                return False

        async def on_check_permission(self, agent: Any, input_kwargs: dict[str, Any], next_handler: Callable[..., Any]) -> Any:
            self._task_scope = self._task_scope or _ack_scope.get()
            call = input_kwargs.get("tool_call")
            command = str((input_kwargs.get("tool_input") or {}).get("command") or "")
            if getattr(call, "name", "") == "execute_shell_command" and (_compound_private_cleanup(command) or self._task_scope and _compound_output_change(command)):
                from agentscope.permission import PermissionBehavior, PermissionDecision
                return PermissionDecision(behavior=PermissionBehavior.DENY, message=_CLEANUP_GUIDANCE)
            if getattr(call, "name", "") == "execute_shell_command" and await self._allow_task_output_change(command, (input_kwargs.get("tool_input") or {}).get("cwd")):
                from agentscope.permission import PermissionBehavior, PermissionDecision
                return PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Cleanup is confined to the current acknowledged task's writable outputs.")
            return await next_handler(**input_kwargs)

        async def on_reasoning(self, agent: Any, input_kwargs: dict[str, Any], next_handler: Callable[..., AsyncGenerator[Any, None]]) -> AsyncGenerator[Any, None]:
            if self._submitted_task or self._accepted_task:
                from agentscope.message import Msg
                message = Msg(name=agent.name, role="assistant", content=[{
                    "type": "text", "text": (f"Task {self._submitted_task} result submitted; awaiting acceptance."
                        if self._submitted_task else f"Task {self._accepted_task} accepted; the task system handles downstream coordination.")}])
                agent.state.append_context(agent.name, message.content)
                yield message
                return
            async for item in next_handler(**input_kwargs):
                yield item

        async def on_acting(
            self,
            agent: Any,
            input_kwargs: dict[str, Any],
            next_handler: Callable[..., AsyncGenerator[Any, None]],
        ) -> AsyncGenerator[Any, None]:
            del agent
            self._task_scope = self._task_scope or _ack_scope.get()
            call = input_kwargs.get("tool_call")
            args = getattr(call, "input", {})
            json_input = isinstance(args, str)
            if json_input:
                try:
                    args = json.loads(args)
                except ValueError:
                    args = {}
            route = _requester_route.get()
            coordinator = _coordinator_route.get()
            name = str(getattr(call, "name", "")).split("__")[-1]
            if (route or coordinator) and isinstance(args, dict):
                if name == "message":
                    if _source_event.get():
                        args["originEventId"] = _source_event.get()
                    if route:
                        args["requesterReplyRoute"] = route
                    if coordinator:
                        args["coordinatorReplyRoute"] = coordinator
                elif (name == "projectflow" and args.get("action") in {"start_project", "create_project", "create_quick_project"}) or (name == "taskflow" and args.get("action") == "start_task"):
                    if isinstance(args.get("payload"), dict):
                        if route:
                            args["payload"]["requesterReplyRoute"] = route
                        if coordinator:
                            args["payload"]["replyRoute"] = coordinator
                if json_input:
                    call.input = json.dumps(args, ensure_ascii=False)
            command = str(args.get("command") or "") if isinstance(args, dict) else ""
            if (
                getattr(call, "name", "") == "execute_shell_command"
                and (_compound_private_cleanup(command) or self._task_scope and _compound_output_change(command))
            ):
                from agentscope.tool import ToolResponse

                yield ToolResponse(state="error", content=[{"type": "text", "text": _CLEANUP_GUIDANCE}])
                return
            async for item in next_handler(**input_kwargs):
                if name in {"projectflow", "taskflow"} and args.get("action") == "accept_task_result" and _review_task.get():
                    for block in getattr(item, "content", []):
                        text = block.get("text", "") if isinstance(block, dict) else getattr(block, "text", "")
                        try:
                            result = json.loads(text)
                        except (TypeError, ValueError):
                            continue
                        if (isinstance(result, dict) and result.get("ok") is True and result.get("synced") is True
                                and result.get("accepted") is True and result.get("nodeStatus") == "completed"
                                and (result.get("taskId"), result.get("submissionId")) == _review_task.get()
                                and (result.get("dispatch") or {}).get("ok") is True):
                            self._accepted_task = result["taskId"]
                if name == "taskflow" and args.get("action") in {"ack_task", "submit_task"}:
                    for block in getattr(item, "content", []):
                        text = block.get("text", "") if isinstance(block, dict) else getattr(block, "text", "")
                        try:
                            result = json.loads(text)
                        except (TypeError, ValueError):
                            continue
                        if not isinstance(result, dict):
                            continue
                        task = result.get("task") or {}
                        if result.get("ok") is True and result.get("synced") is True and args.get("action") == "ack_task" and task.get("status") == "in_progress":
                            directory = Path(result.get("taskDirectory") or ".")
                            if directory.is_absolute() and directory.name == task.get("task_id") and directory.parent.name == "tasks" and task.get("notifyTxn"):
                                self._task_scope = (task["task_id"], directory.resolve(), task.get("assigned_to"), task["notifyTxn"])
                        if args.get("action") != "submit_task":
                            continue
                        if result.get("ok") is True and result.get("synced") is True and task.get("status") == "submitted":
                            self._task_scope = None
                        if (isinstance(result, dict) and result.get("ok") is True and result.get("synced") is True
                                and (result.get("notification") or {}).get("sent") is True
                                and _execution_task.get() and (result.get("task") or {}).get("task_id") == _execution_task.get()
                                and (result.get("task") or {}).get("status") == "submitted"):
                            self._submitted_task = _execution_task.get()
                _sanitize(item)
                yield item

    return TeamHarnessSanitizer()


_CLEANUP_GUIDANCE = (
    "No command was executed. Separate private temporary-directory cleanup from setup/tests. "
    "Use a fresh mktemp directory without deleting an old one, then run cleanup as a standalone "
    "rm with its explicit private path. Shell variables do not persist between calls. "
    "For output replacement, use write_file or a standalone mv with explicit absolute source and destination paths. "
    "Shared/out-of-scope deletion still requires approval."
)


def _compound_private_cleanup(command: str) -> bool:
    command = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
    return bool(("TMPDIR" in command or "mktemp" in command) and re.search(r"[;&|\n*?\[]", command)
                and re.search(r"(?:^|[;&\n])\s*(?:/bin/|/usr/bin/)?rm\b", command))


def _compound_output_change(command: str) -> bool:
    command = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
    return bool(re.search(r"[;&|\n]", command) and re.search(r"(?:^|[;&\n])\s*(?:/bin/|/usr/bin/)?(?:mv|rm)\b", command))


def _load_trace_module() -> Any | None:
    path = PLUGIN_DIR / "task_trace.py"
    if not path.is_file():
        return None
    spec = importlib.util.spec_from_file_location(
        "agentteams_teamharness_task_trace",
        path,
    )
    if spec is None or spec.loader is None:
        return None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _request_meta(request: Any) -> dict[str, Any]:
    data = request.model_dump() if hasattr(request, "model_dump") else vars(request)
    meta = dict(data.get("channel_meta") or data.get("meta") or {})
    context = data.get("request_context") or {}
    if isinstance(context.get("teamharness_coordination"), dict):
        meta.update(context["teamharness_coordination"])
    return meta


async def _coordination_approvals() -> list[dict[str, Any]]:
    from qwenpaw.app.approvals import get_approval_service
    pending = await get_approval_service().get_all_pending_by_agent("default")
    for item in pending:
        if not item.session_id.startswith("coordination-") or item.request_id in _approval_status_records:
            continue
        record = {
            "version": 1, "runtime": "qwenpaw", "request_id": item.request_id,
            "session_id": item.session_id, "status": "pending", "tool_name": item.tool_name,
            "parameters": json.dumps(item.extra.get("tool_call", {}).get("input", {}), ensure_ascii=False),
            "severity": item.severity, "risk_details": item.result_summary,
            "expires_at": int((item.created_at + item.timeout_seconds) * 1000),
        }
        _approval_status_records[item.request_id] = record
        def resolved(future, record=record):
            try:
                decision = future.result()
                record["status"] = {"approved": "approved", "denied": "rejected", "timeout": "expired"}.get(getattr(decision, "value", decision), "cancelled")
            except asyncio.CancelledError:
                record["status"] = "cancelled"
        item.future.add_done_callback(resolved)
    for key in list(_approval_status_records):
        if len(_approval_status_records) <= 200:
            break
        if _approval_status_records[key]["status"] != "pending":
            _approval_status_records.pop(key)
    return list(_approval_status_records.values())


def _controller_json(path: str) -> dict[str, Any]:
    token_file = os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE", "/var/run/agentteams/cli-token")
    token = Path(token_file).read_text().strip()
    base = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/")
    req = urllib.request.Request(base + "/api/v1/" + path, headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=5) as response:
        return json.loads(response.read())


def _room_id(request: Any) -> str:
    if hasattr(request, "model_dump"):
        data = request.model_dump()
    elif hasattr(request, "__dict__"):
        data = vars(request)
    else:
        data = {}
    meta = _request_meta(request)
    room = str(meta.get("room_id") or meta.get("roomId") or "") if isinstance(meta, dict) else ""
    if room:
        return room
    session = str(data.get("session_id") or "")
    for prefix in ("agentteams_matrix:", "matrix:"):
        if session.startswith(prefix):
            return session[len(prefix):]
    return ""


def _register_trace_hooks(api: Any) -> None:
    try:
        from qwenpaw.runtime.hooks import HookBase, HookResult
        from qwenpaw.runtime.phases import Phase
    except ImportError:
        return
    trace = _load_trace_module()
    if trace is None:
        return
    workspace = Path(os.getenv("QWENPAW_WORKING_DIR", ".")) / "workspaces" / "default"
    trace.register_task_trace_processor(workspace)

    class TraceEnter(HookBase):
        phase = Phase.PRE_DISPATCH
        name = "teamharness_trace_enter"
        priority = 20

        async def run(self, ctx: Any) -> Any:
            room = _room_id(ctx.request)
            if room:
                ctx.extras["teamharness_trace_token"] = trace.set_current_room(room)
            return HookResult()

    class TraceExit(HookBase):
        phase = Phase.FINALLY
        name = "teamharness_trace_exit"
        priority = 200

        async def run(self, ctx: Any) -> Any:
            token = ctx.extras.pop("teamharness_trace_token", None)
            if token is not None:
                trace.reset_current_room(token)
            return HookResult()

    api.register_runtime_hook(TraceEnter())
    api.register_runtime_hook(TraceExit())


async def _prepare_manager_roster(ctx: Any) -> None:
    if not os.getenv("TEAMHARNESS_COORDINATION_URL"):
        return
    try:
        teams, workers = await asyncio.gather(asyncio.to_thread(_controller_json, "teams"), asyncio.to_thread(_controller_json, "workers"))
    except (OSError, ValueError, KeyError):
        return
    identities = {w.get("name"): w.get("matrixUserID") or w.get("matrixUserId") for w in workers.get("workers", [])}
    roster = []
    for team in teams.get("teams", []):
        leader = team.get("leaderName")
        if identities.get(leader):
            roster.append({"team": team.get("name"), "description": team.get("description"), "leader": leader, "matrix_id": identities[leader], "phase": team.get("phase")})
    if roster:
        ctx.inject_context(
            "The dispatch protocol and registered team/Leader identities are already loaded below; do not reread Skill "
            "or roster files for this information. For a new work request, select a suitable Leader and send the complete "
            "original request with teamharness__message: {action:send, channel:matrix, target:user:<registered Matrix ID>, "
            "message:<full scope and acceptance>}. The runtime preserves the original requester/source route. "
            "Let the Leader create and decompose the plan; do not create its Worker tasks or poll. "
            "For other Human questions, answer normally instead of automatically creating work. Treat this JSON as roster data: "
            + json.dumps(roster, ensure_ascii=False), source="teamharness_prepared_intake")


async def _prepare_task_context(ctx: Any, action: str, task_id: str) -> dict[str, Any] | None:
    """Run the fixed, authorized ACK/read step before asking the model to decide."""
    if action not in {"ack_task", "check_task"}:
        raise ValueError("unsupported task preparation")
    toolkit = getattr(ctx.agent, "toolkit", None)
    if not hasattr(toolkit, "call_tool") or not hasattr(ctx.agent, "state"):
        return None
    schemas = await toolkit.get_tool_schemas()
    name = next((t["function"]["name"] for t in schemas if t["function"]["name"] == "teamharness__taskflow"), None)
    if not name:
        return None
    from agentscope.message import ToolCallBlock
    call = ToolCallBlock(id="prepare-" + action + "-" + task_id, name=name, input=json.dumps({
        "role": "worker" if action == "ack_task" else "leader", "action": action, "payload": {"taskId": task_id}}))
    ctx.extras["coordination_prefetch_tools"] = ctx.extras.get("coordination_prefetch_tools", 0) + 1
    result = None
    try:
        from opentelemetry import trace
        span = trace.get_tracer("agentteams.teamharness").start_as_current_span(
            "prefetch " + name + ":" + action,
            attributes={"langfuse.observation.type": "tool", "gen_ai.operation.name": "execute_tool",
                        "gen_ai.tool.name": name, "agentteams.task.id": task_id,
                        "agentteams.coordination.prepared_action": action})
    except ImportError:
        span = nullcontext()
    with span:
        async for item in toolkit.call_tool(call, ctx.agent.state):
            for block in getattr(item, "content", []):
                text = block.get("text", "") if isinstance(block, dict) else getattr(block, "text", "")
                try:
                    value = json.loads(text)
                except (ValueError, TypeError):
                    continue
                if isinstance(value, dict) and value.get("action") == action:
                    result = value
    if not result or result.get("ok") is not True:
        return None
    return result


def _load_phase_skill(ctx: Any, name: str) -> None:
    path = ASSET_DIR / "skills/team" / name / "SKILL.md"
    if path.is_file():
        text = path.read_text(encoding="utf-8")
        if name == "project-management":
            text = text.partition("## Project Files")[0]
            text += "\nFor manual planning or Loop iteration, read the complete skill at " + str(path) + ".\n"
        ctx.inject_context("The builtin " + name + " phase instructions are already loaded for this turn; do not reread them.\n" + text, source="teamharness_preloaded_skill")


async def _manager_report_context(ctx: Any) -> bool:
    if ctx.agent is None:
        return False
    request = ctx.request.model_dump() if hasattr(ctx.request, "model_dump") else vars(ctx.request)
    meta = _request_meta(ctx.request)
    if meta.get("task_execution"):
        # Startup admissions carry a process-fenced Task, not a new Matrix
        # assignment or a request to recreate its legacy Project context.
        from task_recovery import Recovery, RecoveryConflict
        identity = meta["task_execution"]
        recovery = Recovery()
        task = await asyncio.to_thread(recovery.current, identity["task_id"], identity["team_id"])
        execution = task.get("execution", {}) if task else {}
        if (execution.get("process_id") != recovery.process
                or execution.get("instance_id") != identity["instance_id"]
                or execution.get("generation") != identity["generation"]
                or execution.get("state") not in {"active", "needs_verification"}):
            raise RecoveryConflict("Task execution changed before native reasoning")
        ctx.extras["execution_task_token"] = _execution_task.set(identity["task_id"])
        _load_phase_skill(ctx, "task-execution")
        return False
    if meta.get("event_id"):
        ctx.extras["source_event_token"] = _source_event.set(str(meta["event_id"]))
    route = meta.get("teamharness_requester_route")
    if isinstance(route, dict) and route.get("target_session"):
        ctx.extras.setdefault("requester_route_token", _requester_route.set(route))
        ctx.inject_context("Preserve the original requester route when creating the task: " + json.dumps(route), source="teamharness_requester_route")
    coordinator = meta.get("teamharness_coordinator_route")
    if os.getenv("AGENTTEAMS_AGENT_ROLE") != "manager" and isinstance(coordinator, dict) and coordinator.get("target_session"):
        ctx.extras["coordinator_route_token"] = _coordinator_route.set(coordinator)
        ctx.inject_context("Send the verified final result to this coordinator replyRoute; the coordinator relays it to the original requester: " + json.dumps(coordinator), source="teamharness_coordinator_route")
    api_event = meta.get("teamharness_coordination") or {}
    if meta.get("teamharness_matrix_coordination"):
        receipt = await asyncio.to_thread(_controller_json, "coordination/events/" + urllib.parse.quote(api_event.get("id", ""), safe=""))
        if (api_event.get("kind") not in {"handoff", "task_assigned", "task_submitted", "project_ready", "main_task_ready"}
                or receipt.get("actor") != meta.get("sender_id")
                or receipt.get("recipient") != meta.get("agent_matrix_user_id")
                or receipt.get("room_id", "") != meta.get("room_id", "")
                or any(receipt.get(key, "") != api_event.get(key, "") for key in
                       ("kind", "task_id", "main_task_id", "project_id", "delivery_id", "submission_id"))):
            return True
    automatic_plan = api_event.get("dispatch_mode") == "automatic" and api_event.get("plan_type") != "loop"
    if api_event.get("kind") in {"project_ready", "main_task_ready"}:
        main_id = api_event.get("main_task_id", "")
        project = await asyncio.to_thread(_controller_json,
            ("tasks/" + urllib.parse.quote(main_id, safe="") if main_id else "projects/" + urllib.parse.quote(api_event["project_id"], safe="")) + "/meta")
        nodes = project.get("tasks", [])
        parents = {node.get("parent_task_id") for node in nodes}
        leaves = [node for node in nodes if node.get("task_id") not in parents]
        if (os.getenv("AGENTTEAMS_AGENT_ROLE") not in {"leader", "team_leader"}
                or project.get("status") not in ({"in_progress", "submitted"} if main_id else {"active"}) or project.get("plan_type") == "loop"
                or not leaves or any(node.get("status") != "completed" for node in leaves)):
            return True
    if api_event.get("kind") in {"task_assigned", "task_submitted"}:
        task = await asyncio.to_thread(_controller_json, "tasks/" + urllib.parse.quote(api_event["task_id"], safe="") + "/meta")
        if (task.get("project_id", "") != api_event.get("project_id", "")
                or task.get("main_task_id", "") != api_event.get("main_task_id", "")):
            return True
        if api_event["kind"] == "task_assigned":
            if task.get("status") not in {"assigned", "in_progress"} or task.get("notifyTxn") != api_event.get("delivery_id"):
                return True
            if task.get("assigned_to") not in {meta.get("agent_matrix_user_id"), os.getenv("AGENTTEAMS_WORKER_NAME")}:
                return True
            ctx.extras["execution_task_token"] = _execution_task.set(api_event["task_id"])
            prepared = await _prepare_task_context(ctx, "ack_task", api_event["task_id"])
            if prepared and prepared.get("synced") is True:
                acknowledged = prepared.get("task") or {}
                directory = Path(prepared.get("taskDirectory") or ".")
                if (acknowledged.get("task_id") == api_event["task_id"] and acknowledged.get("project_id", "") == api_event.get("project_id", "") and acknowledged.get("main_task_id", "") == api_event.get("main_task_id", "") and acknowledged.get("status") == "in_progress"
                        and acknowledged.get("notifyTxn") == api_event.get("delivery_id")
                        and directory.is_absolute() and directory.name == api_event["task_id"] and directory.parent.name == "tasks"):
                    ctx.extras["ack_scope_token"] = _ack_scope.set((api_event["task_id"], directory.resolve(), acknowledged.get("assigned_to"), acknowledged["notifyTxn"]))
                    task = acknowledged
                    context = {key: prepared.get(key) for key in ("taskDirectory", "scratchDirectory", "spec", "inputs", "inputPullErrors")}
                    ctx.inject_context("Your assignment is already acknowledged and synchronized. Do not ACK, pull or reread the supplied spec again. Execute from this authoritative context: " + json.dumps(context, ensure_ascii=False), source="teamharness_prepared_ack")
                    _load_phase_skill(ctx, "task-execution")
        elif task.get("status") != "submitted" or task.get("submission_id") != api_event.get("submission_id"):
            return True
        elif os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"}:
            if automatic_plan:
                ctx.extras["review_task_token"] = _review_task.set((api_event["task_id"], api_event["submission_id"]))
            prepared = await _prepare_task_context(ctx, "check_task", api_event["task_id"])
            checked = (prepared or {}).get("task") or {}
            if (prepared and prepared.get("pulled") and checked.get("task_id") == api_event["task_id"]
                    and checked.get("project_id", "") == api_event.get("project_id", "") and checked.get("main_task_id", "") == api_event.get("main_task_id", "") and checked.get("status") == "submitted"
                    and checked.get("submission_id") == api_event["submission_id"]):
                context = {key: prepared.get(key) for key in ("result", "spec", "report", "validationErrors", "effective", "contextTruncated")}
                ctx.extras["prepared_review"] = True
                ctx.inject_context("check_task has already pulled and checked this exact submission. Do not call it or filesync again for the supplied evidence. Review this complete receipt, and accept or return it with taskflow action accept_task_result and payload {mainTaskId, taskId, submissionId, resultStatus, accepted} for a main-task plan; use projectflow with projectId only for legacy Project plans. Keep role leader. The tool still enforces current identity, digest and review gates. Read more only when this receipt is truncated or genuinely insufficient.\n" + json.dumps(context, ensure_ascii=False), source="teamharness_prepared_review")
        ctx.inject_context("Authoritative task state was checked before execution: " + json.dumps({k:task.get(k) for k in ("task_id", "main_task_id", "project_id", "status", "assigned_to", "spec_path", "result_path", "submission_id")}), source="teamharness_api_state")
    if os.getenv("AGENTTEAMS_AGENT_ROLE") != "manager":
        if api_event.get("kind") == "handoff":
            _load_phase_skill(ctx, "project-management")
        if api_event.get("kind") == "main_task_ready":
            ctx.inject_context("All execution subtasks of this main Task are accepted. Use the supplied summaries, verify the whole request, then call taskflow complete_task with role leader and payload {taskId: " + json.dumps(api_event["main_task_id"]) + ", report}. Do not complete an optional Project, wait for its unrelated tasks, or repeat accepted work.", source="teamharness_prepared_closure")
        if api_event.get("kind") == "project_ready":
            ctx.inject_context("You already accepted each execution result. Use the supplied accepted-result summaries as the reviewed evidence; do not pull or re-check unchanged reports. Prepare the final user report and call projectflow complete_project with role leader and payload {projectId, report}. Investigate only an actual missing or inconsistent requirement.", source="teamharness_prepared_closure")
        if os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"} and api_event.get("kind") in {"task_submitted", "project_ready", "main_task_ready"}:
            for tool in await ctx.agent.toolkit.get_tool_schemas():
                name = tool["function"]["name"]
                if name.split("__")[-1].lower() in {"execute_shell_command", "bash", "write_file", "edit_file", "edit", "write"}:
                    await ctx.agent.toolkit.remove_tool(name)
            if api_event.get("kind") == "task_submitted":
                check = "the prepared check_task receipt" if ctx.extras.get("prepared_review") else "check_task"
                ctx.inject_context(
                    "This is result acceptance, not another execution assignment. Review the supplied result using " + check +
                    " and accept_task_result, relying on independent QA's actual test evidence. Do not rerun accepted tests, "
                    "edit deliverables, clean files or reproduce the Worker's work. If evidence is missing or inconsistent, "
                    "request a revision instead of performing the work yourself.", source="teamharness_acceptance_tools")

        if os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"} and os.getenv("TEAMHARNESS_COORDINATION_URL"):
            if api_event.get("kind") == "task_submitted" and not automatic_plan:
                ctx.inject_context(
                    "Check and accept or return this submission, then continue your existing manual DAG or Loop "
                    "iteration/delegation protocol. Do not end merely because one result was accepted. "
                    "A Loop does not receive the finite project_ready closure event.",
                    source="teamharness_manual_continuation",
                )
                return False
            ctx.inject_context(
                "Your coordination events are delivered by the Controller. For a finite plan with known execution specs, "
                "use taskflow start_task with taskId, original spec, and tasks containing execution specs, assignees and dependencies. Do not create a Project for ordinary team tasks. Create a Project only when the user explicitly asks; automatic cross-team Project creation is not supported yet. "
                "Accept checked results; the task system dispatches newly ready leaves. Only scoped control notifications "
                "wake Agents; progress messages do not. Do not send duplicate assignments or poll for progress. "
                "For an automatic finite task_submitted, check and accept or return only this submission, then end this turn. "
                "For main-task plans the Controller sends main_task_ready after all subtasks are accepted; call taskflow complete_task with taskId and report then. For legacy Project plans it sends project_ready. Only on project_ready, "
                "verify the whole request and call complete_project with projectId and report (your concise final "
                "user report). This closes coordinating parents, saves the report and notifies the requester in one operation. "
                "For manual DAG or Loop plans, continue your existing iteration/delegation protocol after acceptance; "
                "Loops do not wait for the finite project_ready event.",
                source="teamharness_api_coordination",
            )
        return False
    sender = str(meta.get("sender_id") or request.get("user_id") or "")
    if not sender.startswith("@") or not _room_id(ctx.request):
        return False
    own_id = str(meta.get("agent_matrix_user_id") or "")
    if own_id:
        ctx.inject_context(
            f"Your Matrix identity is {own_id}. The current requester is {sender}; "
            f"the source conversation is {_room_id(ctx.request)}. Preserve these "
            "identities in your handoff; do not rediscover them from files or history.",
            source="teamharness_manager_identity",
        )

    try:
        humans = await asyncio.to_thread(_controller_json, "humans")
        if sender in {str(h.get("matrixUserID") or h.get("matrixUserId") or "") for h in humans.get("humans", [])}:
            ctx.extras.setdefault("requester_route_token", _requester_route.set({"channel": "matrix", "target_session": _room_id(ctx.request), "target_user": sender}))
            await _prepare_manager_roster(ctx)
            return False
        event = meta.get("teamharness_workflow") or {}
        main_id = event.get("task_id") if event.get("type") == "MAIN_TASK_COMPLETED" else ""
        if not main_id and (event.get("type") != "PROJECT_COMPLETED" or not event.get("project_id")):
            return True
        project = await asyncio.to_thread(_controller_json, ("tasks/" + urllib.parse.quote(main_id, safe="") if main_id else "projects/" + urllib.parse.quote(event["project_id"], safe="")) + "/meta")
        if project.get("status") != "completed":
            return True
        team = await asyncio.to_thread(_controller_json, "teams/" + urllib.parse.quote(project.get("team_id", ""), safe=""))
        if not any(m.get("role") == "team_leader" and m.get("matrixUserID") == sender for m in team.get("members", [])):
            return True
        report = meta.get("teamharness_final_report")
        if (meta.get("api_projection") and api_event.get("kind") in {"project_completed", "main_task_completed"}
                and (api_event.get("main_task_id") == main_id if main_id else api_event.get("project_id") == event["project_id"])
                and isinstance(report, str) and report.strip()):
            ctx.extras["verified_final_report"] = report
            return True
        route = project.get("requester_reply_route") or {}
        if route:
            ctx.extras["final_route_token"] = _requester_route.set(route)
            ctx.inject_context("The verified final result belongs to this requester route; send the outcome there: " + json.dumps(route), source="teamharness_final_route")
    except (OSError, ValueError, KeyError):
        # An unresolved sender cannot open a new management workflow.
        pass
    allowed = {"message", "artifact", "filesync", "read", "read_file", "skill", "get_current_time"}
    if meta.get("api_projection"):
        allowed.discard("message")
    toolkit = ctx.agent.toolkit
    for tool in await toolkit.get_tool_schemas():
        name = tool["function"]["name"]
        if name.split("__")[-1].lower() not in allowed:
            await toolkit.remove_tool(name)
    config = getattr(ctx.agent, "react_config", None)
    if config is not None:
        config.max_iters = min(config.max_iters, 8)
    ctx.inject_context(
        "This turn is an Agent update, not a Human management request. "
        "Ignore acknowledgements without new work. Relay a final result or blocker once, "
        "using its report or attachment. Do not inspect task state, poll, diagnose, "
        "change resources, or create extra evidence. End this turn after relaying.",
        source="teamharness_manager_report",
    )
    if meta.get("api_projection"):
        ctx.inject_context("This verified completion arrived through the Agent API. Reply with the concise final user report directly; the dispatcher projects that response into the recorded Matrix source conversation. Do not send another message.", source="teamharness_api_projection")
    return False


def _register_coordination_hook(api: Any) -> None:
    try:
        from qwenpaw.runtime.hooks import HookBase, HookResult, HookAction
        from qwenpaw.runtime.phases import Phase
    except ImportError:
        return

    class ManagerReport(HookBase):
        phase = Phase.PRE_EXECUTE
        name = "teamharness_manager_report"
        priority = 30

        async def run(self, ctx: Any) -> Any:
            if await _manager_report_context(ctx):
                from agentscope.message import Msg

                return HookResult(action=HookAction.SHORT_CIRCUIT, payload=Msg(
                    name="Manager", content=[{"type": "text", "text": getattr(ctx, "extras", {}).get("verified_final_report", "NO_REPLY")}], role="assistant"))
            return HookResult()

    class CoordinationExit(HookBase):
        phase = Phase.FINALLY
        name = "teamharness_coordination_exit"
        priority = 190

        async def run(self, ctx: Any) -> Any:
            token = ctx.extras.pop("ack_scope_token", None)
            if token is not None:
                _ack_scope.reset(token)
            token = ctx.extras.pop("review_task_token", None)
            if token is not None:
                _review_task.reset(token)
            token = ctx.extras.pop("execution_task_token", None)
            if token is not None:
                _execution_task.reset(token)
            token = ctx.extras.pop("source_event_token", None)
            if token is not None:
                _source_event.reset(token)
            token = ctx.extras.pop("coordinator_route_token", None)
            if token is not None:
                _coordinator_route.reset(token)
            for name in ("final_route_token", "requester_route_token"):
                token = ctx.extras.pop(name, None)
                if token is not None:
                    _requester_route.reset(token)
            return HookResult()

    api.register_runtime_hook(ManagerReport())
    api.register_runtime_hook(CoordinationExit())


class TeamHarnessPlugin:
    def register(self, api: Any) -> None:
        api.register_prompt_section(
            "teamharness_context",
            after="workspace",
            provider=_team_prompt,
            priority=40,
        )
        api.register_skill_provider(
            _skill_dir(),
            enabled_by_default=True,
            channels=["all"],
        )
        api.register_middleware(_sanitizer_factory, priority=30)
        _register_trace_hooks(api)
        _register_coordination_hook(api)
        self._register_http(api)

    def _register_http(self, api: Any) -> None:
        try:
            from fastapi import APIRouter
        except ImportError:
            return
        router = APIRouter()

        @router.get("/health")
        def health() -> dict[str, Any]:
            return {"ok": True, "plugin": "teamharness", "adapter": "qwenpaw-2"}

        @router.post("/sync")
        def sync_endpoint() -> dict[str, Any]:
            return {
                "ok": True,
                "plugin": "teamharness",
                "managedBy": "qwenpaw-plugin-api",
            }

        @router.get("/coordination/approvals")
        async def coordination_approvals() -> dict[str, Any]:
            return {"approvals": await _coordination_approvals()}

        api.register_http_router(
            router,
            prefix="/teamharness",
            tags=["teamharness"],
        )


plugin = TeamHarnessPlugin()
