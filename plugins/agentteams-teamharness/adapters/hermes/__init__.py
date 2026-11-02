"""Hermes directory-plugin entry point for the AgentTeams TeamHarness adapter.

``hermes_cli.plugins.PluginManager._load_directory_module`` imports this file
as ``hermes_plugins.<plugin name>`` and calls the module-level ``register``
with a ``PluginContext``. The loader sets the module ``__path__`` to the
plugin directory, so the sibling ``register`` module is importable relative
to this package.
"""

from .register import register

__all__ = ["register"]
