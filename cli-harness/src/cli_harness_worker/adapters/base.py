"""Base types shared by all CLI adapters."""
from __future__ import annotations

import asyncio
import logging
import os
import shutil
from dataclasses import dataclass, field
from typing import Optional

logger = logging.getLogger(__name__)

# Hard cap for a single headless CLI run. Tasks that legitimately need longer
# should be split by the coordinator — a Worker that silently blocks forever
# is worse than one that reports a timeout back to the room.
DEFAULT_TASK_TIMEOUT_SECONDS = 30 * 60


def env_first(*names: str) -> str:
    """Return the first non-empty env var among ``names``."""
    for name in names:
        value = os.environ.get(name, "")
        if value:
            return value
    return ""


def extra_cli_args() -> list[str]:
    """Extra argv appended to every CLI run (escape hatch for flag drift)."""
    extra = os.environ.get("AGENTTEAMS_CLI_EXTRA_ARGS", "")
    return extra.split() if extra else []


class AdapterError(RuntimeError):
    """Raised when an adapter cannot run its CLI (binary missing, bad config)."""


@dataclass
class RunResult:
    ok: bool
    output: str
    exit_code: int = 0
    timed_out: bool = False
    command: list[str] = field(default_factory=list)


class CLIAdapter:
    """One headless CLI coding agent.

    Subclasses declare ``runtime`` (the AgentTeams runtime name) and
    ``binary`` (the executable on PATH), and implement ``build_command``.
    """

    runtime: str = ""
    binary: str = ""

    def __init__(self, workspace: str | os.PathLike[str] | None = None) -> None:
        self.workspace = str(workspace) if workspace else os.getcwd()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def available(self) -> bool:
        return bool(self.binary) and shutil.which(self.binary) is not None

    def required_env(self, model: str, base_url: str, api_key: str) -> dict[str, str]:
        """Provider env vars this adapter consumes (bridge merges these)."""
        return {}

    def base_env(self) -> dict[str, str]:
        """Env adjustments applied to every CLI run (cwd, terminal, CI flags)."""
        return {}

    def build_command(self, prompt: str) -> list[str]:
        raise NotImplementedError

    async def run(self, prompt: str, timeout: Optional[int] = None) -> RunResult:
        """Run the CLI headlessly with ``prompt`` and capture final output."""
        if not self.available():
            raise AdapterError(
                f"{self.binary!r} not found on PATH for runtime {self.runtime!r}"
            )
        command = self.build_command(prompt)
        env = os.environ.copy()
        env.update(self.base_env())
        timeout = timeout or DEFAULT_TASK_TIMEOUT_SECONDS

        logger.info("cli run [%s]: %s", self.runtime, " ".join(command[:6]))
        process = await asyncio.create_subprocess_exec(
            *command,
            cwd=self.workspace,
            env=env,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
            stdin=asyncio.subprocess.DEVNULL,
        )
        timed_out = False
        try:
            stdout, _ = await asyncio.wait_for(process.communicate(), timeout)
        except asyncio.TimeoutError:
            timed_out = True
            process.kill()
            stdout, _ = await process.communicate()

        text = (stdout or b"").decode("utf-8", errors="replace").strip()
        if timed_out:
            return RunResult(
                ok=False,
                output=text or f"task timed out after {timeout}s",
                exit_code=process.returncode or 124,
                timed_out=True,
                command=command,
            )
        return RunResult(
            ok=process.returncode == 0,
            output=text,
            exit_code=process.returncode,
            command=command,
        )
