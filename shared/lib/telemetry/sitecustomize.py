"""Opt-in Python runtime bootstrap; loaded by Python via PYTHONPATH."""

import logging
import os

export_enabled = os.getenv("AGENTTEAMS_OTEL_ENABLED", "").lower() in ("true", "1")
if export_enabled or os.getenv("AGENTTEAMS_LOG_NATIVE_EVENTS") == "1":
    try:
        from at_telemetry import configure

        if not export_enabled:
            os.environ.setdefault("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "false")
        configure(local_only=not export_enabled)
    except Exception:
        # A telemetry outage must not prevent your Worker from starting.
        logging.getLogger("agentteams.telemetry").exception("OTel bootstrap failed")
