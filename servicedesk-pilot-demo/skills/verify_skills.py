"""
验证类 + 监控类 Skills（方案 10.4 / 10.5）

  Skill 10  RecoveryVerifier     —— 验证账号/权限/访问是否恢复
  Skill 11  Postmortem           —— 生成复盘报告与改进建议
  Skill 12  KnowledgeReflector   —— 把成功/失败/Badcase 真实回流到知识库
  Skill 13  ObservabilityProbe   —— 采集端到端指标

KnowledgeReflector 是『知识沉淀』闭环的落地点：
它会真实修改 mock/knowledge_base.json，运行前后可以 diff 出新增条目。
"""

from __future__ import annotations

from datetime import date
from typing import Any, Dict, List, Optional, Tuple

from core.models import (Incident, PostmortemReport, RiskLevel, TicketStatus,
                         VerificationResult, now_iso)
from skills.knowledge_skills import KnowledgeBase

# --------------------------------------------------------------------------
# Skill 10: RecoveryVerifier
# --------------------------------------------------------------------------

# 每类异常的期望终态（探针校验目标）。system 指明状态落在哪个系统上，
# 与动作模板的 authoritative system 保持一致，避免「动作跑 A 系统、探针查 B 系统」。
_EXPECTED_STATE: Dict[str, Dict[str, Any]] = {
    "account_locked": {"system": "vpn_gateway", "expect": {"status": "active"}},
    "mfa_lost": {"system": "idaas", "expect": {"mfa_bound": False}},  # 解绑成功 + 待用户扫码绑定
    "suspected_compromise": {"system": "idaas", "expect": {"active_sessions": 0}},
    "legacy_account": {"system": "legacy_erp_u8", "expect": {}},      # 无接口，只能人工确认
    "sso_federation_error": {"system": "supplier_portal", "expect": {}},
}


class RecoveryVerifier:
    name = "RecoveryVerifier"

    def __init__(self, mcp):
        self.mcp = mcp

    def run(self, category: str, system_id: str, user_id: str,
            executed_actions: List[Any], user_confirm: Optional[bool] = None
            ) -> VerificationResult:
        cfg = _EXPECTED_STATE.get(category)
        probe_system = cfg["system"] if cfg else system_id
        expect = cfg["expect"] if cfg else {}
        probes: List[Dict[str, Any]] = []
        evidence: List[str] = []

        if expect:
            p = self.mcp.probe(probe_system, user_id, expect)
            probes.append(p)
            evidence.append(f"探针 {probe_system}: {p.get('probe')} → passed={p.get('passed')}")
        else:
            p = self.mcp.probe(probe_system, user_id, {})
            probes.append(p)
            evidence.append(f"探针 {probe_system}: {p.get('message', '无自动探针，依赖人工确认')}")

        done = [a for a in executed_actions if a.status in ("done", "manual_done")]
        failed = [a for a in executed_actions if a.status == "failed"]
        pending = [a for a in executed_actions if a.status in ("planned", "manual_pending")]
        evidence.append(f"动作执行统计: 完成 {len(done)} / 失败 {len(failed)} / 未执行 {len(pending)}")

        probe_pass = all(p.get("passed") is not False for p in probes)
        probe_needs_human = any(p.get("passed") is None for p in probes)

        if failed:
            return VerificationResult(
                conclusion="failed", probes=probes, user_confirmed=bool(user_confirm),
                evidence=evidence,
                reason=f"{len(failed)} 个动作执行失败，触发回滚/升级，不判定为成功")

        if not done:
            return VerificationResult(
                conclusion="not_applicable", probes=probes, user_confirmed=False,
                evidence=evidence,
                reason="没有任何动作被 Agent 执行（等待人工或已升级），暂不做恢复判定")

        if pending:
            return VerificationResult(
                conclusion="partial", probes=probes, user_confirmed=bool(user_confirm),
                evidence=evidence,
                reason=f"仍有 {len(pending)} 个动作待人工执行，仅部分恢复")

        if probe_needs_human:
            confirmed = bool(user_confirm)
            return VerificationResult(
                conclusion="success" if confirmed else "partial",
                probes=probes, user_confirmed=confirmed, evidence=evidence,
                reason=("无接口系统，由用户在 IM 确认恢复" if confirmed
                        else "无接口系统且用户尚未确认，暂判定为部分恢复"))

        if not probe_pass:
            return VerificationResult(
                conclusion="failed", probes=probes, user_confirmed=False, evidence=evidence,
                reason="探针校验未通过，系统状态未达预期终态")

        return VerificationResult(
            conclusion="success", probes=probes,
            user_confirmed=True if user_confirm is None else bool(user_confirm),
            evidence=evidence, reason="探针校验通过且全部动作执行完成")


# --------------------------------------------------------------------------
# Skill 11: Postmortem
# --------------------------------------------------------------------------

class Postmortem:
    name = "Postmortem"

    def run(self, inc: Incident, duration_seconds: int) -> PostmortemReport:
        verification = inc.verification
        conclusion = verification.conclusion if verification else "unknown"

        outcome_map = {
            "success": "resolved", "partial": "partially_resolved",
            "failed": "failed", "not_applicable": "escalated", "unknown": "unknown",
        }
        outcome = outcome_map.get(conclusion, "unknown")

        timeline = [{"at": h["at"], "status": h["status"], "note": h["note"]}
                    for h in inc.status_history]

        what_worked, improvements = [], []
        kg = inc.knowledge.knowledge_gap if inc.knowledge else False

        if inc.knowledge and inc.knowledge.top_runbook:
            rb = inc.knowledge.top_runbook
            what_worked.append(f"命中 Runbook {rb['doc_id']} {rb['version']}（历史成功率 {rb.get('success_rate')}）")
        if inc.approval:
            from core.models import ApprovalDecision
            if inc.approval.decision == ApprovalDecision.SKIPPED:
                what_worked.append(f"命中免审批规则 {inc.approval.skip_rule_id}，节省人工审批等待")
            elif inc.approval.decision == ApprovalDecision.APPROVED:
                what_worked.append(f"审批闭环完成，耗时 {inc.approval.latency_seconds}s")

        auto_done = [a for a in (inc.plan.actions if inc.plan else []) if a.status == "done"]
        if auto_done:
            what_worked.append(f"{len(auto_done)} 个白名单动作自动执行成功，无需人工介入")

        if kg:
            improvements.append("知识库缺少该场景 Runbook，已记为 Badcase 并生成知识补充请求")
        if inc.triage and inc.triage.system_connectivity == "no_api":
            improvements.append("目标系统无开放接口，人工执行占总耗时主要部分；建议评估中间库/RPA 过渡方案")
        if inc.approval and inc.approval.latency_seconds > 60:
            improvements.append(f"审批等待 {inc.approval.latency_seconds}s；建议对双因子已核验场景下放审批权限")
        if verification and verification.conclusion in ("partial", "failed"):
            improvements.append("验证未完全通过，需跟进二次处置并复验")
        if not improvements:
            improvements.append("本次处置链路顺畅，无明显改进项；建议持续观察同类工单占比")

        root_cause = self._infer_root_cause(inc)

        proposals: List[Dict[str, Any]] = []
        if kg:
            proposals.append({
                "type": "badcase",
                "reason": inc.knowledge.gap_reason if inc.knowledge else "知识缺口",
            })
        else:
            proposals.append({"type": "case", "reason": "本次处置可作为同类工单参考案例入库"})

        return PostmortemReport(
            incident_id=inc.incident_id,
            outcome=outcome,
            duration_seconds=duration_seconds,
            timeline=timeline,
            root_cause=root_cause,
            what_worked=what_worked,
            improvements=improvements,
            knowledge_proposals=proposals,
            is_badcase=kg or conclusion in ("failed",),
        )

    @staticmethod
    def _infer_root_cause(inc: Incident) -> str:
        cat = inc.classification.category if inc.classification else "unknown"
        mapping = {
            "account_locked": "用户连续 5 次密码输入错误触发安全策略自动锁定",
            "mfa_lost": "用户更换手机设备但未迁移 MFA 验证器，导致二次验证不可用",
            "suspected_compromise": "存在境外 IP 成功登录记录，判定为疑似凭据泄露，已按安全事件处置",
            "legacy_account": "老 ERP 账号状态异常（疑似被误停用或账套锁定），需人工在服务器端确认",
            "sso_federation_error": "SAML 联邦断言签名校验失败，疑似 IdP 证书指纹变更，超出现有知识范围",
        }
        return mapping.get(cat, "根因未明确，需人工进一步分析")


# --------------------------------------------------------------------------
# Skill 12: KnowledgeReflector —— 真实写回知识库
# --------------------------------------------------------------------------

class KnowledgeReflector:
    name = "KnowledgeReflector"

    def __init__(self, kb: KnowledgeBase, policy: Dict[str, Any]):
        self.kb = kb
        self.cfg = policy.get("knowledge_reflection", {})

    def run(self, inc: Incident, report: PostmortemReport) -> List[Dict[str, Any]]:
        if not self.cfg.get("enabled", True):
            return [{"op": "skipped", "reason": "knowledge_reflection.enabled = false"}]

        updates: List[Dict[str, Any]] = []
        cat = inc.classification.category if inc.classification else "unknown"
        sys_id = inc.triage.target_system if inc.triage else ""
        today = date.today().isoformat()

        # 1) 案例入库（脱敏后）
        case_id = f"CASE-{today.replace('-', '')[:8]}-{inc.incident_id[-4:]}"
        path_map = {
            "auto_execute": "auto_execute", "approval_then_execute": "approval_then_execute",
            "legacy_manual": "legacy_manual", "human_only": "human_only", "escalate": "escalate_human",
        }
        new_case = {
            "case_id": case_id,
            "title": self._case_title(inc),
            "category": cat,
            "keywords": self._keywords(inc),
            "occurred_at": today,
            "resolution_path": path_map.get(
                inc.triage.execution_path.value if inc.triage else "", "unknown"),
            "runbook_used": (inc.knowledge.top_runbook["doc_id"]
                             if inc.knowledge and inc.knowledge.top_runbook else "none"),
            "outcome": report.outcome,
            "duration_seconds": report.duration_seconds,
            "lesson": (report.improvements[0] if report.improvements else "无特别经验"),
            "_source_incident": inc.incident_id,
            "_auto_generated": True,
        }
        self.kb.data.setdefault("cases", []).append(new_case)
        updates.append({"op": "add_case", "id": case_id, "title": new_case["title"]})

        # 2) Badcase 入库（知识缺口 / 处置失败）
        if report.is_badcase and self.cfg.get("always_record_badcase", True):
            bc_id = f"BAD-{today.replace('-', '')[:8]}-{inc.incident_id[-4:]}"
            badcase = {
                "badcase_id": bc_id,
                "incident_id": inc.incident_id,
                "category": cat,
                "system": sys_id,
                "recorded_at": now_iso(),
                "symptom": (inc.event.redacted_text[:160] if inc.event else ""),
                "error_codes": (inc.event.entities.get("error_codes", []) if inc.event else []),
                "gap_reason": (inc.knowledge.gap_reason if inc.knowledge else ""),
                "handling": report.outcome,
                "next_action": "补充该场景 Runbook；纳入评测集用于回归验证",
                "_auto_generated": True,
            }
            self.kb.data.setdefault("badcases", []).append(badcase)
            updates.append({"op": "add_badcase", "id": bc_id, "reason": badcase["gap_reason"][:60]})

        # 3) 知识缺口 → 生成 Runbook 草稿（draft 状态，不直接生效）
        if inc.knowledge and inc.knowledge.knowledge_gap:
            draft_id = f"RB-DRAFT-{today.replace('-', '')[:8]}-{inc.incident_id[-4:]}"
            draft = {
                "doc_id": draft_id,
                "title": f"[待完善] {self._case_title(inc)} 处置流程",
                "category": cat,
                "keywords": self._keywords(inc),
                "version": "v0.1-draft",
                "effective_from": today,
                "expires_at": "9999-12-31",
                "permission_tag": "internal",
                "applicable_systems": [sys_id] if sys_id else [],
                "risk_level": (inc.triage.risk_level.value if inc.triage else "L2"),
                "success_rate": 0.0,
                "status": "draft_pending_review",
                "steps": [
                    "【自动生成草稿，需专家补全】",
                    f"症状：{inc.event.redacted_text[:120] if inc.event else ''}",
                    f"已尝试：{', '.join(a.name for a in inc.plan.actions) if inc.plan else '无'}",
                    "待补充：确认根因、标准处置步骤、验证方式、回滚方案",
                ],
                "verification": "待补充",
                "rollback": "待补充",
                "_source_incident": inc.incident_id,
                "_auto_generated": True,
            }
            self.kb.data.setdefault("runbooks", []).append(draft)
            updates.append({"op": "add_runbook_draft", "id": draft_id, "title": draft["title"]})

        # 4) Legacy 人工执行经验回填
        if inc.triage and inc.triage.system_connectivity == "no_api":
            notes = self.kb.data.setdefault("legacy_operator_notes", [])
            target = next((n for n in notes if n.get("system") == sys_id), None)
            lesson = f"[{today} 工单 {inc.incident_id}] {report.root_cause}"
            if target is not None:
                target.setdefault("field_experience", []).append(lesson)
                updates.append({"op": "append_legacy_experience", "id": target["note_id"],
                                "lesson": lesson[:70]})
            else:
                new_note = {
                    "note_id": f"LEG-NOTE-{len(notes) + 1:03d}",
                    "system": sys_id, "operation": "账号异常处置", "has_api": False,
                    "operator_role": "系统管理员",
                    "evidence_required": ["操作前截图", "操作后截图"],
                    "cautions": [], "field_experience": [lesson], "_auto_generated": True,
                }
                notes.append(new_note)
                updates.append({"op": "add_legacy_note", "id": new_note["note_id"], "system": sys_id})

        # 5) 真实落盘
        if self.cfg.get("write_back_to_kb", True):
            self.kb.save()
            updates.append({"op": "persist", "file": self.kb.path})
        else:
            updates.append({"op": "dry_run", "reason": "write_back_to_kb = false，仅生成不落盘"})

        return updates

    @staticmethod
    def _case_title(inc: Incident) -> str:
        rep = inc.event.reporter if inc.event else {}
        dept = rep.get("department", "某部门")
        name = (rep.get("name", "某员工") or "")[:1] + "某"
        cat_cn = {
            "account_locked": "账号锁定", "mfa_lost": "MFA 验证器丢失",
            "suspected_compromise": "疑似账号失陷", "legacy_account": "老系统账号异常",
            "sso_federation_error": "SSO 联邦登录失败", "unknown": "未分类账户异常",
        }.get(inc.classification.category if inc.classification else "unknown", "账户异常")
        return f"{dept}{name} {cat_cn}"

    @staticmethod
    def _keywords(inc: Incident) -> List[str]:
        kws: List[str] = []
        if inc.event:
            kws += inc.event.entities.get("systems", [])
            kws += inc.event.entities.get("error_codes", [])
            kws += inc.event.entities.get("symptoms", [])
        if inc.classification:
            kws.append(inc.classification.category)
        seen, out = set(), []
        for k in kws:
            if k and k not in seen:
                seen.add(k)
                out.append(k)
        return out[:10]


# --------------------------------------------------------------------------
# Skill 13: ObservabilityProbe
# --------------------------------------------------------------------------

class ObservabilityProbe:
    name = "ObservabilityProbe"

    def run(self, incidents: List[Incident], tracer, wall_seconds: float) -> Dict[str, Any]:
        total = len(incidents)
        if total == 0:
            return {}

        resolved = sum(1 for i in incidents
                       if i.verification and i.verification.conclusion == "success")
        partial = sum(1 for i in incidents
                      if i.verification and i.verification.conclusion == "partial")
        escalated = sum(1 for i in incidents if i.status == TicketStatus.ESCALATED)
        badcases = sum(1 for i in incidents if i.postmortem and i.postmortem.is_badcase)

        approvals = [i.approval for i in incidents if i.approval]
        from core.models import ApprovalDecision
        skipped = sum(1 for a in approvals if a.decision == ApprovalDecision.SKIPPED)
        approved = sum(1 for a in approvals if a.decision == ApprovalDecision.APPROVED)
        apv_latency = [a.latency_seconds for a in approvals if a.latency_seconds]

        tool_events = [e for e in tracer.events if e.kind == "tool"]
        tool_ok = sum(1 for e in tool_events if e.status == "ok")
        rag_events = [e for e in tracer.events if e.kind == "rag"]
        rag_hit = sum(1 for e in rag_events if e.status == "ok")

        auto_actions = sum(
            1 for i in incidents if i.plan for a in i.plan.actions if a.status == "done")
        manual_actions = sum(
            1 for i in incidents if i.plan for a in i.plan.actions
            if a.status in ("manual_pending", "manual_done"))

        kb_updates = sum(len(i.kb_updates) for i in incidents)

        return {
            "incidents_total": total,
            "first_time_resolved": resolved,
            "partially_resolved": partial,
            "escalated": escalated,
            "resolution_rate": round(resolved / total, 3),
            "escalation_rate": round(escalated / total, 3),
            "badcase_rate": round(badcases / total, 3),
            "approval_total": len(approvals),
            "approval_skipped": skipped,
            "approval_approved": approved,
            "approval_avg_latency_s": (round(sum(apv_latency) / len(apv_latency), 1)
                                       if apv_latency else 0),
            "mcp_tool_calls": len(tool_events),
            "mcp_tool_success_rate": round(tool_ok / len(tool_events), 3) if tool_events else 0,
            "rag_queries": len(rag_events),
            "rag_hit_rate": round(rag_hit / len(rag_events), 3) if rag_events else 0,
            "actions_auto_executed": auto_actions,
            "actions_manual": manual_actions,
            "kb_write_backs": kb_updates,
            "e2e_wall_seconds": round(wall_seconds, 3),
            "trace_events": len(tracer.events),
        }
