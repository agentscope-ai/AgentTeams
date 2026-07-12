"""AgentTeams Matrix overlay for hermes-worker.

This package no longer replaces hermes-agent's native Matrix transport.
Instead, the image build renames hermes-agent's stock
``gateway/platforms/matrix.py`` to ``_matrix_native.py`` and installs a tiny
shim at the original path.  That shim re-exports the native module while
replacing only ``MatrixAdapter`` with ``hermes_matrix.adapter.MatrixAdapter``.

The subclass keeps hermes-agent's native mautrix implementation for media,
streaming, typing, reactions, threads, and E2EE, while layering AgentTeams's
policy-only behavior on top:

  * outbound ``m.mentions`` enrichment
  * DM / group split allow-lists
  * copaw-style history buffering in group rooms
  * image downgrade when the active model lacks vision support
"""
