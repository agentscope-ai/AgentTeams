"""
6 个 Agent 实现（方案第十一章 Agent Identity 清单）

  TeamLeader     —— 任务总控与编排（见 orchestrator.py）
  TicketIntake   —— IM 消息 → 标准化事件
  Knowledge      —— 专家经验检索
  TriageAnalyst  —— 影响面与风险判定
  Resolution     —— 方案生成与执行
  Verify         —— 结果验证与知识回流

每个 Agent 严格遵守方案定义的 DecisionBoundary：
  · Knowledge 不执行处置
  · Triage 不执行变更
  · Resolution 的 L2 必须等审批、L3 不得执行
  · Verify 验证不通过不得判定成功
"""

from __future__ import annotations

import time
from typing import Any, Dict, List, Optional, Tuple

from core.models import (Action, ApprovalDecision, Classification, ExecutionPath,
                         Incident, KnowledgeResult, NormalizedEvent, RawMessage,
                         ResolutionPlan, RiskLevel, TicketStatus,
                         VerificationResult, new_id, now_iso)
from skills.classify_skills import AccountAnomalyClassifier, ImTicketNormalizer
from skills.knowledge_skills import CaseRetrieval, LegacySystemOperatorRag, RunbookRag
from skills.resolution_skills import (EscalationRouter, LegacySystemOperator,
                                      RiskEngine, SafeResolver)
from skills.verify_skills import (KnowledgeReflector, Postmortem, RecoveryVerifier)


class BaseAgent:
    name = "BaseAgent"
    role = ""

    def __init__(self, tracer):
        self.tracer = tracer

    def _t(self) -> float:
        return time.perf_counter()

    def _ms(self, t0: float) -> int:
        return int((time.perf_counter() - t0) * 1000)


# --------------------------------------------------------------------------
# Agent 2: Ticket Intake
# --------------------------------------------------------------------------

class TicketIntakeAgent(BaseAgent):
    name = "Ticket Intake"
    role = "IM 消息 → 标准化服务台事件（只做标准化与紧急度初判，不决定处置方案）"

    def __init__(self, tracer, policy: Dict[str, Any]):
        super().__init__(tracer)
        self.normalizer = ImTicketNormalizer()
        self.classifier = AccountAnomalyClassifier()
        self.policy = policy

    def run(self, inc: Incident, messages: List[RawMessage],
            existing: List[Incident]) -> Tuple[Optional[Incident], str]:
        """返回 (被合并到的主工单 或 None, 说明)。None 表示这是独立新工单。"""
        self.tracer.agent(self.name, f"受理 {len(messages)} 条群消息")

        for m in messages:
            self.tracer.im("inbound", f"{m.sender['name']}({m.sender['department']})", m.content)
            if m.attachments:
                for a in m.attachments:
                    if a.get("ocr_text"):
                        self.tracer.note(f"截图 OCR: {a['ocr_text']}")

        # Skill 1: ImTicketNormalizer
        t0 = self._t()
        event = self.normalizer.run(messages)
        inc.event = event
        self.tracer.skill(
            self.normalizer.name,
            f"→ {event.event_id} | 系统={event.entities['systems'] or '未识别'} "
            f"| 紧急度={event.urgency} | 身份因子={len(event.identity_factors)}",
            duration_ms=self._ms(t0),
            detail={"preprocess_reason": event.preprocess_reason,
                    "redacted": event.redacted_text[:120],
                    "missing_fields": event.missing_fields},
        )
        self.tracer.note(f"预处理原因: {event.preprocess_reason}")
        if event.redacted_text != event.raw_text:
            self.tracer.note("已执行脱敏，仅脱敏文本可进入知识库")

        inc.set_status(TicketStatus.NORMALIZED, f"标准化为 {event.event_id}")

        # 去重（方案 dedup）
        dedup_cfg = self.policy.get("dedup", {})
        if dedup_cfg.get("enabled", True):
            window = dedup_cfg.get("window_seconds", 900)
            for other in existing:
                if not other.event or other.incident_id == inc.incident_id:
                    continue
                dup, why = ImTicketNormalizer.is_duplicate(other.event, event, window)
                if dup:
                    self.tracer.skill(self.normalizer.name,
                                      f"判定为重复报障 → 合并到 {other.incident_id}",
                                      status="skipped", detail={"reason": why})
                    self.tracer.note(f"去重依据: {why}", level="warn")
                    inc.set_status(TicketStatus.DEDUPED, f"合并到 {other.incident_id}")
                    other.merged_msg_ids.extend([m.msg_id for m in messages])
                    return other, why

        # Skill 2: AccountAnomalyClassifier
        t0 = self._t()
        cls = self.classifier.run(event)
        inc.classification = cls
        status = "ok" if not cls.needs_human_review else "fail"
        self.tracer.skill(
            self.classifier.name,
            f"→ {cls.category} (置信度 {cls.confidence})"
            + ("  ⚠ 低于阈值需人工复核" if cls.needs_human_review else ""),
            status=status, duration_ms=self._ms(t0),
            detail={"candidates": cls.candidates, "evidence": cls.evidence_fields},
        )
        inc.set_status(TicketStatus.CLASSIFIED, f"分类 {cls.category}")
        return None, ""


# --------------------------------------------------------------------------
# Agent 3: Knowledge
# --------------------------------------------------------------------------

class KnowledgeAgent(BaseAgent):
    name = "Knowledge"
    role = "专家经验检索者（只输出检索结果与建议，不直接执行处置）"

    def __init__(self, tracer, kb, mcp):
        super().__init__(tracer)
        self.runbook_rag = RunbookRag(kb)
        self.case_retrieval = CaseRetrieval(kb)
        self.legacy_rag = LegacySystemOperatorRag(kb)
        self.mcp = mcp

    def run(self, inc: Incident) -> KnowledgeResult:
        event, cls = inc.event, inc.classification
        self.tracer.agent(self.name, f"检索 {cls.category} 相关专家经验")

        query = " ".join(filter(None, [
            event.redacted_text[:120],
            " ".join(event.entities.get("systems", [])),
            " ".join(event.entities.get("error_codes", [])),
            cls.category,
        ]))

        # Skill 3: RunbookRag
        t0 = self._t()
        runbooks, gap, gap_reason = self.runbook_rag.run(query, cls.category)
        citations = [f"{r['doc_id']}@{r['version']}" for r in runbooks]
        self.tracer.rag(f"{cls.category} runbook", len(runbooks), citations, gap=gap)
        self.tracer.skill(
            self.runbook_rag.name,
            (f"Top-{len(runbooks)} 命中，最高分 {runbooks[0]['_score']}" if runbooks
             else "无有效召回 → 知识缺口"),
            status="ok" if not gap else "fail", duration_ms=self._ms(t0),
            detail={"query": query[:100], "gap_reason": gap_reason,
                    "hits": [{"doc_id": r["doc_id"], "score": r["_score"],
                              "expired": r["_expired"]} for r in runbooks]},
        )
        for r in runbooks:
            flag = "  ⚠ 已过期，仅供参考不可直接执行" if r["_expired"] else ""
            self.tracer.note(f"{r['doc_id']} {r['version']} 《{r['title']}》 "
                             f"score={r['_score']} 成功率={r.get('success_rate')}{flag}")

        # Skill 4: CaseRetrieval
        t0 = self._t()
        cases = self.case_retrieval.run(query, cls.category)
        self.tracer.skill(
            self.case_retrieval.name,
            (f"命中 {len(cases)} 条历史案例" if cases else "无相似历史案例（不编造经验）"),
            status="ok" if cases else "skipped", duration_ms=self._ms(t0),
            detail={"cases": [{"case_id": c["case_id"], "outcome": c["outcome"],
                               "score": c["_score"]} for c in cases]},
        )
        for c in cases:
            self.tracer.note(f"{c['case_id']} 《{c['title']}》 → {c['outcome']}；经验: {c['lesson'][:60]}…")
        citations += [c["case_id"] for c in cases]

        # Skill 5: LegacySystemOperatorRag（仅无接口系统触发）
        legacy_notes: List[Dict[str, Any]] = []
        systems = event.entities.get("systems", [])
        target = systems[0] if systems else ""
        if target and self.mcp.connectivity(target) == "no_api":
            t0 = self._t()
            legacy_notes = self.legacy_rag.run(target, cls.category)
            self.tracer.skill(
                self.legacy_rag.name,
                (f"命中 {len(legacy_notes)} 条老系统人工经验" if legacy_notes
                 else "无人工经验 → 不自行编排步骤"),
                status="ok" if legacy_notes else "fail", duration_ms=self._ms(t0),
                detail={"system": target},
            )
            for n in legacy_notes:
                citations.append(n["note_id"])
                for c in n.get("cautions", [])[:2]:
                    self.tracer.note(f"操作注意: {c}")

        result = KnowledgeResult(
            runbooks=runbooks, cases=cases, legacy_notes=legacy_notes,
            knowledge_gap=gap, gap_reason=gap_reason, citations=citations,
        )
        inc.knowledge = result
        inc.set_status(TicketStatus.KNOWLEDGE_READY,
                       f"命中 {len(runbooks)} Runbook / {len(cases)} 案例"
                       + ("（知识缺口）" if gap else ""))
        return result


# --------------------------------------------------------------------------
# Agent 4: Triage Analyst
# --------------------------------------------------------------------------

class TriageAnalystAgent(BaseAgent):
    name = "Triage Analyst"
    role = "影响面与风险判定者（可自主判定风险等级与路径，但不执行变更）"

    def __init__(self, tracer, policy: Dict[str, Any], mcp):
        super().__init__(tracer)
        self.engine = RiskEngine(policy)
        self.policy = policy
        self.mcp = mcp

    def run(self, inc: Incident) -> Any:
        from core.models import TriageResult
        event, cls, kn = inc.event, inc.classification, inc.knowledge
        self.tracer.agent(self.name, "判定影响面与风险等级")

        systems = event.entities.get("systems", [])
        target = systems[0] if systems else ""
        connectivity = self.mcp.connectivity(target) if target else "unknown"
        self.tracer.note(f"目标系统 {target or '未识别'} 连通性 = {connectivity}")

        # 身份核验（决定能否走免审批）
        req = self.policy.get("identity_verification", {}).get("required_factors", 2)
        identity_verified = len(event.identity_factors) >= req
        self.tracer.note(
            f"身份核验: {len(event.identity_factors)}/{req} 因子 "
            f"{event.identity_factors} → {'通过' if identity_verified else '不足'}")

        # 敏感权限探测（只读，L0）
        sensitive = False
        if self.mcp.is_auto_executable("code_repo", "list_permissions"):
            r = self.mcp.call("code_repo", "list_permissions",
                              user_id=event.reporter.get("user_id", ""))
            if r.get("ok") and r.get("sensitive_count", 0) > 0:
                sensitive = True
                self.tracer.tool("list_permissions", "code_repo",
                                 f"持有 {r['sensitive_count']} 项敏感权限: {r['sensitive']}")
                self.tracer.note("该账号持有生产敏感权限，风险等级将上调", level="warn")

        ctx = {
            "category": cls.category,
            "department": event.reporter.get("department"),
            "target_system": target,
            "system_connectivity": connectivity,
            "runbook_hit": bool(kn.runbooks) and not kn.knowledge_gap,
            "holds_sensitive_permission": sensitive,
        }
        level, matched, rationale, force_esc = self.engine.evaluate(ctx)

        if cls.needs_human_review:
            force_esc = True
            rationale.append(f"分类置信度 {cls.confidence} 低于阈值 → 强制升级人工")

        path = self.engine.execution_path(level, connectivity, force_esc)

        affected = 1 + len(inc.merged_msg_ids)
        impact = {
            "affected_users": affected,
            "affected_systems": systems,
            "business_impact": ("高（阻塞业务操作）" if event.urgency == "high" else "中"),
            "sla_seconds": self.policy.get("sla", {}).get(level.value, 3600),
        }

        result = TriageResult(
            risk_level=level, execution_path=path, impact_scope=impact,
            matched_rules=matched, rationale=rationale, target_system=target,
            system_connectivity=connectivity, identity_verified=identity_verified,
            sensitive_permission=sensitive, force_escalate=force_esc,
        )
        inc.triage = result

        self.tracer.skill(
            self.engine.name,
            f"风险等级 {level.value} | 执行路径 {path.value} | 命中规则 {matched}",
            detail={"rationale": rationale, "impact": impact},
        )
        for r in rationale:
            self.tracer.note(r)
        self.tracer.note(f"影响面: {affected} 人 / 系统 {systems} / SLA {impact['sla_seconds']}s")

        inc.set_status(TicketStatus.TRIAGED, f"{level.value} / {path.value}")
        return result


# --------------------------------------------------------------------------
# Agent 5: Resolution
# --------------------------------------------------------------------------

class ResolutionAgent(BaseAgent):
    name = "Resolution"
    role = "方案生成与执行者（L0/L1 白名单可自动执行；L2 必须等审批；L3 只生成方案）"

    def __init__(self, tracer, policy: Dict[str, Any], mcp, approval_adapter):
        super().__init__(tracer)
        self.resolver = SafeResolver(policy)
        self.escalator = EscalationRouter()
        self.legacy_op = LegacySystemOperator()
        self.policy = policy
        self.mcp = mcp
        self.approval = approval_adapter

    # ---------------- 方案生成 ----------------

    def plan(self, inc: Incident) -> Tuple[Optional[ResolutionPlan], str]:
        tri, cls, kn, ev = inc.triage, inc.classification, inc.knowledge, inc.event
        self.tracer.agent(self.name, "生成分级处置方案")

        t0 = self._t()
        plan, err = self.resolver.run(
            category=cls.category, target_system=tri.target_system,
            overall_risk=tri.risk_level, execution_path=tri.execution_path,
            knowledge=kn, user_id=ev.reporter.get("user_id", ""),
        )
        if err:
            self.tracer.skill(self.resolver.name, err, status="fail", duration_ms=self._ms(t0))
            return None, err

        inc.plan = plan
        self.tracer.skill(
            self.resolver.name,
            f"→ {plan.plan_id} | {len(plan.actions)} 个动作 | Runbook 引用 {plan.runbook_ref}",
            duration_ms=self._ms(t0),
        )
        for a in plan.actions:
            tag = ("自动执行" if a.agent_executable and not a.requires_approval
                   else "需审批" if a.agent_executable else "仅人工")
            self.tracer.note(f"[{a.risk_level.value}] {a.name} — {a.description}  → {tag}")
        self.tracer.note(f"回滚点: {plan.rollback_point}")

        inc.set_status(TicketStatus.PLANNED, f"{len(plan.actions)} 个动作")
        return plan, ""

    # ---------------- Legacy 人工清单 ----------------

    def build_manual_checklist(self, inc: Incident) -> Tuple[Dict[str, Any], str]:
        tri, kn, ev = inc.triage, inc.knowledge, inc.event
        sys_meta = self.mcp.systems.get(tri.target_system, {})
        t0 = self._t()
        checklist, err = self.legacy_op.run(
            system_id=tri.target_system,
            system_name=sys_meta.get("name", tri.target_system),
            operator=sys_meta.get("manual_operator", "系统管理员"),
            runbook=kn.top_runbook, legacy_notes=kn.legacy_notes,
            user_name=ev.reporter.get("name", ""),
        )
        if err:
            self.tracer.skill(self.legacy_op.name, err, status="fail", duration_ms=self._ms(t0))
            return {}, err

        self.tracer.skill(
            self.legacy_op.name,
            f"→ {checklist['checklist_id']} | {len(checklist['steps'])} 步 | "
            f"指派 {checklist['assigned_operator']}",
            duration_ms=self._ms(t0),
        )
        for i, s in enumerate(checklist["steps"], 1):
            self.tracer.note(f"  {i}. {s}")
        self.tracer.note(f"留证要求: {checklist['evidence_required']}")
        if inc.plan:
            inc.plan.manual_checklist = checklist["steps"]
            inc.plan.evidence_required = checklist["evidence_required"]
        return checklist, ""

    # ---------------- 执行 ----------------

    def execute(self, inc: Incident, approved: bool) -> None:
        plan, ev = inc.plan, inc.event
        user_id = ev.reporter.get("user_id", "")
        self.tracer.agent(self.name, "执行处置动作并标记完成")
        inc.set_status(TicketStatus.EXECUTING, "开始执行")

        for a in plan.actions:
            # 边界 1：L3 / 非 agent 可执行动作，Agent 不碰
            if not a.agent_executable:
                a.status = "manual_pending"
                a.result = {"reason": "高风险动作，按 DecisionBoundary 必须人工执行"}
                self.tracer.tool(a.name, a.system_id,
                                 "跳过 —— L3 高风险，Agent 不得执行，已转人工", status="skipped")
                continue

            # 边界 2：需审批但未获批准
            if a.requires_approval and not approved:
                a.status = "skipped"
                a.result = {"reason": "未获审批批准，动作冻结"}
                self.tracer.tool(a.name, a.system_id, "跳过 —— 未获审批", status="skipped")
                continue

            # 边界 3：无接口系统
            if self.mcp.connectivity(a.system_id) == "no_api":
                a.status = "manual_pending"
                a.result = {"reason": "系统无开放接口，转 Legacy 人工执行"}
                self.tracer.tool(a.name, a.system_id, "无接口 → 转人工执行清单", status="skipped")
                continue

            call_params = dict(a.params)
            call_params.setdefault("user_id", user_id)
            resp = self.mcp.call(a.system_id, a.name, **call_params)
            a.executed_at = now_iso()
            a.executed_by = f"agent:{self.name}"
            a.result = resp
            if resp.get("ok"):
                a.status = "done"
                self.tracer.tool(a.name, a.system_id,
                                 resp.get("message") or resp.get("code", "OK"), status="ok",
                                 detail={"before": resp.get("before"), "after": resp.get("after")})
                if resp.get("before") != resp.get("after") and resp.get("after"):
                    self.tracer.note(f"状态变更: {self._diff(resp.get('before'), resp.get('after'))}")
            else:
                a.status = "failed"
                self.tracer.tool(a.name, a.system_id,
                                 f"{resp.get('code')} {resp.get('message')}", status="fail")

        done = [a for a in plan.actions if a.status == "done"]
        pending = [a for a in plan.actions if a.status == "manual_pending"]
        failed = [a for a in plan.actions if a.status == "failed"]

        if pending:
            inc.set_status(TicketStatus.AWAITING_MANUAL,
                           f"{len(done)} 自动完成 / {len(pending)} 待人工")
        else:
            inc.set_status(TicketStatus.EXECUTED,
                           f"{len(done)} 完成 / {len(failed)} 失败")
        self.tracer.note(f"执行结果: 完成 {len(done)}｜待人工 {len(pending)}｜失败 {len(failed)}")

    # ---------------- Legacy 人工执行回填 ----------------

    def apply_manual_fillback(self, inc: Incident, fillback: Dict[str, Any]) -> None:
        self.tracer.agent(self.name, "接收人工执行结果回填")
        for a in inc.plan.actions:
            if a.status == "manual_pending" and a.name == "legacy_manual_operation":
                a.status = "manual_done" if fillback.get("result") == "success" else "failed"
                a.executed_at = fillback.get("operated_at", now_iso())
                a.executed_by = f"human:{fillback.get('operator', '未知')}"
                a.result = fillback
        self.tracer.note(f"人工执行人: {fillback.get('operator')}｜结果: {fillback.get('result')}")
        self.tracer.note(f"实际根因: {fillback.get('root_cause')}")
        self.tracer.note(f"留证截图: {fillback.get('screenshots')}")
        inc.set_status(TicketStatus.EXECUTED, "人工执行已回填")

    # ---------------- 升级 ----------------

    def escalate(self, inc: Incident, reason: str) -> Dict[str, Any]:
        self.tracer.agent(self.name, "生成升级单并交接人工")
        attempted = [f"{a.name}({a.status})" for a in (inc.plan.actions if inc.plan else [])]
        esc = self.escalator.run(
            incident_id=inc.incident_id, reason=reason, event=inc.event,
            classification=inc.classification, knowledge=inc.knowledge, attempted=attempted,
        )
        self.tracer.skill(self.escalator.name,
                          f"→ {esc['escalation_id']} 交接给 {esc['suggested_owner']}",
                          detail=esc)
        self.tracer.note(f"升级原因: {reason}")
        self.tracer.note(f"知识状态: {esc['handover']['knowledge_status']}")
        inc.set_status(TicketStatus.ESCALATED, reason)
        return esc

    @staticmethod
    def _diff(before: Optional[Dict], after: Optional[Dict]) -> str:
        before, after = before or {}, after or {}
        parts = []
        for k in set(list(before.keys()) + list(after.keys())):
            b, a = before.get(k), after.get(k)
            if b != a:
                parts.append(f"{k}: {b} → {a}")
        return "; ".join(parts[:4]) or "无变化"


# --------------------------------------------------------------------------
# Agent 6: Verify
# --------------------------------------------------------------------------

class VerifyAgent(BaseAgent):
    name = "Verify"
    role = "结果验证与知识回流者（验证不通过不得判定成功）"

    def __init__(self, tracer, mcp, kb, policy: Dict[str, Any]):
        super().__init__(tracer)
        self.verifier = RecoveryVerifier(mcp)
        self.postmortem = Postmortem()
        self.reflector = KnowledgeReflector(kb, policy)
        self.kb = kb

    def verify(self, inc: Incident, user_confirm: Optional[bool] = None) -> VerificationResult:
        self.tracer.agent(self.name, "探针验证恢复结果")

        # 已升级人工的工单：Agent 未执行变更，不做恢复状态判定，保持升级态
        if inc.status == TicketStatus.ESCALATED:
            res = VerificationResult(
                conclusion="not_applicable", probes=[], user_confirmed=False,
                evidence=["升级路径：Agent 未执行变更，不做恢复判定"],
                reason="已升级人工，等待人工闭环")
            inc.verification = res
            self.tracer.skill(self.verifier.name, f"结论 = {res.conclusion} —— {res.reason}",
                              status="skipped")
            return res

        inc.set_status(TicketStatus.VERIFYING, "开始验证")

        t0 = self._t()
        res = self.verifier.run(
            category=inc.classification.category,
            system_id=inc.triage.target_system,
            user_id=inc.event.reporter.get("user_id", ""),
            executed_actions=(inc.plan.actions if inc.plan else []),
            user_confirm=user_confirm,
        )
        inc.verification = res

        status = {"success": "ok", "partial": "skipped",
                  "failed": "fail", "not_applicable": "skipped"}.get(res.conclusion, "ok")
        self.tracer.skill(self.verifier.name, f"结论 = {res.conclusion} —— {res.reason}",
                          status=status, duration_ms=self._ms(t0),
                          detail={"probes": res.probes})
        for p in res.probes:
            if p.get("checks"):
                for c in p["checks"]:
                    mark = "✔" if c["passed"] else "✘"
                    self.tracer.note(f"  {mark} {p['system_id']}.{c['field']}: "
                                     f"期望 {c['expected']}，实际 {c['actual']}")
            else:
                self.tracer.note(f"  {p.get('message', '')}")
        if user_confirm is not None:
            self.tracer.note(f"用户 IM 确认: {'已确认恢复' if user_confirm else '反馈仍未恢复'}")

        inc.set_status(
            TicketStatus.VERIFIED if res.conclusion == "success" else TicketStatus.VERIFY_FAILED,
            res.conclusion)
        return res

    def reflect(self, inc: Incident, duration_seconds: int) -> None:
        self.tracer.agent(self.name, "生成复盘并回流知识库")

        t0 = self._t()
        report = self.postmortem.run(inc, duration_seconds)
        inc.postmortem = report
        self.tracer.skill(self.postmortem.name,
                          f"outcome={report.outcome} | 耗时 {report.duration_seconds}s"
                          + ("  ⚠ 标记为 Badcase" if report.is_badcase else ""),
                          status="ok" if not report.is_badcase else "skipped",
                          duration_ms=self._ms(t0))
        self.tracer.note(f"根因: {report.root_cause}")
        for w in report.what_worked:
            self.tracer.note(f"有效做法: {w}")
        for i in report.improvements:
            self.tracer.note(f"改进项: {i}")

        before = self.kb.snapshot_counts()
        t0 = self._t()
        updates = self.reflector.run(inc, report)
        after = self.kb.snapshot_counts()
        inc.kb_updates = updates

        self.tracer.skill(self.reflector.name, f"知识库写入 {len(updates)} 项变更",
                          duration_ms=self._ms(t0), detail={"updates": updates})
        for u in updates:
            if u["op"] == "persist":
                self.tracer.kb("已落盘", u["file"])
            elif u["op"] == "add_case":
                self.tracer.kb("新增案例", f"{u['id']} 《{u['title']}》")
            elif u["op"] == "add_badcase":
                self.tracer.kb("新增 Badcase", f"{u['id']} — {u['reason']}")
            elif u["op"] == "add_runbook_draft":
                self.tracer.kb("新增 Runbook 草稿", f"{u['id']} 《{u['title']}》")
            elif u["op"] == "append_legacy_experience":
                self.tracer.kb("补充老系统经验", f"{u['id']} — {u['lesson']}")
            elif u["op"] == "add_legacy_note":
                self.tracer.kb("新增老系统经验条目", f"{u['id']} ({u['system']})")

        delta = {k: after[k] - before[k] for k in after if after[k] != before[k]}
        if delta:
            self.tracer.note(f"知识库条目变化: {before} → {after}  Δ={delta}")

        # 已升级人工的工单保持 ESCALATED，不覆盖为 REFLECTED
        if inc.status != TicketStatus.ESCALATED:
            inc.set_status(TicketStatus.REFLECTED, f"知识回流 {len(updates)} 项")
