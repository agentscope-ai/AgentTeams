# -*- coding: utf-8 -*-
"""AI Provider and Model configuration models.

This module provides configuration models for multi-AI provider support
and model switching functionality.
"""
from typing import Optional, List, Dict, Any, Literal
from pydantic import BaseModel, Field, ConfigDict


class ModelSlotConfig(BaseModel):
    """Configuration for a single model slot.
    
    A model slot represents a specific model instance that can be used
    by an agent. It includes provider information and model-specific settings.
    
    Attributes:
        provider_id: The unique identifier of the AI provider (e.g., 'openai', 'anthropic')
        model: The model name/ID (e.g., 'gpt-4', 'claude-3-opus')
        name: Human-readable name for this model slot
        enabled: Whether this model slot is currently enabled
        priority: Priority level for model selection (higher = preferred)
        context_window: Maximum context window size in tokens
        max_output_tokens: Maximum output tokens allowed
        temperature: Default temperature for generation
        top_p: Default top_p for generation
        frequency_penalty: Default frequency penalty
        presence_penalty: Default presence penalty
        extra_config: Additional provider-specific configuration
    """
    model_config = ConfigDict(extra="allow")
    
    provider_id: str = Field(
        default="",
        description="Provider identifier (e.g., 'openai', 'anthropic', 'google')"
    )
    model: str = Field(
        default="",
        description="Model name/ID within the provider"
    )
    name: str = Field(
        default="",
        description="Human-readable name for this model slot"
    )
    enabled: bool = Field(
        default=True,
        description="Whether this model slot is enabled"
    )
    priority: int = Field(
        default=0,
        description="Priority level for model selection (higher = preferred)"
    )
    context_window: Optional[int] = Field(
        default=None,
        description="Maximum context window size in tokens"
    )
    max_output_tokens: Optional[int] = Field(
        default=None,
        description="Maximum output tokens allowed"
    )
    temperature: Optional[float] = Field(
        default=None,
        ge=0.0,
        le=2.0,
        description="Default temperature for generation"
    )
    top_p: Optional[float] = Field(
        default=None,
        ge=0.0,
        le=1.0,
        description="Default top_p for nucleus sampling"
    )
    frequency_penalty: Optional[float] = Field(
        default=None,
        ge=-2.0,
        le=2.0,
        description="Default frequency penalty"
    )
    presence_penalty: Optional[float] = Field(
        default=None,
        ge=-2.0,
        le=2.0,
        description="Default presence penalty"
    )
    extra_config: Dict[str, Any] = Field(
        default_factory=dict,
        description="Additional provider-specific configuration"
    )
    
    @property
    def full_model_id(self) -> str:
        """Return the full model identifier in format 'provider_id/model'."""
        if self.provider_id and self.model:
            return f"{self.provider_id}/{self.model}"
        return self.model or ""
    
    @classmethod
    def from_full_id(cls, full_id: str, **kwargs) -> "ModelSlotConfig":
        """Create a ModelSlotConfig from a full model ID string.
        
        Args:
            full_id: Full model ID in format 'provider_id/model'
            **kwargs: Additional arguments to pass to the constructor
            
        Returns:
            ModelSlotConfig instance
        """
        if "/" in full_id:
            provider_id, model = full_id.split("/", 1)
        else:
            provider_id = ""
            model = full_id
        
        return cls(provider_id=provider_id, model=model, **kwargs)
    
    def __str__(self) -> str:
        """Return string representation of the model slot."""
        if self.name:
            return self.name
        return self.full_model_id or "unnamed_slot"


class ProviderConfig(BaseModel):
    """Configuration for an AI provider.
    
    Attributes:
        id: Unique provider identifier
        name: Human-readable provider name
        base_url: API base URL
        api_key: API key for authentication
        api_key_prefix: Prefix for API key header (if needed)
        chat_model: Chat model class name to use
        models: List of available models from this provider
        enabled: Whether this provider is enabled
        rate_limit: Requests per minute limit
        timeout: Request timeout in seconds
        retry_count: Number of retries on failure
        extra_headers: Additional HTTP headers
    """
    model_config = ConfigDict(extra="allow")
    
    id: str = Field(..., description="Unique provider identifier")
    name: str = Field(default="", description="Human-readable provider name")
    default_base_url: str = Field(default="", description="Default API base URL")
    base_url: str = Field(default="", description="Current API base URL")
    api_key: str = Field(default="", description="API key for authentication")
    api_key_prefix: str = Field(default="", description="API key header prefix")
    chat_model: str = Field(default="OpenAIChatModel", description="Chat model class name")
    models: List[Dict[str, str]] = Field(
        default_factory=list,
        description="List of available models: [{'id': '...', 'name': '...'}]"
    )
    enabled: bool = Field(default=True, description="Whether this provider is enabled")
    rate_limit: Optional[int] = Field(
        default=None,
        description="Requests per minute limit (0 = unlimited)"
    )
    timeout: float = Field(default=60.0, description="Request timeout in seconds")
    retry_count: int = Field(default=3, description="Number of retries on failure")
    extra_headers: Dict[str, str] = Field(
        default_factory=dict,
        description="Additional HTTP headers"
    )
    
    @property
    def model_ids(self) -> List[str]:
        """Return list of model IDs available from this provider."""
        return [m.get("id", "") for m in self.models if m.get("id")]
    
    def get_model(self, model_id: str) -> Optional[Dict[str, str]]:
        """Get model configuration by ID.
        
        Args:
            model_id: The model ID to look up
            
        Returns:
            Model configuration dict or None if not found
        """
        for model in self.models:
            if model.get("id") == model_id:
                return model
        return None


class ModelSwitchConfig(BaseModel):
    """Configuration for model switching behavior.
    
    This configuration controls how the system switches between different
    AI providers and models based on various criteria.
    
    Attributes:
        enabled: Whether automatic model switching is enabled
        mode: Switching mode ('manual', 'auto', 'fallback')
        default_provider: Default provider ID to use
        default_model: Default model ID to use
        fallback_chain: Ordered list of fallback models
        switch_on_error: Whether to switch on API errors
        switch_on_rate_limit: Whether to switch on rate limit errors
        switch_on_timeout: Whether to switch on timeout errors
        health_check_enabled: Whether to perform provider health checks
        health_check_interval: Health check interval in seconds
    """
    model_config = ConfigDict(extra="ignore")
    
    enabled: bool = Field(
        default=False,
        description="Whether automatic model switching is enabled"
    )
    mode: Literal["manual", "auto", "fallback"] = Field(
        default="manual",
        description="Switching mode: manual (user), auto (smart), fallback (error-only)"
    )
    default_provider: str = Field(
        default="",
        description="Default provider ID to use"
    )
    default_model: str = Field(
        default="",
        description="Default model ID to use"
    )
    fallback_chain: List[str] = Field(
        default_factory=list,
        description="Ordered list of fallback model IDs (format: 'provider/model')"
    )
    switch_on_error: bool = Field(
        default=True,
        description="Whether to switch to fallback on API errors"
    )
    switch_on_rate_limit: bool = Field(
        default=True,
        description="Whether to switch on rate limit (429) errors"
    )
    switch_on_timeout: bool = Field(
        default=True,
        description="Whether to switch on timeout errors"
    )
    health_check_enabled: bool = Field(
        default=False,
        description="Whether to perform provider health checks"
    )
    health_check_interval: int = Field(
        default=300,
        ge=60,
        description="Health check interval in seconds"
    )
    
    def get_next_fallback(self, current_model: str) -> Optional[str]:
        """Get the next fallback model in the chain.
        
        Args:
            current_model: Current model ID that failed
            
        Returns:
            Next fallback model ID or None if no more fallbacks
        """
        if current_model not in self.fallback_chain:
            return self.fallback_chain[0] if self.fallback_chain else None
        
        idx = self.fallback_chain.index(current_model)
        if idx < len(self.fallback_chain) - 1:
            return self.fallback_chain[idx + 1]
        return None


class ProvidersStore(BaseModel):
    """Main container for all provider configurations.
    
    This is the root configuration object that contains all provider
    and model switching settings.
    
    Attributes:
        providers: Dictionary of provider configurations by ID
        custom_providers: Dictionary of custom provider configurations
        active_llm: Currently active LLM configuration
        model_switch: Model switching configuration
    """
    model_config = ConfigDict(extra="ignore")
    
    providers: Dict[str, ProviderConfig] = Field(
        default_factory=dict,
        description="Built-in provider configurations"
    )
    custom_providers: Dict[str, ProviderConfig] = Field(
        default_factory=dict,
        description="Custom provider configurations"
    )
    active_llm: Dict[str, str] = Field(
        default_factory=lambda: {"provider_id": "", "model": ""},
        description="Currently active LLM: {'provider_id': '...', 'model': '...'}"
    )
    model_switch: ModelSwitchConfig = Field(
        default_factory=ModelSwitchConfig,
        description="Model switching configuration"
    )
    
    def get_active_model_slot(self) -> ModelSlotConfig:
        """Get the currently active model as a ModelSlotConfig.
        
        Returns:
            ModelSlotConfig for the active model
        """
        provider_id = self.active_llm.get("provider_id", "")
        model = self.active_llm.get("model", "")
        return ModelSlotConfig(provider_id=provider_id, model=model)
    
    def set_active_model(self, provider_id: str, model: str) -> None:
        """Set the active model.
        
        Args:
            provider_id: Provider ID to activate
            model: Model ID to activate
        """
        self.active_llm["provider_id"] = provider_id
        self.active_llm["model"] = model
    
    def get_all_providers(self) -> Dict[str, ProviderConfig]:
        """Get all providers (built-in + custom).
        
        Returns:
            Combined dictionary of all providers
        """
        result = dict(self.providers)
        result.update(self.custom_providers)
        return result
    
    def get_provider(self, provider_id: str) -> Optional[ProviderConfig]:
        """Get a provider by ID.
        
        Args:
            provider_id: Provider ID to look up
            
        Returns:
            ProviderConfig or None if not found
        """
        if provider_id in self.custom_providers:
            return self.custom_providers[provider_id]
        return self.providers.get(provider_id)
    
    def get_all_models(self) -> List[ModelSlotConfig]:
        """Get all available models from all providers.
        
        Returns:
            List of ModelSlotConfig for all available models
        """
        models = []
        for provider_id, provider in self.get_all_providers().items():
            if provider.enabled:
                for model_info in provider.models:
                    model_id = model_info.get("id", "")
                    if model_id:
                        models.append(ModelSlotConfig(
                            provider_id=provider_id,
                            model=model_id,
                            name=model_info.get("name", model_id)
                        ))
        return models
