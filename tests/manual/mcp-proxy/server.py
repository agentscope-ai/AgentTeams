"""HTTP API + official SDK SSE/Streamable HTTP fixture for AT gateway tests."""
import base64
import json
import os
from contextlib import AsyncExitStack, asynccontextmanager
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import parse_qs

import uvicorn
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from starlette.applications import Starlette
from starlette.responses import JSONResponse
from starlette.routing import Mount, Route

SECRET = os.environ["MCP_TEST_SECRET"]
AUDIT = Path(os.environ.get("MCP_TEST_AUDIT", "/tmp/at-mcp-audit.jsonl"))
SERVERS = []


def audit(**fields):
    with AUDIT.open("a") as output:
        output.write(json.dumps({"time": datetime.now(timezone.utc).isoformat(), **fields}) + "\n")


class Credentials:
    def __init__(self, app, mode):
        self.app, self.mode = app, mode

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            return await self.app(scope, receive, send)
        headers = dict(scope["headers"])
        query = parse_qs(scope["query_string"].decode())
        expected = {
            "bearer": (headers.get(b"authorization", b"").decode(), f"Bearer {SECRET}"),
            "basic": (headers.get(b"authorization", b"").decode(), "Basic " + base64.b64encode(f"tester:{SECRET}".encode()).decode()),
            "header": (headers.get(b"x-api-key", b"").decode(), SECRET),
            "query": (query.get("api_key", [""])[0], SECRET),
            "none": ("", ""),
        }
        actual, required = expected[self.mode]
        accepted = actual == required
        audit(mode=self.mode, path=scope["path"], method=scope["method"], accepted=accepted,
              marker=headers.get(b"x-test-marker", b"").decode(), tenant=query.get("tenant", [""])[0])
        if not accepted:
            return await JSONResponse({"error": "invalid backend credential"}, status_code=401)(scope, receive, send)

        async def record_receive():
            message = await receive()
            if message.get("body"):
                try:
                    body = json.loads(message["body"])
                    audit(mode=self.mode, rpc=body.get("method"), arguments=body.get("params", {}).get("arguments"))
                except (ValueError, AttributeError):
                    pass
            return message

        await self.app(scope, record_receive, send)


def backend(mode):
    mcp = FastMCP(f"at-fixture-{mode}", streamable_http_path="/", json_response=False,
                  transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False))
    SERVERS.append(mcp)

    @mcp.tool()
    def echo(text: str) -> dict:
        """Return text and the fixture authentication mode."""
        audit(mode=mode, tool="echo", text=text)
        return {"text": text, "mode": mode, "source": "official-mcp-sdk"}

    async def http_echo(request):
        text = request.query_params.get("text", "")
        audit(mode=mode, tool="http_echo", text=text)
        return JSONResponse({"text": text, "mode": mode, "source": "native-http"})

    return Credentials(Starlette(routes=[Route("/api/echo", http_echo),
        Mount("/stream", app=mcp.streamable_http_app()),
        Mount("/", app=mcp.sse_app())]), mode)


@asynccontextmanager
async def lifespan(app):
    async with AsyncExitStack() as stack:
        for server in SERVERS:
            await stack.enter_async_context(server.session_manager.run())
        yield


app = Starlette(routes=[Mount(f"/{mode}", app=backend(mode)) for mode in ["none", "bearer", "basic", "header", "query"]], lifespan=lifespan)

if __name__ == "__main__":
    uvicorn.run(app, host=os.environ.get("MCP_TEST_HOST", "127.0.0.1"), port=int(os.environ.get("MCP_TEST_PORT", "19100")), access_log=False)
