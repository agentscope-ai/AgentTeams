"""
ServiceDesk Pilot - 核心数据模型

对应方案第十一章 Agent Identity 与第十二章证据规范。
所有跨 Agent 传递的上下文都在这里定义，保证契约统一：
Mock 阶段与真实接入阶段共用同一套 Schema。
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone, timedelta
from enum import Enum
from typing import Any, Dict, List, Optional

CST = timezone(timedelta(hours=8))


def now_iso() -> str:
    return datetime.now(CST).isoformat(timespec="seconds")


def new_id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:8].upper()}"


# --------------------------------------------------------------------------
# 枚举
# --------------------------------------------------------------------------

class RiskLevel(str, Enum):
    L0 = "L0"
    L1 = "L1"
    L2 = "L2"
    L3 = "L3"

    @property
    def order(self) -> int:
        return {"L0": 0, "L1": 1, "L2": 2, "L3": 3}[self.value]

    @staticmethod
    def from_order(n: int) -> "RiskLevel":
        return [RiskLevel.L0, RiskLevel.L1, RiskLevel.L2, RiskLevel.L3][max(0, min(3, n))]


class TicketStatus(str, Enum):
    """工单状态机。对应方案『统一 Incident/Ticket State』要求。"""
    RECEIVED = "received"              # 已从 IM 接收
    NORMALIZED = "normalized"          # 已标准化为服务台事件
    DEDUPED = "deduped"                # 判定为重复，合并到主工单
    CLASSIFIED = "classified"          # 已完成异常分类
    KNOWLEDGE_READY = "knowledge_ready"  # 知识检索完成
    TRIAGED = "triaged"                # 风险分诊完成
    PLANNED = "planned"                # 处置方案已生成
    AWAITING_APPROVAL = "awaiting_approval"  # 等待审批
    APPROVAL_SKIPPED = "approval_skipped"    # 按策略免审批
    APPROVED = "approved"
    REJECTED = "rejected"
    EXECUTING = "executing"
    AWAITING_MANUAL = "awaiting_manual"      # Legacy 系统等待人工执行回填
    EXECUTED = "executed"
    VERIFYING = "verifying"
    VERIFIED = "verified"
    VERIFY_FAILED = "verify_failed"
    ESCALATED = "escalated"            # 升级人工
    REFLECTED = "reflected"            # 知识已回流
    CLOSED = "closed"


class ExecutionPath(str, Enum):
    AUTO_EXECUTE = "auto_execute"              # L0/L1 白名单自动执行
    APPROVAL_THEN_EXECUTE = "approval_then_execute"  # L2 审批后执行
    LEGACY_MANUAL = "legacy_manual"            # 无接口老系统人工执行
    HUMAN_ONLY = "human_only"                  # L3 强制人工
    ESCALATE = "escalate"                      # 升级交接


class ApprovalDecision(str, Enum):
    PENDING = "pending"
    APPROVED = "approved"
    REJECTED = "rejected"
    SKIPPED = "skipped"       # 按策略免审批
    ESCALATED = "escalated"
    TIMEOUT = "timeout"


# --------------------------------------------------------------------------
# 数据结构
# --------------------------------------------------------------------------

@dataclass
class RawMessage:
    """IM 原始消息（Ticket Intake 的输入）"""
    msg_id: str
    scenario: str
    timestamp: str
    sender: Dict[str, Any]
    msg_type: str
    content: str
    attachments: List[Dict[str, Any]] = field(default_factory=list)
    mentions: List[str] = field(default_factory=list)
    channel: Dict[str, Any] = field(default_factory=dict)


@dataclass
class NormalizedEvent:
    """标准化服务台事件（ImTicketNormalizer 的输出）"""
    event_id: str
    source_msg_ids: List[str]
    channel_type: str
    chat_id: str
    reporter: Dict[str, Any]
    raw_text: str
    redacted_text: str            # 脱敏后文本，唯一允许进入知识库的版本
    reported_at: str
    entities: Dict[str, Any] = field(default_factory=dict)   # 系统、错误码、症状等
    attachments_ocr: List[str] = field(default_factory=list)
    urgency: str = "medium"
    missing_fields: List[str] = field(default_factory=list)
    preprocess_reason: str = ""   # 方案第十三章：处理原因必须可审计
    identity_factors: List[str] = field(default_factory=list)


@dataclass
class Classification:
    """AccountAnomalyClassifier 的输出"""
    category: str
    confidence: float
    evidence_fields: Dict[str, Any] = field(default_factory=dict)
    candidates: List[Dict[str, Any]] = field(default_factory=list)
    needs_human_review: bool = False


@dataclass
class KnowledgeResult:
    """Knowledge Agent 汇总输出"""
    runbooks: List[Dict[str, Any]] = field(default_factory=list)
    cases: List[Dict[str, Any]] = field(default_factory=list)
    legacy_notes: List[Dict[str, Any]] = field(default_factory=list)
    knowledge_gap: bool = False
    gap_reason: str = ""
    citations: List[str] = field(default_factory=list)

    @property
    def top_runbook(self) -> Optional[Dict[str, Any]]:
        return self.runbooks[0] if self.runbooks else None


@dataclass
class TriageResult:
    """Triage Analyst 的输出"""
    risk_level: RiskLevel
    execution_path: ExecutionPath
    impact_scope: Dict[str, Any] = field(default_factory=dict)
    matched_rules: List[str] = field(default_factory=list)
    rationale: List[str] = field(default_factory=list)
    target_system: str = ""
    system_connectivity: str = ""
    identity_verified: bool = False
    sensitive_permission: bool = False
    force_escalate: bool = False


@dataclass
class Action:
    """单个处置动作"""
    action_id: str
    name: str                 # 对应 execution_whitelist 中的动作名
    system_id: str
    description: str
    risk_level: RiskLevel
    params: Dict[str, Any] = field(default_factory=dict)
    rollback: str = ""
    requires_approval: bool = False
    agent_executable: bool = True
    status: str = "planned"   # planned | skipped | executing | done | failed | manual_pending | manual_done
    result: Dict[str, Any] = field(default_factory=dict)
    executed_at: str = ""
    executed_by: str = ""


@dataclass
class ResolutionPlan:
    """SafeResolver 的输出"""
    plan_id: str
    actions: List[Action] = field(default_factory=list)
    runbook_ref: str = ""
    overall_risk: RiskLevel = RiskLevel.L1
    execution_path: ExecutionPath = ExecutionPath.AUTO_EXECUTE
    rollback_point: str = ""
    manual_checklist: List[str] = field(default_factory=list)
    evidence_required: List[str] = field(default_factory=list)


@dataclass
class ApprovalTicket:
    """ApprovalGenerator 的输出"""
    approval_id: str
    incident_id: str
    risk_level: RiskLevel
    actions_summary: List[str]
    requester: str
    approver_id: str
    approver_name: str
    reason: str
    impact: str
    rollback_point: str
    created_at: str
    decision: ApprovalDecision = ApprovalDecision.PENDING
    decided_at: str = ""
    decided_by: str = ""
    comment: str = ""
    skip_rule_id: str = ""       # 若被策略跳过，记录命中的规则，保留审计痕迹
    latency_seconds: int = 0


@dataclass
class VerificationResult:
    """RecoveryVerifier 的输出"""
    conclusion: str            # success | partial | failed | not_applicable
    probes: List[Dict[str, Any]] = field(default_factory=list)
    user_confirmed: bool = False
    evidence: List[str] = field(default_factory=list)
    reason: str = ""


@dataclass
class PostmortemReport:
    """Postmortem 的输出"""
    incident_id: str
    outcome: str
    duration_seconds: int
    timeline: List[Dict[str, Any]] = field(default_factory=list)
    root_cause: str = ""
    what_worked: List[str] = field(default_factory=list)
    improvements: List[str] = field(default_factory=list)
    knowledge_proposals: List[Dict[str, Any]] = field(default_factory=list)
    is_badcase: bool = False


@dataclass
class Incident:
    """
    工单主体 —— 全流程唯一上下文载体。
    每个 Agent 读写这个对象的不同字段，对应方案 8.2『上下文传递』。
    """
    incident_id: str
    scenario: str
    status: TicketStatus = TicketStatus.RECEIVED
    created_at: str = field(default_factory=now_iso)
    closed_at: str = ""

    raw_messages: List[RawMessage] = field(default_factory=list)
    event: Optional[NormalizedEvent] = None
    classification: Optional[Classification] = None
    knowledge: Optional[KnowledgeResult] = None
    triage: Optional[TriageResult] = None
    plan: Optional[ResolutionPlan] = None
    approval: Optional[ApprovalTicket] = None
    verification: Optional[VerificationResult] = None
    postmortem: Optional[PostmortemReport] = None

    merged_msg_ids: List[str] = field(default_factory=list)  # 去重合并进来的消息
    kb_updates: List[Dict[str, Any]] = field(default_factory=list)
    im_replies: List[Dict[str, Any]] = field(default_factory=list)  # 回到微信群的进度同步
    status_history: List[Dict[str, str]] = field(default_factory=list)

    def set_status(self, status: TicketStatus, note: str = "") -> None:
        self.status = status
        self.status_history.append({
            "status": status.value,
            "at": now_iso(),
            "note": note,
        })

    def to_dict(self) -> Dict[str, Any]:
        def conv(o: Any) -> Any:
            if isinstance(o, Enum):
                return o.value
            if hasattr(o, "__dataclass_fields__"):
                return {k: conv(v) for k, v in asdict(o).items()}
            if isinstance(o, list):
                return [conv(i) for i in o]
            if isinstance(o, dict):
                return {k: conv(v) for k, v in o.items()}
            return o
        return conv(self)
