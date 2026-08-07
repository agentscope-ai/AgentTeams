"""
Agent 1: TeamLeader —— 任务总控与编排

对应方案 8.1『核心协作流程』，串联端到端九个阶段：

  S1 微信群消息接入      → IM Adapter
  S2 事件受理与解析      → Ticket Intake Agent（含去重）
  S3 知识检索            → Knowledge Agent
  S4 风险分诊            → Triage Analyst Agent
  S5 方案生成            → Resolution Agent
  S6 审批决策            → 配置驱动：人工审批 / 自动跳过 / 自动批准
  S7 执行与标记完成      → Resolution Agent（自动执行 / Legacy 人工回填）
  S8 恢复验证            → Verify Agent
  S9 复盘与知识沉淀      → Verify Agent（Postmortem + KnowledgeReflector）

TeamLeader 的 DecisionBoundary：可自主拆解编排，但不代替审批人批准，
也不直接调用业务工具 —— 所有工具调用都由职能 Agent 发起。
"""

from __future__ import annotations

import time
from typing import Any, Dict, List, Optional

from core.models import (ApprovalDecision, ExecutionPath, Incident, RiskLevel,
                         TicketStatus, new_id, now_iso)
from core.trace import C
from agents.service_agents import (KnowledgeAgent, ResolutionAgent,
                                   TicketIntakeAgent, TriageAnalystAgent, VerifyAgent)

# Legacy 人工执行的 mock 回填结果（模拟管理员照清单操作后回填到工单）
_MOCK_MANUAL_FILLBACK = {
    "legacy_erp_u8": {
        "operator": "孙工(工号 CW0031)",
        "operated_at": "2026-08-07T09:52:18+08:00",
        "result": "success",
        "root_cause": "上月离职批量处理时工号前缀 CW/GS 匹配错误，该在职员工账号被误设为『停用』",
        "screenshots": ["img_u8_before_7781", "img_u8_after_7782"],
        "note": "已在系统管理→用户管理中重新勾选启用并保存，用户已可正常登录并进入 2026 账套",
    }
}

# 用户在群里的确认回复（mock，模拟 Verify 阶段的用户确认）
_MOCK_USER_CONFIRM = {
    "S1_vpn_locked": True,
    "S2_mfa_lost": True,
    "S3_legacy_erp": True,
    "S4_suspected_compromise": True,
    "S5_knowledge_gap": None,
}


class TeamLeader:
    name = "TeamLeader"
    role = "服务台任务总控与协调者"

    def __init__(self, tracer, policy: Dict[str, Any], im, mcp, kb, approval_adapter):
        self.tracer = tracer
        self.policy = policy
        self.im = im
        self.mcp = mcp
        self.kb = kb
        self.approval = approval_adapter

        self.intake = TicketIntakeAgent(tracer, policy)
        self.knowledge = KnowledgeAgent(tracer, kb, mcp)
        self.triage = TriageAnalystAgent(tracer, policy, mcp)
        self.resolution = ResolutionAgent(tracer, policy, mcp, approval_adapter)
        self.verify = VerifyAgent(tracer, mcp, kb, policy)

        self.incidents: List[Incident] = []

    # ======================================================================
    # 主流程
    # ======================================================================

    def handle_scenario(self, scenario: str) -> Incident:
        t_start = time.time()
        messages = self.im.fetch_messages(scenario)

        inc = Incident(incident_id=new_id("INC"), scenario=scenario)
        self.tracer.current_incident = inc.incident_id
        inc.raw_messages = messages

        reporter = messages[0].sender
        self.tracer.banner(
            f"工单 {inc.incident_id}   场景 {scenario}",
            f"报障人 {reporter['name']}({reporter['department']}·{reporter['title']})  "
            f"首条消息 {messages[0].timestamp}",
        )

        # ---------------- S1 微信群消息接入 ----------------
        self.tracer.stage("S1", "微信群消息接入", f"IM Adapter 从『{self.im.channel['chat_name']}』拉取消息")
        self.tracer.agent(self.name, f"接收 {len(messages)} 条群消息，创建工单 {inc.incident_id}")
        self.tracer.note(f"渠道 {self.im.channel['type']} | 群 {self.im.channel['chat_id']} "
                         f"| 群成员 {self.im.channel['member_count']} 人")
        inc.set_status(TicketStatus.RECEIVED, f"从群 {self.im.channel['chat_id']} 接收")
        self._reply(inc, f"@{reporter['name']} 收到，已创建工单 {inc.incident_id}，正在分析…")
        self.tracer.stage_done(f"工单 {inc.incident_id} 已创建，状态 = {inc.status.value}")

        # ---------------- S2 事件受理与解析 ----------------
        self.tracer.stage("S2", "事件受理与解析", "Ticket Intake Agent 标准化 + 去重 + 分类")
        merged_into, why = self.intake.run(inc, messages, self.incidents)
        if merged_into is not None:
            self._reply(inc, f"@{reporter['name']} 你反馈的问题与工单 {merged_into.incident_id} "
                             f"为同一故障，已合并处理，进度会在群里同步")
            self.tracer.stage_done(f"重复报障，已合并到 {merged_into.incident_id}", status="skipped")
            self.incidents.append(inc)
            self._finalize(inc, t_start, merged=True)
            return inc

        cls = inc.classification
        self.tracer.stage_done(
            f"标准化完成，分类 = {cls.category} (置信度 {cls.confidence})，状态 = {inc.status.value}")
        self._reply(inc, f"@{reporter['name']} 已识别为「{self._cn(cls.category)}」，正在检索处置方案…")

        # ---------------- S3 知识检索 ----------------
        self.tracer.stage("S3", "知识检索", "Knowledge Agent 检索 Runbook / 历史案例 / 老系统人工经验")
        kn = self.knowledge.run(inc)
        if kn.knowledge_gap:
            self.tracer.stage_done(f"知识缺口：{kn.gap_reason[:60]}…", status="skipped")
        else:
            self.tracer.stage_done(
                f"命中 {len(kn.runbooks)} 条 Runbook / {len(kn.cases)} 条案例，"
                f"引用 {len(kn.citations)} 项")

        # ---------------- S4 风险分诊 ----------------
        self.tracer.stage("S4", "风险分诊", "Triage Analyst 判定风险等级与执行路径")
        tri = self.triage.run(inc)
        self.tracer.stage_done(f"风险 {tri.risk_level.value} → 执行路径 {tri.execution_path.value}")

        # 强制升级分支（知识缺口 / 低置信度）
        if tri.execution_path == ExecutionPath.ESCALATE:
            return self._handle_escalation(inc, t_start, reporter)

        # ---------------- S5 方案生成 ----------------
        self.tracer.stage("S5", "方案生成", "Resolution Agent 生成分级处置方案")
        plan, err = self.resolution.plan(inc)
        if plan is None:
            self.tracer.stage_done(f"无法生成安全方案：{err}", status="fail")
            return self._handle_escalation(inc, t_start, reporter, reason=err)

        if tri.execution_path == ExecutionPath.LEGACY_MANUAL:
            checklist, cerr = self.resolution.build_manual_checklist(inc)
            if cerr:
                self.tracer.stage_done(f"无人工经验：{cerr}", status="fail")
                return self._handle_escalation(inc, t_start, reporter, reason=cerr)

        self.tracer.stage_done(
            f"方案 {plan.plan_id} 就绪，{len(plan.actions)} 个动作，整体风险 {plan.overall_risk.value}")

        # L3 强制人工：方案已生成，但 Agent 不自动执行，转人工执行与审批
        if tri.execution_path == ExecutionPath.HUMAN_ONLY:
            self.tracer.stage("S6", "审批决策", "L3 强制人工：跳过自动审批，转人工闭环")
            self.tracer.stage_done("L3 高风险动作由人工执行，Agent 仅交付方案与上下文", status="skipped")
            return self._handle_escalation(
                inc, t_start, reporter,
                reason="L3 强制人工：Agent 已生成处置方案，转人工执行与审批")

        # ---------------- S6 审批决策 ----------------
        approved = self._stage_approval(inc, reporter)

        if inc.approval and inc.approval.decision == ApprovalDecision.REJECTED:
            self._reply(inc, f"@{reporter['name']} 审批未通过：{inc.approval.comment}，已转人工跟进")
            return self._handle_escalation(inc, t_start, reporter, reason="审批驳回")
        if inc.approval and inc.approval.decision == ApprovalDecision.ESCALATED:
            self._reply(inc, f"@{reporter['name']} 审批人已升级处理：{inc.approval.comment}")
            return self._handle_escalation(inc, t_start, reporter, reason="审批人主动升级")

        # ---------------- S7 执行与标记完成 ----------------
        self.tracer.stage("S7", "执行与标记完成", "白名单自动执行 / Legacy 人工执行回填")
        self.resolution.execute(inc, approved=approved)

        if inc.status == TicketStatus.AWAITING_MANUAL:
            fb = _MOCK_MANUAL_FILLBACK.get(tri.target_system)
            if fb:
                self.tracer.note("等待人工执行…（Mock：管理员照清单操作后回填）", level="warn")
                self._reply(inc, f"@{reporter['name']} 该系统无开放接口，已指派"
                                 f"{self.mcp.systems.get(tri.target_system, {}).get('manual_operator', '管理员')}"
                                 f"人工处理，操作清单与留证要求已下发")
                self.resolution.apply_manual_fillback(inc, fb)
            else:
                self.tracer.note("无 mock 回填数据，保持等待人工状态", level="warn")

        done = [a for a in inc.plan.actions if a.status in ("done", "manual_done")]
        self.tracer.stage_done(f"执行完成 {len(done)}/{len(inc.plan.actions)} 个动作，"
                               f"状态 = {inc.status.value}")

        # ---------------- S8 恢复验证 ----------------
        self.tracer.stage("S8", "恢复验证", "Verify Agent 探针校验 + 用户 IM 确认")
        user_confirm = _MOCK_USER_CONFIRM.get(scenario)
        if user_confirm is not None:
            self.tracer.im("inbound", reporter["name"],
                           "可以了，刚试了一下能正常登录了，谢谢！" if user_confirm
                           else "还是不行，跟之前一样的报错")
        res = self.verify.verify(inc, user_confirm=user_confirm)
        self.tracer.stage_done(f"验证结论 = {res.conclusion}",
                               status="ok" if res.conclusion == "success"
                               else "skipped" if res.conclusion == "partial" else "fail")

        self._reply(inc, self._result_reply(inc, reporter))

        # ---------------- S9 复盘与知识沉淀 ----------------
        return self._finalize(inc, t_start)

    # ======================================================================
    # S6 审批决策
    # ======================================================================

    def _stage_approval(self, inc: Incident, reporter: Dict[str, Any]) -> bool:
        tri, kn, plan = inc.triage, inc.knowledge, inc.plan
        mode = self.policy.get("approval", {}).get("mode", "manual")
        self.tracer.stage("S6", "审批决策", f"当前 approval.mode = {mode}")
        self.tracer.agent(self.name, "判定是否需要人工审批")

        need = [a for a in plan.actions if a.requires_approval]
        if not need:
            self.tracer.approval("方案内全部动作均在自动执行白名单内，无需审批", "ok")
            inc.set_status(TicketStatus.APPROVAL_SKIPPED, "无需审批的低风险方案")
            self.tracer.stage_done("免审批（L0/L1 白名单动作）", status="ok")
            return True

        # 免审批规则判定
        rb = kn.top_runbook
        ctx = {
            "category": inc.classification.category,
            "risk_level": tri.risk_level,
            "identity_verified": tri.identity_verified,
            "runbook_hit": bool(kn.runbooks) and not kn.knowledge_gap,
            "runbook_success_rate": (rb.get("success_rate", 0.0) if rb else 0.0),
            "system_connectivity": tri.system_connectivity,
        }
        can_skip, rule_id, why = self.approval.evaluate_skip(ctx)
        self.tracer.note(f"免审批判定: {why}")

        ticket = self.approval.create_ticket(
            incident_id=inc.incident_id,
            risk_level=tri.risk_level,
            actions_summary=[f"[{a.risk_level.value}] {a.name} — {a.description}" for a in need],
            requester=f"{reporter['name']}({reporter['department']}) via {inc.event.channel_type}",
            reason=(f"{self._cn(inc.classification.category)}；"
                    f"引用 {plan.runbook_ref}；身份因子 {inc.event.identity_factors}"),
            impact=(f"影响 {tri.impact_scope['affected_users']} 人 / "
                    f"系统 {tri.impact_scope['affected_systems']} / "
                    f"业务影响 {tri.impact_scope['business_impact']}"),
            rollback_point=plan.rollback_point,
        )
        inc.approval = ticket

        if can_skip:
            ticket.decision = ApprovalDecision.SKIPPED
            ticket.skip_rule_id = rule_id
            ticket.decided_at = now_iso()
            ticket.decided_by = f"policy-engine({rule_id})"
            ticket.comment = why
            self.tracer.approval(
                f"{ticket.approval_id} 命中免审批规则 {rule_id} → 跳过人工审批（审批单已留痕可审计）",
                "skipped", detail={"rule": rule_id, "why": why})
            inc.set_status(TicketStatus.APPROVAL_SKIPPED, f"命中 {rule_id}")
            self.tracer.stage_done(f"自动跳过审批（规则 {rule_id}）", status="skipped")
            return True

        # 走审批
        self.tracer.approval(
            f"{ticket.approval_id} 生成审批单 → 审批人 {ticket.approver_name}，"
            f"{len(need)} 个待批动作", "pending",
            detail={"actions": ticket.actions_summary, "impact": ticket.impact})
        for s in ticket.actions_summary:
            self.tracer.note(f"待批动作: {s}")
        self._reply(inc, f"@{reporter['name']} 该操作需 {ticket.approver_name} 审批，"
                         f"审批单 {ticket.approval_id} 已发出")
        inc.set_status(TicketStatus.AWAITING_APPROVAL, ticket.approval_id)

        self.approval.decide(ticket, inc.scenario)

        if ticket.decision == ApprovalDecision.APPROVED:
            self.tracer.approval(
                f"{ticket.approval_id} 已批准 · {ticket.decided_by} · 等待 {ticket.latency_seconds}s",
                "ok", detail={"comment": ticket.comment})
            self.tracer.note(f"审批意见: {ticket.comment}")
            inc.set_status(TicketStatus.APPROVED, ticket.decided_by)
            self.tracer.stage_done(f"审批通过（等待 {ticket.latency_seconds}s）")
            return True

        if ticket.decision == ApprovalDecision.REJECTED:
            self.tracer.approval(f"{ticket.approval_id} 已驳回 · {ticket.decided_by}", "fail",
                                 detail={"comment": ticket.comment})
            inc.set_status(TicketStatus.REJECTED, ticket.comment)
            self.tracer.stage_done("审批驳回", status="fail")
            return False

        self.tracer.approval(f"{ticket.approval_id} 审批人升级处理 · {ticket.decided_by}", "skipped",
                             detail={"comment": ticket.comment})
        self.tracer.note(f"审批意见: {ticket.comment}")
        self.tracer.stage_done("审批人主动升级", status="skipped")
        return False

    # ======================================================================
    # 升级分支
    # ======================================================================

    def _handle_escalation(self, inc: Incident, t_start: float,
                           reporter: Dict[str, Any], reason: str = "") -> Incident:
        if not reason:
            if inc.knowledge and inc.knowledge.knowledge_gap:
                reason = f"知识缺口：{inc.knowledge.gap_reason}"
            elif inc.classification and inc.classification.needs_human_review:
                reason = f"分类置信度 {inc.classification.confidence} 低于阈值"
            else:
                reason = "命中强制升级规则"

        self.tracer.stage("S7", "升级人工（跳过自动执行）", "EscalationRouter 生成上下文交接单")
        esc = self.resolution.escalate(inc, reason)
        self._reply(inc, f"@{reporter['name']} 这个问题超出当前自动处置范围，"
                         f"已升级给 {esc['suggested_owner']}（升级单 {esc['escalation_id']}），"
                         f"完整上下文已交接，无需重复描述")
        self.tracer.stage_done(f"已升级 {esc['escalation_id']}", status="skipped")

        self.tracer.stage("S8", "恢复验证", "升级路径：Agent 未执行变更，不做恢复判定")
        self.verify.verify(inc, user_confirm=None)
        self.tracer.stage_done("未执行变更 → 验证结论 not_applicable", status="skipped")

        return self._finalize(inc, t_start)

    # ======================================================================
    # S9 收尾：复盘 + 知识沉淀
    # ======================================================================

    def _finalize(self, inc: Incident, t_start: float, merged: bool = False) -> Incident:
        if merged:
            inc.set_status(TicketStatus.CLOSED, "重复工单，随主工单关闭")
            inc.closed_at = now_iso()
            return inc

        duration = self._business_duration(inc, t_start)
        self.tracer.stage("S9", "复盘与知识沉淀", "Postmortem + KnowledgeReflector 写回知识库")
        self.verify.reflect(inc, duration)

        # 升级 / 强制人工的工单不关闭，保持 ESCALATED 等待人工闭环
        if inc.status == TicketStatus.ESCALATED:
            inc.closed_at = now_iso()
            self.tracer.stage_done(f"已升级人工，知识库写入 {len(inc.kb_updates)} 项变更（工单保持升级态）")
        else:
            inc.set_status(TicketStatus.CLOSED, "工单关闭")
            inc.closed_at = now_iso()
            self.tracer.stage_done(f"工单关闭，知识库写入 {len(inc.kb_updates)} 项变更")

        if inc not in self.incidents:
            self.incidents.append(inc)
        self._print_summary(inc)
        return inc

    @staticmethod
    def _business_duration(inc: Incident, t_start: float) -> int:
        """业务耗时 = 审批等待 + 处置耗时（mock 场景下用审批时长 + 基线估算）"""
        base = 90
        if inc.approval:
            base += inc.approval.latency_seconds
        if inc.triage and inc.triage.system_connectivity == "no_api":
            base += 900   # 人工执行耗时
        return base

    # ======================================================================
    # 输出
    # ======================================================================

    def _reply(self, inc: Incident, text: str) -> None:
        rec = self.im.reply(inc.incident_id, text)
        inc.im_replies.append(rec)
        self.tracer.im("outbound", "ServiceDesk Pilot", text)

    def _result_reply(self, inc: Incident, reporter: Dict[str, Any]) -> str:
        v = inc.verification
        name = reporter["name"]
        if v.conclusion == "success":
            return (f"@{name} 已处理完成并验证通过 ✅ 工单 {inc.incident_id} 关闭。"
                    f"引用方案 {inc.plan.runbook_ref}，如再次出现请直接在群里回复本工单号")
        if v.conclusion == "partial":
            return (f"@{name} 部分处理完成 ⚠ {v.reason}，剩余动作已转人工跟进，"
                    f"工单 {inc.incident_id} 保持跟踪")
        if v.conclusion == "failed":
            return f"@{name} 处置未达预期 ❌ {v.reason}，已触发回滚并升级人工"
        return f"@{name} 工单 {inc.incident_id} 已记录，等待人工进一步处理"

    def _print_summary(self, inc: Incident) -> None:
        if not self.tracer.verbose:
            return
        v, tri, apv, pm = inc.verification, inc.triage, inc.approval, inc.postmortem
        icon = {"success": f"{C.GREEN}✅ 已解决{C.RESET}",
                "partial": f"{C.YELLOW}⚠ 部分解决{C.RESET}",
                "failed": f"{C.RED}❌ 失败{C.RESET}",
                "not_applicable": f"{C.YELLOW}↗ 已升级人工{C.RESET}"}.get(
            v.conclusion if v else "", "-")

        apv_txt = "无需审批"
        if apv:
            apv_txt = {
                ApprovalDecision.SKIPPED: f"自动跳过（规则 {apv.skip_rule_id}）",
                ApprovalDecision.APPROVED: f"人工批准 · {apv.decided_by} · 等待 {apv.latency_seconds}s",
                ApprovalDecision.REJECTED: f"驳回 · {apv.decided_by}",
                ApprovalDecision.ESCALATED: f"审批人升级 · {apv.decided_by}",
            }.get(apv.decision, apv.decision.value)

        acts = inc.plan.actions if inc.plan else []
        auto = sum(1 for a in acts if a.status == "done")
        manual = sum(1 for a in acts if a.status == "manual_done")
        skipped = sum(1 for a in acts if a.status in ("skipped", "manual_pending"))

        print(f"\n{C.BOLD}  ┌─ 工单小结 {inc.incident_id} ─────────────────────────────{C.RESET}")
        print(f"  │ 结果      : {icon}   状态 = {inc.status.value}")
        print(f"  │ 分类/风险 : {inc.classification.category} / {tri.risk_level.value} "
              f"({tri.execution_path.value})")
        print(f"  │ 审批      : {apv_txt}")
        print(f"  │ 动作      : 自动执行 {auto} · 人工执行 {manual} · 跳过/待人工 {skipped}")
        print(f"  │ 知识引用  : {', '.join(inc.knowledge.citations[:4]) or '无'}")
        print(f"  │ 知识沉淀  : {len([u for u in inc.kb_updates if u['op'].startswith('add')])} 条新增"
              f"{'  (含 Badcase)' if pm and pm.is_badcase else ''}")
        print(f"  │ 群内同步  : {len(inc.im_replies)} 条进度消息")
        print(f"{C.BOLD}  └────────────────────────────────────────────────────{C.RESET}")

    @staticmethod
    def _cn(category: str) -> str:
        return {
            "account_locked": "账号锁定", "mfa_lost": "MFA 验证器丢失",
            "suspected_compromise": "疑似账号失陷", "legacy_account": "老系统账号异常",
            "sso_federation_error": "SSO 联邦登录失败", "access_denied": "访问被拒绝",
            "unknown": "未分类账户异常",
        }.get(category, category)
