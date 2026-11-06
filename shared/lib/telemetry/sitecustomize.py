"""Opt-in Python runtime bootstrap; loaded by Python via PYTHONPATH."""

import logging
import os

if os.getenv("AGENTTEAMS_OTEL_ENABLED", "").lower() in ("true", "1"):
    try:
        from at_telemetry import configure

        configure()
    except Exception:
        # A telemetry outage must not prevent your Worker from starting.
        logging.getLogger("agentteams.telemetry").exception("OTel bootstrap failed")
