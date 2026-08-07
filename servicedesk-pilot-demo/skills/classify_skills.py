"""
分类类 Skills（方案 10.1）

  Skill 1  ImTicketNormalizer        —— 通用·高复用：IM 消息 → 标准化服务台事件
  Skill 2  AccountAnomalyClassifier  —— 场景专属：账户异常分类
"""

from __future__ import annotations

import re
from typing import Any, Dict, List, Optional, Tuple

from core.models import (Classification, NormalizedEvent, RawMessage,
                         new_id, now_iso)

# --------------------------------------------------------------------------
# 脱敏：方案 8.1『密钥、个人信息禁止进入 RAG 与日志』
# --------------------------------------------------------------------------

_REDACT_PATTERNS = [
    (re.compile(r"1[3-9]\d{9}"), "<PHONE>"),
    (re.compile(r"[\w.\-]+@[\w\-]+\.[a-zA-Z]{2,}"), "<EMAIL>"),
    (re.compile(r"\b\d{15,19}\b"), "<CARD>"),
    (re.compile(r"(密码|password|pwd)[是为:：\s]+\S+", re.I), r"\1:<REDACTED>"),
    (re.compile(r"\b\d{1,3}(\.\d{1,3}){3}\b"), "<IP>"),
]


def redact(text: str) -> str:
    out = text
    for pat, repl in _REDACT_PATTERNS:
        out = pat.sub(repl, out)
    return out


# --------------------------------------------------------------------------
# Skill 1: ImTicketNormalizer
# --------------------------------------------------------------------------

_SYSTEM_KEYWORDS: List[Tuple[str, List[str]]] = [
    ("vpn_gateway", ["vpn", "内网", "远程接入"]),
    ("legacy_erp_u8", ["u8", "用友", "erp", "老财务", "财务系统"]),
    ("supplier_portal", ["供应商协同", "供应商平台", "协同平台"]),
    ("mail_system", ["邮箱", "邮件", "exchange", "收信"]),
    ("oa_system", ["oa", "泛微", "办公系统"]),
    ("code_repo", ["gitlab", "代码仓库", "git"]),
    ("idaas", ["动态口令", "验证器", "mfa", "二次验证", "otp", "身份认证"]),
]

_ERROR_CODE_RE = re.compile(r"\b([A-Z]{2,6}-\d{2,4})\b")

_URGENCY_HINTS = {
    "high": ["急", "马上", "立刻", "客户等", "影响业务", "出不了", "盗号", "被盗", "境外"],
    "low": ["有空看下", "不急", "方便的时候"],
}


class ImTicketNormalizer:
    """把 IM 群消息映射为标准化服务台事件。任何入口型场景的第一站。"""

    name = "ImTicketNormalizer"

    def run(self, messages: List[RawMessage]) -> NormalizedEvent:
        primary = messages[0]
        all_text = " ".join(m.content for m in messages)
        ocr_texts = [a.get("ocr_text", "") for m in messages for a in m.attachments if a.get("ocr_text")]
        corpus = all_text + " " + " ".join(ocr_texts)
        low = corpus.lower()

        # 实体抽取
        systems = [sid for sid, kws in _SYSTEM_KEYWORDS if any(k in low for k in kws)]
        error_codes = _ERROR_CODE_RE.findall(corpus)

        symptoms = []
        for kw, sym in [("锁定", "account_locked"), ("登不上", "login_failed"),
                        ("登录", "login_failed"), ("用户不存在", "user_not_found"),
                        ("进不去", "access_denied"), ("验证器", "mfa_unavailable"),
                        ("盗号", "suspected_compromise"), ("境外", "abnormal_login"),
                        ("转圈", "login_hang"), ("校验失败", "assertion_failed")]:
            if kw in corpus and sym not in symptoms:
                symptoms.append(sym)

        # 紧急度
        urgency = "medium"
        if any(h in corpus for h in _URGENCY_HINTS["high"]):
            urgency = "high"
        elif any(h in corpus for h in _URGENCY_HINTS["low"]):
            urgency = "low"

        # 身份因子（决定后续能否走免审批）
        factors = []
        s = primary.sender
        if s.get("name"):
            factors.append("im_real_name")
        if s.get("phone_tail"):
            factors.append("phone_tail")
        if s.get("user_id"):
            factors.append("employee_id")

        # 缺失字段
        missing = []
        if not systems:
            missing.append("target_system")
        if not error_codes and not ocr_texts:
            missing.append("error_code_or_screenshot")

        reasons = [
            f"聚合 {len(messages)} 条群消息为 1 个事件",
            f"识别系统: {systems or '未识别'}",
            f"识别症状: {symptoms or '未识别'}",
            f"错误码: {error_codes or '无'}",
            f"紧急度判定 {urgency}（依据关键词命中）",
            f"身份因子 {len(factors)} 个: {factors}",
        ]
        if ocr_texts:
            reasons.append(f"从 {len(ocr_texts)} 张截图 OCR 补全上下文")
        if missing:
            reasons.append(f"缺失字段: {missing}，已标记待补全")

        return NormalizedEvent(
            event_id=new_id("EVT"),
            source_msg_ids=[m.msg_id for m in messages],
            channel_type=primary.channel.get("type", "wechat_work"),
            chat_id=primary.channel.get("chat_id", ""),
            reporter={
                "user_id": s.get("user_id"), "name": s.get("name"),
                "department": s.get("department"), "title": s.get("title"),
                "phone_tail": s.get("phone_tail"),
            },
            raw_text=all_text,
            redacted_text=redact(corpus),
            reported_at=primary.timestamp,
            entities={"systems": systems, "error_codes": error_codes, "symptoms": symptoms},
            attachments_ocr=ocr_texts,
            urgency=urgency,
            missing_fields=missing,
            preprocess_reason="; ".join(reasons),
            identity_factors=factors,
        )

    # 去重：方案 dedup 策略
    @staticmethod
    def is_duplicate(a: NormalizedEvent, b: NormalizedEvent, window_seconds: int) -> Tuple[bool, str]:
        from datetime import datetime
        sys_a = set(a.entities.get("systems", []))
        sys_b = set(b.entities.get("systems", []))
        sym_a = set(a.entities.get("symptoms", []))
        sym_b = set(b.entities.get("symptoms", []))
        if not (sys_a & sys_b):
            return False, "目标系统不同"
        if not (sym_a & sym_b):
            return False, "症状不同"
        try:
            ta = datetime.fromisoformat(a.reported_at)
            tb = datetime.fromisoformat(b.reported_at)
            gap = abs((tb - ta).total_seconds())
        except Exception:
            return False, "时间无法比较"
        if gap > window_seconds:
            return False, f"时间间隔 {int(gap)}s 超出去重窗口 {window_seconds}s"
        return True, (f"同系统 {sorted(sys_a & sys_b)} + 同症状 {sorted(sym_a & sym_b)}，"
                      f"间隔 {int(gap)}s 在 {window_seconds}s 窗口内")


# --------------------------------------------------------------------------
# Skill 2: AccountAnomalyClassifier
# --------------------------------------------------------------------------

_CATEGORY_RULES: List[Tuple[str, List[str], float]] = [
    ("suspected_compromise", ["盗号", "被盗", "境外", "异地登录", "没操作过", "安全提醒"], 0.93),
    ("mfa_lost", ["验证器", "动态口令", "mfa", "换手机", "二次验证", "otp"], 0.91),
    ("legacy_account", ["u8", "用友", "老财务", "用户不存在"], 0.88),
    ("account_locked", ["锁定", "auth-429", "密码错误", "已被锁"], 0.95),
    ("sso_federation_error", ["saml", "扫码登录", "联邦", "断言", "校验失败"], 0.62),
    ("access_denied", ["进不去", "没权限", "拒绝访问"], 0.70),
]


class AccountAnomalyClassifier:
    """识别账号锁定 / MFA 丢失 / 密码错误被锁 / 异地登录 / 疑似盗号。"""

    name = "AccountAnomalyClassifier"
    CONFIDENCE_THRESHOLD = 0.70

    def run(self, event: NormalizedEvent) -> Classification:
        corpus = (event.raw_text + " " + " ".join(event.attachments_ocr)).lower()

        scored: List[Dict[str, Any]] = []
        for cat, kws, base in _CATEGORY_RULES:
            hits = [k for k in kws if k in corpus]
            if hits:
                # 命中越多置信度略升，上限 0.98
                conf = min(0.98, base + 0.02 * (len(hits) - 1))
                scored.append({"category": cat, "confidence": round(conf, 2), "hits": hits})

        scored.sort(key=lambda x: x["confidence"], reverse=True)

        if not scored:
            return Classification(
                category="unknown", confidence=0.0,
                evidence_fields={"note": "无规则命中"},
                candidates=[], needs_human_review=True,
            )

        top = scored[0]
        evidence = {
            "matched_keywords": top["hits"],
            "systems": event.entities.get("systems", []),
            "error_codes": event.entities.get("error_codes", []),
            "reported_at": event.reported_at,
            "reporter_dept": event.reporter.get("department"),
        }
        return Classification(
            category=top["category"],
            confidence=top["confidence"],
            evidence_fields=evidence,
            candidates=scored[:3],
            needs_human_review=top["confidence"] < self.CONFIDENCE_THRESHOLD,
        )
