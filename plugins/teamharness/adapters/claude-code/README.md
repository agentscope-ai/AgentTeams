# TeamHarness Claude Code Adapter

This adapter owns Claude Code-specific installation and hook integration.

The TeamHarness base package does not define runtime-neutral top-level hooks.
Claude Code hooks should live under this adapter when the Claude Code integration
phase defines the concrete local runtime trigger points, payload format, and
enforcement behavior.
