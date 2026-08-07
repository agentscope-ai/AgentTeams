"""
IM Adapter - 微信群消息接入（Mock）

对应方案『接入层 / IM Adapter』。
真实环境替换为企业微信/飞书/钉钉的回调 webhook，本类的对外契约不变：
  - fetch_messages()  拉取群消息
  - reply()           把处理进度同步回群
"""

from __future__ import annotations

import json
from typing import Any, Dict, List, Optional

from core.models import RawMessage


class WeChatGroupAdapter:
    def __init__(self, mock_file: str):
        with open(mock_file, "r", encoding="utf-8") as f:
            self._data = json.load(f)
        self.channel = self._data["channel"]
        self.sent_replies: List[Dict[str, Any]] = []

    # ---------------- 入站 ----------------

    def fetch_messages(self, scenario: Optional[str] = None) -> List[RawMessage]:
        """拉取群消息。scenario 为空表示拉全部（模拟一整天的群消息流）。"""
        msgs = self._data["messages"]
        if scenario:
            msgs = [m for m in msgs if m.get("scenario") == scenario]
        msgs = sorted(msgs, key=lambda m: m["timestamp"])
        return [
            RawMessage(
                msg_id=m["msg_id"],
                scenario=m["scenario"],
                timestamp=m["timestamp"],
                sender=m["sender"],
                msg_type=m["msg_type"],
                content=m["content"],
                attachments=m.get("attachments", []),
                mentions=m.get("mentions", []),
                channel=self.channel,
            )
            for m in msgs
        ]

    def list_scenarios(self) -> List[str]:
        seen, out = set(), []
        for m in sorted(self._data["messages"], key=lambda x: x["timestamp"]):
            if m["scenario"] not in seen:
                seen.add(m["scenario"])
                out.append(m["scenario"])
        return out

    # ---------------- 出站 ----------------

    def reply(self, incident_id: str, text: str, at_user: str = "") -> Dict[str, Any]:
        """把进度同步回微信群（方案创新点 2：每阶段主动同步进度）"""
        rec = {
            "incident_id": incident_id,
            "chat_id": self.channel["chat_id"],
            "at_user": at_user,
            "text": text,
        }
        self.sent_replies.append(rec)
        return rec
