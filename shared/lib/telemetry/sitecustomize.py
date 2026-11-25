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
    except Exception as exc:
        # A telemetry outage must not prevent your Worker from starting.
        logger = logging.getLogger("agentteams.telemetry")
        if isinstance(exc, ModuleNotFoundError) and (
            exc.name == "opentelemetry" or (exc.name or "").startswith("opentelemetry.")
        ):
            # Child interpreters may not have the optional SDK. Keep their
            # business stdout/stderr usable for tools and JSON pipelines.
            logger.debug("Optional OTel SDK is unavailable: %s", exc.name)
        else:
            logger.exception("OTel bootstrap failed")
