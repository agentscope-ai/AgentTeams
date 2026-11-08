import asyncio
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib/telemetry"))

from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

import at_telemetry as telemetry


def test_concurrent_turns_do_not_share_identity(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "demo-worker")
    monkeypatch.setenv("AGENTTEAMS_OTEL_RUNTIME", "qwenpaw")
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(telemetry.IdentityProcessor())
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)

    async def turn(task):
        with telemetry.operation("worker.turn", task_id=task, room_id=f"!{task}:matrix"):
            await asyncio.sleep(0)
            with telemetry.operation("tool.lookup"):
                await asyncio.sleep(0)

    async def run():
        await asyncio.gather(turn("a"), turn("b"))

    asyncio.run(run())
    spans = exporter.get_finished_spans()
    assert len(spans) == 4
    for task in ("a", "b"):
        own = [s for s in spans if s.attributes["agentteams.task.id"] == task]
        assert len(own) == 2
        assert len({s.context.trace_id for s in own}) == 1
        root = next(s for s in own if not s.parent)
        child = next(s for s in own if s.parent)
        assert child.parent.span_id == root.context.span_id
        assert root.attributes["langfuse.session.id"] == telemetry.session_key(f"!{task}:matrix")
        assert f"task:{task}" in root.attributes["langfuse.trace.tags"]
    assert telemetry._correlation.get() == {}
    try:
        with telemetry.operation("worker.failure", task_id="failed"):
            raise ValueError("demo error")
    except ValueError:
        pass
    assert telemetry._correlation.get() == {}
    failure = exporter.get_finished_spans()[-1]
    assert failure.status.status_code.name == "ERROR"
    assert failure.attributes["agentteams.task.id"] == "failed"
