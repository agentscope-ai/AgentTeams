"""QwenPaw Task-Trace correlation for AgentTeams.

Provides a QwenPaw OpenTelemetry SpanProcessor that attaches
``agentteams.task.id`` and ``agentteams.project.id`` to the **entry span** of each
agent turn trace.

Entry span choice
-----------------
LoongSuite/QwenPaw instrumentation creates one trace per user turn with this
hierarchy (outer → inner)::

    enter_ai_application_system   ← trace root / GenAI application entry
      agent_step
        invoke_agent <name>
          react step
            chat / execute_tool / …

We tag **only** ``enter_ai_application_system`` because:

* It is the trace root — CMS/ARMS filters on root-span attributes efficiently.
* There is exactly one per turn (same cardinality as a "trace" in the product).
* ``agent_step`` is nested inside it; tagging both would be redundant.

Task status matching
--------------------
The current room/session is the primary correlation key.  A unique project is
derived from ``shared/tasks/*/meta.json`` records in the room, without requiring
the worker's local copy of ``shared/projects/<project_id>/meta.json`` to be
present.  ``assigned`` and ``in_progress`` tasks are then matched by
``project_id + room_id + assigned_to + status`` so the ACK/work/submit turn's
entry span is tagged before ``submit_task`` flips status to ``submitted``.
After submit, the room can still be tagged with ``agentteams.project.id`` but
not ``agentteams.task.id``.

Session isolation
-----------------
A Worker may serve multiple sessions (rooms) concurrently.  Each turn is
tied to a specific ``room_id`` via :func:`set_current_room` which the Agent
runtime calls before invoking the turn handler.  The processor reads the
current room via :func:`get_current_room` and only matches tasks whose
``room_id`` equals the active room.  This prevents cross-session task tagging.

If the room context is not set, the processor does not guess from active tasks
because that can cross-tag unrelated sessions.

Debug logging
-------------
Set ``AGENTTEAMS_TASK_TRACE_DEBUG=1`` to enable verbose processor logs.
Default is silent — no I/O overhead on the hot span path.
"""

from __future__ import annotations

import contextvars
import json
import logging
import os
import sys
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Debug logging (controlled by AGENTTEAMS_TASK_TRACE_DEBUG env var)
# ---------------------------------------------------------------------------

_DEBUG_ENABLED: bool = os.getenv("AGENTTEAMS_TASK_TRACE_DEBUG", "").strip() in ("1", "true", "yes")


def _trace_log(msg: str, *args: Any) -> None:
    """Emit debug log only when AGENTTEAMS_TASK_TRACE_DEBUG=1."""
    if not _DEBUG_ENABLED:
        return
    formatted = msg % args if args else msg
    print(f"[AgentTeamsTaskSpanProcessor] {formatted}", file=sys.stderr, flush=True)
    logger.info(msg, *args)


def _trace_warning(msg: str, *args: Any) -> None:
    """Report a non-fatal trace-correlation problem."""
    formatted = msg % args if args else msg
    if _DEBUG_ENABLED:
        print(f"[AgentTeamsTaskSpanProcessor] {formatted}", file=sys.stderr, flush=True)
    logger.warning(msg, *args)


def set_trace_debug(enabled: bool) -> None:
    """Programmatically enable/disable processor debug logging."""
    global _DEBUG_ENABLED
    _DEBUG_ENABLED = enabled

# ---------------------------------------------------------------------------
# Session (room) context propagation
# ---------------------------------------------------------------------------

_current_room: contextvars.ContextVar[str] = contextvars.ContextVar(
    "agentteams_current_room", default=""
)

# Entry span captured in on_start when room context is not yet available.
# The QwenPawAgent wrapper sets the room and then calls
# tag_pending_entry_span() to retroactively tag it.
_pending_entry_span: contextvars.ContextVar[Any] = contextvars.ContextVar(
    "agentteams_pending_entry_span", default=None
)
def set_current_room(room_id: str) -> contextvars.Token[str]:
    """Set the current Matrix room for this coroutine/thread.

    The Agent runtime must call this before processing each turn so the
    SpanProcessor can correlate spans with the correct task.

    Returns a token for resetting (useful in try/finally blocks).
    """
    return _current_room.set(canonical_room_id(room_id))


def get_current_room() -> str:
    """Return the room_id for the current coroutine/thread, or empty string."""
    return _current_room.get()


def reset_current_room(token: contextvars.Token[str]) -> None:
    """Reset room context to previous value."""
    _current_room.reset(token)


ATTR_AGENTTEAMS_TASK_ID = "agentteams.task.id"
ATTR_AGENTTEAMS_PROJECT_ID = "agentteams.project.id"

# Trace root span created by loongsuite-instrumentation-qwenpaw per user turn.
ENTRY_SPAN_NAME = "enter_ai_application_system"

# assigned: ACK turn (before ack_task); in_progress: work/submit turns.
_ACTIVE_STATUSES = frozenset({"assigned", "in_progress"})

_CACHE_TTL = 0.0  # retained for API compatibility; entry spans always re-scan meta.json
_REGISTERED_PROVIDER_IDS: set[int] = set()


def is_entry_span(span_name: str) -> bool:
    """Return True if *span_name* is the per-turn trace entry span."""
    return span_name == ENTRY_SPAN_NAME


def canonical_room_id(room_id: Any) -> str:
    """Return a stable Matrix room key across TeamHarness/QwenPaw prefixes."""
    value = str(room_id or "").strip()
    changed = True
    while changed:
        changed = False
        for prefix in ("matrix:", "room:"):
            if value.startswith(prefix):
                value = value[len(prefix) :].strip()
                changed = True
    return value


def canonical_worker_name(worker_id: Any) -> str:
    """Normalize Matrix user ids and bare worker names to the worker name."""
    value = str(worker_id or "").strip()
    if value.startswith("@"):
        value = value[1:]
    if ":" in value:
        value = value.split(":", 1)[0]
    return value


def _meta_text(meta: dict[str, Any], *keys: str) -> str:
    """Return the first non-empty string value for *keys* in *meta*."""
    for key in keys:
        value = str(meta.get(key) or "").strip()
        if value:
            return value
    return ""


def _task_room_id(meta: dict[str, Any]) -> str:
    return _meta_text(meta, "room_id", "roomId")


def _task_assignee(meta: dict[str, Any]) -> str:
    return _meta_text(meta, "assigned_to", "assignedTo", "assignee")


def _task_project_id(meta: dict[str, Any]) -> str:
    return _meta_text(meta, "project_id", "projectId")


def _task_id(meta: dict[str, Any]) -> str:
    return _meta_text(meta, "task_id", "taskId")


def _read_task_metas(workspace_dir: Path) -> list[dict[str, Any]]:
    tasks_dir = workspace_dir / "shared" / "tasks"
    if not tasks_dir.is_dir():
        return []
    metas: list[dict[str, Any]] = []
    for entry in sorted(tasks_dir.iterdir(), key=lambda path: path.name):
        if not entry.is_dir():
            continue
        meta_path = entry / "meta.json"
        if not meta_path.exists():
            continue
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            continue
        if isinstance(meta, dict):
            metas.append(meta)
    return metas


def find_task_trace_context(
    workspace_dir: Path,
    *,
    room_id: str = "",
) -> dict[str, Any]:
    """Return project/task context for the current room.

    Project attribution is room-based: all task metas in one room must agree on
    a single project id.  Task attribution is narrower: among that project and
    room, exactly one active task may be assigned to ``AGENTTEAMS_WORKER_NAME``.
    Ambiguous or missing task matches keep project attribution but omit task id.
    """
    canonical_room = canonical_room_id(room_id)
    context: dict[str, Any] = {
        "project_id": "",
        "task": None,
        "reason": "",
    }
    if not canonical_room:
        context["reason"] = "missing room context"
        return context

    try:
        room_tasks = [
            meta for meta in _read_task_metas(workspace_dir)
            if canonical_room_id(_task_room_id(meta)) == canonical_room
        ]
        if not room_tasks:
            context["reason"] = "no task meta for room"
            return context

        project_ids = sorted({
            project_id
            for meta in room_tasks
            if (project_id := _task_project_id(meta))
        })
        if len(project_ids) != 1:
            _trace_warning(
                "task trace project ambiguity room=%s projects=%s matches=%d",
                canonical_room,
                ",".join(project_ids) or "(none)",
                len(room_tasks),
            )
            context["reason"] = "project ambiguity"
            return context

        project_id = project_ids[0]
        context["project_id"] = project_id

        worker_name = canonical_worker_name(os.getenv("AGENTTEAMS_WORKER_NAME", ""))
        if not worker_name:
            context["reason"] = "missing worker name"
            return context

        active_matches = []
        for meta in room_tasks:
            if _task_project_id(meta) != project_id:
                continue
            if meta.get("status") not in _ACTIVE_STATUSES:
                continue
            assignee = canonical_worker_name(_task_assignee(meta))
            if assignee == worker_name:
                active_matches.append(meta)

        if len(active_matches) == 1:
            task = active_matches[0]
            context["task"] = task
            _trace_log(
                "find_task_trace_context: project_id=%s task_id=%s status=%s "
                "room_id=%s assignee=%s",
                project_id,
                _task_id(task),
                task.get("status"),
                _task_room_id(task),
                _task_assignee(task),
            )
            return context
        if not active_matches:
            _trace_warning(
                "task trace active task not found room=%s project_id=%s worker=%s",
                canonical_room,
                project_id,
                worker_name,
            )
            context["reason"] = "active task not found"
            return context

        _trace_warning(
            "task trace active task ambiguity room=%s project_id=%s worker=%s matches=%d",
            canonical_room,
            project_id,
            worker_name,
            len(active_matches),
        )
        context["reason"] = "active task ambiguity"
        return context
    except Exception as exc:
        _trace_log("find_task_trace_context: unexpected error workspace=%s: %s", workspace_dir, exc)
        logger.warning("find_task_trace_context failed: %s", exc)
        context["reason"] = "unexpected error"
        return context


def find_active_task(
    workspace_dir: Path,
    *,
    room_id: str = "",
) -> dict[str, Any] | None:
    """Return the current worker's active task for ``room_id``, if unique."""
    context = find_task_trace_context(workspace_dir, room_id=room_id)
    task = context.get("task")
    return task if isinstance(task, dict) else None


def register_task_trace_processor(
    workspace_dir: Path | str,
    *,
    cache_ttl: float = _CACHE_TTL,
) -> dict[str, Any]:
    """Register :class:`AgentTeamsTaskSpanProcessor` on the active TracerProvider.

    Safe to call even when the OTel SDK is not installed — returns a result
    dict describing what happened.
    """
    try:
        from opentelemetry import trace as trace_api
        from opentelemetry.sdk.trace import TracerProvider
    except ImportError:
        return {"ok": False, "reason": "opentelemetry SDK unavailable"}

    provider = trace_api.get_tracer_provider()

    # The SDK may wrap the real provider in a ProxyTracerProvider.
    real = getattr(provider, "_real_provider", None) or provider
    if not isinstance(real, TracerProvider):
        return {"ok": False, "reason": "TracerProvider not initialised yet"}
    if id(real) in _REGISTERED_PROVIDER_IDS:
        return {"ok": True, "processor": "AgentTeamsTaskSpanProcessor", "action": "unchanged"}

    processor = AgentTeamsTaskSpanProcessor(Path(workspace_dir), cache_ttl=cache_ttl)
    real.add_span_processor(processor)
    _REGISTERED_PROVIDER_IDS.add(id(real))
    # Registration is always logged regardless of debug flag.
    formatted = (
        f"[AgentTeamsTaskSpanProcessor] registered workspace={workspace_dir} "
        f"worker={os.getenv('AGENTTEAMS_WORKER_NAME', '')} entry_span={ENTRY_SPAN_NAME} "
        f"debug={_DEBUG_ENABLED}"
    )
    print(formatted, file=sys.stderr, flush=True)
    logger.info(
        "AgentTeamsTaskSpanProcessor registered workspace=%s worker=%s debug=%s",
        workspace_dir,
        os.getenv("AGENTTEAMS_WORKER_NAME", ""),
        _DEBUG_ENABLED,
    )
    return {"ok": True, "processor": "AgentTeamsTaskSpanProcessor"}


def _span_is_recording(span: Any) -> bool:
    is_recording = getattr(span, "is_recording", None)
    if callable(is_recording):
        return bool(is_recording())
    return True


def tag_entry_span(span: Any, workspace_dir: Path | str, *, room_id: str = "") -> dict[str, Any]:
    """Attach AgentTeams task attributes to an already-started entry span."""
    span_name = str(getattr(span, "name", "") or "")
    if not _span_is_recording(span):
        return {"ok": False, "reason": "span not recording"}
    if not is_entry_span(span_name):
        return {"ok": False, "reason": "not entry span", "span": span_name}

    active_room = room_id or get_current_room()
    context = find_task_trace_context(Path(workspace_dir), room_id=active_room)
    project_id = str(context.get("project_id") or "")
    task = context.get("task")
    if not project_id:
        _trace_log(
            "tag_entry_span: entry span=%s no trace project room=%s reason=%s",
            span_name,
            active_room or "(none)",
            context.get("reason", ""),
        )
        return {
            "ok": False,
            "reason": context.get("reason", "missing project"),
            "span": span_name,
        }

    span.set_attribute(ATTR_AGENTTEAMS_PROJECT_ID, project_id)

    task_id = ""
    if isinstance(task, dict):
        task_id = _task_id(task)
        if task_id:
            span.set_attribute(ATTR_AGENTTEAMS_TASK_ID, task_id)
        _trace_log(
            "tag_entry_span: tagged entry span=%s task_id=%s project_id=%s "
            "status=%s room=%s",
            span_name,
            task_id,
            project_id,
            task.get("status"),
            active_room or "(none)",
        )
    else:
        _trace_log(
            "tag_entry_span: tagged entry span=%s project_id=%s without task room=%s reason=%s",
            span_name,
            project_id,
            active_room or "(none)",
            context.get("reason", ""),
        )

    return {
        "ok": True,
        "span": span_name,
        "project_id": project_id,
        "task_id": task_id,
        "reason": context.get("reason", ""),
    }


def tag_current_entry_span(workspace_dir: Path | str, *, room_id: str = "") -> dict[str, Any]:
    """Attach AgentTeams task attributes to the active OTel entry span, if any.

    First checks the pending entry span saved by the SpanProcessor (for the
    common case where the current span has moved past the entry span by the
    time the room context becomes available).  Falls back to the OTel current
    span.
    """
    pending = _pending_entry_span.get()
    if pending is not None:
        return tag_pending_entry_span(workspace_dir, room_id=room_id)

    try:
        from opentelemetry import trace as trace_api
    except ImportError:
        return {"ok": False, "reason": "opentelemetry API unavailable"}

    return tag_entry_span(trace_api.get_current_span(), workspace_dir, room_id=room_id)


def get_pending_entry_span() -> Any:
    """Return the entry span saved by on_start, or None."""
    return _pending_entry_span.get()


def clear_pending_entry_span() -> None:
    """Discard the pending entry span reference."""
    _pending_entry_span.set(None)


def tag_pending_entry_span(workspace_dir: Path | str, *, room_id: str = "") -> dict[str, Any]:
    """Tag the pending entry span (if any) and clear it.

    Called by the QwenPawAgent wrapper after ``set_current_room`` so the entry
    span — which was created *before* the room was known — gets tagged with
    the correct task attributes.
    """
    span = _pending_entry_span.get()
    if span is None:
        return {"ok": False, "reason": "no pending entry span"}
    _pending_entry_span.set(None)
    return tag_entry_span(span, workspace_dir, room_id=room_id)


# ---------------------------------------------------------------------------
# SpanProcessor implementation
# ---------------------------------------------------------------------------

class AgentTeamsTaskSpanProcessor:
    """OTel SpanProcessor that tags the per-turn entry span with the AgentTeams task.

    Each entry span reads current ``shared/tasks/*/meta.json`` state.  This
    keeps submit/cancel transitions authoritative without relying on taskflow
    tool results to invalidate a local cache.
    """

    def __init__(self, workspace_dir: Path, *, cache_ttl: float = _CACHE_TTL) -> None:
        _ = cache_ttl
        self._workspace_dir = workspace_dir

    def on_start(self, span: Any, parent_context: Any = None) -> None:
        span_name = str(getattr(span, "name", "") or "")
        if not _span_is_recording(span):
            return
        if not is_entry_span(span_name):
            return

        room = get_current_room()
        if room:
            tag_entry_span(span, self._workspace_dir, room_id=room)
        else:
            # Room context is not yet available (will be set by the
            # QwenPawAgent wrapper).  Stash the span so the wrapper can
            # tag it once the room is known via tag_pending_entry_span().
            _pending_entry_span.set(span)
            _trace_log(
                "on_start: deferred entry span=%s (room not set yet)",
                span_name,
            )

    def on_end(self, span: Any) -> None:
        span_name = str(getattr(span, "name", "") or "")
        if not is_entry_span(span_name):
            return
        if not _DEBUG_ENABLED:
            return
        attrs: dict[str, Any] = {}
        if hasattr(span, "attributes") and span.attributes:
            attrs = dict(span.attributes)
        elif hasattr(span, "_attributes") and span._attributes:
            attrs = dict(span._attributes)
        _trace_log(
            "on_end: entry span=%s agentteams.task.id=%s agentteams.project.id=%s",
            span_name,
            attrs.get(ATTR_AGENTTEAMS_TASK_ID),
            attrs.get(ATTR_AGENTTEAMS_PROJECT_ID),
        )

    def _on_ending(self, span: Any) -> None:
        pass

    def shutdown(self) -> None:
        pass

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        return True

    # -- internal -----------------------------------------------------------

    def invalidate_cache(self, room_id: str = "") -> None:
        """Compatibility no-op; entry spans already re-read meta.json."""
        return None

    def _get_trace_context(self, room_id: str = "") -> dict[str, Any]:
        cache_key = canonical_room_id(room_id)
        context = find_task_trace_context(self._workspace_dir, room_id=cache_key)
        if not context.get("project_id"):
            _trace_log(
                "meta scan: no trace context workspace=%s room=%s reason=%s",
                self._workspace_dir,
                room_id or "(none)",
                context.get("reason", ""),
            )
        return context
