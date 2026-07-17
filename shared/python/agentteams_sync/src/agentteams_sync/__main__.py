"""``python -m agentteams_sync`` entrypoint."""

from __future__ import annotations

import sys


def main() -> None:
    if len(sys.argv) > 1 and sys.argv[1] == "openclaw-matrix":
        from agentteams_sync.openclaw_matrix import main as matrix_main

        matrix_main()
        return
    from agentteams_sync.daemon import main as daemon_main

    daemon_main()


if __name__ == "__main__":
    main()
