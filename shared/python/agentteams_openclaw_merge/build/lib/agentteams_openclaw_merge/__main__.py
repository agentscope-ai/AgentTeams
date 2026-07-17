"""CLI entrypoint for file-based openclaw.json merge (shell wrapper target)."""
from __future__ import annotations

import shutil
import sys
from pathlib import Path

from agentteams_openclaw_merge.merge import merge_openclaw_config


def main(argv: list[str] | None = None) -> int:
    args = argv if argv is not None else sys.argv[1:]
    if len(args) < 2:
        print(
            "usage: python -m agentteams_openclaw_merge "
            "<remote_path> <local_path> [<output_path>]",
            file=sys.stderr,
        )
        return 2

    remote_path = Path(args[0])
    local_path = Path(args[1])
    output_path = Path(args[2]) if len(args) > 2 else local_path

    if not remote_path.is_file():
        return 0

    if not local_path.is_file():
        shutil.move(str(remote_path), output_path)
        return 0

    try:
        merged = merge_openclaw_config(
            remote_path.read_text(encoding="utf-8"),
            local_path.read_text(encoding="utf-8"),
        )
        output_path.write_text(merged, encoding="utf-8")
    except Exception:
        # Match shell/jq behavior: on merge failure, keep local unchanged.
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
