# -*- coding: utf-8 -*-
"""AI Provider module.

This module provides configuration models, health checking, and intelligent
routing for multi-AI provider support with Higress AI Gateway integration.
"""
from .models import (
    ModelSlotConfig,
    ProviderConfig,
    ModelSwitchConfig,
    ProvidersStore,
)
from .health import (
    HealthStatus,
    CircuitState,
    ProviderStats,
    CircuitBreaker,
    CircuitBreakerConfig,
    HealthCheckResult,
    ProviderHealthChecker,
    ModelRouter,
)

__all__ = [
    # Models
    "ModelSlotConfig",
    "ProviderConfig",
    "ModelSwitchConfig",
    "ProvidersStore",
    # Health & Routing
    "HealthStatus",
    "CircuitState",
    "ProviderStats",
    "CircuitBreaker",
    "CircuitBreakerConfig",
    "HealthCheckResult",
    "ProviderHealthChecker",
    "ModelRouter",
]
