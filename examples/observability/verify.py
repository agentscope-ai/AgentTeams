"""Read Langfuse back to verify native runtime traces, not just HTTP export."""

import base64
import json
import time
from urllib.request import Request, urlopen

BASE = "http://127.0.0.1:13100/api/public"
AUTH = "Basic " + base64.b64encode(b"pk-lf-at-local-demo:sk-lf-at-local-demo").decode()


def read(path):
    with urlopen(Request(BASE + path, headers={"Authorization": AUTH}), timeout=10) as response:
        return json.load(response)


def main():
    pending = {"qwenpaw", "hermes"}
    evidence = []
    deadline = time.monotonic() + 120
    while pending and time.monotonic() < deadline:
        traces = read("/traces?limit=50&tags=agentteams&orderBy=timestamp.desc")["data"]
        for summary in traces:
            runtime = next((r for r in pending if r in summary.get("tags", [])), None)
            if not runtime:
                continue
            trace = read("/traces/" + summary["id"])
            observations = trace.get("observations", [])
            generations = [o for o in observations if o["type"] == "GENERATION"]
            if not generations:
                continue
            usage = sum(o.get("usage", {}).get("total", 0) or
                        sum((o.get("usageDetails") or {}).get(k, 0) for k in ("input", "output"))
                        for o in generations)
            if not usage:
                continue
            ids = {o["id"] for o in observations}
            if len(observations) < 3 or not any(o.get("parentObservationId") in ids for o in generations):
                continue
            if any(o.get("level") == "ERROR" for o in observations):
                continue
            evidence.append({"runtime": runtime, "traceId": trace["id"],
                             "worker": trace.get("userId"), "tags": trace.get("tags"),
                             "spans": len(observations), "generations": len(generations),
                             "tokens": usage, "source": trace.get("metadata", {}).get("model_source")})
            pending.remove(runtime)
        if pending:
            time.sleep(3)
    print(json.dumps({"verified": evidence, "missing": sorted(pending)}, ensure_ascii=False, indent=2))
    if pending:
        raise SystemExit("Native runtime traces have not passed Langfuse readback")


if __name__ == "__main__":
    main()
