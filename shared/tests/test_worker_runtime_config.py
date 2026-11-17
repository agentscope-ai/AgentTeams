"""Contract/projection tests; no native startup or Task execution is implied."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location("worker_runtime_config", Path(__file__).parents[1] / "lib/worker_runtime_config.py")
config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(config)


class WorkerRuntimeConfigTests(unittest.TestCase):
    def record(self, harness="openhuman"):
        return {
            "apiVersion": "agentteams.io/v1beta1", "kind": "MemberRuntimeConfig",
            "member": {"runtime": harness, "runtimeName": "worker", "matrixUserId": "@worker:test", "personalRoomId": "!home:test"},
            "storage": {"memberPrefix": "agents/worker"},
            "matrix": {"transport": {"homeserver": "https://matrix.test", "dm": {"policy": "allowlist", "allowFrom": ["@leader:test"]}, "groupAllowFrom": ["@leader:test"]}},
            "desired": {"model": {"providerId": "agentteams-gateway", "model": "current-model", "gatewayUrl": "https://gateway.test"}},
            "credentials": {"matrixTokenEnv": "TEST_MATRIX_BINDING", "gatewayKeyEnv": "TEST_GATEWAY_BINDING"},
        }

    def project(self, record, legacy=None):
        with patch.dict("os.environ", {"TEST_MATRIX_BINDING": "current-matrix-token", "TEST_GATEWAY_BINDING": "current-model-key"}):
            return config.project(record, record["member"]["runtime"], "worker", legacy)

    def test_same_format_projects_all_six_harnesses_without_native_files(self):
        for harness in ("codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"):
            with self.subTest(harness=harness):
                projection = self.project(self.record(harness))
                matrix = projection["channels"]["matrix"]
                self.assertEqual(matrix["userId"], "@worker:test")
                self.assertEqual(matrix["homeRoomId"], "!home:test")
                self.assertEqual(matrix["accessToken"], "current-matrix-token")
                self.assertEqual(matrix["groupAllowFrom"], ["@leader:test"])
                self.assertEqual(projection["models"]["providers"]["agentteams-gateway"]["baseUrl"], "https://gateway.test/v1")

    def test_authority_replaces_old_credentials_and_model_without_mutating_legacy(self):
        old = {"channels": {"matrix": {"accessToken": "old-token", "deviceId": "old-device"}},
               "agents": {"defaults": {"model": {"primary": "old/model"}}}, "tools": {"custom": True}}
        original = copy.deepcopy(old)
        result = self.project(self.record(), old)
        self.assertEqual(old, original)
        self.assertNotIn("old-token", json.dumps(result))
        self.assertNotIn("old-device", json.dumps(result))
        self.assertEqual(result["agents"]["defaults"]["model"]["primary"], "agentteams-gateway/current-model")
        self.assertTrue(result["tools"]["custom"])

    def test_missing_bindings_cannot_use_legacy_secrets(self):
        old = {"channels": {"matrix": {"accessToken": "old-token"}}, "models": {"providers": {"old": {"apiKey": "old-key"}}}}
        for missing in ("TEST_MATRIX_BINDING", "TEST_GATEWAY_BINDING"):
            with self.subTest(missing=missing), patch.dict("os.environ", {"TEST_MATRIX_BINDING": "new-token", "TEST_GATEWAY_BINDING": "new-key"}):
                import os
                os.environ.pop(missing)
                with self.assertRaisesRegex(ValueError, "binding"):
                    config.project(self.record(), "openhuman", "worker", old)

    def test_version_identity_and_storage_scope_are_validated(self):
        for section, key, value in ((None, "apiVersion", "unsupported/v9"), (None, "kind", "Other"),
                                   ("member", "runtime", "hermes"), ("member", "runtimeName", "other"),
                                   ("storage", "memberPrefix", "agents/other")):
            with self.subTest(key=key):
                record = self.record()
                (record[section] if section else record)[key] = value
                with self.assertRaises(ValueError):
                    config.project(record, "openhuman", "worker")

    def test_undeclared_model_requires_external_worker_compatibility(self):
        record = self.record()
        record["desired"].pop("model")
        with self.assertRaisesRegex(ValueError, "model"):
            self.project(record)
        model = {"providers": {"gateway": {"apiKey": "explicit-legacy-model-key"}}}
        self.assertEqual(self.project(record, {"models": model})["models"], model)

    def test_materialization_is_private_and_failed_update_preserves_previous_output(self):
        with tempfile.TemporaryDirectory() as temp:
            workspace = Path(temp)
            runtime = workspace / "runtime/runtime.yaml"
            runtime.parent.mkdir()
            runtime.write_text(yaml.safe_dump(self.record()))
            (workspace / "openclaw.json").write_text("invalid old compatibility input")
            with patch.dict("os.environ", {"TEST_MATRIX_BINDING": "new-token", "TEST_GATEWAY_BINDING": "new-key"}):
                output = config.materialize(workspace, "openhuman", "worker")
                self.assertEqual(output, workspace / "runtime/compat-config.json")
                previous = output.read_bytes()
                self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                runtime.write_text(yaml.safe_dump(self.record("hermes")))
                with self.assertRaises(ValueError):
                    config.materialize(workspace, "openhuman", "worker")
                self.assertEqual(output.read_bytes(), previous)


if __name__ == "__main__":
    unittest.main()
