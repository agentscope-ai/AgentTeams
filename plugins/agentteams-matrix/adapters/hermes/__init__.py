"""AgentTeams Matrix Hermes platform plugin (P9, PR1 M6).

The Hermes plugin system loads ``adapter.py`` directly; this package
init re-exports :func:`register` so the plugin directory also imports
cleanly as a package (tests, tooling).
"""

from .adapter import PLATFORM_NAME, register  # noqa: F401

__all__ = ["PLATFORM_NAME", "register"]
