"""
审批流适配层（Mock）

对应方案第七章『审批分层与风险分类』。
三种模式由 config/policy.json 的 approval.mode 驱动：

  manual      —— 生成审批单，等待审批人决策
                 · interactive=true  → 终端等待真人输入
                 · interactive=false → 用 mock 审批人的脚本化决策（可复现）
  auto_skip   —— 命中 auto_skip_rules 时免审批直接执行（仍留审计记录）
  auto_approve—— 生成审批单并留痕，由策略引擎自动批准

无论哪种模式，never_skip_categories 里的类别永远不允许跳过。
"""

from __future__ import annotations

import json
import sys
from typing import Any, Dict, List, Optional, Tuple

from core.models import ApprovalDecision, ApprovalTicket, RiskLevel, new_id, now_iso


class ApprovalAdapter:
    def __init__(self, approvers_file: str, policy: Dict[str, Any]):
        with open(approvers_file, "r", encoding="utf-8") as f:
            self._data = json.load(f)
        self.approvers = {a["approver_id"]: a for a in self._data["approvers"]}
        self.routing = self._data["routing"]
        self.scripted = self._data["scripted_decisions"]
        self.policy = policy
        self.approval_cfg = policy.get("approval", {})

    # ---------------- 路由 ----------------

    def route(self, risk_level: RiskLevel) -> Dict[str, Any]:
        aid = self.routing.get(risk_level.value) or self.routing.get("_fallback")
        return self.approvers.get(aid, {"approver_id": aid, "name": "未知审批人", "role": "-"})

    # ---------------- 免审批判定 ----------------

    def evaluate_skip(self, ctx: Dict[str, Any]) -> Tuple[bool, str, str]:
        """
        判定当前工单能否免审批。
        返回 (可跳过, 命中规则ID, 说明)
        """
        mode = self.approval_cfg.get("mode", "manual")
        category = ctx.get("category", "")
        never = self.approval_cfg.get("never_skip_categories", [])

        if category in never:
            return False, "", f"类别 {category} 在 never_skip 列表中，强制人工闭环"

        if mode != "auto_skip":
            return False, "", f"当前审批模式为 {mode}，不启用免审批通道"

        risk: RiskLevel = ctx["risk_level"]
        for rule in self.approval_cfg.get("auto_skip_rules", []):
            cond = rule.get("conditions", {})
            rid = rule.get("rule_id", "?")

            max_risk = cond.get("max_risk_level")
            if max_risk and risk.order > RiskLevel(max_risk).order:
                continue
            if category in cond.get("exclude_categories", []):
                continue
            if cond.get("require_identity_verified") and not ctx.get("identity_verified"):
                continue
            if cond.get("require_runbook_hit") and not ctx.get("runbook_hit"):
                continue
            min_sr = cond.get("min_runbook_success_rate")
            if min_sr is not None and ctx.get("runbook_success_rate", 0.0) < min_sr:
                continue
            want_conn = cond.get("require_system_connectivity")
            if want_conn and ctx.get("system_connectivity") != want_conn:
                continue

            return True, rid, rule.get("description", "命中免审批规则")

        return False, "", "未命中任何免审批规则，转人工审批"

    # ---------------- 创建审批单 ----------------

    def create_ticket(self, incident_id: str, risk_level: RiskLevel,
                      actions_summary: List[str], requester: str,
                      reason: str, impact: str, rollback_point: str) -> ApprovalTicket:
        approver = self.route(risk_level)
        return ApprovalTicket(
            approval_id=new_id("APV"),
            incident_id=incident_id,
            risk_level=risk_level,
            actions_summary=actions_summary,
            requester=requester,
            approver_id=approver.get("approver_id", ""),
            approver_name=f"{approver.get('name', '')}({approver.get('role', '')})",
            reason=reason,
            impact=impact,
            rollback_point=rollback_point,
            created_at=now_iso(),
        )

    # ---------------- 决策 ----------------

    def decide(self, ticket: ApprovalTicket, scenario: str) -> ApprovalTicket:
        mode = self.approval_cfg.get("mode", "manual")

        if mode == "auto_approve":
            ticket.decision = ApprovalDecision.APPROVED
            ticket.decided_at = now_iso()
            ticket.decided_by = "policy-engine(auto_approve)"
            ticket.comment = "按 approval.mode=auto_approve 策略自动批准，审批单已留痕可审计"
            ticket.latency_seconds = 0
            return ticket

        if self.approval_cfg.get("interactive"):
            return self._decide_interactive(ticket)

        return self._decide_scripted(ticket, scenario)

    def _decide_scripted(self, ticket: ApprovalTicket, scenario: str) -> ApprovalTicket:
        d = self.scripted.get(scenario) or self.scripted.get("_default", {})
        decision = d.get("decision", "approved")
        ticket.decision = ApprovalDecision(decision)
        ticket.decided_at = now_iso()
        approver = self.approvers.get(d.get("approver_id", ""), {})
        ticket.decided_by = f"{approver.get('name', '审批人')}({approver.get('role', '-')})"
        ticket.comment = d.get("comment", "")
        ticket.latency_seconds = d.get("latency_seconds", 0)
        return ticket

    def _decide_interactive(self, ticket: ApprovalTicket) -> ApprovalTicket:
        print("\n" + "─" * 68)
        print(f"  【待审批】{ticket.approval_id}   风险等级 {ticket.risk_level.value}")
        print(f"  工单     : {ticket.incident_id}")
        print(f"  申请人   : {ticket.requester}")
        print(f"  审批人   : {ticket.approver_name}")
        print(f"  原因     : {ticket.reason}")
        print(f"  影响面   : {ticket.impact}")
        print(f"  回滚点   : {ticket.rollback_point}")
        print("  待批动作 :")
        for a in ticket.actions_summary:
            print(f"      - {a}")
        print("─" * 68)
        try:
            ans = input("  批准执行？[y=批准 / n=驳回 / e=升级] : ").strip().lower()
        except (EOFError, KeyboardInterrupt):
            ans = "n"
            print()

        mapping = {"y": ApprovalDecision.APPROVED, "": ApprovalDecision.APPROVED,
                   "n": ApprovalDecision.REJECTED, "e": ApprovalDecision.ESCALATED}
        ticket.decision = mapping.get(ans, ApprovalDecision.REJECTED)
        ticket.decided_at = now_iso()
        ticket.decided_by = f"{ticket.approver_name}[终端人工输入]"
        ticket.comment = "由真人在终端审批"
        return ticket
