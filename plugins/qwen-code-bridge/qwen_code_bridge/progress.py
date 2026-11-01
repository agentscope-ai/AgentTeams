# -*- coding: utf-8 -*-
"""Compact live progress derived from a headless CLI's stream-json stream.

The raw event stream is the truth; this module keeps a small derived
state ("summary-level realtime") so a human or API caller can answer
*what is it doing now / is it stuck* without parsing the whole log:

    {"phase":"editing","last_tool":"edit",
     "last_target":"pkg/server.py","turns":7,"tool_calls":9,
     "tokens":{"input":105092,"output":3204},
     "tests":{"passed":7,"failed":2},
     "elapsed_s":750,"last_event_at":"19:58:41Z","stalled_s":3}

Phases: ``init`` → ``thinking`` → ``tool``/``editing``/``testing`` →
``done``/``failed``.  ``stalled`` is computed at read time (no events for
``stall_after`` seconds while running).
"""

from __future__ import annotations

import json
import os
import re
import time
from pathlib import Path
from typing import Any

_WRITE_TOOLS = {"write_file", "edit", "multi_edit", "apply_patch", "notebook_edit"}
_TEST_PATTERNS = (
    re.compile(r"\b(pytest|python3?\s+-m\s+pytest)\b"),
    re.compile(r"\b(npm|pnpm|yarn)\s+(test|run\s+test)\b"),
    re.compile(r"\bgo\s+test\b"),
    re.compile(r"\bcargo\s+test\b"),
)
_PASSED_RE = re.compile(r"(\d+)\s+passed")
_FAILED_RE = re.compile(r"(\d+)\s+failed")


class ProgressTracker:
    """Feed stream-json events; get a compact, JSON-safe snapshot."""

    def __init__(self, roots: list[str] | None = None, stall_after: float = 90.0):
        self.roots = roots or []
        self.stall_after = stall_after
        self.started = time.time()
        self.last_event = time.time()
        self.first_event: float | None = None
        self.phase = "init"
        self.last_tool = ""
        self.last_target = ""
        self.turns = 0
        self.tool_calls = 0
        self.tokens = {"input": 0, "output": 0}
        self.tests = {"passed": 0, "failed": 0}
        self.last_error = ""
        self.result: dict[str, Any] | None = None

    def feed(self, event: dict[str, Any]) -> None:
        if not isinstance(event, dict):
            return
        self.last_event = time.time()
        if self.first_event is None:
            self.first_event = self.last_event
        kind = str(event.get("type") or "")
        if kind == "system":
            if self.phase == "init":
                self.phase = "thinking"
            return
        if kind == "assistant":
            self.turns += 1
            message = event.get("message") or {}
            usage = message.get("usage") or event.get("usage") or {}
            for key, out_key in (("input_tokens", "input"), ("output_tokens", "output")):
                value = usage.get(key)
                if isinstance(value, (int, float)) and value > self.tokens[out_key]:
                    self.tokens[out_key] = int(value)
            content = message.get("content")
            if not isinstance(content, list):
                content = []
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    self.tool_calls += 1
                    self.last_tool = str(block.get("name") or "")
                    self.last_target = self._target(block.get("input"))
                    self.phase = self._phase_for(self.last_tool, block.get("input"))
                elif block.get("type") == "text" and not self.last_tool:
                    self.phase = "thinking"
            return
        if kind == "user":
            message = event.get("message") or {}
            content = message.get("content")
            if not isinstance(content, list):
                content = []
            for block in content:
                if not isinstance(block, dict) or block.get("type") != "tool_result":
                    continue
                text = _content_text(block.get("content"))
                self._observe_tests(text)
                if block.get("is_error"):
                    self.last_error = text[:400]
            return
        if kind == "result":
            self.result = event
            is_error = bool(event.get("is_error")) or str(event.get("subtype") or "") == "error"
            self.phase = "failed" if is_error else "done"
            usage = event.get("usage") or {}
            for key, out_key in (("input_tokens", "input"), ("output_tokens", "output")):
                value = usage.get(key)
                if isinstance(value, (int, float)) and value > self.tokens[out_key]:
                    self.tokens[out_key] = int(value)
            if is_error and not self.last_error:
                self.last_error = _content_text(event.get("result"))[:400]
            return

    def _phase_for(self, tool: str, tool_input: Any) -> str:
        lowered = tool.lower()
        if lowered in _WRITE_TOOLS or "write" in lowered or "edit" in lowered:
            return "editing"
        command = ""
        if isinstance(tool_input, dict):
            command = str(tool_input.get("command") or "")
        if any(p.search(command) for p in _TEST_PATTERNS):
            return "testing"
        return "tool"

    def _target(self, tool_input: Any) -> str:
        if not isinstance(tool_input, dict):
            return ""
        for key in ("file_path", "path", "target_file", "command"):
            value = tool_input.get(key)
            if isinstance(value, str) and value:
                return value[:200]
        return ""

    def _observe_tests(self, text: str) -> None:
        if not text:
            return
        passed = _PASSED_RE.findall(text)
        failed = _FAILED_RE.findall(text)
        if passed:
            self.tests["passed"] = max(self.tests["passed"], int(passed[-1]))
        if failed:
            self.tests["failed"] = max(self.tests["failed"], int(failed[-1]))

    def snapshot(self, state: str = "running") -> dict[str, Any]:
        now = time.time()
        stalled = 0.0
        if state == "running":
            stalled = round(max(0.0, now - self.last_event), 1)
        phase = self.phase
        if state == "running" and stalled > self.stall_after and phase not in ("done", "failed"):
            phase = "stalled"
        return {
            "phase": phase,
            "last_tool": self.last_tool,
            "last_target": self.last_target,
            "turns": self.turns,
            "tool_calls": self.tool_calls,
            "tokens": dict(self.tokens),
            "tests": dict(self.tests),
            "elapsed_s": round(now - self.started, 1),
            "first_event_s": (
                round(self.first_event - self.started, 1)
                if self.first_event
                else None
            ),
            "stalled_s": stalled,
            "last_error": self.last_error[-400:] if self.last_error else "",
        }

    def line(self, state: str = "running") -> str:
        """One human-readable status line (chat / status table)."""
        snap = self.snapshot(state)
        bits = [f"▶ {snap['phase']}", f"{snap['turns']} turns", f"{snap['tool_calls']} tools"]
        if snap["last_tool"]:
            target = f" {snap['last_target']}" if snap["last_target"] else ""
            bits.append(f"last={snap['last_tool']}{target}")
        if snap["tests"]["passed"] or snap["tests"]["failed"]:
            bits.append(f"tests {snap['tests']['passed']}p/{snap['tests']['failed']}f")
        if snap["stalled_s"] and snap["phase"] == "stalled":
            bits.append(f"STALLED {snap['stalled_s']}s")
        return " · ".join(bits)


def _content_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                parts.append(str(block.get("text") or ""))
            elif isinstance(block, str):
                parts.append(block)
        return "\n".join(parts)
    return ""


def write_progress(path: Path, snapshot: dict[str, Any]) -> None:
    """Atomic write of the derived progress JSON."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(snapshot, ensure_ascii=False), encoding="utf-8")
    os.replace(tmp, path)
