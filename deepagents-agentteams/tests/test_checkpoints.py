from unittest.mock import Mock, patch

import pytest

from deepagents_agentteams.checkpoints import (
    encrypted_serializer,
    postgres_checkpointer,
    setup_checkpoint_database,
)


def aes_key() -> str:
    return "k" * 32


def test_encrypted_serializer_round_trip_does_not_store_plaintext() -> None:
    serializer = encrypted_serializer(aes_key())

    type_name, ciphertext = serializer.dumps_typed({"message": "top-secret-value"})
    restored = serializer.loads_typed((type_name, ciphertext))

    assert b"top-secret-value" not in ciphertext
    assert restored == {"message": "top-secret-value"}


def test_rejects_invalid_aes_key_length() -> None:
    with pytest.raises(ValueError, match="16, 24, or 32"):
        encrypted_serializer("short")


def test_postgres_saver_uses_encrypted_serializer_and_closes_connection() -> None:
    connection = Mock()
    saver = Mock()
    with (
        patch("deepagents_agentteams.checkpoints.Connection.connect", return_value=connection) as connect,
        patch("deepagents_agentteams.checkpoints.PostgresSaver", return_value=saver) as saver_type,
        postgres_checkpointer("postgresql://checkpoint", aes_key()) as actual,
    ):
        assert actual is saver

    connect.assert_called_once()
    assert connect.call_args.args == ("postgresql://checkpoint",)
    assert connect.call_args.kwargs["autocommit"] is True
    serializer = saver_type.call_args.kwargs["serde"]
    type_name, ciphertext = serializer.dumps_typed("sensitive")
    assert serializer.loads_typed((type_name, ciphertext)) == "sensitive"
    connection.close.assert_called_once_with()


def test_setup_runs_idempotent_postgres_migrations() -> None:
    saver = Mock()
    context = Mock()
    context.__enter__ = Mock(return_value=saver)
    context.__exit__ = Mock(return_value=False)

    with patch("deepagents_agentteams.checkpoints.postgres_checkpointer", return_value=context):
        setup_checkpoint_database("postgresql://checkpoint", aes_key())

    saver.setup.assert_called_once_with()
