"""AgentTeams Matrix platform plugin.

Hermes v0.22.5 moved Matrix from ``gateway/platforms/matrix.py`` to the
bundled ``plugins/platforms/matrix`` plugin.  At container startup this
plugin is installed under the same path-derived key (``platforms/matrix``) and
manifest name, so Hermes' user-over-bundled precedence replaces the stock
registration.  Its register() then delegates to the bundled register() with
the AgentTeams adapter subclass swapped in.

Keeping the swap at register() time avoids copying the bundled plugin's large
registration metadata (dashboard card, env bridge, send-target grammar, setup
flow) into this overlay.
"""
from __future__ import annotations


def register(ctx) -> None:
    """Register the AgentTeams Matrix adapter under the stock platform name."""
    from plugins.platforms.matrix import adapter as native
    from hermes_matrix.overlay_adapter import MatrixAdapter

    native.MatrixAdapter = MatrixAdapter
    native.register(ctx)
