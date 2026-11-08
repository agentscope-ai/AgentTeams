# -*- coding: utf-8 -*-
"""Pending human questions for headless dispatch runs.

An ``ask_user_question`` call arrives as a ``can_use_tool`` control
request. Nobody sits in front of the CLI dialog in a dispatch run, so the
host asks the human, waits ``hold_seconds``, then hands the question to
the dispatch owner (the agent) for another ``agent_grace`` seconds, then
applies the default policy.

Answers are delivered back to the CLI as ``updatedInput.answers`` — the
shape AskUserQuestionTool consumes (``answerKey`` = the question index as
a string, value = the chosen option label or free text).
"""

from __future__ import annotations

import asyncio
import time
from typing import Any

#: Tools that need a human answer rather than a plain allow/deny.
INTERACTION_TOOLS = frozenset({"ask_user_question", "propose_goal"})

#: How long the human gets before the agent is asked to answer.
DEFAULT_HOLD_SECONDS = 180.0

#: Extra window the agent gets before the default policy applies.
DEFAULT_AGENT_GRACE_SECONDS = 60.0


def extract_questions(tool_input: Any) -> list[dict[str, Any]]:
    """Normalise the AskUserQuestionTool input into renderable questions.

    Schema (QwenCode 0.26): ``{questions: [{header, question,
    options: [{label, description}], multiSelect?}]}`` with 1-4
    questions. Legacy inputs may carry a bare ``questions`` list of
    strings; both are accepted, anything else yields an empty list and
    the caller falls back to the raw input text.
    """
    if not isinstance(tool_input, dict):
        return []
    raw = tool_input.get("questions")
    if not isinstance(raw, list):
        return []
    out: list[dict[str, Any]] = []
    for index, item in enumerate(raw):
        if isinstance(item, str):
            out.append(
                {
                    "answer_key": str(index),
                    "header": "",
                    "question": item,
                    "options": [],
                    "multi_select": False,
                }
            )
            continue
        if not isinstance(item, dict):
            continue
        options: list[dict[str, str]] = []
        raw_options = item.get("options")
        if isinstance(raw_options, list):
            for option in raw_options:
                if isinstance(option, dict) and option.get("label"):
                    options.append(
                        {
                            "label": str(option.get("label")),
                            "description": str(option.get("description") or ""),
                        }
                    )
                elif isinstance(option, str) and option:
                    options.append({"label": option, "description": ""})
        question = str(item.get("question") or "").strip()
        if not question:
            continue
        out.append(
            {
                "answer_key": str(index),
                "header": str(item.get("header") or ""),
                "question": question,
                "options": options,
                "multi_select": bool(item.get("multiSelect")),
            }
        )
    return out


def render_questions(questions: list[dict[str, Any]], tool_input: Any = None) -> str:
    """Human-readable question text (falls back to the raw input)."""
    if not questions:
        if isinstance(tool_input, dict):
            return " / ".join(
                f"{k}={v}" for k, v in tool_input.items() if v not in ("", None)
            )[:800]
        return str(tool_input)[:800]
    lines: list[str] = []
    for q in questions:
        header = f"[{q['header']}] " if q.get("header") else ""
        lines.append(f"{header}{q['question']}")
        for i, option in enumerate(q.get("options") or []):
            desc = f" — {option['description']}" if option.get("description") else ""
            lines.append(f"  {i + 1}) {option['label']}{desc}")
        if q.get("multi_select"):
            lines.append("  （可多选）")
    return "\n".join(lines)


def normalise_answers(
    questions: list[dict[str, Any]],
    payload: dict[str, Any],
) -> dict[str, Any]:
    """Build the CLI ``answers`` map from an answer payload.

    Accepts ``answers`` (already a map), or ``choice`` (index / label /
    free text for the first question). Unknown shapes are returned as the
    free-text answer for the first question so the model still gets it.
    """
    explicit = payload.get("answers")
    if isinstance(explicit, dict) and explicit:
        return {str(k): v for k, v in explicit.items()}
    choice = payload.get("choice", payload.get("answer"))
    if choice is None:
        return {}
    key = questions[0]["answer_key"] if questions else "0"
    if isinstance(choice, int):
        if questions and questions[0].get("options"):
            labels = [o["label"] for o in questions[0]["options"]]
            if 0 <= choice < len(labels):
                return {key: labels[choice]}
        return {key: str(choice)}
    text = str(choice).strip()
    if questions and questions[0].get("options"):
        labels = [o["label"] for o in questions[0]["options"]]
        if text.isdigit():
            idx = int(text) - 1
            if 0 <= idx < len(labels):
                return {key: labels[idx]}
        for label in labels:
            if label.lower() == text.lower():
                return {key: label}
    return {key: text}


class PendingQuestion:
    """One open interaction request for a task."""

    def __init__(
        self,
        *,
        task_id: str,
        request_id: str,
        tool_name: str,
        tool_input: Any,
        notifications: dict[str, Any] | None = None,
    ) -> None:
        self.task_id = task_id
        self.request_id = request_id
        self.tool_name = tool_name
        self.tool_input = tool_input
        self.questions = extract_questions(tool_input)
        self.question_text = render_questions(self.questions, tool_input)
        self.created_at = time.time()
        self.notified_at: float | None = None
        self.notifications: dict[str, Any] = dict(notifications or {})
        self.answers: dict[str, Any] | None = None
        self.answered_by: str = ""
        self.answered_at: float | None = None
        self._event = asyncio.Event()

    # -- state -------------------------------------------------------
    @property
    def answered(self) -> bool:
        return self.answers is not None

    def resolve(self, answers: dict[str, Any], by: str) -> bool:
        if self.answers is not None:
            return False
        self.answers = dict(answers)
        self.answered_by = str(by or "human")
        self.answered_at = time.time()
        self._event.set()
        return True

    async def wait(self, timeout: float) -> dict[str, Any] | None:
        if self.answers is not None:
            return self.answers
        if timeout <= 0:
            return None
        try:
            await asyncio.wait_for(self._event.wait(), timeout=timeout)
        except (asyncio.TimeoutError, TimeoutError):
            return None
        return self.answers

    def as_dict(self) -> dict[str, Any]:
        return {
            "request_id": self.request_id,
            "task_id": self.task_id,
            "tool_name": self.tool_name,
            "question": self.question_text,
            "questions": self.questions,
            "created_at": self.created_at,
            "notified_at": self.notified_at,
            "notifications": self.notifications,
            "answered": self.answered,
            "answered_by": self.answered_by,
            "answers": self.answers,
        }


class InteractionStore:
    """Per-dispatch-directory store of open questions (process lifetime)."""

    def __init__(self) -> None:
        self._tasks: dict[str, dict[str, PendingQuestion]] = {}

    def create(
        self,
        *,
        task_id: str,
        request_id: str,
        tool_name: str,
        tool_input: Any,
    ) -> PendingQuestion:
        question = PendingQuestion(
            task_id=task_id,
            request_id=request_id,
            tool_name=tool_name,
            tool_input=tool_input,
        )
        self._tasks.setdefault(task_id, {})[request_id] = question
        return question

    def get(self, task_id: str, request_id: str) -> PendingQuestion | None:
        return (self._tasks.get(task_id) or {}).get(request_id)

    def answer(
        self,
        task_id: str,
        request_id: str,
        answers: dict[str, Any],
        by: str = "human",
    ) -> bool:
        question = self.get(task_id, request_id)
        if question is None:
            return False
        return question.resolve(answers, by)

    def pending(self, task_id: str) -> list[dict[str, Any]]:
        return [
            q.as_dict()
            for q in (self._tasks.get(task_id) or {}).values()
            if not q.answered
        ]

    def all_for_task(self, task_id: str) -> list[dict[str, Any]]:
        return [q.as_dict() for q in (self._tasks.get(task_id) or {}).values()]

    def drop_task(self, task_id: str) -> None:
        self._tasks.pop(task_id, None)


_STORES: dict[str, InteractionStore] = {}


def store_for(key: str | None = None) -> InteractionStore:
    """One store per dispatch directory (survives per-request services)."""
    name = str(key or "")
    store = _STORES.get(name)
    if store is None:
        store = InteractionStore()
        _STORES[name] = store
    return store
