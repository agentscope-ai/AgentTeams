"""Exercise service-source registration without a running Higress instance."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "agent/skills/mcp-server-management/scripts/setup-mcp-server.sh"


class ApiDomainTest(unittest.TestCase):
    def register(self, domain, url="https://api.example.com/tools"):
        source = SCRIPT.read_text()
        step = source[source.index('log "Step 1:'):source.index('# Step 2:')]
        with tempfile.TemporaryDirectory() as directory:
            yaml = Path(directory) / "tools.yaml"
            yaml.write_text(f'url: "{url}"\n')
            return subprocess.run(
                ["bash", "-c", '''set -euo pipefail
log() { echo "$*" >&2; }
higress_api() { printf '%s\\n' "$4"; }
EXPLICIT_API_DOMAIN=$1
MCP_YAML_FILE=$2
SERVER_NAME=test
MCP_SERVER_NAME=mcp-test
''' + step, "test", domain, str(yaml)],
                capture_output=True, text=True,
            )

    def test_explicit_addresses(self):
        for address, host, protocol, port in [
            ("api.example.com", "api.example.com", "https", 443),
            ("api.example.com:8443", "api.example.com", "https", 8443),
            ("https://api.example.com", "api.example.com", "https", 443),
            ("https://api.example.com:8443", "api.example.com", "https", 8443),
            ("http://tool.example.local", "tool.example.local", "http", 80),
            ("http://tool.example.local:8080", "tool.example.local", "http", 8080),
        ]:
            with self.subTest(address=address):
                result = self.register(address)
                self.assertEqual(result.returncode, 0, result.stderr)
                body = json.loads(result.stdout)
                self.assertEqual((body["domain"], body["protocol"], body["port"]),
                                 (host, protocol, port))

    def test_yaml_extraction(self):
        for url, protocol, port in [
            ("https://api.example.com/tools", "https", 443),
            ("http://tool.example.local:8080/tools", "http", 8080),
        ]:
            with self.subTest(url=url):
                result = self.register("", url)
                self.assertEqual(result.returncode, 0, result.stderr)
                body = json.loads(result.stdout)
                self.assertEqual((body["protocol"], body["port"]), (protocol, port))

    def test_invalid_explicit_addresses_fail_before_registration(self):
        for address in ["tool", "http://tool", "ftp://api.example.com",
                        "http://", "api.example.com:", "api.example.com:abc",
                        "api.example.com:0", "api.example.com:65536",
                        "http://api.example.com/path", "user@api.example.com",
                        "api.example.com?query", "api.example.com#fragment"]:
            with self.subTest(address=address):
                result = self.register(address)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertIn("ERROR", result.stderr)


if __name__ == "__main__":
    unittest.main()
