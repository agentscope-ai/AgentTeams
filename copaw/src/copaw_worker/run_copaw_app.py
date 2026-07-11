"""Run the upstream CoPaw app with HiClaw runtime hooks installed."""

from __future__ import annotations

import runpy

from copaw_worker.hooks import install_tool_hooks


def main() -> None:
    install_tool_hooks()
    runpy.run_module("copaw", run_name="__main__", alter_sys=True)


if __name__ == "__main__":
    main()
