import copy
import unittest

from deepagents_agentteams.config import ConfigError, RuntimeConfig


def valid_document() -> dict[str, object]:
    return {
        "apiVersion": "agentteams.io/v1beta1",
        "kind": "MemberRuntimeConfig",
        "metadata": {"generation": 7},
        "member": {
            "name": "researcher-cr",
            "uid": "worker-uid-1",
            "runtime": "deepagents",
            "runtimeName": "researcher",
            "matrixUserId": "@researcher:example.org",
            "personalRoomId": "!room:example.org",
        },
        "matrix": {
            "homeserverUrl": "https://matrix.example.org",
            "encryptionEnabled": True,
            "agentUserIds": ["@manager:example.org"],
        },
        "team": {
            "name": "research",
            "teamRoomId": "!team:example.org",
            "admin": {
                "name": "operator",
                "matrixUserId": "@operator:example.org",
            },
            "members": [
                {
                    "name": "leader",
                    "matrixUserId": "@leader:example.org",
                    "role": "team_leader",
                },
                {
                    "name": "human-coordinator",
                    "matrixUserId": "@coordinator:example.org",
                    "role": "coordinator",
                },
                {
                    "name": "peer-worker",
                    "matrixUserId": "@peer:example.org",
                    "role": "worker",
                }
            ],
        },
        "desired": {
            "state": "Running",
            "model": {
                "model": "qwen-max",
                "gatewayUrl": "https://higress.example.org/v1",
            },
            "mcpServers": [
                {
                    "name": "github",
                    "url": "https://higress.example.org/mcp/github",
                    "transport": "http",
                }
            ],
            "runtimeConfig": {
                "deepagents": {
                    "approvals": {
                        "fileWrites": "required",
                        "mcpDefault": "required",
                        "coordinators": ["@reviewer:example.org"],
                        "mcpRules": [
                            {
                                "server": "github",
                                "tool": "get_issue",
                                "mode": "notRequired",
                            }
                        ],
                    },
                    "execution": {
                        "mode": "sandbox",
                        "idleTimeout": "45m",
                        "maxLifetime": "6h30m",
                    },
                }
            },
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
        "AT_CHECKPOINT_AES_KEY": "a" * 32,
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
        document = valid_document()
        document["desired"]["runtimeConfig"] = {"deepagents": {}}  # type: ignore[index]
        config = RuntimeConfig.from_document(document, environ=valid_environ())

        self.assertEqual(config.generation, 7)
        self.assertEqual(config.runtime_name, "researcher")
        self.assertEqual(config.worker_name, "researcher-cr")
        self.assertEqual(config.worker_uid, "worker-uid-1")
        self.assertEqual(config.model.name, "qwen-max")
        self.assertEqual(config.model.gateway_key, "gateway-secret")
        self.assertEqual(config.matrix.access_token, "matrix-secret")
        self.assertEqual(config.checkpoint.dsn, "postgresql://checkpoint")
        self.assertEqual(config.execution.mode, "disabled")
        self.assertEqual(config.execution.idle_timeout_seconds, 1800)
        self.assertEqual(config.execution.max_lifetime_seconds, 28800)
        self.assertEqual(config.approvals.file_writes, "notRequired")
        self.assertEqual(config.approvals.mcp_default, "required")

    def test_parses_runtime_policy_mcp_and_controller_identity(self) -> None:
        environ = valid_environ()
        environ["AGENTTEAMS_CONTROLLER_URL"] = "http://controller.agentteams-system.svc:8090"
        config = RuntimeConfig.from_document(valid_document(), environ=environ)

        self.assertEqual(config.execution.mode, "sandbox")
        self.assertEqual(config.execution.idle_timeout_seconds, 2700)
        self.assertEqual(config.execution.max_lifetime_seconds, 23400)
        self.assertEqual(config.approvals.coordinators, ("@reviewer:example.org",))
        self.assertEqual(config.approvals.mcp_rules[0].server, "github")
        self.assertEqual(config.approvals.mcp_rules[0].tool, "get_issue")
        self.assertEqual(config.mcp_servers[0]["name"], "github")
        self.assertEqual(config.controller_url, "http://controller.agentteams-system.svc:8090")
        self.assertEqual(config.service_account_token_path, "/var/run/secrets/agentteams/token")
        self.assertEqual(config.room_ids, ("!room:example.org", "!team:example.org"))
        self.assertEqual(
            config.human_approver_ids,
            frozenset(
                {
                    "@operator:example.org",
                    "@reviewer:example.org",
                    "@coordinator:example.org",
                }
            ),
        )
        self.assertEqual(
            config.agent_matrix_ids,
            frozenset(
                {
                    "@researcher:example.org",
                    "@manager:example.org",
                    "@leader:example.org",
                    "@peer:example.org",
                }
            ),
        )

    def test_known_agent_identity_cannot_become_a_human_approver(self) -> None:
        document = copy.deepcopy(valid_document())
        document["desired"]["runtimeConfig"]["deepagents"]["approvals"]["coordinators"] = [  # type: ignore[index]
            "@manager:example.org"
        ]

        config = RuntimeConfig.from_document(document, environ=valid_environ())

        self.assertIn("@manager:example.org", config.agent_matrix_ids)
        self.assertNotIn("@manager:example.org", config.human_approver_ids)

    def test_global_agent_identity_cannot_become_a_human_coordinator(self) -> None:
        document = copy.deepcopy(valid_document())
        document["matrix"]["agentUserIds"] = [  # type: ignore[index]
            "@manager:example.org",
            "@unrelated-worker:example.org",
        ]
        document["team"]["members"].append(  # type: ignore[index]
            {
                "name": "unrelated-worker",
                "matrixUserId": "@unrelated-worker:example.org",
                "role": "coordinator",
            }
        )

        config = RuntimeConfig.from_document(document, environ=valid_environ())

        self.assertIn("@unrelated-worker:example.org", config.agent_matrix_ids)
        self.assertNotIn("@unrelated-worker:example.org", config.human_approver_ids)

    def test_parses_inline_identity_soul_and_agents(self) -> None:
        document = copy.deepcopy(valid_document())
        document["desired"]["inlineConfig"] = {  # type: ignore[index]
            "identity": "You are a security reviewer.",
            "soul": "Be skeptical and precise.",
            "agents": "Run tests before reporting results.",
        }

        config = RuntimeConfig.from_document(document, environ=valid_environ())

        self.assertEqual(config.inline_config.identity, "You are a security reviewer.")
        self.assertEqual(config.inline_config.soul, "Be skeptical and precise.")
        self.assertEqual(config.inline_config.agents, "Run tests before reporting results.")

    def test_rejects_invalid_approval_mode_and_duration(self) -> None:
        document = copy.deepcopy(valid_document())
        document["desired"]["runtimeConfig"]["deepagents"]["approvals"]["fileWrites"] = "sometimes"  # type: ignore[index]
        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ=valid_environ())

        document = copy.deepcopy(valid_document())
        document["desired"]["runtimeConfig"]["deepagents"]["execution"]["idleTimeout"] = "forever"  # type: ignore[index]
        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ=valid_environ())

    def test_rejects_checkpoint_key_with_invalid_aes_length(self) -> None:
        environ = valid_environ()
        environ["AT_CHECKPOINT_AES_KEY"] = "too-short"

        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(valid_document(), environ=environ)

    def test_rejects_unknown_execution_mode(self) -> None:
        document = copy.deepcopy(valid_document())
        document["desired"]["runtimeConfig"]["deepagents"] = {  # type: ignore[index]
            "execution": {"mode": "local"}
        }

        with self.assertRaises(ConfigError):
            RuntimeConfig.from_document(document, environ=valid_environ())


if __name__ == "__main__":
    unittest.main()
