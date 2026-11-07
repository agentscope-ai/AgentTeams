"""Verify persisted native sessions, tools, model content and causal handoffs."""

import argparse
import json
from pathlib import Path
import subprocess
import time
from urllib.parse import urlencode
from urllib.request import urlopen

from verify import read


def metadata(observation):
    meta = observation.get("metadata") or {}
    return {**meta.get("attributes", {}), **meta}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--run", required=True)
    args = parser.parse_args()
    evidence_dir = Path(__file__).resolve().parents[2] / "output/observability-demo"
    traces = []
    for worker in ("trace-qwenpaw", "trace-hermes"):
        for summary in read("/traces?" + urlencode({"limit": 100, "tags": "worker-turn", "userId": worker, "orderBy": "timestamp.desc"}))["data"]:
            detail = read("/traces/" + summary["id"])
            roots = [o for o in detail["observations"] if o["name"] == "worker.turn"]
            if len(roots) != 1 or roots[0].get("parentObservationId"):
                continue
            if f"tracing-{args.run}.txt" not in json.dumps(roots[0].get("input")):
                continue
            traces.append(detail)
    report = []
    for worker in ("trace-qwenpaw", "trace-hermes"):
        own = [t for t in traces if t["userId"] == worker]
        main = [t for t in own if not t.get("metadata", {}).get("agentteams.upstream.trace_id")]
        tool_turns = [t for t in main if any(o["type"] == "TOOL" for o in t["observations"])]
        assert len(tool_turns) >= 2, f"{worker}: fewer than two independent tool turns"
        assert len({t["sessionId"] for t in main}) == 1, f"{worker}: human session split"
        expected_task = "matrix-" + worker.removeprefix("trace-") + "-acceptance"
        runtime = worker.removeprefix("trace-")
        artifact = subprocess.check_output(["docker", "exec", f"at-tracing-worker-{runtime}",
            f"/opt/venv/{runtime}/bin/python", "-c", "from pathlib import Path; import sys; print(Path(sys.argv[1]).read_text())",
            f"/root/agentteams-fs/agents/{worker}/tracing-{args.run}.txt"], text=True)
        assert "MATRIX_TRACE_OK" in artifact and "MATRIX_SECOND_TURN_OK" in artifact, f"{worker}: real artifact incomplete"
        assert all(t.get("metadata", {}).get("agentteams.task.id") == expected_task for t in own), f"{worker}: wrong task attribution"
        assert any(o["level"] == "ERROR" and o["type"] == "TOOL" for t in own for o in t["observations"]), f"{worker}: missing returned tool failure"
        for t in own:
            observations = t["observations"]
            generations = [o for o in observations if o["type"] == "GENERATION"]
            tools = [o for o in observations if o["type"] == "TOOL"]
            sends = [o for o in observations if o["name"] == "matrix.send"]
            ids = {o["id"] for o in observations}
            assert generations and all(o.get("input") is not None and o.get("output") is not None for o in generations), f"{t['id']}: missing model content"
            assert all((o.get("usageDetails") or {}).get("input") is not None for o in generations), f"{t['id']}: missing model usage"
            assert all(o.get("input") is not None and o.get("output") is not None for o in tools), f"{t['id']}: missing tool content"
            delivery_actions = [metadata(o).get("agentteams.delivery.action") for o in observations if o["name"] == "matrix.delivery_decision"]
            assert sends or "retrigger" in delivery_actions, f"{t['id']}: reply detached from turn without delivery decision"
            assert all(not o.get("parentObservationId") or o["parentObservationId"] in ids for o in observations), f"{t['id']}: orphan span"
            response_ids = [metadata(o).get("gen_ai.response.id") for o in generations]
            known_ids = [i for i in response_ids if i]
            assert len(known_ids) == len(set(known_ids)), f"{t['id']}: double-counted model response"
            params = urlencode({"session": t["sessionId"]})
            with urlopen("http://localhost:13003/api/agentteams/traces/?" + params, timeout=15) as response:
                listed = json.load(response)
            assert listed["meta"]["totalItems"] >= (1 if t not in main else 2), "Dashboard cannot browse complete session"
            with urlopen("http://localhost:13003/api/agentteams/traces/" + t["id"] + "/", timeout=15) as response:
                dashboard = json.load(response)
            assert len(dashboard["observations"]) == len(observations), "Dashboard dropped observations"
            report.append({"worker": worker, "traceId": t["id"], "sessionId": t["sessionId"], "spans": len(observations),
                "generations": len(generations), "tools": len(tools), "sends": len(sends),
                "knownModelResponseIds": len(known_ids),
                "toolErrors": sum(o["level"] == "ERROR" for o in tools),
                "deliveryActions": delivery_actions,
                "tokens": sum(sum((o.get("usageDetails") or {}).get(k, 0) for k in ("input", "output")) for o in generations),
                "upstreamTraceId": t.get("metadata", {}).get("agentteams.upstream.trace_id")})
    linked = [t for t in traces if t.get("metadata", {}).get("agentteams.upstream.trace_id")]
    assert linked, "No persisted cross-worker handoff"
    assert any(t["userId"] == "trace-hermes" and any(up["id"] == t["metadata"]["agentteams.upstream.trace_id"] and up["userId"] == "trace-qwenpaw" for up in traces) for t in linked), "Handoff does not point to the sending Worker"
    result = {"verifiedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "run": args.run, "verified": report}
    (evidence_dir / "full-session-readback.json").write_text(json.dumps(traces, ensure_ascii=False, indent=2))
    (evidence_dir / "full-session-acceptance.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
