#!/usr/bin/env python3
"""Mechanically split qwenpaw_worker/update.py — line-range based, safe."""

from __future__ import annotations

import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
UPDATE_PY = ROOT / "qwenpaw" / "src" / "qwenpaw_worker" / "update.py"
PKG = ROOT / "qwenpaw" / "src" / "qwenpaw_worker" / "update"

# 1-based inclusive line ranges from original update.py
RANGES = {
    "constants": (30, 41),
    "utils": (44, 190),
    "runtime_config": (191, 441),
    "agent_package": (444, 1291),
    "model_sync": (1293, 1527),
    "teams_prompt_mixin": (1677, 1769),
    "channel_writer_mixin": (1868, 2266),
    "runtime_updater_core": (1529, 1676, 1771, 1867, 2268, 2291),
}

INIT = '''"""Runtime desired-state update package."""

from qwenpaw_worker.update.agent_package import AgentPackageManager
from qwenpaw_worker.update.constants import (
    AGENT_IDENTITY_DATA_ENDPOINT_FORMAT,
    DEFAULT_AGENT_ID,
    PACKAGE_PROMPT_FILES,
    PACKAGE_RUNTIME_OWNED_CONFIG_FILES,
    REGION_ID_ENV_NAMES,
    TEAMS_CONTEXT_END,
    TEAMS_CONTEXT_START,
    TEAMS_INTERNAL_CONTROL_MARKER,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.model_sync import ApplyResult, QwenPawModelRuntimeSync
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.runtime_updater import RuntimeUpdater
from qwenpaw_worker.update.utils import _strip_json_line_comments, credential_provider_env_name

__all__ = [
    "AGENT_IDENTITY_DATA_ENDPOINT_FORMAT",
    "AgentPackageManager",
    "ApplyResult",
    "DEFAULT_AGENT_ID",
    "MemberRuntimeConfig",
    "PACKAGE_PROMPT_FILES",
    "PACKAGE_RUNTIME_OWNED_CONFIG_FILES",
    "QwenPawModelRuntimeSync",
    "REGION_ID_ENV_NAMES",
    "RuntimeUpdater",
    "TEAMS_CONTEXT_END",
    "TEAMS_CONTEXT_START",
    "TEAMS_INTERNAL_CONTROL_MARKER",
    "TEAMS_PROMPT_FILE",
    "_strip_json_line_comments",
    "credential_provider_env_name",
]
'''

HEADER = '''"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

'''


def _slice_lines(lines: list[str], start: int, end: int) -> str:
    return "".join(lines[start - 1 : end])


def _slice_ranges(lines: list[str], ranges: list[tuple[int, int]]) -> str:
    parts = [_slice_lines(lines, start, end) for start, end in ranges]
    return "".join(parts)


def main() -> None:
    text = UPDATE_PY.read_text(encoding="utf-8")
    lines = text.splitlines(keepends=True)

    if PKG.exists():
        shutil.rmtree(PKG)
    PKG.mkdir(parents=True)

    (PKG / "constants.py").write_text(
        HEADER + "from pathlib import Path\n\n" + _slice_lines(lines, 30, 41),
        encoding="utf-8",
    )

    utils_header = HEADER + '''import json
import logging
import os
import re
import time
from pathlib import Path
from typing import Any, Dict, Iterable, List
from urllib.parse import urlparse

from qwenpaw_worker.update.constants import REGION_ID_ENV_NAMES

logger = logging.getLogger(__name__)

'''
    (PKG / "utils.py").write_text(utils_header + _slice_lines(lines, 44, 190), encoding="utf-8")

    runtime_config_header = HEADER + '''from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import yaml

from qwenpaw_worker.update.constants import AGENT_IDENTITY_DATA_ENDPOINT_FORMAT
from qwenpaw_worker.update.utils import (
    _credential_provider_env_name,
    _section,
    _stable_json,
    _string,
    _string_fields,
    _string_list,
    _worker_region_id,
)

'''
    (PKG / "runtime_config.py").write_text(
        runtime_config_header + _slice_lines(lines, 192, 441),
        encoding="utf-8",
    )

    agent_package_header = HEADER + '''import base64
import hashlib
import hmac
import json
import logging
import os
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request
import zipfile
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import parse_qs, urlparse

from qwenpaw_worker.update.constants import (
    PACKAGE_PROMPT_FILES,
    PACKAGE_RUNTIME_OWNED_CONFIG_FILES,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _download_path_part, _string, _string_list

logger = logging.getLogger(__name__)

'''
    (PKG / "agent_package.py").write_text(
        agent_package_header + _slice_lines(lines, 444, 1291),
        encoding="utf-8",
    )

    model_sync_header = HEADER + '''import json
import logging
import os
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Dict, Iterable, Optional, Tuple
from urllib.parse import quote, urlencode

from qwenpaw_worker.update.constants import DEFAULT_AGENT_ID
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _duration_ms, _string

logger = logging.getLogger(__name__)

'''
    (PKG / "model_sync.py").write_text(
        model_sync_header + _slice_lines(lines, 1293, 1527),
        encoding="utf-8",
    )

    teams_header = HEADER + '''import logging
from typing import Callable, Optional

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update.constants import (
    TEAMS_CONTEXT_END,
    TEAMS_CONTEXT_START,
    TEAMS_INTERNAL_CONTROL_MARKER,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _section, _stable_json, _string, _string_fields

logger = logging.getLogger(__name__)


class TeamsPromptMixin:
    """TEAMS.md runtime context block writers."""

    config: WorkerConfig
    team_context_renderer: Optional[Callable[[MemberRuntimeConfig], str]]

'''
    teams_body = _slice_lines(lines, 1677, 1769)
    (PKG / "teams_prompt.py").write_text(teams_header + teams_body, encoding="utf-8")

    channel_header = HEADER + '''import json
import logging
import os
from typing import Any, Dict, List, Optional, Tuple

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update.constants import DEFAULT_AGENT_ID
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _bool, _env_bool, _section, _string, _string_list

logger = logging.getLogger(__name__)


class ChannelWriterMixin:
    """Matrix/DingTalk channel and access-control writers."""

    config: WorkerConfig

'''
    channel_body = _slice_lines(lines, 1868, 2266)
    (PKG / "channel_writers.py").write_text(channel_header + channel_body, encoding="utf-8")

    updater_header = HEADER + '''import asyncio
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

'''
    updater_body = _slice_ranges(
        lines,
        [(1529, 1676), (1771, 1867), (2268, 2291)],
    )
    updater_body = updater_body.replace(
        "class RuntimeUpdater:",
        "class RuntimeUpdater(TeamsPromptMixin, ChannelWriterMixin):",
        1,
    )
    (PKG / "runtime_updater.py").write_text(updater_header + updater_body, encoding="utf-8")
    (PKG / "__init__.py").write_text(INIT, encoding="utf-8")

    UPDATE_PY.unlink()
    print(f"Split {UPDATE_PY.name} -> {PKG}/")


if __name__ == "__main__":
    main()
