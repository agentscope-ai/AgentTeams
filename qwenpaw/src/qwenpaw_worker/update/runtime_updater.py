"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update.agent_package import AgentPackageManager
from qwenpaw_worker.update.channel_writers import ChannelWriterMixin
from qwenpaw_worker.update.constants import DEFAULT_AGENT_ID
from qwenpaw_worker.update.model_sync import ApplyResult, QwenPawModelRuntimeSync
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.teams_prompt import TeamsPromptMixin
from qwenpaw_worker.update.utils import (
    _count_collection,
    _duration_ms,
    _named_keys,
    _stable_json,
    _string,
)

logger = logging.getLogger(__name__)

class RuntimeUpdater(TeamsPromptMixin, ChannelWriterMixin):
    """Apply controller-projected runtime desired state inside one worker pod."""

    def __init__(
        self,
        config: WorkerConfig,
        adapter_apply: Optional[Callable[[], None]] = None,
        package_manager: Optional[AgentPackageManager] = None,
        runtime_config_pull: Optional[Callable[[], None]] = None,
        model_runtime_sync: Optional[Callable[[MemberRuntimeConfig], None]] = None,
        team_context_renderer: Optional[Callable[[MemberRuntimeConfig], str]] = None,
    ) -> None:
        self.config = config
        self.adapter_apply = adapter_apply
        self.runtime_config_pull = runtime_config_pull
        self.model_runtime_sync = model_runtime_sync or QwenPawModelRuntimeSync(config.console_port)
        self.team_context_renderer = team_context_renderer
        self.package_manager = package_manager or AgentPackageManager(
            config.qwenpaw_working_dir / "agent-packages",
            workspace_dir=config.default_workspace_dir,
        )
        self.current_config: Optional[MemberRuntimeConfig] = None

    def load(self) -> MemberRuntimeConfig:
        if self.runtime_config_pull is not None:
            self.runtime_config_pull()
        return MemberRuntimeConfig.load(self.config.runtime_config_path)

    def apply_once(
        self,
        runtime_config: Optional[MemberRuntimeConfig] = None,
        force: bool = False,
        reapply_adapter: bool = True,
    ) -> ApplyResult:
        started_at = time.monotonic()
        config = runtime_config or self.load()
        previous = self.current_config
        changed = force or previous is None or config.changed_from(previous)
        if not changed:
            logger.info(
                "runtime config apply skipped component=update worker=%s generation=%s changed=%s "
                "mcp_server_count=%s channel_names=%s credential_binding_count=%s duration_ms=%s",
                self.config.worker_name,
                config.generation,
                False,
                _count_collection(config.mcp_servers),
                _named_keys(config.channels),
                len(config.credential_bindings),
                _duration_ms(started_at),
            )
            return ApplyResult(runtime_config=config, changed=False, agent_package_dir=None)

        adapter_should_apply = (
            reapply_adapter
            and self.adapter_apply is not None
            and not self._adapter_neutral_change(config)
        )
        logger.info(
            "runtime config apply begin component=update worker=%s generation=%s team=%s member=%s role=%s "
            "force=%s reapply_adapter=%s adapter_applied=%s mcp_server_count=%s channel_names=%s "
            "credential_binding_count=%s duration_ms=%s",
            self.config.worker_name,
            config.generation,
            config.team_name,
            config.member_name,
            config.member_role,
            force,
            reapply_adapter,
            adapter_should_apply,
            _count_collection(config.mcp_servers),
            _named_keys(config.channels),
            len(config.credential_bindings),
            _duration_ms(started_at),
        )
        self._apply_member_identity(config)
        self._apply_model(config)
        self._apply_mcp_servers(config)
        self._apply_matrix_channel(config)
        self._apply_dingtalk_channel(config)
        self._apply_channel_policy(config)
        self._apply_team_context_prompt(config)

        applied_package = self.package_manager.apply(config)

        adapter_applied = False
        if adapter_should_apply:
            self.adapter_apply()
            adapter_applied = True

        self._sync_model_runtime_if_needed(previous, config)
        self.current_config = config
        logger.info(
            "runtime config apply complete component=update worker=%s generation=%s changed=%s "
            "agent_package_dir=%s mcp_server_count=%s channel_names=%s credential_binding_count=%s "
            "adapter_applied=%s duration_ms=%s",
            self.config.worker_name,
            config.generation,
            True,
            applied_package,
            _count_collection(config.mcp_servers),
            _named_keys(config.channels),
            len(config.credential_bindings),
            adapter_applied,
            _duration_ms(started_at),
        )
        return ApplyResult(runtime_config=config, changed=True, agent_package_dir=applied_package)

    def _sync_model_runtime_if_needed(
        self,
        previous: Optional[MemberRuntimeConfig],
        config: MemberRuntimeConfig,
    ) -> None:
        if previous is None or self.model_runtime_sync is None:
            return
        if _stable_json(config.model) == _stable_json(previous.model):
            return
        started_at = time.monotonic()
        try:
            self.model_runtime_sync(config)
        except Exception as exc:
            logger.warning(
                "runtime model sync failed component=update step=model_runtime_sync event=failed "
                "worker=%s generation=%s changed=%s error_type=%s safe_error_summary=%s duration_ms=%s",
                self.config.worker_name,
                config.generation,
                True,
                type(exc).__name__,
                type(exc).__name__,
                _duration_ms(started_at),
            )
            raise

    def _adapter_neutral_change(self, config: MemberRuntimeConfig) -> bool:
        previous = self.current_config
        if previous is None:
            return False
        return (
            config.agent_package_identity == previous.agent_package_identity
            and _stable_json(config.model) == _stable_json(previous.model)
            and _stable_json(config.channel_policy) == _stable_json(previous.channel_policy)
            and config.credential_runtime_identity == previous.credential_runtime_identity
            and self._team_context_content_identity(config) == self._team_context_content_identity(previous)
            and (
                _stable_json(config.mcp_servers) != _stable_json(previous.mcp_servers)
                or _stable_json(config.channels) != _stable_json(previous.channels)
            )
        )

    def _apply_member_identity(self, config: MemberRuntimeConfig) -> None:
        role = config.member_role
        if role:
            self.config.agent_role = role
            os.environ["AGENTTEAMS_AGENT_ROLE"] = role
            os.environ["AGENTTEAMS_WORKER_ROLE"] = role

    def _apply_model(self, config: MemberRuntimeConfig) -> None:
        model = config.model
        if not model:
            return
        provider_id = _string(model.get("providerId") or model.get("provider_id") or model.get("provider"))
        model_name = _string(model.get("model") or model.get("name"))
        if not provider_id or not model_name:
            return
        try:
            from qwenpaw.config.config import ModelSlotConfig, load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_model action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        agent_config.active_model = ModelSlotConfig(provider_id=provider_id, model=model_name)
        save_agent_config(DEFAULT_AGENT_ID, agent_config)
        self._apply_openai_compatible_provider(config, provider_id, model_name, ModelSlotConfig)

    def _apply_openai_compatible_provider(
        self,
        config: MemberRuntimeConfig,
        provider_id: str,
        model_name: str,
        model_slot_config_class: Any,
    ) -> None:
        model = config.model
        base_url = _string(
            model.get("baseUrl")
            or model.get("base_url")
            or model.get("gatewayUrl")
            or model.get("gateway_url")
            or model.get("endpoint")
            or os.getenv("AGENTTEAMS_AI_GATEWAY_URL")
        )
        api_key = _string(model.get("apiKey") or model.get("api_key"))
        api_key_env = _string(
            model.get("apiKeyEnv")
            or model.get("api_key_env")
            or config.credentials.get("gatewayKeyEnv")
            or "AGENTTEAMS_WORKER_GATEWAY_KEY"
        )
        if not api_key and api_key_env:
            api_key = _string(os.getenv(api_key_env))
        if not base_url or not api_key:
            return

        try:
            from qwenpaw.providers.provider import ModelInfo, ProviderInfo
            from qwenpaw.providers.provider_manager import ProviderManager
        except ImportError:
            logger.info("qwenpaw provider package unavailable component=update step=apply_provider action=skip")
            return

        manager = ProviderManager.get_instance()
        provider_data = ProviderInfo(
            id=provider_id,
            name=_string(model.get("providerName") or model.get("provider_name") or provider_id),
            base_url=self._openai_compatible_base_url(base_url),
            api_key=api_key,
            chat_model=_string(model.get("chatModel") or model.get("chat_model") or "OpenAIChatModel"),
            models=[ModelInfo(id=model_name, name=model_name)],
            is_custom=True,
            support_model_discovery=False,
            support_connection_check=False,
        )
        provider = manager._provider_from_data(provider_data.model_dump())
        manager.custom_providers[provider_id] = provider
        manager.save_provider_config(provider_id, provider)
        manager.active_model = model_slot_config_class(provider_id=provider_id, model=model_name)
        manager.save_active_model(manager.active_model)

    def _openai_compatible_base_url(self, base_url: str) -> str:
        value = base_url.rstrip("/")
        if value.endswith("/v1"):
            return value
        return f"{value}/v1"

    def _apply_mcp_servers(self, config: MemberRuntimeConfig) -> None:
        servers = self._mcporter_servers(config)
        legacy_path = self.config.default_workspace_dir / "mcporter-servers.json"
        if legacy_path.exists():
            legacy_path.unlink()

        path = self.config.default_workspace_dir / "config" / "mcporter.json"
        payload = {"mcpServers": servers}
        text = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    async def loop(self) -> None:
        logger.info(
            "runtime config update loop started component=update worker=%s interval_seconds=%s",
            self.config.worker_name,
            self.config.runtime_config_poll_interval,
        )
        try:
            while True:
                await asyncio.sleep(self.config.runtime_config_poll_interval)
                try:
                    started_at = time.monotonic()
                    await asyncio.to_thread(self._load_and_apply_once)
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    logger.warning(
                        "runtime config update failed component=update worker=%s error_type=%s duration_ms=%s",
                        self.config.worker_name,
                        type(exc).__name__,
                        _duration_ms(started_at),
                    )
        except asyncio.CancelledError:
            logger.info("runtime config update loop stopped component=update worker=%s", self.config.worker_name)
            raise
