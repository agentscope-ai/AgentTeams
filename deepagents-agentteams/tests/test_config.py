import copy
import unittest

from deepagents_agentteams.config import ConfigError, RuntimeConfig


def valid_document() -> dict[str, object]:
    return {
        "apiVersion": "agentteams.io/v1beta1",
        "kind": "MemberRuntimeConfig",
        "metadata": {"generation": 7},
        "member": {
            "runtime": "deepagents",
            "runtimeName": "researcher",
            "matrixUserId": "@researcher:example.org",
            "personalRoomId": "!room:example.org",
        },
        "matrix": {
            "homeserverUrl": "https://matrix.example.org",
            "encryptionEnabled": True,
        },
        "desired": {
            "state": "Running",
            "model": {
                "model": "qwen-max",
                "gatewayUrl": "https://higress.example.org/v1",
            },
            "runtimeConfig": {"deepagents": {}},
        },
        "storage": {
            "provider": "minio",
            "endpoint": "https://minio.example.org",
            "bucket": "agentteams",
            "memberPrefix": "agents/researcher",
        },
        "credentials": {
            "matrixTokenEnv": "AT_MATRIX_TOKEN",
            "gatewayKeyEnv": "AT_GATEWAY_KEY",
            "storageAccessKeyEnv": "AT_STORAGE_ACCESS",
            "storageSecretKeyEnv": "AT_STORAGE_SECRET",
            "checkpointDSNEnv": "AT_CHECKPOINT_DSN",
            "checkpointAESKeyEnv": "AT_CHECKPOINT_AES_KEY",
        },
    }


def valid_environ() -> dict[str, str]:
    return {
        "AT_MATRIX_TOKEN": "matrix-secret",
        "AT_GATEWAY_KEY": "gateway-secret",
        "AT_STORAGE_ACCESS": "storage-access",
        "AT_STORAGE_SECRET": "storage-secret",
        "AT_CHECKPOINT_DSN": "postgresql://checkpoint",
        "AT_CHECKPOINT_AES_KEY": "aes-secret",
    }


class RuntimeConfigTests(unittest.TestCase):
    def test_rejects_embedded_matrix_access_token(self) -> None:
        document = {
            "apiVersion": "agentteams.io/v1beta1",
            "kind": "MemberRuntimeConfig",
            "member": {"runtime": "deepagents", "runtimeName": "researcher"},
            "matrix": {"accessToken": "must-not-be-here"},
            "desired": {"state": "Running"},
            "storage": {"memberPrefix": "agents/researcher"},
            "credentials": {},
        }

        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ={})

    def test_rejects_embedded_gateway_key(self) -> None:
        document = {
            "apiVersion": "agentteams.io/v1beta1",
            "kind": "MemberRuntimeConfig",
            "member": {"runtime": "deepagents", "runtimeName": "researcher"},
            "desired": {
                "state": "Running",
                "model": {
                    "model": "qwen-max",
                    "gatewayUrl": "http://higress/v1",
                    "gatewayKey": "must-not-be-here",
                },
            },
            "storage": {"memberPrefix": "agents/researcher"},
            "credentials": {},
        }

        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ={})

    def test_resolves_secret_env_references_and_applies_safe_defaults(self) -> None:
        config = RuntimeConfig.from_document(valid_document(), environ=valid_environ())

        self.assertEqual(config.generation, 7)
        self.assertEqual(config.runtime_name, "researcher")
        self.assertEqual(config.model.name, "qwen-max")
        self.assertEqual(config.model.gateway_key, "gateway-secret")
        self.assertEqual(config.matrix.access_token, "matrix-secret")
        self.assertEqual(config.checkpoint.dsn, "postgresql://checkpoint")
        self.assertEqual(config.execution.mode, "disabled")
        self.assertEqual(config.execution.idle_timeout_seconds, 1800)
        self.assertEqual(config.execution.max_lifetime_seconds, 28800)
        self.assertEqual(config.approvals.file_writes, "notRequired")
        self.assertEqual(config.approvals.mcp_default, "required")

    def test_rejects_unknown_execution_mode(self) -> None:
        document = copy.deepcopy(valid_document())
        document["desired"]["runtimeConfig"]["deepagents"] = {  # type: ignore[index]
            "execution": {"mode": "local"}
        }

        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ=valid_environ())


if __name__ == "__main__":
    unittest.main()
