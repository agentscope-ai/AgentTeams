"""TeamHarness MCP artifact tool dispatch."""

from __future__ import annotations

from typing import Any


def artifact(arguments: dict[str, Any]) -> dict[str, Any]:
    import server as _server

    return _server._artifact(arguments)
