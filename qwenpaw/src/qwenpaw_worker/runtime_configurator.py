"""QwenPaw runtime profile and workspace configuration."""

from __future__ import annotations

import logging
import os
import shutil
from pathlib import Path

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.security_bootstrap import SecurityBootstrap

logger = logging.getLogger(__name__)

DEFAULT_AGENT_ID = "default"
BUILTIN_QWENPAW_QA_AGENT_ID = "QwenPaw_QA_Agent_0.2"


class RuntimeConfigurator:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config

    def prepare_env(self) -> None:
        os.environ["AGENTTEAMS_AGENT_NAME"] = self.config.agent_name
        os.environ["AGENTTEAMS_AGENT_ROLE"] = self.config.agent_role
        os.environ["AGENTTEAMS_AGENT_HOME"] = str(self.config.worker_home)
        os.environ["AGENTTEAMS_WORKER_HOME"] = str(self.config.worker_home)
        os.environ.setdefault("AGENTTEAMS_WORKER_NAME", self.config.worker_name)
        os.environ["QWENPAW_WORKING_DIR"] = str(self.config.qwenpaw_working_dir)
        os.environ["AGENT_WORKSPACE"] = str(self.config.default_workspace_dir)
        os.environ["AGENTTEAMS_SHARED_DIR"] = str(self.config.shared_dir)
        os.environ["TEAMHARNESS_SHARED_DIR"] = str(self.config.shared_dir)
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(self.config.runtime_config_path)
        os.environ.setdefault("QWENPAW_SECRET_DIR", f"{self.config.qwenpaw_working_dir}.secret")
        os.environ.setdefault("QWENPAW_RUNNING_IN_CONTAINER", "true")

    def link_workspace_shared(self, shared_dir: Path) -> None:
        workspace_shared = self.config.default_workspace_dir / "shared"
        shared_dir.mkdir(parents=True, exist_ok=True)
        workspace_shared.parent.mkdir(parents=True, exist_ok=True)

        if workspace_shared.is_symlink():
            if workspace_shared.resolve() == shared_dir.resolve():
                return
            workspace_shared.unlink()
        elif workspace_shared.exists():
            if workspace_shared.is_dir():
                shutil.rmtree(workspace_shared)
            else:
                workspace_shared.unlink()

        target = os.path.relpath(shared_dir, workspace_shared.parent)
        workspace_shared.symlink_to(target, target_is_directory=True)
        logger.info(
            "linked qwenpaw workspace shared dir component=worker step=link_workspace_shared worker=%s path=%s target=%s",
            self.config.worker_name,
            workspace_shared,
            target,
        )

    def configure_qwenpaw_runtime(self) -> None:
        try:
            from qwenpaw.config.config import AgentProfileConfig, AgentProfileRef, load_agent_config, save_agent_config
            from qwenpaw.config.utils import load_config, save_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=worker step=configure_qwenpaw_runtime action=skip")
            return

        root = load_config()
        SecurityBootstrap(self.config).ensure_session_file_guard(root)
        root.agents.active_agent = DEFAULT_AGENT_ID
        root.agents.profiles[DEFAULT_AGENT_ID] = AgentProfileRef(
            id=DEFAULT_AGENT_ID,
            workspace_dir=str(self.config.default_workspace_dir),
            enabled=True,
        )
        self._disable_builtin_qwenpaw_qa_agent(root, AgentProfileRef)
        if DEFAULT_AGENT_ID not in root.agents.agent_order:
            root.agents.agent_order.insert(0, DEFAULT_AGENT_ID)
        save_config(root)

        try:
            agent_config = load_agent_config(DEFAULT_AGENT_ID)
        except Exception:
            agent_config = AgentProfileConfig(
                id=DEFAULT_AGENT_ID,
                name=self.config.agent_name,
                description=f"AgentTeams QwenPaw {self.config.agent_role}",
                workspace_dir=str(self.config.default_workspace_dir),
            )

        agent_config.name = self.config.agent_name
        agent_config.workspace_dir = str(self.config.default_workspace_dir)
        agent_config.approval_level = "AUTO"
        prompt_files = list(agent_config.system_prompt_files or [])
        for file_name in ("AGENTS.md", "SOUL.md", "TEAMS.md"):
            if file_name not in prompt_files:
                prompt_files.append(file_name)
        agent_config.system_prompt_files = prompt_files
        save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _disable_builtin_qwenpaw_qa_agent(self, root, agent_profile_ref_cls) -> None:
        qa_workspace = self.config.qwenpaw_working_dir / "workspaces" / BUILTIN_QWENPAW_QA_AGENT_ID
        profiles = root.agents.profiles
        profile = profiles.get(BUILTIN_QWENPAW_QA_AGENT_ID)
        if profile is None:
            profiles[BUILTIN_QWENPAW_QA_AGENT_ID] = agent_profile_ref_cls(
                id=BUILTIN_QWENPAW_QA_AGENT_ID,
                workspace_dir=str(qa_workspace),
                enabled=False,
            )
            logger.info(
                "preseeded disabled builtin QwenPaw QA agent profile component=worker "
                "step=configure_qwenpaw_runtime agent_id=%s",
                BUILTIN_QWENPAW_QA_AGENT_ID,
            )
            return

        if getattr(profile, "enabled", True):
            profile.enabled = False
            logger.info(
                "disabled builtin QwenPaw QA agent profile component=worker "
                "step=configure_qwenpaw_runtime agent_id=%s",
                BUILTIN_QWENPAW_QA_AGENT_ID,
            )