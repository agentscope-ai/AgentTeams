import os
from pathlib import Path
import subprocess
import unittest


ENTRYPOINT = Path(__file__).resolve().parents[1] / "scripts" / "qwen-code-worker-entrypoint.sh"


def run_entrypoint(env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(ENTRYPOINT)],
        env=env,
        text=True,
        capture_output=True,
        timeout=10,
    )


@unittest.skipIf(os.name == "nt", "requires POSIX bash")
class QwenCodeEntrypointTest(unittest.TestCase):
    def test_script_passes_bash_syntax_check(self) -> None:
        completed = subprocess.run(["bash", "-n", str(ENTRYPOINT)], text=True, capture_output=True, timeout=10)

        self.assertEqual(completed.returncode, 0, completed.stderr)

    def test_e2ee_configuration_fails_before_worker_startup(self) -> None:
        completed = run_entrypoint({**os.environ, "AGENTTEAMS_MATRIX_E2EE": "1"})

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn(
            "qwen-code runtime does not support Matrix E2EE",
            completed.stdout + completed.stderr,
        )

    def test_e2ee_true_configuration_fails_before_worker_startup(self) -> None:
        completed = run_entrypoint({**os.environ, "AGENTTEAMS_MATRIX_E2EE": "true"})

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn(
            "qwen-code runtime does not support Matrix E2EE",
            completed.stdout + completed.stderr,
        )

    def test_entrypoint_uses_lf_line_endings(self) -> None:
        self.assertNotIn(b"\r\n", ENTRYPOINT.read_bytes())

    def test_entrypoint_exports_bridge_contract(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")

        self.assertIn('export QWEN_HOME="${WORKER_HOME}/.qwen"', text)
        self.assertIn('export TEAMHARNESS_RUNTIME_CONFIG="${RUNTIME_CONFIG}"', text)
        self.assertIn('export TEAMHARNESS_WORKSPACE="${WORKER_HOME}/workspace"', text)
        self.assertIn('export AGENTTEAMS_MATRIX_USER_ID="@${WORKER_NAME}:${AGENTTEAMS_MATRIX_DOMAIN}"', text)
        self.assertIn('export AGENTTEAMS_QWEN_SERVE_BASE="http://127.0.0.1:${SERVE_PORT}"', text)

    def test_entrypoint_starts_serve_then_waits_for_health_before_bridge(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")
        lines = text.splitlines()

        serve_line = lines.index("nohup qwen serve \\")
        health_line = next(i for i, line in enumerate(lines) if "curl -sf -m 2" in line)
        bridge_line = lines.index("exec python3 /opt/agentteams/scripts/serve_bridge.py")

        self.assertLess(serve_line, health_line)
        self.assertLess(health_line, bridge_line)
        self.assertIn('Authorization: Bearer ${SERVE_TOKEN}', text)
        self.assertIn('if [ "${RETRY}" -ge 24 ]', text)
        self.assertIn('qwen serve did not become healthy', text)
        self.assertIn('qwen serve process exited during startup', text)

    def test_entrypoint_requires_projected_model_credential(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")

        self.assertIn('API_KEY="${AGENTTEAMS_WORKER_GATEWAY_KEY:-}"', text)
        self.assertIn("ERROR: no model credential available", text)
        self.assertIn("ERROR: no model gateway URL", text)

    def test_entrypoint_normalizes_gateway_url_to_openai_v1(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")

        self.assertIn('case "${BASE_URL%/}" in', text)
        self.assertIn('*/v1) : ;;', text)
        self.assertIn('*) BASE_URL="${BASE_URL%/}/v1" ;;', text)

    def test_entrypoint_writes_settings_json_with_restricted_mode(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")

        self.assertIn('python3 - "${QWEN_HOME}/settings.json" "${MODEL}" "${API_KEY}" "${BASE_URL}"', text)
        self.assertIn('"security": {"auth": {"selectedType": "openai"', text)
        self.assertIn("os.chmod(path, 0o600)", text)

    def test_entrypoint_pulls_runtime_state_with_bounded_retry(self) -> None:
        text = ENTRYPOINT.read_text(encoding="utf-8")

        self.assertIn('mc mirror "${REMOTE_WORKER}/runtime/" "${RUNTIME_DIR}/" --overwrite', text)
        self.assertIn('if [ "${RETRY}" -ge 12 ]', text)
        self.assertIn('sleep 5', text)
        self.assertIn('if [ ! -s "${RUNTIME_CONFIG}" ]', text)


if __name__ == "__main__":
    unittest.main()
