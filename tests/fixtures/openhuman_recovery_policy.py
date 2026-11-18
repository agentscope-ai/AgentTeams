#!/usr/bin/env python3
"""Explicit command policy for the disposable recovery fixture image only.

The real native model invokes a bounded acceptance step command. Production
entrypoints and their command allowlists are unchanged by this test wrapper.
"""
import json
import os
from pathlib import Path
import re
import sys
import tomllib
import urllib.parse


def configure(path, prefix, controller):
    if (not re.fullmatch(r"recovery-1631-live-\d{8}-v\d+", prefix)
            or urllib.parse.urlparse(controller).hostname != prefix + "-controller"):
        raise ValueError("Only the explicitly named disposable recovery fixture is allowed")
    text = path.read_text()
    native = tomllib.loads(text)
    commands = native["autonomy"]["allowed_commands"]
    if not isinstance(commands, list) or not all(isinstance(item, str) for item in commands):
        raise ValueError("Native command policy must be an explicit list")
    commands = list(dict.fromkeys([*commands, "agentteams-recovery-step"]))
    section = re.compile(r"(?ms)^\[autonomy\]\s*\n(.*?)(?=^\[|\Z)")
    matches = list(section.finditer(text))
    if len(matches) != 1:
        raise ValueError("Expected one native autonomy section")
    body = matches[0][1]
    # The installed config CLI serializes long command arrays across lines.
    body, count = re.subn(r"(?m)^allowed_commands[ \t]*=[ \t]*\[[^\]]*\]",
                          "allowed_commands = " + json.dumps(commands), body)
    if count != 1:
        raise ValueError("Expected one native command array")
    replacement = text[:matches[0].start()] + "[autonomy]\n" + body + text[matches[0].end():]
    result = tomllib.loads(replacement)
    assert result["autonomy"]["allowed_commands"] == commands
    path.write_text(replacement)


if __name__ == "__main__":
    controller = os.getenv("AGENTTEAMS_CONTROLLER_URL", "")
    hostname = urllib.parse.urlparse(controller).hostname or ""
    prefix = os.getenv("RECOVERY_E2E_PREFIX") or hostname.removesuffix("-controller")
    if sys.argv[1:] == ["serve"] and re.fullmatch(r"recovery-1631-live-\d{8}-v\d+", prefix):
        config = Path(os.environ["OPENHUMAN_CONFIG"])
        configure(config, prefix, controller)
        prompt = config.parent / "workspace" / "SOUL.md"
        fixture_instruction = (
            "\n## AgentTeams recovery acceptance fixture\n\n"
            "Your active Harness in this fixture is OpenHuman. For the synthetic recovery Task, "
            "use your Shell tool to run exactly `agentteams-recovery-step <task_id> recovery-team`, "
            "substituting the Task ID. The command is installed and allowed. "
            "Call it directly without probing its path or downloading a script. "
            "Once all three steps are committed, submit the existing relative artifact paths "
            "through the TeamHarness taskflow tool. The step command already persists the artifacts.\n"
        )
        existing = prompt.read_text()
        if fixture_instruction not in existing:
            prompt.write_text(existing + fixture_instruction)
        print("Recovery fixture policy: bounded step command explicitly allowed before native startup", flush=True)
    os.execv("/usr/local/bin/openhuman-core.native", ["openhuman-core", *sys.argv[1:]])
