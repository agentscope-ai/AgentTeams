"""TeamHarness integration implemented with QwenPaw 2 public plugin APIs."""

from __future__ import annotations

import importlib.util
import asyncio
import json
from contextvars import ContextVar
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
                return False
            task_id, root, assignee, delegation = self._task_scope
            try:
                from qwenpaw_worker.cleanup_policy import is_private_cleanup
                plain = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
                base = Path(cwd) if cwd else None
                words = shlex.split(plain)
                if words and words[0] in {"mv", "/bin/mv", "/usr/bin/mv"}:
                    if len(words) != 3 or re.search(r"[;&|<>`$\n*?(){}\[\]]", plain) or not all(Path(p).is_absolute() for p in words[1:]):
                        return False
                elif not is_private_cleanup(plain, root, None, base):
                    return False
                for word in words[1:]:
                    if word.startswith("-"):
                        continue
                    target = Path(word)
                    # Do not interpret relative paths against a guessed cwd.
                    if not target.is_absolute() and base is None:
                        return False
                    target = (target if target.is_absolute() else base / target).resolve()
                    relative = target.relative_to(root)
                    if not relative.parts or relative.parts[0] in {"meta.json", "spec.md", "result.md", "base", "inputs"}:
                        return False
                current = await asyncio.to_thread(_controller_json, "tasks/" + urllib.parse.quote(task_id, safe="") + "/meta")
                return (current.get("task_id") == task_id and current.get("status") == "in_progress"
                        and current.get("assigned_to") == assignee and current.get("notifyTxn") == delegation)
            except (ImportError, OSError, ValueError, KeyError):
                return False

        async def on_check_permission(self, agent: Any, input_kwargs: dict[str, Any], next_handler: Callable[..., Any]) -> Any:
            call = input_kwargs.get("tool_call")
            command = str((input_kwargs.get("tool_input") or {}).get("command") or "")
            if getattr(call, "name", "") == "execute_shell_command" and (_compound_private_cleanup(command) or self._task_scope and _compound_task_move(command)):
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
                elif name == "projectflow" and args.get("action") in {"start_project", "create_project", "create_quick_project"}:
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
                and (_compound_private_cleanup(command) or self._task_scope and _compound_task_move(command))
            ):
                from agentscope.tool import ToolResponse

                yield ToolResponse(state="error", content=[{"type": "text", "text": _CLEANUP_GUIDANCE}])
                return
            async for item in next_handler(**input_kwargs):
                if name == "projectflow" and args.get("action") == "accept_task_result" and _review_task.get():
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


def _compound_task_move(command: str) -> bool:
    command = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
    return bool(re.search(r"[;&|\n]", command) and re.search(r"(?:^|[;&\n])\s*(?:/bin/|/usr/bin/)?mv\b", command))


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


async def _manager_report_context(ctx: Any) -> bool:
    if ctx.agent is None:
        return False
    request = ctx.request.model_dump() if hasattr(ctx.request, "model_dump") else vars(ctx.request)
    meta = _request_meta(ctx.request)
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
    if api_event.get("kind") == "project_ready":
        project = await asyncio.to_thread(_controller_json, "projects/" + urllib.parse.quote(api_event["project_id"], safe="") + "/meta")
        nodes = project.get("tasks", [])
        parents = {node.get("parent_task_id") for node in nodes}
        leaves = [node for node in nodes if node.get("task_id") not in parents]
        if (os.getenv("AGENTTEAMS_AGENT_ROLE") not in {"leader", "team_leader"}
                or project.get("status") != "active" or project.get("plan_type") == "loop"
                or not leaves or any(node.get("status") != "completed" for node in leaves)):
            return True
    if api_event.get("kind") in {"task_assigned", "task_submitted"}:
        task = await asyncio.to_thread(_controller_json, "tasks/" + urllib.parse.quote(api_event["task_id"], safe="") + "/meta")
        if task.get("project_id") != api_event.get("project_id"):
            return True
        if api_event["kind"] == "task_assigned":
            if task.get("status") not in {"assigned", "in_progress"} or task.get("notifyTxn") != api_event.get("delivery_id"):
                return True
            if task.get("assigned_to") not in {meta.get("agent_matrix_user_id"), os.getenv("AGENTTEAMS_WORKER_NAME")}:
                return True
            ctx.extras["execution_task_token"] = _execution_task.set(api_event["task_id"])
        elif task.get("status") != "submitted" or task.get("submission_id") != api_event.get("submission_id"):
            return True
        elif os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"}:
            ctx.extras["review_task_token"] = _review_task.set((api_event["task_id"], api_event["submission_id"]))
        ctx.inject_context("Authoritative task state was checked before execution: " + json.dumps({k:task.get(k) for k in ("task_id", "project_id", "status", "assigned_to", "spec_path", "result_path", "submission_id")}), source="teamharness_api_state")
    if os.getenv("AGENTTEAMS_AGENT_ROLE") != "manager":
        if os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"} and api_event.get("kind") in {"task_submitted", "project_ready"}:
            for tool in await ctx.agent.toolkit.get_tool_schemas():
                name = tool["function"]["name"]
                if name.split("__")[-1].lower() in {"execute_shell_command", "bash", "write_file", "edit_file", "edit", "write"}:
                    await ctx.agent.toolkit.remove_tool(name)
            ctx.inject_context(
                "This is result acceptance, not another execution assignment. Read the supplied result, "
                "use check_task and accept_task_result, and rely on independent QA's actual test evidence. "
                "Do not rerun accepted tests, edit deliverables, clean files or reproduce the Worker's work. "
                "If evidence is missing or inconsistent, request a revision instead of performing the work yourself.",
                source="teamharness_acceptance_tools",
            )
        if os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"} and os.getenv("TEAMHARNESS_COORDINATION_URL"):
            ctx.inject_context(
                "Your coordination transport is the Agent API. For a finite plan with known execution specs, "
                "use projectflow start_project with dispatchMode automatic, all leaf specs, assignees and dependencies. "
                "Accept checked results; the task system dispatches newly ready leaves. Matrix messages are progress "
                "projections and do not wake Agents. Do not send duplicate assignments or poll for progress. "
                "For task_submitted, check and accept or return only this submission, then end this turn. "
                "The Controller sends project_ready when all execution leaves are accepted. Only on project_ready, "
                "verify the whole request and call complete_project with projectId and report (your concise final "
                "user report). This closes coordinating parents, saves the report and notifies the requester in one operation.",
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
            return False
        event = meta.get("teamharness_workflow") or {}
        if event.get("type") != "PROJECT_COMPLETED" or not event.get("project_id"):
            return True
        project = await asyncio.to_thread(_controller_json, "projects/" + urllib.parse.quote(event["project_id"], safe="") + "/meta")
        if project.get("status") != "completed":
            return True
        team = await asyncio.to_thread(_controller_json, "teams/" + urllib.parse.quote(project.get("team_id", ""), safe=""))
        if not any(m.get("role") == "team_leader" and m.get("matrixUserID") == sender for m in team.get("members", [])):
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
                    name="Manager", content=[{"type": "text", "text": "NO_REPLY"}], role="assistant"))
            return HookResult()

    class CoordinationExit(HookBase):
        phase = Phase.FINALLY
        name = "teamharness_coordination_exit"
        priority = 190

        async def run(self, ctx: Any) -> Any:
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
