from __future__ import annotations

import json
import os
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[4]
SCRIPT = (
    REPO_ROOT
    / "plugins"
    / "teamharness"
    / "skills"
    / "agent"
    / "find-skills"
    / "scripts"
    / "hiclaw-find-skill.sh"
)


def test_find_skills_uses_credential_broker_skill_registry(tmp_path: Path) -> None:
    class BrokerHandler(BaseHTTPRequestHandler):
        def log_message(self, _format: str, *_args: object) -> None:
            return

        def do_GET(self) -> None:
            if self.headers.get("Authorization") != "Bearer local-capability":
                self.send_response(401)
                self.end_headers()
                return
            if self.path != "/v1/credentials/skill-registry":
                self.send_response(404)
                self.end_headers()
                return
            body = json.dumps(
                {
                    "provider": "nacos",
                    "url": "nacos://market.example:80/public",
                    "authType": "sts-hiclaw",
                    "accessKeyId": "sts-ak",
                    "accessKeySecret": "sts-sk",
                    "securityToken": "sts-token",
                    "expiration": "2026-06-10T12:00:00Z",
                }
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    broker = ThreadingHTTPServer(("127.0.0.1", 0), BrokerHandler)
    broker_thread = threading.Thread(target=broker.serve_forever, daemon=True)
    broker_thread.start()

    token_file = tmp_path / "credential-token"
    token_file.write_text("local-capability\n", encoding="utf-8")
    descriptor = tmp_path / "credential-broker.json"
    descriptor.write_text(
        json.dumps(
            {
                "version": 1,
                "endpoint": f"http://127.0.0.1:{broker.server_port}",
                "tokenFile": str(token_file),
                "workerPid": 123,
                "workerUuid": "worker-uuid-1",
            }
        ),
        encoding="utf-8",
    )

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    args_log = tmp_path / "npx-args.jsonl"
    fake_npx = bin_dir / "npx"
    fake_npx.write_text(
        f"""#!/usr/bin/env python3
import json
import sys
from pathlib import Path
with Path({str(args_log)!r}).open("a", encoding="utf-8") as f:
    f.write(json.dumps(sys.argv[1:]) + "\\n")
print("1. broker-skill - Found via broker")
""",
        encoding="utf-8",
    )
    fake_npx.chmod(0o755)

    env: dict[str, str] = {
        **os.environ,
        "PATH": f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}",
        "TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR": str(descriptor),
    }
    for name in (
        "SKILLS_API_URL",
        "HICLAW_SKILLS_API_URL",
        "NACOS_AUTH_TYPE",
        "HICLAW_AUTH_TOKEN",
        "HICLAW_AUTH_TOKEN_FILE",
        "HICLAW_WORKER_API_KEY",
        "HICLAW_CONTROLLER_URL",
        "HICLAW_NACOS_STS_ACCESS_KEY",
        "HICLAW_NACOS_STS_SECRET_KEY",
        "HICLAW_NACOS_STS_SECURITY_TOKEN",
    ):
        env.pop(name, None)

    try:
        result = subprocess.run(
            ["sh", str(SCRIPT), "find", "broker skill"],
            cwd=tmp_path,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    finally:
        broker.shutdown()
        broker_thread.join(timeout=3)

    assert result.returncode == 0, result.stderr
    assert "Registry:" in result.stdout
    assert "Nacos (nacos://market.example:80)" in result.stdout
    assert "broker-skill" in result.stdout
    calls = [json.loads(line) for line in args_log.read_text(encoding="utf-8").splitlines()]
    assert calls
    assert all("--auth-type" in call for call in calls)
    assert all("sts-hiclaw" in call for call in calls)
    assert all("--access-key" in call and "sts-ak" in call for call in calls)
    assert all("--secret-key" in call and "sts-sk" in call for call in calls)
    assert all("--security-token" in call and "sts-token" in call for call in calls)
