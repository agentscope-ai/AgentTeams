from __future__ import annotations

import json
import socket
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[5]
RUNTIME_ROOT = ROOT / "plugins" / "teamharness" / "remote" / "codex-cli"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_runtime.mcp_proxy import McpCapabilityProxy  # noqa: E402


class McpCapabilityProxyTest(unittest.TestCase):
    def test_client_shim_can_run_as_a_standalone_script(self) -> None:
        script = RUNTIME_ROOT / "agentteams_codex_runtime" / "mcp_proxy.py"
        result = subprocess.run(
            [sys.executable, str(script)],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("usage: mcp_proxy.py", result.stderr)

    def test_authenticated_proxy_forwards_mcp_without_disclosing_raw_secret(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temp:
            script = Path(temp) / "fake_mcp.py"
            script.write_text(
                textwrap.dedent(
                    """
                    import json
                    import os
                    import sys

                    for line in sys.stdin:
                        request = json.loads(line)
                        print(json.dumps({
                            "id": request["id"],
                            "result": {
                                "secretAvailableToBrokerChild": bool(
                                    os.getenv("MCP_SECRET")
                                )
                            },
                        }), flush=True)
                    """
                ).lstrip(),
                encoding="utf-8",
            )
            proxy = McpCapabilityProxy(
                script,
                server_environment={"MCP_SECRET": "raw-service-secret"},
                secret_values=("raw-service-secret",),
            )
            proxy.start()
            try:
                connection = socket.create_connection(proxy.address, timeout=2)
                stream = connection.makefile("rwb")
                stream.write((json.dumps({"token": proxy.token}) + "\n").encode())
                stream.write(b'{"id":1,"method":"tools/list"}\n')
                stream.flush()
                response = json.loads(stream.readline().decode())
                self.assertTrue(response["result"]["secretAvailableToBrokerChild"])
                self.assertNotIn("raw-service-secret", json.dumps(response))
                stream.close()
                connection.close()
            finally:
                proxy.close()


if __name__ == "__main__":
    unittest.main()
