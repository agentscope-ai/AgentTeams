"""AgentTeams Matrix overlay for hermes-worker.

hermes-agent moved Matrix out of ``gateway/platforms/`` into the
bundled ``plugins/platforms/matrix`` platform plugin.  This package keeps the
native mautrix transport and layers AgentTeams policy on top through a user
plugin (``hermes/plugins/matrix-platform``) whose ``register()`` swaps the
bundled adapter factory for :class:`hermes_matrix.overlay_adapter.MatrixAdapter`.

The subclass adds only policy behavior:

  * outbound ``m.mentions`` enrichment
  * DM / group split allow-lists
  * copaw-style history buffering in group rooms
  * image downgrade when the active model lacks vision support
"""
