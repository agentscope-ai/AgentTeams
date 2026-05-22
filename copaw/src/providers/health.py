# -*- coding: utf-8 -*-
"""AI Provider Health Checker and Router.

This module provides health checking and intelligent routing for multi-AI
provider support with Higress AI Gateway integration.
"""
import asyncio
import logging
import time
from dataclasses import dataclass, field
from enum import Enum
from typing import Dict, List, Optional, Any, Callable
from collections import defaultdict

logger = logging.getLogger(__name__)


class HealthStatus(Enum):
    """Provider health status."""
    HEALTHY = "healthy"
    UNHEALTHY = "unhealthy"
    DEGRADED = "degraded"
    UNKNOWN = "unknown"


class CircuitState(Enum):
    """Circuit breaker state."""
    CLOSED = "closed"  # Normal operation
    OPEN = "open"  # Failing, reject requests
    HALF_OPEN = "half_open"  # Testing recovery


@dataclass
class ProviderStats:
    """Statistics for a provider."""
    total_requests: int = 0
    successful_requests: int = 0
    failed_requests: int = 0
    consecutive_failures: int = 0
    last_success_time: float = 0.0
    last_failure_time: float = 0.0
    avg_latency_ms: float = 0.0
    latency_samples: List[float] = field(default_factory=list)
    
    def record_success(self, latency_ms: float) -> None:
        """Record a successful request."""
        self.total_requests += 1
        self.successful_requests += 1
        self.consecutive_failures = 0
        self.last_success_time = time.time()
        
        # Maintain rolling average of latency (last 100 samples)
        self.latency_samples.append(latency_ms)
        if len(self.latency_samples) > 100:
            self.latency_samples.pop(0)
        self.avg_latency_ms = sum(self.latency_samples) / len(self.latency_samples)
    
    def record_failure(self) -> None:
        """Record a failed request."""
        self.total_requests += 1
        self.failed_requests += 1
        self.consecutive_failures += 1
        self.last_failure_time = time.time()
    
    @property
    def success_rate(self) -> float:
        """Calculate success rate as percentage."""
        if self.total_requests == 0:
            return 100.0
        return (self.successful_requests / self.total_requests) * 100


@dataclass
class CircuitBreakerConfig:
    """Configuration for circuit breaker."""
    failure_threshold: int = 5  # Failures before opening circuit
    recovery_timeout: float = 30.0  # Seconds before trying again
    half_open_max_calls: int = 3  # Max calls in half-open state


class CircuitBreaker:
    """Circuit breaker for provider failover."""
    
    def __init__(self, config: Optional[CircuitBreakerConfig] = None):
        self.config = config or CircuitBreakerConfig()
        self.state = CircuitState.CLOSED
        self.failure_count = 0
        self.last_failure_time = 0.0
        self.half_open_calls = 0
        self._lock = asyncio.Lock()
    
    async def can_execute(self) -> bool:
        """Check if request can be executed."""
        async with self._lock:
            if self.state == CircuitState.CLOSED:
                return True
            
            if self.state == CircuitState.OPEN:
                # Check if recovery timeout has passed
                if time.time() - self.last_failure_time >= self.config.recovery_timeout:
                    self.state = CircuitState.HALF_OPEN
                    self.half_open_calls = 0
                    logger.info("Circuit breaker transitioning to HALF_OPEN")
                    return True
                return False
            
            # HALF_OPEN state
            if self.half_open_calls < self.config.half_open_max_calls:
                self.half_open_calls += 1
                return True
            return False
    
    async def record_success(self) -> None:
        """Record successful execution."""
        async with self._lock:
            if self.state == CircuitState.HALF_OPEN:
                logger.info("Circuit breaker transitioning to CLOSED after successful call")
                self.state = CircuitState.CLOSED
                self.failure_count = 0
                self.half_open_calls = 0
            elif self.state == CircuitState.CLOSED:
                self.failure_count = max(0, self.failure_count - 1)
    
    async def record_failure(self) -> None:
        """Record failed execution."""
        async with self._lock:
            self.failure_count += 1
            self.last_failure_time = time.time()
            
            if self.state == CircuitState.HALF_OPEN:
                logger.warning("Circuit breaker transitioning to OPEN after failure in HALF_OPEN")
                self.state = CircuitState.OPEN
            elif self.state == CircuitState.CLOSED:
                if self.failure_count >= self.config.failure_threshold:
                    logger.warning(
                        f"Circuit breaker transitioning to OPEN after {self.failure_count} failures"
                    )
                    self.state = CircuitState.OPEN


@dataclass
class HealthCheckResult:
    """Result of a health check."""
    provider_id: str
    status: HealthStatus
    latency_ms: float = 0.0
    error_message: str = ""
    timestamp: float = field(default_factory=time.time)


class ProviderHealthChecker:
    """Health checker for AI providers."""
    
    def __init__(
        self,
        check_interval: float = 300.0,  # 5 minutes
        timeout: float = 10.0,
        health_endpoint: str = "/health",
    ):
        self.check_interval = check_interval
        self.timeout = timeout
        self.health_endpoint = health_endpoint
        self._results: Dict[str, HealthCheckResult] = {}
        self._stats: Dict[str, ProviderStats] = defaultdict(ProviderStats)
        self._circuit_breakers: Dict[str, CircuitBreaker] = {}
        self._running = False
        self._task: Optional[asyncio.Task] = None
        self._check_func: Optional[Callable] = None
    
    def set_health_check_func(self, func: Callable) -> None:
        """Set the health check function."""
        self._check_func = func
    
    def get_stats(self, provider_id: str) -> ProviderStats:
        """Get statistics for a provider."""
        return self._stats[provider_id]
    
    def get_circuit_breaker(self, provider_id: str) -> CircuitBreaker:
        """Get or create circuit breaker for a provider."""
        if provider_id not in self._circuit_breakers:
            self._circuit_breakers[provider_id] = CircuitBreaker()
        return self._circuit_breakers[provider_id]
    
    def get_health_status(self, provider_id: str) -> HealthStatus:
        """Get current health status for a provider."""
        if provider_id not in self._results:
            return HealthStatus.UNKNOWN
        
        result = self._results[provider_id]
        # Consider status degraded if recent failures
        stats = self._stats.get(provider_id)
        if stats and stats.consecutive_failures >= 3:
            return HealthStatus.DEGRADED
        
        return result.status
    
    def is_healthy(self, provider_id: str) -> bool:
        """Check if provider is healthy enough for requests."""
        status = self.get_health_status(provider_id)
        if status == HealthStatus.UNHEALTHY:
            return False
        
        # Check circuit breaker
        cb = self.get_circuit_breaker(provider_id)
        if cb.state == CircuitState.OPEN:
            return False
        
        return True
    
    async def check_health(
        self,
        provider_id: str,
        base_url: str,
        api_key: str,
    ) -> HealthCheckResult:
        """Perform health check for a provider."""
        start_time = time.time()
        
        try:
            if self._check_func:
                # Use custom check function
                result = await self._check_func(provider_id, base_url, api_key)
                if isinstance(result, HealthCheckResult):
                    self._results[provider_id] = result
                    return result
            
            # Default: simple connectivity check
            import aiohttp
            
            headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
            
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    f"{base_url.rstrip('/')}{self.health_endpoint}",
                    headers=headers,
                    timeout=aiohttp.ClientTimeout(total=self.timeout),
                ) as response:
                    latency_ms = (time.time() - start_time) * 1000
                    
                    if response.status < 400:
                        result = HealthCheckResult(
                            provider_id=provider_id,
                            status=HealthStatus.HEALTHY,
                            latency_ms=latency_ms,
                        )
                    else:
                        result = HealthCheckResult(
                            provider_id=provider_id,
                            status=HealthStatus.DEGRADED,
                            latency_ms=latency_ms,
                            error_message=f"HTTP {response.status}",
                        )
        except asyncio.TimeoutError:
            result = HealthCheckResult(
                provider_id=provider_id,
                status=HealthStatus.UNHEALTHY,
                error_message="Timeout",
            )
        except Exception as e:
            result = HealthCheckResult(
                provider_id=provider_id,
                status=HealthStatus.UNHEALTHY,
                error_message=str(e),
            )
        
        self._results[provider_id] = result
        logger.debug(f"Health check for {provider_id}: {result.status.value}")
        return result
    
    async def start_background_checks(
        self,
        providers: Dict[str, Dict[str, Any]],
    ) -> None:
        """Start background health checks."""
        self._running = True
        
        async def _check_loop():
            while self._running:
                for provider_id, config in providers.items():
                    if not self._running:
                        break
                    
                    try:
                        await self.check_health(
                            provider_id,
                            config.get("base_url", ""),
                            config.get("api_key", ""),
                        )
                    except Exception as e:
                        logger.error(f"Health check failed for {provider_id}: {e}")
                
                await asyncio.sleep(self.check_interval)
        
        self._task = asyncio.create_task(_check_loop())
        logger.info(f"Started background health checks (interval={self.check_interval}s)")
    
    async def stop(self) -> None:
        """Stop background health checks."""
        self._running = False
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        logger.info("Stopped background health checks")
    
    def get_all_statuses(self) -> Dict[str, HealthStatus]:
        """Get health status for all providers."""
        return {pid: self.get_health_status(pid) for pid in self._results}


class ModelRouter:
    """Intelligent router for model selection and failover."""
    
    def __init__(
        self,
        health_checker: Optional[ProviderHealthChecker] = None,
        session_sticky: bool = True,
        load_balance_strategy: str = "priority",  # priority, round_robin, weighted
    ):
        self.health_checker = health_checker or ProviderHealthChecker()
        self.session_sticky = session_sticky
        self.load_balance_strategy = load_balance_strategy
        
        # Session tracking for sticky sessions
        self._session_map: Dict[str, str] = {}  # session_id -> provider_id
        self._round_robin_index = 0
        self._current_fallback_chain: List[str] = []
    
    def set_fallback_chain(self, chain: List[str]) -> None:
        """Set the fallback chain (list of provider/model IDs)."""
        self._current_fallback_chain = chain
    
    async def select_provider(
        self,
        available_providers: List[str],
        session_id: Optional[str] = None,
        preferred_provider: Optional[str] = None,
    ) -> Optional[str]:
        """Select the best provider based on strategy and health."""
        if not available_providers:
            return None
        
        # If session sticky and we have a previous provider
        if self.session_sticky and session_id and session_id in self._session_map:
            prev_provider = self._session_map[session_id]
            if prev_provider in available_providers:
                if self.health_checker.is_healthy(prev_provider):
                    return prev_provider
                else:
                    logger.warning(
                        f"Previous provider {prev_provider} unhealthy, selecting new one"
                    )
        
        # If preferred provider specified and healthy
        if preferred_provider:
            if preferred_provider in available_providers:
                if self.health_checker.is_healthy(preferred_provider):
                    if self.session_sticky and session_id:
                        self._session_map[session_id] = preferred_provider
                    return preferred_provider
                else:
                    logger.warning(
                        f"Preferred provider {preferred_provider} unhealthy"
                    )
        
        # Select based on strategy
        healthy_providers = [
            p for p in available_providers
            if self.health_checker.is_healthy(p)
        ]
        
        if not healthy_providers:
            # All unhealthy, use any available (fallback behavior)
            healthy_providers = available_providers
        
        selected = None
        if self.load_balance_strategy == "round_robin":
            idx = self._round_robin_index % len(healthy_providers)
            selected = healthy_providers[idx]
            self._round_robin_index += 1
        elif self.load_balance_strategy == "weighted":
            # Select based on success rate
            selected = max(
                healthy_providers,
                key=lambda p: self.health_checker.get_stats(p).success_rate,
            )
        else:  # priority (default)
            # First healthy provider in list order
            selected = healthy_providers[0]
        
        if self.session_sticky and session_id:
            self._session_map[session_id] = selected
        
        logger.debug(f"Selected provider: {selected} (strategy={self.load_balance_strategy})")
        return selected
    
    async def route_request(
        self,
        providers_config: Dict[str, Any],
        session_id: Optional[str] = None,
        preferred_provider: Optional[str] = None,
        retry_count: int = 3,
        retry_delay: float = 1.0,
    ) -> tuple[Optional[str], Optional[str]]:
        """Route a request to the best provider.
        
        Returns:
            Tuple of (provider_id, model_id) or (None, None) if no provider available
        """
        available = list(providers_config.keys())
        
        provider_id = await self.select_provider(
            available,
            session_id=session_id,
            preferred_provider=preferred_provider,
        )
        
        if not provider_id:
            return None, None
        
        provider_cfg = providers_config.get(provider_id, {})
        models = provider_cfg.get("models", [])
        model_id = models[0]["id"] if models else ""
        
        return provider_id, model_id
    
    async def handle_failure(
        self,
        provider_id: str,
        session_id: Optional[str] = None,
        error_type: str = "unknown",
    ) -> Optional[str]:
        """Handle a provider failure and potentially switch to fallback.
        
        Args:
            provider_id: The failed provider ID
            session_id: Optional session identifier
            error_type: Type of error (timeout, rate_limit, api_error)
        
        Returns:
            Alternative provider ID or None if no alternative
        """
        # Record failure
        stats = self.health_checker.get_stats(provider_id)
        stats.record_failure()
        
        cb = self.health_checker.get_circuit_breaker(provider_id)
        await cb.record_failure()
        
        logger.warning(
            f"Provider {provider_id} failed ({error_type}), "
            f"consecutive_failures={stats.consecutive_failures}, "
            f"circuit_state={cb.state.name}"
        )
        
        # Remove from session map to force re-selection
        if session_id and session_id in self._session_map:
            del self._session_map[session_id]
        
        # Try fallback chain if configured
        if self._current_fallback_chain:
            for fallback in self._current_fallback_chain:
                # Extract provider_id from fallback (format: provider_id/model_id)
                fallback_provider = fallback.split("/")[0] if "/" in fallback else fallback
                if fallback_provider != provider_id:
                    if self.health_checker.is_healthy(fallback_provider):
                        logger.info(f"Switching to fallback provider: {fallback_provider}")
                        return fallback_provider
        
        return None
    
    async def handle_success(
        self,
        provider_id: str,
        latency_ms: float,
    ) -> None:
        """Handle a successful provider request."""
        stats = self.health_checker.get_stats(provider_id)
        stats.record_success(latency_ms)
        
        cb = self.health_checker.get_circuit_breaker(provider_id)
        await cb.record_success()
    
    def clear_session(self, session_id: str) -> None:
        """Clear session mapping."""
        if session_id in self._session_map:
            del self._session_map[session_id]
    
    def get_routing_stats(self) -> Dict[str, Any]:
        """Get routing statistics."""
        stats = {}
        for provider_id in self.health_checker._results.keys():
            pstats = self.health_checker.get_stats(provider_id)
            status = self.health_checker.get_health_status(provider_id)
            cb = self.health_checker.get_circuit_breaker(provider_id)
            
            stats[provider_id] = {
                "health_status": status.value,
                "circuit_state": cb.state.name,
                "total_requests": pstats.total_requests,
                "success_rate": pstats.success_rate,
                "avg_latency_ms": pstats.avg_latency_ms,
                "consecutive_failures": pstats.consecutive_failures,
            }
        
        return stats
