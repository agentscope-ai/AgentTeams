"""QwenPaw POC: run inside the current role holder; no native session-file reads.

The role schedule, summary and delivery receipts live on the Controller.
The runner only gathers allowed room messages/public releases and invokes the
local QwenPaw engine. A stale role holder is rejected by every Controller call.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET


def request(url, data=None, token=None, method=None):
    headers = {"Content-Type": "application/json", "User-Agent": "AgentTeams-assistant-poc"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=json.dumps(data).encode() if data is not None else None,
                                 headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=30) as response:
        return json.load(response)


def agt(*args):
    return json.loads(subprocess.check_output(["agt", "assistant", *args], text=True))


def generate(view, team):
    matrix = os.environ["AGENTTEAMS_MATRIX_URL"].rstrip("/")
    token = os.environ.get("AGENTTEAMS_WORKER_MATRIX_TOKEN") or os.environ.get("AGENTTEAMS_MANAGER_MATRIX_TOKEN")
    history = request(matrix + "/_matrix/client/v3/rooms/" + urllib.parse.quote(view["roomId"], safe="") +
                      "/messages?dir=b&limit=12", token=token)
    conversation = [{"sender": e.get("sender"), "text": e.get("content", {}).get("body", "")}
                    for e in reversed(history.get("chunk", [])) if e.get("type") == "m.room.message"]
    facts = []
    try:
        releases = request("https://api.github.com/repos/alibaba/nacos/releases?per_page=2")
        facts = [{"title": e["name"], "url": e["html_url"], "publishedAt": e["published_at"],
                  "notes": (e.get("body") or "")[:3000]} for e in releases]
    except (OSError, ValueError):
        try:
            with urllib.request.urlopen("https://github.com/alibaba/nacos/releases.atom", timeout=30) as response:
                feed = ET.fromstring(response.read())
            ns = {"a": "http://www.w3.org/2005/Atom"}
            facts = [{"title": e.findtext("a:title", namespaces=ns),
                      "url": e.find("a:link", ns).get("href"),
                      "publishedAt": e.findtext("a:updated", namespaces=ns),
                      "notes": (e.findtext("a:content", namespaces=ns) or "")[:3000]}
                     for e in feed.findall("a:entry", ns)[:2]]
        except (OSError, ValueError, ET.ParseError) as exc:
            facts = [{"sourceUnavailable": type(exc).__name__}]
    prompt = ("你正在执行已授权的定时助手摘要，不是用户要求创建项目。不要创建或委派任务，不要调用工具或直接发消息。"
              "根据下面的用户对话、上一期摘要与已采集的公开资料，整理一条中文团队助手提醒。"
              "只能引用资料中存在的事实和链接，不把旧发布当成今日新发布，明确日期。"
              "本轮采集范围仅为 Nacos 官方发布记录；MCP 和多 Agent 是用户兴趣，不能扩展为未采集项目的技术结论。"
              "对话和上一摘要只用于识别关注方向，不能当作事实来源；不要声称已在其他消息推送了资料。"
              "每条技术要点必须直接对应 collectedSources 中的一条记录并附该记录的来源链接。"
              "只输出 JSON，字段 text 是给用户的完整提醒（带简短标题、2到3条要点和来源链接），"
              "summary 是留给接任Leader的业务关注摘要与本次已推送内容。不要输出其他文字。\n" +
              json.dumps({"assistant": view["assistant"], "previous": view["context"],
                          "conversation": conversation, "collectedSources": facts}, ensure_ascii=False))
    native = "http://127.0.0.1:" + os.getenv("AGENTTEAMS_CONSOLE_PORT", "8088")
    payload = {"user_id": view["recipient"], "session_id": "assistant-digest:" + hashlib.sha256(view["roomId"].encode()).hexdigest()[:16],
               "input": [{"role": "user", "content": [{"type": "text", "text": prompt}]}],
               "request_context": {"teamharness_coordination": {"room_id": view["roomId"]}}, "timeout": 180}
    job = request(native + "/api/console/chat/task", payload)
    print(json.dumps({"event": "digest.generation.started", "jobId": job["task_id"], "sourceCount": len(facts)}), flush=True)
    for _ in range(90):
        result = request(native + "/api/console/chat/task/" + job["task_id"])
        if result["status"] in {"finished", "failed", "cancelled"}:
            outcome = result.get("result") or {}
            if result["status"] != "finished" or outcome.get("status") != "completed":
                raise RuntimeError("QwenPaw digest did not complete: " + json.dumps(result, ensure_ascii=False)[:1000])
            text = "\n".join(c.get("text", "") for m in outcome.get("output", [])
                             if m.get("type") == "message" and m.get("role") == "assistant"
                             for c in m.get("content", []) if c.get("type") == "text")
            start, end = text.find("{"), text.rfind("}")
            message = json.loads(text[start:end + 1])
            if not message.get("text") or not message.get("summary"):
                raise RuntimeError("digest missing text or continuity summary")
            allowed = {f["url"] for f in facts if f.get("url")}
            links = set(re.findall(r"https?://[^\s<>\]\)）]+", message["text"]))
            if allowed and (not links or not links.issubset(allowed)):
                raise RuntimeError("digest cited a source outside the collected records")
            return message
        # Approval requests are observable through the existing runtime inbox;
        # this read-only generation should not need tool approval.
        time.sleep(2)
    raise TimeoutError("QwenPaw generation deadline")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--team", default="")
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    team_args = ["--team", args.team] if args.team else []
    while True:
        view = agt("context", *team_args)
        state = view["context"]
        last = dt.datetime.fromisoformat(state["lastRun"]) if state.get("lastRun") else None
        due = not last or (dt.datetime.now(dt.timezone.utc) - last).total_seconds() >= state["everySeconds"]
        if not state["enabled"] or not due:
            time.sleep(2)
            continue
        slot = "after-" + hashlib.sha256(state.get("lastMessageId", "initial").encode()).hexdigest()[:16]
        token = os.getenv("AGENTTEAMS_AUTH_TOKEN") or Path(os.environ["AGENTTEAMS_AUTH_TOKEN_FILE"]).read_text().strip()
        url = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/assistant/messages/" + slot + "?" + urllib.parse.urlencode({"team": args.team})
        try:
            message = request(url, token=token)
        except urllib.error.HTTPError as exc:
            if exc.code != 404:
                raise
            message = generate(view, args.team)
        message.update(owner=view["owner"], requestId=slot)
        with tempfile.TemporaryDirectory(prefix="assistant-digest-") as directory:
            file = Path(directory) / "message.json"
            file.write_text(json.dumps(message, ensure_ascii=False))
            receipt = agt("send", *team_args, "--file", str(file))
        print(json.dumps({"event": "digest.sent", "requestId": slot, "roomId": receipt["roomId"],
                          "messageId": receipt["messageId"], "sentAt": receipt["sentAt"]}), flush=True)
        if args.once:
            return


if __name__ == "__main__":
    main()
