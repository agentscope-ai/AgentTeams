"""Consume a policy-admitted Matrix answer; never grant tool or Task approval."""
import datetime
import json
import sys
import urllib.parse
import urllib.request
import os

def apply_answer(task, room, event_id, sender, content, owner, now):
    answer = content.get("com.agentteams.answer") or {}
    if task.get("room_id") != room or task.get("status") in {"cancelled", "completed", "accepted", "revision", "blocked"}:
        return False
    records = [record for record in task.get("attention", [])
        if record.get("confirmation_id") == answer.get("confirmation_id")]
    if len(records) != 1 or (answer.get("agent_id") is not None and answer["agent_id"] != owner):
        return False
    record = records[0]
    if task.get("decision_status") == "frozen" and record.get("kind") != "approval":
        return False
    if sender not in record.get("requested_of", []):
        return False
    prior = record.get("answer")
    if prior:
        return prior.get("event_id") == event_id
    if record.get("resolved"):
        return False
    deadline = record.get("timeoutAt") or record.get("expiresAt")
    if deadline and datetime.datetime.fromisoformat(deadline.replace("Z", "+00:00")).timestamp() <= now:
        return False
    responses = answer.get("responses")
    if not isinstance(responses, list) or len(responses) != 1 or not isinstance(responses[0], dict):
        return False
    response = responses[0]
    selected, custom = response.get("selected", []), response.get("custom", "")
    options = {option["id"] for option in record.get("options", [])}
    if (response.get("id") != "decision" or not isinstance(selected, list) or len(selected) > 1
            or any(not isinstance(item, str) or item not in options for item in selected)
            or not isinstance(custom, str) or (not selected and not custom.strip())):
        return False
    record["answer"] = {"event_id": event_id, "sender": sender, "responses": responses,
                        "received_at": datetime.datetime.fromtimestamp(now, datetime.timezone.utc).isoformat()}
    # Business approval provenance stays under its existing human-only gate.
    # Recording a human's answer is not authorization to unfreeze a Task.
    if record.get("kind") != "approval":
        record["resolved"] = True
        record["resolvedVia"] = "im_reply"
        record["resolvedBy"] = sender
    return True


def consume(request):
    import server
    content = request["source"].get("content") or {}
    answer = content.get("com.agentteams.answer") or {}
    parts = str(answer.get("confirmation_id", "")).split(":")
    if len(parts) != 3 or parts[0] != "attention":
        return {"accepted": False}
    task_id = server._safe_id(parts[1], "taskId")
    arguments = {}
    class InvalidAnswer(Exception):
        pass

    def mutate(task):
        if not apply_answer(task, request["room_id"], request["event_id"], request["source"].get("sender"),
                            content, server._matrix_user_id(), datetime.datetime.now().timestamp()):
            raise InvalidAnswer()

    from mcp.artifact.sync import _commit_task_guarded
    try:
        _commit_task_guarded(arguments, task_id, mutate, action="answer_attention", exclude=["spec.md", "base/"])
    except InvalidAnswer:
        return {"accepted": False}
    receipt = {"msgtype": "m.text", "body": "Question answered", "org.agentteams.run": {
        "version": "1", "blocks": [{"type": "confirmation", "tool_name": "request_attention",
            "confirmation_id": answer["confirmation_id"], "status": "answered"}]}}
    room = urllib.parse.quote(request["room_id"], safe="")
    txn = urllib.parse.quote(answer["confirmation_id"] + ":answered", safe="")
    url = os.environ["AGENTTEAMS_MATRIX_URL"].rstrip("/") + f"/_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}"
    req = urllib.request.Request(url, method="PUT", data=json.dumps(receipt).encode(), headers={
        "Authorization": "Bearer " + os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"], "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as response:
        if not json.load(response).get("event_id"):
            raise RuntimeError("Human answer receipt was not accepted by Matrix")
    return {"accepted": True}


if __name__ == "__main__":
    print(json.dumps(consume(json.load(sys.stdin))))
