"""Run the QwenPaw 2.0 app for AgentTeams Manager.

Previously this module called ``install_tool_hooks()`` to monkey-patch
``CoPawAgent._create_toolkit``.  On QwenPaw 2.0 that approach no longer
works — ``QwenPawAgent`` does not inherit from ``CoPawAgent`` and has no
``_create_toolkit`` method.  The four manager tools (projectflow, taskflow,
message, filesync) are now registered via the ``agentteams-manager-tools``
QwenPaw plugin (``api.register_tool()``), so this wrapper simply launches
QwenPaw.
"""

from __future__ import annotations

import runpy


def main() -> None:
    runpy.run_module("qwenpaw", run_name="__main__", alter_sys=True)


if __name__ == "__main__":
    main()
