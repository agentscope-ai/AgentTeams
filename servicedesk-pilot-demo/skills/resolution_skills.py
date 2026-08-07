"""
处置类 Skills（方案 10.3）

  Skill 6  SafeResolver          —— 通用·高复用：风险分级处置方案生成
  Skill 7  ApprovalGenerator     —— 通用：审批单生成（委托 ApprovalAdapter）
  Skill 8  EscalationRouter      —— 通用：升级到人工，带完整上下文交接
  Skill 9  LegacySystemOperator  —— 场景相关：无接口系统人工操作清单 + 留证要求

另含 RiskEngine：Triage Analyst 使用的风险判定引擎（policy.json 规则驱动）。
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Tuple

from core.models import (Action, Classification, ExecutionPath, KnowledgeResult,
                         NormalizedEvent, ResolutionPlan, RiskLevel, new_id, now_iso)


# --------------------------------------------------------------------------
# 风险判定引擎（Triage Analyst 的核心逻辑）
# --------------------------------------------------------------------------

class RiskEngine:
    """按 policy.json 的 risk_rules 判定风险等级，输出可审计的命中规则与依据。"""

    name = "RiskEngine"

    def __init__(self, policy: Dict[str, Any]):
        self.policy = policy
        self.rules = policy.get("risk_rules", [])
        self.levels = policy.get("risk_levels", {})

    def evaluate(self, ctx: Dict[str, Any]) -> Tuple[RiskLevel, List[str], List[str], bool]:
        """
        ctx 需包含: category, department, target_system, system_connectivity,
                   runbook_hit, holds_sensitive_permission
        返回 (风险等级, 命中规则ID列表, 判定依据, 是否强制升级)
        """
        level = RiskLevel.L1          # 默认低风险
        matched: List[str] = []
        rationale: List[str] = []
        force_escalate = False

        for rule in self.rules:
            if not self._match(rule.get("match", {}), ctx):
                continue
            rid = rule["rule_id"]
            matched.append(rid)
            desc = rule.get("description", "")

            if "risk_level" in rule:
                new_level = RiskLevel(rule["risk_level"])
                if new_level.order > level.order or rule.get("risk_level") == "L1":
                    level = new_level
                rationale.append(f"[{rid}] {desc} → 判定 {new_level.value}")

            if "min_risk_level" in rule:
                floor = RiskLevel(rule["min_risk_level"])
                if level.order < floor.order:
                    level = floor
                    rationale.append(f"[{rid}] {desc} → 提升至 {floor.value}")
                else:
                    rationale.append(f"[{rid}] {desc}（当前已满足 ≥{floor.value}）")

            if "escalate_by" in rule:
                bumped = RiskLevel.from_order(level.order + int(rule["escalate_by"]))
                if bumped.order > level.order:
                    rationale.append(f"[{rid}] {desc} → {level.value} 升至 {bumped.value}")
                    level = bumped

            if rule.get("force_escalate"):
                force_escalate = True
                rationale.append(f"[{rid}] {desc} → 强制升级人工")

        if not matched:
            rationale.append("未命中特殊风险规则，按默认 L1 低风险处理")

        return level, matched, rationale, force_escalate

    @staticmethod
    def _match(m: Dict[str, Any], ctx: Dict[str, Any]) -> bool:
        if "category" in m and m["category"] != ctx.get("category"):
            return False

        # department_in 与 or_system_in 是「或」关系
        if "department_in" in m or "or_system_in" in m:
            hit_dept = ctx.get("department") in m.get("department_in", [])
            hit_sys = ctx.get("target_system") in m.get("or_system_in", [])
            if not (hit_dept or hit_sys):
                return False

        if "system_connectivity" in m and m["system_connectivity"] != ctx.get("system_connectivity"):
            return False
        if "runbook_hit" in m and bool(m["runbook_hit"]) != bool(ctx.get("runbook_hit")):
            return False
        if "holds_sensitive_permission" in m and \
                bool(m["holds_sensitive_permission"]) != bool(ctx.get("holds_sensitive_permission")):
            return False
        return True

    def execution_path(self, level: RiskLevel, connectivity: str,
                       force_escalate: bool) -> ExecutionPath:
        if force_escalate:
            return ExecutionPath.ESCALATE
        if level == RiskLevel.L3:
            return ExecutionPath.HUMAN_ONLY
        if connectivity == "no_api":
            return ExecutionPath.LEGACY_MANUAL
        if level == RiskLevel.L2:
            return ExecutionPath.APPROVAL_THEN_EXECUTE
        return ExecutionPath.AUTO_EXECUTE


# --------------------------------------------------------------------------
# Skill 6: SafeResolver
# --------------------------------------------------------------------------

# 按异常类别定义标准动作模板（来自 Runbook 的可执行化表达）
_ACTION_TEMPLATES: Dict[str, List[Dict[str, Any]]] = {
    "account_locked": [
        {"name": "query_account_status", "system": "vpn_gateway",
         "desc": "查询账号当前锁定状态与失败计数", "risk": "L0",
         "rollback": "只读操作，无需回滚"},
        {"name": "unlock_account", "system": "vpn_gateway",
         "desc": "解锁账号并清零密码失败计数器（不重置密码）", "risk": "L1",
         "rollback": "解锁为幂等操作，如判断有误可立即重新锁定"},
    ],
    "mfa_lost": [
        {"name": "query_account_status", "system": "idaas",
         "desc": "确认 IdaaS 中 MFA 绑定状态与原设备", "risk": "L0",
         "rollback": "只读操作，无需回滚"},
        {"name": "unbind_mfa", "system": "idaas",
         "desc": "解绑原 MFA 设备（身份凭据变更）", "risk": "L2",
         "rollback": "15 分钟内可由管理员恢复原绑定记录"},
        {"name": "issue_binding_link", "system": "idaas",
         "desc": "生成 15 分钟有效的一次性绑定链接", "risk": "L1",
         "rollback": "链接超时自动失效"},
    ],
    "suspected_compromise": [
        {"name": "query_login_log", "system": "mail_system",
         "desc": "拉取近 7 天登录日志，确认境外登录记录", "risk": "L0",
         "rollback": "只读操作，无需回滚"},
        {"name": "list_permissions", "system": "code_repo",
         "desc": "列出该账号持有的敏感系统权限，评估横向影响", "risk": "L0",
         "rollback": "只读操作，无需回滚"},
        {"name": "revoke_sessions", "system": "idaas",
         "desc": "吊销全部活跃会话（含境外会话）", "risk": "L2",
         "rollback": "会话吊销不可逆，但用户可重新登录"},
        {"name": "force_password_reset", "system": "idaas",
         "desc": "标记强制改密，下次登录必须修改", "risk": "L2",
         "rollback": "可取消强制改密标记"},
        {"name": "revoke_tokens", "system": "code_repo",
         "desc": "吊销代码仓库长期 token（高风险，仅人工执行）", "risk": "L3",
         "rollback": "token 吊销不可逆，需重新签发"},
    ],
    "legacy_account": [
        {"name": "legacy_manual_operation", "system": "legacy_erp_u8",
         "desc": "无接口老系统，生成人工操作清单交由管理员执行", "risk": "L2",
         "rollback": "启用操作可撤销，恢复为停用状态"},
    ],
    "sso_federation_error": [
        {"name": "query_sso_log", "system": "supplier_portal",
         "desc": "拉取 SSO 联邦登录失败日志与错误码", "risk": "L0",
         "rollback": "只读操作，无需回滚"},
    ],
}


class SafeResolver:
    """根据风险等级生成处置方案（自动执行 / 审批执行 / 人工指引）。"""

    name = "SafeResolver"

    def __init__(self, policy: Dict[str, Any]):
        self.policy = policy
        self.whitelist = policy.get("execution_whitelist", {})

    def run(self, category: str, target_system: str, overall_risk: RiskLevel,
            execution_path: ExecutionPath, knowledge: KnowledgeResult,
            user_id: str) -> Tuple[Optional[ResolutionPlan], str]:
        templates = _ACTION_TEMPLATES.get(category)
        if not templates:
            return None, f"类别 {category} 无安全动作模板，不冒险生成方案 → 转人工"

        rb = knowledge.top_runbook
        auto_ok = set(self.whitelist.get("auto_allowed", []))
        need_apv = set(self.whitelist.get("approval_required", []))
        human_only = set(self.whitelist.get("human_only", []))

        actions: List[Action] = []
        for t in templates:
            aname = t["name"]
            arisk = RiskLevel(t["risk"])

            # 白名单裁决：白名单优先级高于风险等级判定
            if aname in human_only:
                requires_approval, agent_exec = True, False
            elif aname in need_apv:
                requires_approval, agent_exec = True, True
            elif aname in auto_ok:
                requires_approval, agent_exec = False, True
            else:
                requires_approval, agent_exec = True, False

            if execution_path == ExecutionPath.HUMAN_ONLY and arisk.order >= RiskLevel.L2.order:
                agent_exec = False

            actions.append(Action(
                action_id=new_id("ACT"),
                name=aname,
                system_id=t.get("system", target_system),
                description=t["desc"],
                risk_level=arisk,
                params={"user_id": user_id},
                rollback=t["rollback"],
                requires_approval=requires_approval,
                agent_executable=agent_exec,
            ))

        plan = ResolutionPlan(
            plan_id=new_id("PLAN"),
            actions=actions,
            runbook_ref=f"{rb['doc_id']} {rb['version']}" if rb else "无 Runbook 引用",
            overall_risk=overall_risk,
            execution_path=execution_path,
            rollback_point=(rb.get("rollback", "") if rb else "无标准回滚方案，执行前需人工确认"),
            evidence_required=["执行前后账号状态快照", "MCP 调用请求/响应", "验证探针结果"],
        )
        return plan, ""


# --------------------------------------------------------------------------
# Skill 8: EscalationRouter
# --------------------------------------------------------------------------

class EscalationRouter:
    """Agent 无法处理 / 低置信度 / 知识缺口 / 超时 → 升级人工，带上下文交接。"""

    name = "EscalationRouter"

    def run(self, incident_id: str, reason: str, event: NormalizedEvent,
            classification: Optional[Classification], knowledge: Optional[KnowledgeResult],
            attempted: List[str], suggested_owner: str = "兼职IT(赵工)") -> Dict[str, Any]:
        return {
            "escalation_id": new_id("ESC"),
            "incident_id": incident_id,
            "created_at": now_iso(),
            "reason": reason,
            "suggested_owner": suggested_owner,
            "handover": {
                "reporter": event.reporter,
                "reported_at": event.reported_at,
                "symptom_summary": event.redacted_text[:200],
                "entities": event.entities,
                "classification": (
                    {"category": classification.category, "confidence": classification.confidence}
                    if classification else None
                ),
                "knowledge_status": (
                    "知识缺口，无可用 Runbook" if (knowledge and knowledge.knowledge_gap)
                    else f"命中 {len(knowledge.runbooks)} 条 Runbook" if knowledge else "未检索"
                ),
                "attempted_steps": attempted,
            },
            "sla_hint": "已在群内同步升级状态，避免用户重复追问",
        }


# --------------------------------------------------------------------------
# Skill 9: LegacySystemOperator
# --------------------------------------------------------------------------

class LegacySystemOperator:
    """为无接口老系统生成人工操作清单 + 留证要求 + 结果回填模板。"""

    name = "LegacySystemOperator"

    def run(self, system_id: str, system_name: str, operator: str,
            runbook: Optional[Dict[str, Any]],
            legacy_notes: List[Dict[str, Any]], user_name: str) -> Tuple[Dict[str, Any], str]:
        if not runbook and not legacy_notes:
            return {}, "无该系统人工操作经验，不自行编排步骤 → 直接转人工专家"

        steps = list(runbook.get("steps", [])) if runbook else []
        note = legacy_notes[0] if legacy_notes else {}
        evidence = note.get("evidence_required", ["操作前状态截图", "操作后状态截图", "操作人与时间"])
        cautions = note.get("cautions", [])

        return {
            "checklist_id": new_id("MANUAL"),
            "system_id": system_id,
            "system_name": system_name,
            "assigned_operator": operator,
            "target_user": user_name,
            "steps": steps,
            "cautions": cautions,
            "evidence_required": evidence,
            "fillback_template": {
                "operator": "<操作人工号>",
                "operated_at": "<操作时间>",
                "result": "<success|failed>",
                "root_cause": "<实际根因>",
                "screenshots": ["<操作前截图ID>", "<操作后截图ID>"],
                "note": "<备注>",
            },
            "created_at": now_iso(),
        }, ""
