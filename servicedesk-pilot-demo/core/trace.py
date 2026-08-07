"""
ServiceDesk Pilot - 全链路 Trace 与控制台可视化

对应方案第十二章『全链路可追溯与证据规范』和第十四章『实时监控与可观测设计』。
每一次 Agent 调用、Skill 调用、MCP 工具调用、RAG 检索都会落一条 Trace，
既用于终端分阶段状态展示，也用于导出 trace.json / HTML 看板回放。
"""

from __future__ import annotations

import json
import os
import sys
import time
from dataclasses import dataclass, field, asdict
from typing import Any, Dict, List, Optional

from .models import now_iso

# --------------------------------------------------------------------------
# 终端着色（Windows Git Bash / CMD 自动降级为无色）
# --------------------------------------------------------------------------

def _supports_color() -> bool:
    if os.environ.get("NO_COLOR"):
        return False
    if os.environ.get("FORCE_COLOR"):
        return True
    return sys.stdout.isatty()


_COLOR = _supports_color()

if sys.platform == "win32":
    try:  # 让 Windows 终端支持 ANSI
        import ctypes
        kernel32 = ctypes.windll.kernel32
        kernel32.SetConsoleMode(kernel32.GetStdHandle(-11), 7)
    except Exception:
        pass


class C:
    RESET = "\033[0m" if _COLOR else ""
    BOLD = "\033[1m" if _COLOR else ""
    DIM = "\033[2m" if _COLOR else ""
    RED = "\033[31m" if _COLOR else ""
    GREEN = "\033[32m" if _COLOR else ""
    YELLOW = "\033[33m" if _COLOR else ""
    BLUE = "\033[34m" if _COLOR else ""
    MAGENTA = "\033[35m" if _COLOR else ""
    CYAN = "\033[36m" if _COLOR else ""
    GREY = "\033[90m" if _COLOR else ""


# --------------------------------------------------------------------------
# Trace 事件
# --------------------------------------------------------------------------

@dataclass
class TraceEvent:
    seq: int
    at: str
    kind: str           # stage | agent | skill | tool | rag | llm | im | approval | kb | metric
    name: str
    incident_id: str
    status: str = "ok"  # ok | fail | pending | skipped
    duration_ms: int = 0
    detail: Dict[str, Any] = field(default_factory=dict)
    citations: List[str] = field(default_factory=list)


# 阶段定义：对应用户要求的端到端七个可见阶段
STAGES = [
    ("S1", "微信群消息接入", "IM Adapter 拉取群消息"),
    ("S2", "事件受理与解析", "Ticket Intake Agent 标准化 + 去重"),
    ("S3", "知识检索", "Knowledge Agent 检索 Runbook / 案例 / Legacy 经验"),
    ("S4", "风险分诊", "Triage Analyst 判定风险等级与执行路径"),
    ("S5", "方案生成", "Resolution Agent 生成分级处置方案"),
    ("S6", "审批决策", "按配置走人工审批 / 自动跳过"),
    ("S7", "执行与标记完成", "白名单自动执行 或 Legacy 人工执行回填"),
    ("S8", "恢复验证", "Verify Agent 探针验证 + 用户确认"),
    ("S9", "复盘与知识沉淀", "Postmortem + KnowledgeReflector 写回知识库"),
]


class Tracer:
    """Trace 记录器 + 终端渲染器"""

    def __init__(self, verbose: bool = True):
        self.events: List[TraceEvent] = []
        self._seq = 0
        self.verbose = verbose
        self.started_at = time.time()
        self._stage_start: Optional[float] = None
        self.current_incident = ""

    # ---------------- 记录 ----------------

    def _emit(self, kind: str, name: str, status: str = "ok",
              duration_ms: int = 0, detail: Optional[Dict[str, Any]] = None,
              citations: Optional[List[str]] = None) -> TraceEvent:
        self._seq += 1
        ev = TraceEvent(
            seq=self._seq,
            at=now_iso(),
            kind=kind,
            name=name,
            incident_id=self.current_incident,
            status=status,
            duration_ms=duration_ms,
            detail=detail or {},
            citations=citations or [],
        )
        self.events.append(ev)
        return ev

    # ---------------- 终端输出 ----------------

    def banner(self, title: str, subtitle: str = "") -> None:
        if not self.verbose:
            return
        line = "═" * 74
        print(f"\n{C.CYAN}{C.BOLD}╔{line}╗{C.RESET}")
        print(f"{C.CYAN}{C.BOLD}║{C.RESET} {C.BOLD}{title}{C.RESET}".ljust(88) + f"{C.CYAN}{C.BOLD}║{C.RESET}")
        if subtitle:
            print(f"{C.CYAN}{C.BOLD}║{C.RESET} {C.GREY}{subtitle}{C.RESET}".ljust(97) + f"{C.CYAN}{C.BOLD}║{C.RESET}")
        print(f"{C.CYAN}{C.BOLD}╚{line}╝{C.RESET}")

    def stage(self, stage_id: str, title: str, desc: str = "") -> None:
        self._stage_start = time.time()
        self._emit("stage", f"{stage_id} {title}", detail={"desc": desc})
        if not self.verbose:
            return
        print(f"\n{C.BLUE}{C.BOLD}┏━ [{stage_id}] {title}{C.RESET}  {C.GREY}{desc}{C.RESET}")

    def stage_done(self, summary: str, status: str = "ok") -> None:
        dur = int((time.time() - (self._stage_start or time.time())) * 1000)
        self._emit("stage_end", summary, status=status, duration_ms=dur)
        if not self.verbose:
            return
        mark = f"{C.GREEN}✔{C.RESET}" if status == "ok" else (
            f"{C.YELLOW}⚠{C.RESET}" if status == "skipped" else f"{C.RED}✘{C.RESET}")
        print(f"{C.BLUE}┗━{C.RESET} {mark} {summary} {C.GREY}({dur}ms){C.RESET}")

    def agent(self, agent_name: str, action: str, detail: Optional[Dict[str, Any]] = None) -> None:
        self._emit("agent", agent_name, detail={"action": action, **(detail or {})})
        if self.verbose:
            print(f"{C.BLUE}┃{C.RESET}  {C.MAGENTA}◆ Agent{C.RESET} {C.BOLD}{agent_name}{C.RESET} {C.GREY}→{C.RESET} {action}")

    def skill(self, skill_name: str, summary: str, status: str = "ok",
              detail: Optional[Dict[str, Any]] = None, duration_ms: int = 0) -> None:
        self._emit("skill", skill_name, status=status, duration_ms=duration_ms, detail=detail or {})
        if not self.verbose:
            return
        mark = {"ok": f"{C.GREEN}·{C.RESET}", "fail": f"{C.RED}✘{C.RESET}",
                "skipped": f"{C.YELLOW}~{C.RESET}", "pending": f"{C.YELLOW}…{C.RESET}"}.get(status, "·")
        print(f"{C.BLUE}┃{C.RESET}    {mark} {C.CYAN}Skill{C.RESET} {skill_name:<28} {summary}")

    def tool(self, tool_name: str, system_id: str, summary: str,
             status: str = "ok", detail: Optional[Dict[str, Any]] = None) -> None:
        self._emit("tool", tool_name, status=status,
                   detail={"system_id": system_id, "summary": summary, **(detail or {})})
        if not self.verbose:
            return
        mark = f"{C.GREEN}✔{C.RESET}" if status == "ok" else (
            f"{C.YELLOW}~{C.RESET}" if status == "skipped" else f"{C.RED}✘{C.RESET}")
        print(f"{C.BLUE}┃{C.RESET}      {mark} {C.YELLOW}MCP{C.RESET} {system_id}.{tool_name} {C.GREY}→{C.RESET} {summary}")

    def rag(self, query: str, hits: int, citations: List[str], gap: bool = False) -> None:
        self._emit("rag", "RAG检索", status="fail" if gap else "ok",
                   detail={"query": query, "hits": hits, "gap": gap}, citations=citations)
        if not self.verbose:
            return
        if gap:
            print(f"{C.BLUE}┃{C.RESET}      {C.RED}✘{C.RESET} {C.YELLOW}RAG{C.RESET} \"{query}\" {C.RED}无有效召回 → 标记知识缺口{C.RESET}")
        else:
            print(f"{C.BLUE}┃{C.RESET}      {C.GREEN}✔{C.RESET} {C.YELLOW}RAG{C.RESET} \"{query}\" 命中 {hits} 条 {C.GREY}[{', '.join(citations)}]{C.RESET}")

    def im(self, direction: str, who: str, text: str) -> None:
        self._emit("im", direction, detail={"who": who, "text": text})
        if not self.verbose:
            return
        arrow = f"{C.GREEN}←{C.RESET}" if direction == "inbound" else f"{C.CYAN}→{C.RESET}"
        label = "群消息" if direction == "inbound" else "机器人回复"
        preview = text if len(text) <= 100 else text[:98] + "…"
        print(f"{C.BLUE}┃{C.RESET}    {arrow} {C.DIM}[{label}]{C.RESET} {C.BOLD}{who}{C.RESET}: {preview}")

    def approval(self, summary: str, status: str, detail: Optional[Dict[str, Any]] = None) -> None:
        self._emit("approval", summary, status=status, detail=detail or {})
        if not self.verbose:
            return
        color = {"ok": C.GREEN, "skipped": C.YELLOW, "fail": C.RED, "pending": C.YELLOW}.get(status, C.GREY)
        print(f"{C.BLUE}┃{C.RESET}    {color}▣ 审批{C.RESET} {summary}")

    def kb(self, op: str, target: str, detail: Optional[Dict[str, Any]] = None) -> None:
        self._emit("kb", op, detail={"target": target, **(detail or {})})
        if self.verbose:
            print(f"{C.BLUE}┃{C.RESET}    {C.GREEN}⊕ 知识库{C.RESET} {op}: {C.BOLD}{target}{C.RESET}")

    def note(self, text: str, level: str = "info") -> None:
        self._emit("note", text, status="ok" if level == "info" else level)
        if not self.verbose:
            return
        color = {"info": C.GREY, "warn": C.YELLOW, "error": C.RED}.get(level, C.GREY)
        print(f"{C.BLUE}┃{C.RESET}    {color}› {text}{C.RESET}")

    def metric(self, metrics: Dict[str, Any]) -> None:
        self._emit("metric", "ObservabilityProbe", detail=metrics)

    # ---------------- 导出 ----------------

    def export(self, path: str, extra: Optional[Dict[str, Any]] = None) -> str:
        payload = {
            "generated_at": now_iso(),
            "total_events": len(self.events),
            "wall_clock_seconds": round(time.time() - self.started_at, 3),
            "stages": [{"id": s[0], "title": s[1], "desc": s[2]} for s in STAGES],
            "events": [asdict(e) for e in self.events],
        }
        if extra:
            payload.update(extra)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            json.dump(payload, f, ensure_ascii=False, indent=2)
        return path
