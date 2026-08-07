"""
ServiceDesk Pilot —— 端到端 Demo 入口

一键跑通方案定义的九阶段闭环：
  S1 微信群消息接入 → S2 事件解析去重 → S3 知识检索 → S4 风险分诊
  → S5 方案生成 → S6 审批决策(配置驱动) → S7 执行 → S8 验证 → S9 复盘+知识沉淀

用法：
  python run_demo.py                       # 跑全部场景（默认）
  python run_demo.py --scenario S1_vpn_locked
  python run_demo.py --approval-mode auto_skip
  python run_demo.py --approval-mode auto_approve --scenario S2_mfa_lost
  python run_demo.py --list                # 列出全部场景后退出
  python run_demo.py --keep-kb             # 不还原知识库（保留运行后的写入结果）

产物（out/ 目录）：
  out/trace.json   全链路 Trace（可被 HTML 看板回放）
  out/report.md    文字版复盘报告
  out/dashboard.html  亮色主题可视化看板（泳道图/审批卡/知识库 diff/调用链）
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import shutil
import sys
import time
from typing import Any, Dict, List, Optional

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, BASE_DIR)

from core.trace import Tracer, STAGES                          # noqa: E402
from core.models import TicketStatus, ApprovalDecision          # noqa: E402
from adapters.im_adapter import WeChatGroupAdapter              # noqa: E402
from adapters.mcp_gateway import MockMcpGateway                 # noqa: E402
from adapters.approval_adapter import ApprovalAdapter          # noqa: E402
from skills.knowledge_skills import KnowledgeBase               # noqa: E402
from agents.orchestrator import TeamLeader                      # noqa: E402
from skills.verify_skills import ObservabilityProbe            # noqa: E402


# --------------------------------------------------------------------------
# 路径
# --------------------------------------------------------------------------
MOCK_DIR = os.path.join(BASE_DIR, "mock")
OUT_DIR = os.path.join(BASE_DIR, "out")
KB_FILE = os.path.join(MOCK_DIR, "knowledge_base.json")
KB_BASE = os.path.join(MOCK_DIR, "knowledge_base.base.json")
WECHAT_FILE = os.path.join(MOCK_DIR, "wechat_messages.json")
SYSTEMS_FILE = os.path.join(MOCK_DIR, "systems.json")
APPROVERS_FILE = os.path.join(MOCK_DIR, "approvers.json")
POLICY_FILE = os.path.join(BASE_DIR, "config", "policy.json")


# --------------------------------------------------------------------------
# 知识库基线管理（保证可复现：每次运行从 pristine 基线重置，运行后默认还原）
# --------------------------------------------------------------------------
def ensure_kb_baseline() -> None:
    if not os.path.exists(KB_BASE):
        shutil.copyfile(KB_FILE, KB_BASE)


def reset_kb_to_baseline() -> None:
    shutil.copyfile(KB_BASE, KB_FILE)


def _kb_counts_from_file(path: str) -> Dict[str, int]:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    return {
        "runbooks": len(data.get("runbooks", [])),
        "cases": len(data.get("cases", [])),
        "legacy_notes": len(data.get("legacy_operator_notes", [])),
        "badcases": len(data.get("badcases", [])),
    }


# --------------------------------------------------------------------------
# 上下文装配
# --------------------------------------------------------------------------
def build_context(policy: Dict[str, Any]) -> Dict[str, Any]:
    im = WeChatGroupAdapter(WECHAT_FILE)
    mcp = MockMcpGateway(SYSTEMS_FILE)
    kb = KnowledgeBase(KB_FILE)
    approval = ApprovalAdapter(APPROVERS_FILE, policy)
    tracer = Tracer(verbose=policy.get("verbose", True))
    leader = TeamLeader(tracer, policy, im, mcp, kb, approval)
    return {"im": im, "mcp": mcp, "kb": kb, "approval": approval,
            "tracer": tracer, "leader": leader}


# --------------------------------------------------------------------------
# 从 Trace 提取每个工单的九阶段状态
# --------------------------------------------------------------------------
def extract_stage_matrix(tracer: Tracer) -> Dict[str, Dict[str, str]]:
    matrix: Dict[str, Dict[str, str]] = {}
    for ev in tracer.events:
        if ev.kind == "stage_end":
            inc = ev.incident_id
            sid = ev.name.split(" ")[0] if ev.name else ""
            if sid.startswith("S") and len(sid) <= 3:
                matrix.setdefault(inc, {})[sid] = ev.status
    return matrix


# --------------------------------------------------------------------------
# 运行
# --------------------------------------------------------------------------
def run_scenarios(leader: TeamLeader, scenarios: List[str]) -> List[Any]:
    incidents = []
    for sc in scenarios:
        inc = leader.handle_scenario(sc)
        incidents.append(inc)
        print()
    return incidents


# --------------------------------------------------------------------------
# 报告（Markdown）
# --------------------------------------------------------------------------
def write_report(path: str, policy: Dict[str, Any], incidents: List[Any],
                 metrics: Dict[str, Any], kb_before: Dict[str, int],
                 kb_after: Dict[str, int]) -> None:
    def risk(inc):
        return inc.triage.risk_level.value if inc.triage else "-"

    def path_(inc):
        return inc.triage.execution_path.value if inc.triage else "-"

    def apv(inc):
        if not inc.approval:
            return "无需审批"
        m = {
            ApprovalDecision.SKIPPED: f"自动跳过({inc.approval.skip_rule_id})",
            ApprovalDecision.APPROVED: f"人工批准({inc.approval.decided_by})",
            ApprovalDecision.REJECTED: "驳回",
            ApprovalDecision.ESCALATED: "审批人升级",
        }
        return m.get(inc.approval.decision, inc.approval.decision.value)

    def verdict(inc):
        c = inc.verification.conclusion if inc.verification else "-"
        return {"success": "✅ 已解决", "partial": "⚠ 部分解决",
                "failed": "❌ 失败", "not_applicable": "↗ 已升级人工"}.get(c, c)

    lines: List[str] = []
    lines.append("# ServiceDesk Pilot · 端到端复盘报告\n")
    lines.append(f"- 运行时间: `{policy.get('_run_at', '')}`")
    lines.append(f"- 审批模式: `{policy.get('approval', {}).get('mode', 'manual')}`"
                 f"{'（交互式）' if policy.get('approval', {}).get('interactive') else ''}")
    lines.append(f"- 场景数: {len(incidents)} · 工单数: {metrics.get('incidents_total', 0)}\n")

    lines.append("## 一、关键指标\n")
    lines.append("| 指标 | 数值 |")
    lines.append("|---|---|")
    lines.append(f"| 工单总数 | {metrics.get('incidents_total', 0)} |")
    lines.append(f"| 一次解决 | {metrics.get('first_time_resolved', 0)} |")
    lines.append(f"| 部分解决 | {metrics.get('partially_resolved', 0)} |")
    lines.append(f"| 升级人工 | {metrics.get('escalated', 0)} |")
    lines.append(f"| 解决率 | {metrics.get('resolution_rate', 0):.0%} |")
    lines.append(f"| 审批总数 / 免审批 / 批准 | {metrics.get('approval_total', 0)} / "
                 f"{metrics.get('approval_skipped', 0)} / {metrics.get('approval_approved', 0)} |")
    lines.append(f"| 白名单自动动作 | {metrics.get('actions_auto_executed', 0)} |")
    lines.append(f"| 人工/Legacy 动作 | {metrics.get('actions_manual', 0)} |")
    lines.append(f"| MCP 工具调用 / 成功率 | {metrics.get('mcp_tool_calls', 0)} / "
                 f"{metrics.get('mcp_tool_success_rate', 0):.0%} |")
    lines.append(f"| RAG 查询 / 命中率 | {metrics.get('rag_queries', 0)} / "
                 f"{metrics.get('rag_hit_rate', 0):.0%} |")
    lines.append(f"| 知识库写回项 | {metrics.get('kb_write_backs', 0)} |")
    lines.append("")

    lines.append("## 二、逐工单复盘\n")
    for inc in incidents:
        cls = inc.classification
        lines.append(f"### {inc.incident_id} · `{inc.scenario}`\n")
        lines.append(f"- **分类**：{cls.category if cls else '-'}（置信度 "
                     f"{cls.confidence if cls else '-'}）")
        lines.append(f"- **风险等级 / 执行路径**：{risk(inc)} / {path_(inc)}")
        lines.append(f"- **审批**：{apv(inc)}")
        lines.append(f"- **处置结论**：{verdict(inc)}")
        acts = inc.plan.actions if inc.plan else []
        done = sum(1 for a in acts if a.status == "done")
        manual = sum(1 for a in acts if a.status == "manual_done")
        lines.append(f"- **动作执行**：自动 {done} · 人工 {manual} · 共 {len(acts)}")
        if inc.postmortem:
            lines.append(f"- **根因**：{inc.postmortem.root_cause}")
            if inc.postmortem.what_worked:
                lines.append(f"- **做得好的**：{'; '.join(inc.postmortem.what_worked)}")
            if inc.postmortem.improvements:
                lines.append(f"- **改进项**：{'; '.join(inc.postmortem.improvements)}")
        if inc.kb_updates:
            ups = "; ".join(f"{u['op']}={u.get('id', '')}" for u in inc.kb_updates)
            lines.append(f"- **知识沉淀**：{ups}")
        if inc.im_replies:
            lines.append(f"- **群内同步**：{len(inc.im_replies)} 条进度消息")
        lines.append("")

    lines.append("## 三、知识库变更（运行前 → 运行后）\n")
    lines.append("| 维度 | 运行前 | 运行后 | 增量 |")
    lines.append("|---|---|---|---|")
    for k in ("runbooks", "cases", "legacy_notes", "badcases"):
        b, a = kb_before.get(k, 0), kb_after.get(k, 0)
        lines.append(f"| {k} | {b} | {a} | +{a - b} |")
    lines.append("")
    lines.append("> 说明：KnowledgeReflector 真实写回 `mock/knowledge_base.json`，"
                 "以上为本次运行的增量；默认情况下脚本运行结束会还原基线，详见 HTML 看板。\n")

    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))


# --------------------------------------------------------------------------
# HTML 看板（亮色主题）
# --------------------------------------------------------------------------
def _esc(s: Any) -> str:
    return (str(s).replace("&", "&amp;").replace("<", "&lt;")
            .replace(">", "&gt;").replace('"', "&quot;"))


def _stage_badge(status: str) -> str:
    cls = {"ok": "ok", "fail": "fail", "skipped": "skip"}.get(status, "pend")
    label = {"ok": "✔", "fail": "✘", "skipped": "↷", "pend": "·"}.get(cls, "·")
    return f'<span class="stage {cls}">{label}</span>'


def _pill(text: str, kind: str) -> str:
    return f'<span class="pill {kind}">{_esc(text)}</span>'


def write_dashboard(path: str, policy: Dict[str, Any], incidents: List[Any],
                    metrics: Dict[str, Any], stage_matrix: Dict[str, Dict[str, str]],
                    kb_before: Dict[str, int], kb_after: Dict[str, int],
                    all_kb_updates: List[Dict[str, Any]]) -> None:
    mode = policy.get("approval", {}).get("mode", "manual")
    run_at = policy.get("_run_at", "")

    # KPI
    kpi = [
        ("工单总数", metrics.get("incidents_total", 0), ""),
        ("一次解决", metrics.get("first_time_resolved", 0), "ok"),
        ("部分解决", metrics.get("partially_resolved", 0), "skip"),
        ("升级人工", metrics.get("escalated", 0), "fail"),
        ("解决率", f"{metrics.get('resolution_rate', 0):.0%}", "ok"),
        ("免审批", metrics.get("approval_skipped", 0), "skip"),
        ("知识写回", metrics.get("kb_write_backs", 0), "info"),
    ]
    kpi_html = "".join(
        f'<div class="kpi"><div class="kpi-v {k[2]}">{_esc(k[1])}</div>'
        f'<div class="kpi-l">{_esc(k[0])}</div></div>' for k in kpi)

    # 泳道表
    head = "".join(f"<th>{s[0]}<br><small>{_esc(s[1])}</small></th>" for s in STAGES)
    rows = ""
    for inc in incidents:
        cells = ""
        for sid, _, _ in STAGES:
            st = stage_matrix.get(inc.incident_id, {}).get(sid, "")
            cells += f"<td>{_stage_badge(st)}</td>"
        cat = inc.classification.category if inc.classification else "-"
        risk_v = inc.triage.risk_level.value if inc.triage else "-"
        risk_cls = {"L0": "r0", "L1": "r1", "L2": "r2", "L3": "r3"}.get(risk_v, "r0")
        concl = inc.verification.conclusion if inc.verification else "-"
        concl_pill = {"success": "ok", "partial": "skip", "failed": "fail",
                      "not_applicable": "warn"}.get(concl, "info")
        rows += (f'<tr><td class="inc-id">{_esc(inc.incident_id)}<br>'
                 f'<small>{_esc(inc.scenario)}</small></td>'
                 f'<td>{_pill(risk_v, risk_cls)}</td>'
                 f'<td><small>{_esc(cat)}</small></td>'
                 f'{cells}'
                 f'<td>{_pill(concl, concl_pill)}</td></tr>')

    # 审批卡
    approval_cards = ""
    for inc in incidents:
        a = inc.approval
        if not a:
            decision_html = _pill("无需审批", "info")
            meta = "方案内动作均在自动执行白名单"
        else:
            dmap = {
                ApprovalDecision.SKIPPED: ("免审批", "skip"),
                ApprovalDecision.APPROVED: ("已批准", "ok"),
                ApprovalDecision.REJECTED: ("已驳回", "fail"),
                ApprovalDecision.ESCALATED: ("审批人升级", "warn"),
                ApprovalDecision.PENDING: ("待审批", "info"),
            }
            label, kind = dmap.get(a.decision, (a.decision.value, "info"))
            decision_html = _pill(label, kind)
            meta = (f"审批人 {_esc(a.approver_name)} · 规则 {_esc(a.skip_rule_id or '-')} · "
                    f"等待 {a.latency_seconds}s")
        approval_cards += (
            f'<div class="card"><div class="card-h">{_esc(inc.incident_id)}</div>'
            f'<div class="card-b">{decision_html}</div>'
            f'<div class="card-m">{meta}</div>'
            f'<div class="card-c">{_esc(a.comment if a else "")}</div></div>')

    # 知识库 diff
    diff_rows = ""
    for k in ("runbooks", "cases", "legacy_notes", "badcases"):
        b, a = kb_before.get(k, 0), kb_after.get(k, 0)
        diff_rows += (f'<tr><td>{_esc(k)}</td><td>{b}</td><td>{a}</td>'
                      f'<td class="{"plus" if a > b else ""}">+{a - b}</td></tr>')

    # 新增条目清单
    new_items_html = ""
    if all_kb_updates:
        grouped: Dict[str, List[str]] = {}
        for u in all_kb_updates:
            grouped.setdefault(u.get("op", "?"), []).append(
                f'{_esc(u.get("id", ""))} {_esc(u.get("title", ""))}'.strip())
        for op, items in grouped.items():
            new_items_html += (f'<div class="kb-op"><b>{_esc(op)}</b> '
                               f'({len(items)})</div><ul>')
            for it in items:
                new_items_html += f"<li>{it}</li>"
            new_items_html += "</ul>"
    else:
        new_items_html = '<p class="muted">本次运行无知识库写入</p>'

    # 调用链（按工单折叠）—— 直接复用已导出的 trace.json 事件
    with open(os.path.join(OUT_DIR, "trace.json"), "r", encoding="utf-8") as f:
        trace_data = json.load(f)
    trace_html = ""
    for inc in incidents:
        evs = [e for e in trace_data["events"] if e.get("incident_id") == inc.incident_id]
        items = ""
        for e in evs:
            kind = e["kind"]
            if kind in ("stage", "stage_end"):
                continue
            st = e.get("status", "ok")
            mark = {"ok": "✔", "fail": "✘", "skipped": "↷", "pending": "…"}.get(st, "·")
            detail = e.get("detail") or {}
            extra = ""
            if "summary" in detail:
                extra = _esc(detail["summary"])
            elif kind == "im":
                extra = f'{_esc(detail.get("who",""))}: {_esc(detail.get("text",""))}'
            elif kind == "rag":
                extra = f'命中 {detail.get("hits",0)} 条'
            elif kind == "tool":
                extra = f'{_esc(detail.get("system_id",""))} → {_esc(detail.get("summary",""))}'
            elif kind == "approval":
                extra = _esc(e["name"])
            items += (f'<li><span class="mk {st}">{mark}</span>'
                      f'<span class="knd">{_esc(kind)}</span> {extra}</li>')
        trace_html += (f'<details><summary>{_esc(inc.incident_id)} · {_esc(inc.scenario)}'
                       f'（{len(evs)} 事件）</summary><ul class="trace">{items}</ul></details>')

    html = f"""<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ServiceDesk Pilot · 运行看板</title>
<style>
  :root {{
    --bg:#f6f8fa; --panel:#ffffff; --ink:#1f2328; --muted:#656d76;
    --line:#d0d7de; --blue:#0969da; --green:#1a7f37; --greenbg:#e6f4ea;
    --amber:#9a6700; --amberbg:#fff8c5; --red:#cf222e; --redbg:#ffebe9;
    --greybg:#eaeef2;
  }}
  * {{ box-sizing:border-box; }}
  body {{ font-family:-apple-system,"Segoe UI","Microsoft YaHei",Helvetica,Arial,sans-serif;
    background:var(--bg); color:var(--ink); margin:0; padding:24px; line-height:1.5; }}
  .wrap {{ max-width:1180px; margin:0 auto; }}
  h1 {{ font-size:22px; margin:0 0 4px; }}
  .sub {{ color:var(--muted); font-size:13px; margin-bottom:18px; }}
  .kpis {{ display:flex; flex-wrap:wrap; gap:12px; margin-bottom:22px; }}
  .kpi {{ background:var(--panel); border:1px solid var(--line); border-radius:10px;
    padding:12px 16px; min-width:104px; flex:1; }}
  .kpi-v {{ font-size:24px; font-weight:700; }}
  .kpi-v.ok {{ color:var(--green); }} .kpi-v.fail {{ color:var(--red); }}
  .kpi-v.skip {{ color:var(--amber); }} .kpi-v.info {{ color:var(--blue); }}
  .kpi-l {{ font-size:12px; color:var(--muted); margin-top:2px; }}
  .panel {{ background:var(--panel); border:1px solid var(--line); border-radius:10px;
    padding:16px 18px; margin-bottom:20px; }}
  .panel h2 {{ font-size:15px; margin:0 0 12px; }}
  table {{ border-collapse:collapse; width:100%; font-size:13px; }}
  th, td {{ border:1px solid var(--line); padding:7px 9px; text-align:center; }}
  th {{ background:var(--greybg); font-weight:600; }}
  th small {{ color:var(--muted); font-weight:400; }}
  td.inc-id {{ text-align:left; font-weight:600; }}
  td.inc-id small {{ color:var(--muted); font-weight:400; }}
  td.plus {{ color:var(--green); font-weight:700; }}
  .stage {{ display:inline-block; width:22px; height:22px; line-height:22px;
    border-radius:6px; font-weight:700; font-size:13px; }}
  .stage.ok {{ background:var(--greenbg); color:var(--green); }}
  .stage.fail {{ background:var(--redbg); color:var(--red); }}
  .stage.skip {{ background:var(--amberbg); color:var(--amber); }}
  .stage.pend {{ background:var(--greybg); color:var(--muted); }}
  .pill {{ display:inline-block; padding:2px 9px; border-radius:20px; font-size:12px;
    font-weight:600; border:1px solid var(--line); }}
  .pill.ok {{ background:var(--greenbg); color:var(--green); border-color:#aad9b5; }}
  .pill.fail {{ background:var(--redbg); color:var(--red); border-color:#f0b3b8; }}
  .pill.skip {{ background:var(--amberbg); color:var(--amber); border-color:#ecd98a; }}
  .pill.warn {{ background:#fff1e6; color:#bc4c00; border-color:#f5c89a; }}
  .pill.info {{ background:#ddf4ff; color:var(--blue); border-color:#aad4f0; }}
  .pill.r0 {{ background:var(--greybg); color:var(--muted); }}
  .pill.r1 {{ background:#ddf4ff; color:var(--blue); }}
  .pill.r2 {{ background:#fff1e6; color:#bc4c00; }}
  .pill.r3 {{ background:var(--redbg); color:var(--red); }}
  .cards {{ display:flex; flex-wrap:wrap; gap:12px; }}
  .card {{ background:var(--panel); border:1px solid var(--line); border-radius:10px;
    padding:12px 14px; width:200px; }}
  .card-h {{ font-weight:700; font-size:13px; margin-bottom:6px; }}
  .card-b {{ margin-bottom:6px; }} .card-m {{ font-size:12px; color:var(--muted); }}
  .card-c {{ font-size:12px; color:var(--ink); margin-top:6px; }}
  .kb-op {{ margin:10px 0 4px; font-size:13px; }}
  .kb-op ul, .trace {{ margin:4px 0 4px 18px; font-size:12px; }}
  details {{ margin-bottom:8px; border:1px solid var(--line); border-radius:8px;
    padding:8px 12px; background:var(--panel); }}
  summary {{ cursor:pointer; font-weight:600; font-size:13px; }}
  .mk {{ display:inline-block; width:16px; font-weight:700; }}
  .mk.ok {{ color:var(--green); }} .mk.fail {{ color:var(--red); }}
  .mk.skipped {{ color:var(--amber); }} .mk.pending {{ color:var(--muted); }}
  .knd {{ display:inline-block; min-width:64px; color:var(--muted); font-size:11px;
    text-transform:uppercase; }}
  .muted {{ color:var(--muted); font-size:13px; }}
</style></head>
<body><div class="wrap">
  <h1>ServiceDesk Pilot · 端到端运行看板</h1>
  <div class="sub">GOAI Agent Infra 赛道初赛 Demo · 运行时间 {_esc(run_at)} ·
    审批模式 <b>{_esc(mode)}</b> · 场景数 {len(incidents)}</div>

  <div class="kpis">{kpi_html}</div>

  <div class="panel"><h2>九阶段泳道（每工单 × S1–S9 状态）</h2>
    <table><thead><tr><th>工单 / 场景</th><th>风险</th><th>分类</th>
      {head}<th>结论</th></tr></thead>
      <tbody>{rows}</tbody></table>
    <p class="muted">✔ 完成 · ✘ 失败 · ↷ 跳过/升级 · · 未进入该阶段</p>
  </div>

  <div class="panel"><h2>审批决策卡（S6）</h2>
    <div class="cards">{approval_cards}</div>
  </div>

  <div class="panel"><h2>知识库变更（knowledge_base.json 运行前 → 运行后）</h2>
    <table><thead><tr><th>维度</th><th>运行前</th><th>运行后</th><th>增量</th></tr></thead>
      <tbody>{diff_rows}</tbody></table>
    <div style="margin-top:14px;">{new_items_html}</div>
  </div>

  <div class="panel"><h2>全链路调用链（点击展开）</h2>
    {trace_html}
  </div>
</div></body></html>"""

    with open(path, "w", encoding="utf-8") as f:
        f.write(html)


# --------------------------------------------------------------------------
# 主流程
# --------------------------------------------------------------------------
def main() -> int:
    ap = argparse.ArgumentParser(description="ServiceDesk Pilot 端到端 Demo")
    ap.add_argument("--scenario", help="只跑指定场景（默认全部）")
    ap.add_argument("--approval-mode", choices=["manual", "auto_skip", "auto_approve"],
                    help="覆盖 policy.json 的 approval.mode")
    ap.add_argument("--interactive", action="store_true",
                    help="审批走终端真人输入（默认用 scripted 决策可复现）")
    ap.add_argument("--quiet", action="store_true", help="仅输出结果摘要")
    ap.add_argument("--list", action="store_true", help="列出全部场景后退出")
    ap.add_argument("--keep-kb", action="store_true",
                    help="运行结束后不还原知识库（保留写入结果）")
    args = ap.parse_args()

    # 载入配置
    with open(POLICY_FILE, "r", encoding="utf-8") as f:
        policy = json.load(f)
    policy["verbose"] = not args.quiet
    if args.approval_mode:
        policy.setdefault("approval", {})["mode"] = args.approval_mode
    if args.interactive:
        policy.setdefault("approval", {})["interactive"] = True
    policy["_run_at"] = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime())

    ensure_kb_baseline()
    reset_kb_to_baseline()
    kb_before = _kb_counts_from_file(KB_BASE)

    # 列出场景
    tmp_im = WeChatGroupAdapter(WECHAT_FILE)
    all_scenarios = tmp_im.list_scenarios()
    if args.list:
        print("可用场景：")
        for s in all_scenarios:
            msgs = [m for m in tmp_im.fetch_messages(s)]
            rep = msgs[0].sender["name"] if msgs else "-"
            print(f"  {s:<18} 报障人: {rep}")
        return 0

    scenarios = [args.scenario] if args.scenario else all_scenarios

    ctx = build_context(policy)
    leader = ctx["leader"]
    tracer = ctx["tracer"]
    kb = ctx["kb"]

    print(f"{'='*72}\n  ServiceDesk Pilot Demo · 审批模式={policy['approval']['mode']}"
          f" · 场景={len(scenarios)}\n{'='*72}")

    incidents = run_scenarios(leader, scenarios)

    # 指标
    wall = time.time() - tracer.started_at
    metrics = ObservabilityProbe().run(leader.incidents, tracer, wall)

    # 知识库写回统计（运行时内存已更新）
    kb_after = kb.snapshot_counts()
    all_kb_updates: List[Dict[str, Any]] = []
    for inc in incidents:
        all_kb_updates.extend(inc.kb_updates)

    # 阶段矩阵
    stage_matrix = extract_stage_matrix(tracer)

    # 导出 artifact
    os.makedirs(OUT_DIR, exist_ok=True)
    extra = {
        "run_at": policy["_run_at"],
        "approval_mode": policy["approval"]["mode"],
        "interactive": bool(policy["approval"].get("interactive")),
        "scenarios_run": scenarios,
        "incidents": [inc.to_dict() for inc in incidents],
        "metrics": metrics,
        "kb_before": kb_before,
        "kb_after": kb_after,
        "kb_new_items": all_kb_updates,
    }
    trace_path = os.path.join(OUT_DIR, "trace.json")
    tracer.export(trace_path, extra)

    report_path = os.path.join(OUT_DIR, "report.md")
    write_report(report_path, policy, incidents, metrics, kb_before, kb_after)

    dashboard_path = os.path.join(OUT_DIR, "dashboard.html")
    write_dashboard(dashboard_path, policy, incidents, metrics, stage_matrix,
                    kb_before, kb_after, all_kb_updates)

    # 还原知识库基线（保持仓库可复现；diff 已写入 artifact）
    if not args.keep_kb:
        reset_kb_to_baseline()

    # 控制台小结
    print(f"\n{'='*72}\n  运行完成 · 产物：")
    print(f"    trace.json   → {trace_path}")
    print(f"    report.md    → {report_path}")
    print(f"    dashboard.html → {dashboard_path}")
    print(f"  指标：工单 {metrics.get('incidents_total',0)} · "
          f"解决 {metrics.get('first_time_resolved',0)} · "
          f"升级 {metrics.get('escalated',0)} · "
          f"免审批 {metrics.get('approval_skipped',0)} · "
          f"知识写回 {metrics.get('kb_write_backs',0)}")
    print(f"  知识库状态：{'已还原基线' if not args.keep_kb else '保留运行结果'}"
          f"（运行前 {kb_before} → 运行后 {kb_after}）")
    print(f"{'='*72}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
