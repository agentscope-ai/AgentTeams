"""Exercise the shipped mapping in a real Collector, without a tracing backend."""
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request
import uuid

import pytest
import yaml


@pytest.mark.skipif(not os.getenv("AT_TEST_COLLECTOR_IMAGE"), reason="requires a local Collector image")
def test_native_identity_mapping_preserves_spans_and_existing_backend_attributes(tmp_path):
    config = yaml.safe_load((Path(__file__).resolve().parents[1] / "examples/observability/collector.yaml").read_text())
    config["receivers"] = {"otlp": {"protocols": {"http": {"endpoint": "0.0.0.0:4318"}}}}
    config.pop("extensions")
    config["exporters"] = {"file": {"path": "/evidence/spans.json"}}
    config["service"].pop("extensions")
    config["service"]["pipelines"]["traces"]["exporters"] = ["file"]
    config_path = tmp_path / "collector.yaml"
    config_path.write_text(yaml.safe_dump(config))
    tmp_path.chmod(0o777)
    config_path.chmod(0o644)
    name = "at-identity-test-" + uuid.uuid4().hex[:10]
    spans = []
    for index, runtime in enumerate(["codex", "qwenpaw", "hermes", "deepseek-harness", "claudecode", "existing", "unrelated"], 1):
        attributes = {"agentteams.worker.name": "member", "agentteams.runtime": runtime,
                      "agentteams.run.id": "run", "agentteams.room.id": "!room:test",
                      "agentteams.task.id": "task", "agentteams.main_task.id": "main", "agentteams.team.id": "team",
                      "gen_ai.agent.id": "member", "gen_ai.agent.name": "member",
                      "agentteams.upstream.trace_id": "a" * 32, "agentteams.upstream.span_id": "b" * 16}
        if runtime == "existing":
            attributes.update({"langfuse.user.id": "existing-user", "langfuse.trace.tags": ["existing-tag"],
                               "langfuse.trace.metadata.agentteams.task.id": "existing-task"})
        if runtime == "unrelated":
            attributes = {"service.name": "unrelated"}
        encoded = [{"key": key, "value": {"arrayValue": {"values": [{"stringValue": v} for v in value]}} if isinstance(value, list)
                    else {"stringValue": value}} for key, value in attributes.items()]
        spans.append({"traceId": f"{index:032x}", "spanId": f"{index:016x}", "name": runtime,
                      "startTimeUnixNano": "1795737600000000000", "endTimeUnixNano": "1795737601000000000",
                      "attributes": encoded})
    try:
        subprocess.run(["docker", "run", "-d", "--name", name, "-p", "127.0.0.1::4318", "-v", f"{tmp_path}:/evidence",
                        os.environ["AT_TEST_COLLECTOR_IMAGE"], "--config=/evidence/collector.yaml"], check=True, capture_output=True)
        port = subprocess.check_output(["docker", "port", name, "4318/tcp"], text=True).strip().rsplit(":", 1)[1]
        payload = json.dumps({"resourceSpans": [{"scopeSpans": [{"spans": spans}]}]}).encode()
        for attempt in range(40):
            try:
                request = urllib.request.Request(f"http://127.0.0.1:{port}/v1/traces", payload, {"Content-Type": "application/json"})
                with urllib.request.urlopen(request, timeout=2) as response:
                    assert response.status == 200
                break
            except OSError:
                if attempt == 39:
                    raise AssertionError(subprocess.check_output(["docker", "logs", name], stderr=subprocess.STDOUT, text=True))
                time.sleep(0.1)
        output = tmp_path / "spans.json"
        records = []
        for _ in range(50):
            if output.exists():
                records = [json.loads(line) for line in output.read_text().splitlines() if line]
                if records:
                    break
            time.sleep(0.1)
        actual = {span["name"]: span for record in records for resource in record["resourceSpans"]
                  for scope in resource["scopeSpans"] for span in scope["spans"]}
        assert len(actual) == len(spans)
        for original in spans:
            mapped = actual[original["name"]]
            for field in ("traceId", "spanId", "startTimeUnixNano", "endTimeUnixNano"):
                assert mapped[field] == original[field]
            attrs = {item["key"]: item["value"] for item in mapped["attributes"]}
            if original["name"] == "unrelated":
                assert "langfuse.user.id" not in attrs
            elif original["name"] == "existing":
                assert attrs["langfuse.user.id"]["stringValue"] == "existing-user"
                assert attrs["langfuse.trace.tags"]["arrayValue"]["values"] == [{"stringValue": "existing-tag"}]
                assert attrs["langfuse.trace.metadata.agentteams.task.id"]["stringValue"] == "existing-task"
            else:
                assert attrs["langfuse.user.id"]["stringValue"] == "member"
                tags = [value["stringValue"] for value in attrs["langfuse.trace.tags"]["arrayValue"]["values"]]
                assert set(tags) == {"agentteams", "worker-turn", original["name"], "task:task", "main_task:main", "team:team", "room:!room:test"}
                assert attrs["langfuse.trace.metadata.agentteams.run.id"]["stringValue"] == "run"
                assert attrs["langfuse.trace.metadata.agentteams.main_task.id"]["stringValue"] == "main"
                assert attrs["langfuse.trace.metadata.gen_ai.agent.id"]["stringValue"] == "member"
                assert attrs["langfuse.trace.metadata.agentteams.upstream.trace_id"]["stringValue"] == "a" * 32
                assert attrs["langfuse.trace.metadata.agentteams.upstream.span_id"]["stringValue"] == "b" * 16
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)
