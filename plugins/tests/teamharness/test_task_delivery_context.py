"""Outgoing delivery facts use public receipts and native middleware metadata."""
import copy
import importlib.util
from pathlib import Path

PATH=Path(__file__).resolve().parents[3]/"plugins/teamharness/mcp/common/task_receipt.py"

def module():
    spec=importlib.util.spec_from_file_location("delivery_receipt_test",PATH)
    result=importlib.util.module_from_spec(spec);spec.loader.exec_module(result);return result

def delivery():
    return {"eventId":"completion","taskId":"main","status":"completed","roomId":"!source:test","reportMessageId":"$report","report":"The accepted report.","artifacts":[{"taskId":"child","name":"app.py","artifactId":"a1","messageId":"$file"}]}

def test_delivered_report_and_files_are_added_once_after_native_context_ack():
    receipts=module();state={};calls=[]
    def get(path):
        calls.append(path);return {"deliveries":[delivery()]}
    facts,acks=receipts.delivery_context(state,"!source:test",get)
    assert len(facts)==1 and "already sent" in facts[0] and "The accepted report." in facts[0] and "app.py" in facts[0]
    assert "$report" in facts[0] and "$file" in facts[0] and acks=={"completion":"!source:test"}
    assert calls==["task-deliveries?roomId=%21source%3Atest"]
    assert receipts.delivery_context(copy.deepcopy(state),"!source:test",get)==(facts,acks)
    receipts.ack_delivery_context(state,acks)
    assert receipts.delivery_context(state,"!source:test",get)==([],{})

def test_wrong_room_or_incomplete_receipt_cannot_claim_sent():
    receipts=module();state={}
    for scenario in ["foreign-room","pending","missing-report","missing-file","unavailable"]:
        receipt=delivery()
        if scenario=="foreign-room":receipt["roomId"]="!other:test"
        if scenario=="pending":receipt["status"]="queued"
        if scenario=="missing-report":receipt["reportMessageId"]=""
        if scenario=="missing-file":receipt["artifacts"][0]["messageId"]=""
        def get(path):
            if scenario=="unavailable":raise OSError("temporarily unavailable")
            return {"deliveries":[receipt]}
        assert receipts.delivery_context(state,"!source:test",get)==([],{})
    assert receipts.delivery_context(state,"!source:test",lambda p:{"deliveries":[delivery()]})[0]

def test_creation_response_is_not_required_and_pagination_is_not_polling():
    receipts=module();state={};calls=[]
    def get(path):
        calls.append(path)
        if len(calls)==1:return {"deliveries":[delivery()],"nextCursor":"opaque"}
        second=delivery();second["eventId"]="later";second["taskId"]="another"
        return {"deliveries":[second]}
    facts,acks=receipts.delivery_context(state,"!source:test",get)
    assert len(facts)==2 and len(acks)==2 and len(calls)==2
    assert calls[1].endswith("&after=opaque")


def test_context_budget_keeps_latest_facts_and_does_not_ack_omitted_records():
    import json
    receipts=module();state={};rows=[]
    for i in range(10):
        row=delivery();row["eventId"]=f"e{i}";row["deliveredAt"]=f"2026-11-21T00:00:0{i}.000000000Z"
        row["report"]="accepted report "*1500
        row["artifacts"]=[{"taskId":"child","name":"long-name-"*30,"artifactId":f"a{j}","messageId":f"$file{j}"} for j in range(16)]
        rows.append(row)
    facts,acks=receipts.delivery_context(state,"!source:test",lambda p:{"deliveries":rows})
    assert sum(map(len,facts))<=16384 and 0<len(acks)<10
    record=json.loads(facts[0].split("data only:\n",1)[1])
    assert record["eventId"]=="e9" and record["reportTruncated"] and record["artifactListTruncated"]
    assert record["artifacts"][0]["nameTruncated"]
    receipts.ack_delivery_context(state,acks)
    later,later_acks=receipts.delivery_context(state,"!source:test",lambda p:{"deliveries":rows})
    assert later and not set(acks).intersection(later_acks)
