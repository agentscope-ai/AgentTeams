import importlib.util
from pathlib import Path
import tempfile
import unittest


MODULE_PATH = Path(__file__).parents[1] / "scripts" / "runtime_env.py"
SPEC = importlib.util.spec_from_file_location("dsh_runtime_env", MODULE_PATH)
runtime_env = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(runtime_env)


class DeepSeekBaseUrlTests(unittest.TestCase):
    def test_explicit_deepseek_url_wins_without_openai_path_rewrite(self) -> None:
        self.assertEqual(
            runtime_env.deepseek_base_url("https://api.deepseek.com/", "http://higress:80"),
            "https://api.deepseek.com",
        )

    def test_agentteams_gateway_root_gets_openai_v1_prefix(self) -> None:
        self.assertEqual(
            runtime_env.deepseek_base_url("", "http://higress:80/"),
            "http://higress:80/v1",
        )

    def test_existing_v1_prefix_is_not_duplicated(self) -> None:
        self.assertEqual(
            runtime_env.deepseek_base_url("", "http://higress:80/v1/"),
            "http://higress:80/v1",
        )

    def test_empty_values_keep_dsh_public_default_available(self) -> None:
        self.assertEqual(runtime_env.deepseek_base_url("", ""), "")

    def test_runtime_gateway_overrides_cluster_default(self) -> None:
        self.assertEqual(
            runtime_env.deepseek_base_url("", "http://cluster-gateway", "http://provider-route"),
            "http://provider-route/v1",
        )


class DesiredModelTests(unittest.TestCase):
    def test_reads_controller_generated_model_and_gateway(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "runtime.yaml"
            path.write_text(
                """member:\n  runtimeName: dsh-a\ndesired:\n  model:\n    providerId: agentteams-gateway\n    model: 'deepseek-chat'\n    gatewayUrl: \"http://gateway/route\"\n  state: Running\nstorage:\n  provider: minio\n""",
                encoding="utf-8",
            )
            self.assertEqual(
                runtime_env.desired_model(path),
                ("deepseek-chat", "http://gateway/route"),
            )

    def test_missing_model_returns_empty_values(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "runtime.yaml"
            path.write_text("desired:\n  state: Running\n", encoding="utf-8")
            self.assertEqual(runtime_env.desired_model(path), ("", ""))


if __name__ == "__main__":
    unittest.main()
