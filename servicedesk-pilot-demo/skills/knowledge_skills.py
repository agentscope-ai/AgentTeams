"""
知识类 Skills（方案 10.2）

  Skill 3  RunbookRag               —— 通用·高复用：检索 Runbook / FAQ / 标准处置流程
  Skill 4  CaseRetrieval            —— 通用：检索历史成功/失败案例
  Skill 5  LegacySystemOperatorRag  —— 场景相关：无接口老系统人工处置经验

检索实现为轻量 BM25-lite（关键词加权 + 类别匹配 + 时效性过滤），
不引入外部依赖；真实环境替换为 PolarDB pgvector 等向量检索，接口契约不变。
"""

from __future__ import annotations

import json
from datetime import date, datetime
from typing import Any, Dict, List, Optional, Tuple

from core.models import KnowledgeResult, NormalizedEvent


def _tokenize(text: str) -> List[str]:
    """极简中英文混合切词：英文按词，中文按 2-gram。

    注意：只保留中文 2-gram，不保留单字。单字（的/不/人…）在长短文本中
    几乎必然共现，会制造大量噪声重叠、拉高跨类 Runbook 的伪命中分，
    导致『知识缺口』误判为假命中。2-gram 才能表达真实语义。
    """
    import re
    text = text.lower()
    en = re.findall(r"[a-z0-9\-]{2,}", text)
    zh_chars = re.findall(r"[\u4e00-\u9fa5]", text)
    zh_bigrams = ["".join(zh_chars[i:i + 2]) for i in range(len(zh_chars) - 1)]
    return en + zh_bigrams


class KnowledgeBase:
    """知识库读写封装。KnowledgeReflector 通过它真实写回 JSON 文件。"""

    def __init__(self, kb_file: str):
        self.path = kb_file
        self.reload()

    def reload(self) -> None:
        with open(self.path, "r", encoding="utf-8") as f:
            self.data = json.load(f)

    def save(self) -> None:
        from core.models import now_iso
        self.data["last_updated"] = now_iso()
        with open(self.path, "w", encoding="utf-8") as f:
            json.dump(self.data, f, ensure_ascii=False, indent=2)

    def snapshot_counts(self) -> Dict[str, int]:
        return {
            "runbooks": len(self.data.get("runbooks", [])),
            "cases": len(self.data.get("cases", [])),
            "legacy_notes": len(self.data.get("legacy_operator_notes", [])),
            "badcases": len(self.data.get("badcases", [])),
        }


# --------------------------------------------------------------------------
# Skill 3: RunbookRag
# --------------------------------------------------------------------------

class RunbookRag:
    name = "RunbookRag"
    TOP_K = 3
    MIN_SCORE = 0.30   # 低于此分视为无有效召回 → 知识缺口

    def __init__(self, kb: KnowledgeBase):
        self.kb = kb

    def run(self, query: str, category: str, top_k: int = TOP_K,
            today: Optional[date] = None) -> Tuple[List[Dict[str, Any]], bool, str]:
        today = today or date.today()
        q_tokens = set(_tokenize(query))

        scored: List[Tuple[float, Dict[str, Any]]] = []
        for rb in self.kb.data.get("runbooks", []):
            doc_tokens = set(_tokenize(" ".join(
                [rb["title"], rb["category"]] + rb.get("keywords", [])
            )))
            overlap = q_tokens & doc_tokens
            if not overlap:
                continue
            score = len(overlap) / max(6, len(doc_tokens) ** 0.5 * 3)
            if rb["category"] == category:
                score += 0.45          # 类别精确命中强加权
            score += 0.10 * rb.get("success_rate", 0)

            # 时效性：方案 6.3 过期 Runbook 只能作为参考
            expired = False
            try:
                if datetime.strptime(rb["expires_at"], "%Y-%m-%d").date() < today:
                    expired = True
                    score *= 0.4
            except Exception:
                pass

            item = dict(rb)
            item["_score"] = round(min(score, 0.99), 3)
            item["_expired"] = expired
            item["_matched_terms"] = sorted(list(overlap))[:8]
            scored.append((item["_score"], item))

        scored.sort(key=lambda x: x[0], reverse=True)
        hits = [it for sc, it in scored[:top_k] if sc >= self.MIN_SCORE]

        if not hits:
            reason = (f"检索 '{query}' 无得分高于 {self.MIN_SCORE} 的 Runbook；"
                      f"候选 {len(scored)} 条均不适用，不编造方案")
            return [], True, reason
        return hits, False, ""


# --------------------------------------------------------------------------
# Skill 4: CaseRetrieval
# --------------------------------------------------------------------------

class CaseRetrieval:
    name = "CaseRetrieval"
    TOP_K = 2
    MIN_SCORE = 0.25

    def __init__(self, kb: KnowledgeBase):
        self.kb = kb

    def run(self, query: str, category: str, top_k: int = TOP_K) -> List[Dict[str, Any]]:
        q_tokens = set(_tokenize(query))
        scored: List[Tuple[float, Dict[str, Any]]] = []
        for cs in self.kb.data.get("cases", []):
            doc_tokens = set(_tokenize(" ".join(
                [cs["title"], cs["category"]] + cs.get("keywords", [])
            )))
            overlap = q_tokens & doc_tokens
            if not overlap:
                continue
            score = len(overlap) / max(6, len(doc_tokens) ** 0.5 * 3)
            if cs["category"] == category:
                score += 0.5
            item = dict(cs)
            item["_score"] = round(min(score, 0.99), 3)
            scored.append((item["_score"], item))

        scored.sort(key=lambda x: x[0], reverse=True)
        return [it for sc, it in scored[:top_k] if sc >= self.MIN_SCORE]


# --------------------------------------------------------------------------
# Skill 5: LegacySystemOperatorRag
# --------------------------------------------------------------------------

class LegacySystemOperatorRag:
    name = "LegacySystemOperatorRag"

    def __init__(self, kb: KnowledgeBase):
        self.kb = kb

    def run(self, system_id: str, operation_hint: str = "") -> List[Dict[str, Any]]:
        notes = [n for n in self.kb.data.get("legacy_operator_notes", [])
                 if n.get("system") == system_id]
        if operation_hint:
            hint_tokens = set(_tokenize(operation_hint))
            notes.sort(
                key=lambda n: len(hint_tokens & set(_tokenize(n.get("operation", "")))),
                reverse=True,
            )
        return notes
