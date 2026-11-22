"""TeamHarness integration implemented with QwenPaw 2 public plugin APIs."""

from __future__ import annotations

import importlib.util
import asyncio
import json
import logging
from functools import lru_cache
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
_handled_refresh_input: ContextVar[str] = ContextVar("teamharness_handled_refresh_input", default="")
_creation_sources: ContextVar[list[dict] | None] = ContextVar("teamharness_creation_sources", default=None)
_execution_task: ContextVar[str] = ContextVar("teamharness_execution_task", default="")
_completion_task: ContextVar[str] = ContextVar("teamharness_completion_task", default="")
_planning_tasks: ContextVar[frozenset[str]] = ContextVar("teamharness_planning_tasks", default=frozenset())
_review_task: ContextVar[tuple[str, str] | None] = ContextVar("teamharness_review_task", default=None)
_ack_scope: ContextVar[tuple | None] = ContextVar("teamharness_ack_scope", default=None)
_current_deliveries: ContextVar[tuple[str, ...]] = ContextVar("teamharness_current_deliveries", default=())
_delivery_ack: ContextVar[dict | None] = ContextVar("teamharness_delivery_ack", default=None)
_approval_status_records: dict[str, dict[str, Any]] = {}
_phase_retry: ContextVar[dict | None] = ContextVar("teamharness_phase_retry", default=None)
_phase_jobs: set[asyncio.Task] = set()


def _is_task_tool(name: str) -> bool:
    return name in {"mcp__teamharness__task", "teamharness__task"}
ASSET_DIR = PLUGIN_DIR / "teamharness"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent


def _team_prompt(_agent: Any) -> str:
    if os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
        if not os.getenv("AGENTTEAMS_CONTROLLER_URL"):
            return ""
        # Read the one public intake contract for the native system prompt;
        # transient roster hints must not duplicate or override this policy.
        try:
            return (ASSET_DIR / "skills/team/task-management/SKILL.md").read_text(encoding="utf-8").partition("## Plan as Leader")[0].strip()
        except OSError:
            return ""
    path = ASSET_DIR / "prompts" / "team" / "TEAMS.md"
    try:
        text = path.read_text(encoding="utf-8").strip()
        if os.getenv("AGENTTEAMS_AGENT_ROLE") in {"leader", "team_leader"}:
            text += "\n\n" + (ASSET_DIR / "prompts/agent/leader.md").read_text(encoding="utf-8").strip()
        elif os.getenv("AGENTTEAMS_AGENT_ROLE") in {"worker", "standalone"}:
            text += "\n\n" + (ASSET_DIR / "prompts/agent/worker.md").read_text(encoding="utf-8").strip()
        return text
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


@lru_cache(maxsize=1)
def _receipt_module():
    receipt_spec = importlib.util.spec_from_file_location(
        "teamharness_task_receipt", ASSET_DIR / "mcp/common/task_receipt.py")
    receipts = importlib.util.module_from_spec(receipt_spec)
    receipt_spec.loader.exec_module(receipts)
    return receipts


@lru_cache(maxsize=1)
def _phase_module():
    spec = importlib.util.spec_from_file_location(
        "teamharness_phase_recovery", ASSET_DIR / "mcp/common/phase_recovery.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@lru_cache(maxsize=1)
def _canonical_module():
    import importlib
    import sys
    from types import ModuleType
    package = '_agentteams_qwen_task_common'
    if package not in sys.modules:
        module = ModuleType(package)
        module.__path__ = [str(ASSET_DIR / 'mcp/common')]
        sys.modules[package] = module
    return importlib.import_module(package + '.task_canonical')


def _record_phase_error(result: Any) -> None:
    phase = _phase_retry.get()
    if phase is not None and isinstance(result, dict) and result.get("ok") is False and result.get("retryable") is True:
        phase["failed"] = True


def _sanitizer_factory(_ctx: Any, _agent_config: Any):
    try:
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    receipts = _receipt_module()
    if _ctx is not None:
        from qwenpaw_message_execution import canonical_registration
        async def registration(agent, call):
            return await canonical_registration(agent, call, ASSET_DIR / 'mcp/server.py')
        _ctx.extras['agentteams_task_registration'] = registration
        async def admit_canonical(agent):
            if not os.getenv('AGENTTEAMS_CONTROLLER_URL'):
                return
            sources = _creation_sources.get()
            if sources is None and _source_event.get() and _requester_route.get():
                route = _requester_route.get()
                sources = [{'eventId': _source_event.get(), 'roomId': route['target_session'],
                            'userId': route['target_user']}]
            identity = _request_meta(_ctx.request).get('task_execution') or {}
            context = {'role': os.getenv('AGENTTEAMS_AGENT_ROLE'), 'sources': sources or [],
                       'execution': _execution_task.get(), 'planning': list(_planning_tasks.get()),
                       'review': _review_task.get(), 'completion': _completion_task.get(),
                       'team': identity.get('team_id') or os.getenv('AGENTTEAMS_TEAM_ID')}
            result = await asyncio.to_thread(_canonical_module().admit, context)
            if result.get('ok') is not True:
                from task_recovery import RecoveryConflict
                raise RecoveryConflict(result.get('message') or 'Canonical Task outcome requires verification')
            from agentscope.message import Msg
            seen = agent.state.middle_context.setdefault('teamharness.canonical_receipts', {})
            for receipt in result.get('receipts', []):
                if receipt['receiptId'] not in seen:
                    agent.state.context.append(Msg(name=agent.name, role='assistant',
                        content=[{'type': 'text', 'text': receipt['fact']}]))
                    seen[receipt['receiptId']] = True
        _ctx.extras['agentteams_canonical_admit'] = admit_canonical

    def source_for(input_id, related_inputs=None):
        sources = _creation_sources.get()
        if sources is None:
            route = _requester_route.get() or {}
            sources = [{"inputId": receipts.input_reference(route.get("target_session", ""), _source_event.get()),
                        "eventId": _source_event.get(),
                        "userId": route.get("target_user", ""), "roomId": route.get("target_session", "")}]
            if not sources[0]["eventId"] or not sources[0]["userId"] or not sources[0]["roomId"]:
                raise ValueError("native original-conversation context unavailable")
        return receipts.select_creation_source(sources, input_id, related_inputs)

    class TeamHarnessSanitizer(MiddlewareBase):
        _submitted_task = ""
        _accepted_task = ""
        _task_scope = None
        _finished_phase_receipt = ""
        _creation_refresh_pending = False

        async def on_system_prompt(self, agent: Any, current_prompt: str) -> str:
            contract = _team_prompt(agent)
            if contract and contract not in current_prompt:
                return current_prompt + "\n\n" + contract
            return current_prompt

        def _inject_task_receipt(
            self, item: Any, tool_name: str, args: Any, creation_source: str = "",
            related_sources: list[str] | None = None,
        ) -> None:
            if tool_name not in {"execute_shell_command", "task"} or not isinstance(args, dict):
                return
            if not creation_source:
                operation = ((args.get("action"), args.get("taskId")) if tool_name == "task"
                             and isinstance(args.get("action"), str) and args.get("action") in {"plan", "submit", "accept", "reject", "complete"}
                             and isinstance(args.get("taskId"), str) else receipts.task_operation_command(args.get("command")))
                if not operation:
                    return
                action, task_id = operation
                review_scope = _review_task.get()
                if action == "plan":
                    expected = task_id if task_id in _planning_tasks.get() else ""
                elif action == "submit":
                    expected = _execution_task.get()
                elif action == "complete":
                    expected = _completion_task.get()
                else:
                    expected = review_scope[0] if review_scope else ""
                if task_id != expected:
                    return
            from agentscope.tool import ToolResponse
            from agentscope.message import TextBlock

            if not isinstance(item, ToolResponse):
                return
            # One complete CLI JSON result is required. Errors, stderr-prefixed
            # output and streaming chunks are never interpreted as receipts.
            text = "".join(block.get("text", "") if isinstance(block, dict)
                           else getattr(block, "text", "") for block in item.content)
            try:
                result = json.loads(text)
            except (TypeError, ValueError):
                return
            _record_phase_error(result)
            if item.state != "success":
                return
            if creation_source:
                sources = _creation_sources.get()
                refreshed = receipts.refresh_creation_inputs(result, sources or [], creation_source)
                if refreshed:
                    self._record_intake_outcome(creation_source, "input_changed")
                    sources.extend({key: entry[key] for key in ("inputId", "eventId", "roomId", "userId")} for entry in refreshed)
                    self._refreshed_inputs = getattr(self, "_refreshed_inputs", set()) | {entry["eventId"] for entry in refreshed}
                    self._creation_refresh_pending = True
                    self._pending_refreshed_inputs = getattr(self, "_pending_refreshed_inputs", []) + refreshed
                    # Human messages and the tool's no-effect receipt have distinct roles.
                    item.content[:] = [TextBlock(text=receipts.input_refresh_context(result, refreshed, sources))]
                    return
            receipt = (receipts.creation_receipt(result, creation_source) if creation_source
                       else receipts.planning_receipt(result, task_id) if action == "plan"
                       else receipts.review_receipt(result, task_id, review_scope[1], action) if action in {"accept", "reject"}
                       else receipts.task_receipt(result, task_id, action))
            seen = getattr(self, "_task_receipt_ids", set())
            if receipt is None or receipt[0] in seen:
                return
            # Native Agent orchestration saves this final ToolResponse in the
            # current tool context after on_acting. Keep the fact before any
            # truncation while preserving the original result blocks.
            fact = receipt[1]
            if creation_source:
                self._record_intake_outcome(creation_source, "registered")
                self._registered_inputs = getattr(self, "_registered_inputs", set()) | {creation_source, *(related_sources or [])}
                fact += receipts.creation_coverage_context(_creation_sources.get() or [], self._registered_inputs)
            item.content.insert(0, TextBlock(text=fact + "\n"))
            self._task_receipt_ids = seen | {receipt[0]}
            if creation_source:
                return
            if not creation_source and action == "plan":
                planned = getattr(self, "_planned_tasks", set()) | {task_id}
                self._planned_tasks = planned
                if _planning_tasks.get() - planned:
                    return
            self._finished_phase_receipt = receipt[1]

        def _record_intake_outcome(self, event_id: str, outcome: str) -> None:
            if os.getenv("AGENTTEAMS_OTEL_ENABLED", "").lower() != "true":
                return
            try:
                import at_telemetry
                from opentelemetry import trace
            except ImportError:
                return
            span = at_telemetry._active_turn.get() or trace.get_current_span()
            outcomes = getattr(self, "_intake_outcomes", [])
            outcomes.append({"sourceEventId": event_id, "outcome": outcome})
            self._intake_outcomes = outcomes
            span.set_attribute("agentteams.task.intake_outcomes", json.dumps(outcomes, separators=(",", ":")))

        async def _allow_task_output_change(self, command: str, cwd: str | None) -> bool:
            if not self._task_scope:
                self._task_scope = _ack_scope.get()
            if not self._task_scope:
                return False
            task_id, root, assignee, delegation = self._task_scope
            try:
                from qwenpaw_worker.cleanup_policy import is_private_cleanup
                from qwenpaw.config.context import get_tool_base_dir
                plain = re.sub(r"^export QWENPAW_SESSION_ID='[^'\r\n]*' && ", "", command, count=1)
                base = (get_tool_base_dir() / Path(cwd)).resolve() if cwd else get_tool_base_dir()
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
            if _is_task_tool(str(getattr(call, "name", ""))):
                from agentscope.permission import PermissionBehavior, PermissionDecision
                return PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Task system authenticates and authorizes this fixed Task operation; no arbitrary shell is available.")
            command = str((input_kwargs.get("tool_input") or {}).get("command") or "")
            if getattr(call, "name", "") == "execute_shell_command" and (_compound_private_cleanup(command) or self._task_scope and _compound_output_change(command)):
                from agentscope.permission import PermissionBehavior, PermissionDecision
                return PermissionDecision(behavior=PermissionBehavior.DENY, message=_CLEANUP_GUIDANCE)
            if getattr(call, "name", "") == "execute_shell_command" and await self._allow_task_output_change(command, (input_kwargs.get("tool_input") or {}).get("cwd")):
                from agentscope.permission import PermissionBehavior, PermissionDecision
                return PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Cleanup is confined to the current acknowledged task's writable outputs.")
            return await next_handler(**input_kwargs)

        async def on_reasoning(self, agent: Any, input_kwargs: dict[str, Any], next_handler: Callable[..., AsyncGenerator[Any, None]]) -> AsyncGenerator[Any, None]:
            acknowledgements = _delivery_ack.get()
            middle = getattr(getattr(agent, "state", None), "middle_context", None)
            if acknowledgements and isinstance(middle, dict):
                receipts.ack_delivery_context(middle, acknowledgements)
                _delivery_ack.set(None)
            handled = _handled_refresh_input.get()
            if handled:
                from agentscope.message import Msg
                # Enter the normal native reply lifecycle: input checkpoint and
                # read acknowledgement still run, while no model is invoked.
                fact = Msg(name=agent.name, role="assistant", content=[{"type": "text", "text": handled}])
                agent.state.append_context(agent.name, fact.content)
                yield Msg(name=agent.name, role="assistant", content=[{"type": "text", "text": "NO_REPLY"}])
                return
            if self._finished_phase_receipt or self._submitted_task or self._accepted_task:
                from agentscope.message import Msg
                message = Msg(name=agent.name, role="assistant", content=[{
                    "type": "text", "text": self._finished_phase_receipt or (f"Task {self._submitted_task} result submitted; awaiting acceptance."
                        if self._submitted_task else f"Task {self._accepted_task} accepted; the task system handles downstream coordination.")}])
                agent.state.append_context(agent.name, message.content)
                yield message
                return
            if (_phase_retry.get() or {}).get("failed"):
                from agentscope.message import Msg
                message = Msg(name=agent.name, role="assistant", content=[{
                    "type": "text", "text": "Task call outcome awaits verification; the task system will resume from its saved state."}])
                agent.state.append_context(agent.name, message.content)
                yield message
                return
            pending_inputs = getattr(self, "_pending_refreshed_inputs", [])
            if pending_inputs:
                from agentscope.message import Msg
                for source in pending_inputs:
                    if not any(message.role == "user" and message.metadata.get("agentteams.event.id") == source["eventId"]
                               for message in agent.state.context):
                        agent.state.context.append(Msg(name="Human", role="user",
                            content=[{"type": "text", "text": "[Message " + source["eventId"] + "]\n" + source["body"]}],
                            metadata={"agentteams.event.id": source["eventId"], "agentteams.room.id": source["roomId"],
                                      "agentteams.sender.id": source["userId"], "agentteams.input.id": source["inputId"]}))
                if _ctx is not None:
                    checkpoint = getattr(_ctx, "extras", {}).get("agentteams_checkpoint_inputs")
                    if not callable(checkpoint):
                        raise RuntimeError("Native Human input checkpoint support is unavailable")
                    await checkpoint(agent, pending_inputs)
                self._pending_refreshed_inputs = []
            self._creation_refresh_pending = False
            async for item in next_handler(**input_kwargs):
                # A final model response has admitted and interpreted the
                # refreshed inputs. Native session persistence saves this fact.
                from agentscope.message import Msg
                if (getattr(self, "_refreshed_inputs", set())
                        and isinstance(item, Msg) and getattr(item, "role", None) == "assistant"
                        and isinstance(middle, dict)):
                    seen = middle.setdefault("teamharness.refreshed_inputs", {})
                    for event_id in getattr(self, "_refreshed_inputs", set()):
                        seen[event_id] = True
                yield item

        async def on_model_call(self, agent, input_kwargs, next_handler):
            facts = _current_deliveries.get()
            if facts:
                from agentscope.message import Msg, TextBlock

                messages = list(input_kwargs["messages"])
                first = messages[0] if messages and messages[0].role == "system" else None
                rules = (
                    "For any Task ID below, the verified terminal delivery is current. "
                    "Use it to answer this turn even if the conversation archive contains an earlier status."
                )
                current = TextBlock(text=rules + "\n\n" + "\n\n".join(facts))
                if first is None:
                    messages.insert(0, Msg(name="Task system", role="system", content=[current]))
                else:
                    messages[0] = first.model_copy(update={"content": [*first.get_content_blocks(), current]})
                input_kwargs = {**input_kwargs, "messages": messages}
            sources = _creation_sources.get()
            if sources and os.getenv("AGENTTEAMS_OTEL_ENABLED", "").lower() == "true":
                try:
                    from opentelemetry import trace
                except ImportError:
                    pass
                else:
                    roster_context = _ctx.extras.get("teamharness_manager_roster_context", "") if _ctx is not None else ""
                    check = receipts.input_context_check(input_kwargs["messages"], _team_prompt(agent), sources,
                                                         roster_context=roster_context, tools=input_kwargs.get("tools", []))
                    checks = getattr(self, "_intake_context_checks", [])
                    checks.append(check)
                    self._intake_context_checks = checks
                    trace.get_current_span().set_attribute("agentteams.task.intake_checks", json.dumps(checks, separators=(",", ":")))
                    if (check["currentSystemContractPresent"] and check["providedInputs"]
                            == check["userSourceMarkers"] == check["visibleInputReferences"]):
                        spans = [trace.get_current_span()]
                        try:
                            import at_telemetry
                        except ImportError:
                            pass
                        else:
                            if at_telemetry._active_turn.get() is not None:
                                spans.append(at_telemetry._active_turn.get())
                        for span in spans:
                            previous = (getattr(span, "attributes", None) or {}).get("agentteams.input.event_ids", [])
                            span.set_attribute("agentteams.input.event_ids", list(dict.fromkeys([
                                *previous, *(source["eventId"] for source in sources)])))
            return await next_handler(**input_kwargs)

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
            original_input = dict(args) if isinstance(args, dict) else args
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
            creation_source, related_sources = "", None
            task_tool = _is_task_tool(str(getattr(call, "name", "")))
            if task_tool and isinstance(args, dict):
                args.pop("_nativeSource", None)
                if args.get("action") == "create" and os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
                    if self._creation_refresh_pending:
                        from agentscope.tool import ToolResponse
                        yield ToolResponse(state="error", content=[{"type":"text", "text":"No Task created. Interpret the refreshed Human inputs before choosing another operation."}])
                        return
                    try:
                        selected = source_for(args.get("inputId"), args.get("relatedInputIds"))
                    except ValueError as error:
                        from agentscope.tool import ToolResponse
                        yield ToolResponse(state="error", content=[{"type": "text", "text": str(error)}])
                        return
                    creation_source = selected["eventId"]
                    related_sources = selected.get("relatedSources")
                    args["_nativeSource"] = selected
                if json_input:
                    call.input = json.dumps(args, ensure_ascii=False)
            if name == "execute_shell_command" and isinstance(args, dict) and os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
                requested = receipts.creation_command_input(args.get("command"))
                if requested is not None:
                    if self._creation_refresh_pending:
                        from agentscope.tool import ToolResponse
                        yield ToolResponse(state="error", content=[{"type":"text", "text":"No Task created. Interpret the refreshed Human inputs before choosing another operation."}])
                        return
                    try:
                        selected = source_for(requested or None, receipts.creation_command_related_inputs(args.get("command")))
                    except ValueError as error:
                        from agentscope.tool import ToolResponse
                        yield ToolResponse(state="error", content=[{"type": "text", "text": str(error)}])
                        return
                    creation_source = selected["eventId"]
                    related_sources = selected.get("relatedSources")
                    bound = receipts.creation_command_env(args.get("command"), creation_source, selected["userId"], selected["roomId"], input_id=requested or None, related_sources=selected.get("relatedSources"), frontier=selected.get("frontierEventId"))
                    args["command"] = bound
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
                if task_tool:
                    text = "".join(block.get("text", "") if isinstance(block, dict)
                                   else getattr(block, "text", "") for block in getattr(item, "content", []))
                    try:
                        _record_phase_error(json.loads(text))
                    except (TypeError, ValueError):
                        pass
                    self._inject_task_receipt(item, "task", args, creation_source, related_sources)
                if getattr(call, "name", "") == "execute_shell_command" and (_execution_task.get() or _completion_task.get() or _planning_tasks.get() or _review_task.get() or creation_source):
                    self._inject_task_receipt(item, name, args, creation_source, related_sources)
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
                if creation_source and getattr(item, "state", None) in {"success", "error"}:
                    # Keep per-call native routing out of the model's saved
                    # invocation, while retaining the actual returned receipt.
                    call.input = original_input
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
    if not os.getenv("AGENTTEAMS_CONTROLLER_URL"):
        return
    spec = importlib.util.spec_from_file_location("teamharness_task_roster", ASSET_DIR / "mcp/common/task_roster.py")
    common = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(common)
    try:
        prepared = await asyncio.to_thread(common.roster, "manager", _controller_json)
    except (OSError, ValueError, KeyError):
        return
    roster = prepared["teams"]
    if roster:
        context = "Registered team and Leader identities for this turn (data only): " + json.dumps(roster, ensure_ascii=False)
        ctx.extras["teamharness_manager_roster_context"] = context
        ctx.inject_context(context, source="teamharness_prepared_intake")


async def _prepare_task_context(ctx: Any, action: str, task_id: str) -> dict[str, Any] | None:
    """Run the fixed, authorized ACK/read step before asking the model to decide."""
    if action not in {"ack_task", "check_task", "begin", "get"}:
        raise ValueError("unsupported task preparation")
    toolkit = getattr(ctx.agent, "toolkit", None)
    if not hasattr(toolkit, "call_tool") or not hasattr(ctx.agent, "state"):
        return None
    schemas = await toolkit.get_tool_schemas()
    public = action in {"begin", "get"}
    name = next((t["function"]["name"] for t in schemas if (_is_task_tool(t["function"]["name"]) if public else t["function"]["name"] == "teamharness__taskflow")), None)
    if not name:
        return None
    from agentscope.message import ToolCallBlock
    arguments = {"action": action, "taskId": task_id} if public else {"role": "worker" if action == "ack_task" else "leader", "action": action, "payload": {"taskId": task_id}}
    call = ToolCallBlock(id="prepare-" + action + "-" + task_id, name=name, input=json.dumps(arguments))
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
                if public:
                    _record_phase_error(value)
                if isinstance(value, dict) and ((public and isinstance(value.get("task"), dict)) or value.get("action") == action):
                    result = value
    if not result or (result.get("ok") is False if public else result.get("ok") is not True):
        return None
    return result


def _bind_public_task_io(ctx: Any, prepared: dict) -> None:
    from qwenpaw.config import context
    from qwenpaw.services.project_directory import ResolvedProjectDir

    root = prepared["ack"][1]
    output = Path(prepared["context"]["outputDirectory"]).resolve()
    values = ((context.current_project_dir, output),
              (context.current_project_dirs, (ResolvedProjectDir(output), ResolvedProjectDir(root / "inputs"))),
              (context.current_project_dir_source, "request"))
    for variable, value in values:
        ctx.extras.setdefault("task_io_tokens", []).append((variable, variable.set(value)))


def _reset_public_task_io(ctx: Any) -> None:
    for variable, token in reversed(ctx.extras.pop("task_io_tokens", [])):
        variable.reset(token)


async def _prepare_public_task_event(ctx: Any, event: dict) -> None:
    """Bind a verified common-protocol event to the native public tool/context."""
    kind = event.get("kind")
    spec = importlib.util.spec_from_file_location("teamharness_task_context", ASSET_DIR / "mcp/common/task_context.py")
    common = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(common)
    if kind != "handoff":
        task_id = event.get("main_task_id") if kind == "main_task_ready" else event.get("task_id")
        result = await _prepare_task_context(ctx, "begin" if kind == "task_assigned" else "get", task_id)
        if (_phase_retry.get() or {}).get("failed"):
            return
        prepared = common.prepare(kind, event, result)
        if kind == "task_assigned":
            _bind_public_task_io(ctx, prepared)
        for field, variable in (("execution", _execution_task), ("ack", _ack_scope), ("review", _review_task), ("completion", _completion_task)):
            if field in prepared:
                key = {"execution": "execution_task_token", "ack": "ack_scope_token", "review": "review_task_token", "completion": "completion_task_token"}[field]
                ctx.extras[key] = variable.set(prepared[field])
        ctx.inject_context(prepared["fact"] + "\n" + json.dumps(prepared["context"], ensure_ascii=False), source="teamharness_public_task_prepared")
    _load_phase_skill(ctx, "task-management")
    for tool in await ctx.agent.toolkit.get_tool_schemas():
        name = tool["function"]["name"]
        short = name.split("__")[-1].lower()
        legacy = short in {"taskflow", "projectflow", "filesync"}
        restricted = kind in common.COORDINATION_PHASES and not _is_task_tool(name)
        if legacy or restricted:
            await ctx.agent.toolkit.remove_tool(name)


def _load_phase_skill(ctx: Any, name: str) -> None:
    path = ASSET_DIR / "skills/team" / name / "SKILL.md"
    if path.is_file():
        text = path.read_text(encoding="utf-8")
        if name == "project-management":
            text = text.partition("## Project Files")[0]
            text += "\nFor manual planning or Loop iteration, read the complete skill at " + str(path) + ".\n"
        ctx.inject_context("The builtin " + name + " phase instructions are already loaded for this turn; do not reread them.\n" + text, source="teamharness_preloaded_skill")


async def _prepare_delivery_context(ctx: Any) -> None:
    if os.getenv("AGENTTEAMS_AGENT_ROLE") != "manager" or ctx.extras.get("delivery_context_prepared"):
        return
    middle = getattr(getattr(ctx.agent, "state", None), "middle_context", None)
    if not isinstance(middle, dict):
        return
    ctx.extras["delivery_context_prepared"] = True
    input_text = "\n".join(message.get_text_content() for message in ctx.input_msgs or []
                           if message.role == "user")
    receipts = _receipt_module()
    facts, acknowledgements = await asyncio.to_thread(
        receipts.delivery_context, middle, _room_id(ctx.request), _controller_json, input_text=input_text)
    if receipts.requested_delivery_tasks(input_text):
        ctx.extras["current_deliveries_token"] = _current_deliveries.set(tuple(facts))
    else:
        for fact in facts:
            ctx.inject_context(fact, source="teamharness_sent_delivery")
    if acknowledgements:
        ctx.extras["delivery_ack_token"] = _delivery_ack.set(acknowledgements)


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
                or execution.get("state") != "active"):
            raise RecoveryConflict("Task execution changed before native reasoning")
        if task.get("task_plan_id"):
            await _prepare_public_task_event(ctx, {"kind": "task_assigned", "task_id": identity["task_id"], "main_task_id": task.get("main_task_id", ""), "project_id": task.get("project_id", ""), "delivery_id": task.get("notifyTxn")})
            return False
        ctx.extras["execution_task_token"] = _execution_task.set(identity["task_id"])
        _load_phase_skill(ctx, "task-execution")
        return False
    if meta.get("event_id"):
        ctx.extras["source_event_token"] = _source_event.set(str(meta["event_id"]))
    if os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager" and meta.get("event_id"):
        # An unavailable/invalid batch must never fall back to its first source.
        ctx.extras["creation_sources_token"] = _creation_sources.set([])
    api_event = meta.get("teamharness_coordination") or {}
    receipt = {}
    if meta.get("teamharness_matrix_coordination"):
        receipt = await _phase_module().until_available(_controller_json, "coordination/events/" + urllib.parse.quote(api_event.get("id", ""), safe=""))
        if (api_event.get("kind") not in {"handoff", "task_assigned", "task_submitted", "project_ready", "main_task_ready"}
                or receipt.get("actor") != meta.get("sender_id")
                or receipt.get("recipient") != meta.get("agent_matrix_user_id")
                or receipt.get("room_id", "") != meta.get("room_id", "")
                or any(receipt.get(key, "") != api_event.get(key, "") for key in
                       ("kind", "task_id", "main_task_id", "project_id", "delivery_id", "submission_id"))):
            return True
    if api_event.get("kind") == "handoff" and receipt:
        saved_tasks = (((receipt.get("control") or {}).get("channel_meta") or {}).get("teamharness_main_tasks") or [])
        ctx.extras["planning_tasks_token"] = _planning_tasks.set(frozenset(
            task["taskId"] for task in saved_tasks if isinstance(task, dict) and isinstance(task.get("taskId"), str)))
    if receipt.get("protocol") == "task/v1":
        if api_event.get("kind") in {"handoff", "task_submitted", "main_task_ready"}:
            native_id = str(meta.get("event_id") or "")
            if not native_id.startswith("$"):
                raise ValueError("current Matrix event required for coordination lifecycle")
            phase = {"id": api_event["id"], "native_id": native_id, "failed": False}
            ctx.extras["phase_retry_token"] = _phase_retry.set(phase)
            admission = await _phase_module().notify(phase["id"], native_id, "begin")
            if admission.get("active") is not True:
                return True
            if api_event.get("kind") == "handoff":
                _planning_tasks.set(frozenset(admission["pending_task_ids"]))
        await _prepare_public_task_event(ctx, api_event)
        return bool((_phase_retry.get() or {}).get("failed"))
    route = meta.get("teamharness_requester_route")
    if isinstance(route, dict) and route.get("target_session"):
        ctx.extras.setdefault("requester_route_token", _requester_route.set(route))
        ctx.inject_context("Preserve the original requester route when creating the task: " + json.dumps(route), source="teamharness_requester_route")
    coordinator = meta.get("teamharness_coordinator_route")
    if os.getenv("AGENTTEAMS_AGENT_ROLE") != "manager" and isinstance(coordinator, dict) and coordinator.get("target_session"):
        ctx.extras["coordinator_route_token"] = _coordinator_route.set(coordinator)
        ctx.inject_context("Send the verified final result to this coordinator replyRoute; the coordinator relays it to the original requester: " + json.dumps(coordinator), source="teamharness_coordinator_route")
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
    if api_event.get("kind") == "main_task_ready":
        ctx.extras["completion_task_token"] = _completion_task.set(api_event["main_task_id"])
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
        human_ids = {str(h.get("matrixUserID") or h.get("matrixUserId") or "") for h in humans.get("humans", [])}
        sources = _creation_sources.get()
        if sources is not None:
            current = [s for s in _receipt_module().creation_sources(meta) if s["userId"] in human_ids]
            middle = getattr(getattr(ctx.agent, "state", None), "middle_context", {})
            seen = middle.get("teamharness.refreshed_inputs", {}) if isinstance(middle, dict) else {}
            remaining = [s for s in current if not seen.get(s["eventId"])]
            if current and not remaining:
                ctx.extras["handled_refresh_input_token"] = _handled_refresh_input.set(_receipt_module().handled_input_context(current))
                return False
            if len(remaining) != len(current):
                ctx.inject_context("Some supplied messages were already interpreted through a verified Task input refresh. Do not apply them again: " + json.dumps([s["inputId"] for s in current if seen.get(s["eventId"])]), source="teamharness_handled_inputs")
            sources.extend(remaining)
            ctx.inject_context("[Task input references]\n" + json.dumps([{key: s[key] for key in ("inputId", "eventId")} for s in sources]),
                               source="teamharness_creation_inputs")
        if sender in human_ids or sources:
            if sender in human_ids:
                ctx.extras.setdefault("requester_route_token", _requester_route.set({"channel": "matrix", "target_session": _room_id(ctx.request), "target_user": sender}))
            await _prepare_delivery_context(ctx)
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
            _reset_public_task_io(ctx)
            phase = _phase_retry.get()
            token = ctx.extras.pop("phase_retry_token", None)
            if token is not None:
                _phase_retry.reset(token)
                if phase and (phase.get("failed") or _phase_module().retryable_error(getattr(ctx, "error", None))):
                    job = asyncio.create_task(_phase_module().notify(phase["id"], phase["native_id"], "retry"))
                    _phase_jobs.add(job)
                    def finished(task):
                        _phase_jobs.discard(task)
                        if not task.cancelled():
                            error = task.exception()
                            if error is not None:
                                logging.getLogger(__name__).warning(
                                    "Coordination lifecycle callback ended: %s", type(error).__name__)
                    job.add_done_callback(finished)
            token = ctx.extras.pop("planning_tasks_token", None)
            if token is not None:
                _planning_tasks.reset(token)
            token = ctx.extras.pop("current_deliveries_token", None)
            if token is not None:
                _current_deliveries.reset(token)
            token = ctx.extras.pop("delivery_ack_token", None)
            if token is not None:
                _delivery_ack.reset(token)
            token = ctx.extras.pop("ack_scope_token", None)
            if token is not None:
                _ack_scope.reset(token)
            token = ctx.extras.pop("review_task_token", None)
            if token is not None:
                _review_task.reset(token)
            token = ctx.extras.pop("completion_task_token", None)
            if token is not None:
                _completion_task.reset(token)
            token = ctx.extras.pop("execution_task_token", None)
            if token is not None:
                _execution_task.reset(token)
            token = ctx.extras.pop("source_event_token", None)
            if token is not None:
                _source_event.reset(token)
            token = ctx.extras.pop("handled_refresh_input_token", None)
            if token is not None:
                _handled_refresh_input.reset(token)
            token = ctx.extras.pop("creation_sources_token", None)
            if token is not None:
                _creation_sources.reset(token)
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
