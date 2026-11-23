"""Exercise the real Dashboard management BFF (browser login state required)."""
import argparse
import json
import time
from pathlib import Path

import httpx

parser = argparse.ArgumentParser()
parser.add_argument("--dashboard", default="http://127.0.0.1:13005")
parser.add_argument("--backend", default="http://host.docker.internal:19100")
parser.add_argument("--state", required=True)
parser.add_argument("--secret-file", required=True)
parser.add_argument("--auth", choices=["bearer", "basic", "header", "query", "none"], default="bearer")
parser.add_argument("--credential", choices=["valid", "wrong", "missing"], default="valid")
parser.add_argument("--output", required=True)
args = parser.parse_args()
secret = Path(args.secret_file).read_text().strip()
cookies = {c["name"]: c["value"] for c in json.loads(Path(args.state).read_text())["cookies"]}
client = httpx.Client(base_url=args.dashboard, cookies=cookies, timeout=40, follow_redirects=True)


def request(payload):
    response = client.post("/api/agentteams/mcps/manage", json=payload)
    return {"status": response.status_code, "body": response.json()}


results = []
for kind in ["rest", "sse", "stream"]:
    name = f"at-mcp-{kind}-1116"
    auth = {"type": "none"}
    if args.credential != "missing" and args.auth != "none":
        auth = {"type": {"header": "api-key", "query": "api-key"}.get(args.auth, args.auth),
                "secret": secret if args.credential == "valid" else "incorrect-test-credential"}
        if args.auth == "basic":
            auth["username"] = "tester"
        if args.auth in ["header", "query"]:
            auth.update(name="X-API-Key" if args.auth == "header" else "api_key", **{"in": "header" if args.auth == "header" else "query"})
    suffix = "" if kind == "rest" else "/sse" if kind == "sse" else "/stream/"
    connection = {"kind": "rest" if kind == "rest" else "proxy", "backendUrl": f"{args.backend}/{args.auth}{suffix}",
                  "transport": "sse" if kind == "sse" else "streaminghttp", "timeout": 10000, "auth": auth,
                  "headers": [{"name": "X-Test-Marker", "value": f"{kind}-{args.auth}-{args.credential}"}],
                  "query": [{"name": "tenant", "value": "at-regression"}], "tools": []}
    if kind == "rest":
        connection["tools"] = [{"name": "echo", "description": "Echo a native HTTP query through MCP", "method": "GET", "path": "/api/echo",
                                "parameters": [{"name": "text", "type": "string", "position": "query", "required": True}]}]
    saved = request({"action": "save", "input": {"name": name, "connection": connection}})
    result = {"name": name, "auth": args.auth, "credential": args.credential, "save": saved}
    print(name, "save", saved["status"], flush=True)
    if saved["status"] == 200:
        time.sleep(3)  # Wait for the gateway's configuration subscription.
        verified = request({"action": "verify", "name": name})
        for delay in [3, 5, 8, 12, 12]:
            if verified["body"].get("success") or args.credential != "valid":
                break
            time.sleep(delay)
            verified = request({"action": "verify", "name": name})
        result["verify"] = verified
        text = f"gateway-{kind}-{args.auth}-{args.credential}"
        result["call"] = request({"action": "call", "name": name, "tool": "echo", "arguments": {"text": text}})
        print(name, "verify", verified["body"].get("success"), "call", result["call"]["body"], flush=True)
    else:
        print(saved["body"], flush=True)
    results.append(result)
    Path(args.output).write_text(json.dumps(results, indent=2, ensure_ascii=False))
    assert secret not in json.dumps(result), "BFF response leaked backend credentials"
    assert saved["status"] == 200, "Configuration save failed"
    called = result["call"]["body"]
    if args.credential == "valid":
        assert result["verify"]["body"].get("success") is True, "Tool discovery failed"
        assert called.get("success") is True, "Tool call failed"
        echoed = json.loads(called["result"]["content"][0]["text"])
        assert echoed["text"] == text and echoed["mode"] == args.auth, "Wrong backend response"
    else:
        assert called.get("success") is not True, "Invalid backend credential unexpectedly succeeded"
