"""Encrypted PostgreSQL checkpoint construction and schema migration."""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator
from contextlib import asynccontextmanager, contextmanager

from langgraph.checkpoint.postgres import PostgresSaver
from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
from langgraph.checkpoint.serde.encrypted import EncryptedSerializer
from psycopg import Connection
from psycopg.rows import dict_row


def encrypted_serializer(aes_key: str) -> EncryptedSerializer:
    """Build the LangGraph AES-EAX serializer from validated key material."""
    key = aes_key.encode("utf-8")
    if len(key) not in {16, 24, 32}:
        raise ValueError("checkpoint AES key must be 16, 24, or 32 UTF-8 bytes")
    return EncryptedSerializer.from_pycryptodome_aes(key=key)


@contextmanager
def postgres_checkpointer(dsn: str, aes_key: str) -> Iterator[PostgresSaver]:
    """Yield an encrypted LangGraph saver and close its database connection."""
    if not dsn.strip():
        raise ValueError("checkpoint PostgreSQL DSN must be non-empty")
    connection = Connection.connect(
        dsn,
        autocommit=True,
        prepare_threshold=0,
        row_factory=dict_row,
    )
    try:
        yield PostgresSaver(connection, serde=encrypted_serializer(aes_key))
    finally:
        connection.close()


@asynccontextmanager
async def async_postgres_checkpointer(dsn: str, aes_key: str) -> AsyncIterator[AsyncPostgresSaver]:
    """Yield the encrypted async saver used by the Matrix event loop."""
    if not dsn.strip():
        raise ValueError("checkpoint PostgreSQL DSN must be non-empty")
    async with AsyncPostgresSaver.from_conn_string(
        dsn,
        serde=encrypted_serializer(aes_key),
    ) as saver:
        yield saver


def setup_checkpoint_database(dsn: str, aes_key: str) -> None:
    """Apply the idempotent LangGraph checkpoint schema migrations."""
    with postgres_checkpointer(dsn, aes_key) as saver:
        saver.setup()
