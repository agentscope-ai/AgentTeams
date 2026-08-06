"""CLI for applying DeepAgents checkpoint database migrations."""

from __future__ import annotations

import os

from deepagents_agentteams.checkpoints import setup_checkpoint_database


def main() -> None:
    """Apply migrations using the same secret environment as the Worker."""
    dsn = os.environ.get("AGENTTEAMS_CHECKPOINT_DSN", "")
    aes_key = os.environ.get("AGENTTEAMS_CHECKPOINT_AES_KEY", "")
    setup_checkpoint_database(dsn, aes_key)


if __name__ == "__main__":
    main()
