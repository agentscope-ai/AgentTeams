#!/usr/bin/env python3
"""Explicit command policy for the disposable recovery fixture image only.

The real native model invokes the deterministic Python step tool. Production
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
    commands = list(dict.fromkeys([*commands, "python3"]))
    section = re.compile(r"(?ms)^\[autonomy\]\s*\n(.*?)(?=^\[|\Z)")
    matches = list(section.finditer(text))
    if len(matches) != 1:
        raise ValueError("Expected one native autonomy section")
    body = matches[0][1]
    body = re.sub(r"(?m)^(?:allowed_commands|level|require_approval_for_medium_risk)\s*=.*\n?", "", body)
    body += ('allowed_commands = ' + json.dumps(commands) + '\n'
             'level = "full"\nrequire_approval_for_medium_risk = false\n\n')
    replacement = text[:matches[0].start()] + "[autonomy]\n" + body + text[matches[0].end():]
    result = tomllib.loads(replacement)
    assert result["autonomy"]["allowed_commands"] == commands
    path.write_text(replacement)


if __name__ == "__main__":
    if sys.argv[1:] == ["serve"]:
        configure(Path(os.environ["OPENHUMAN_CONFIG"]), os.environ.get("RECOVERY_E2E_PREFIX", ""),
                  os.environ.get("AGENTTEAMS_CONTROLLER_URL", ""))
        print("Recovery fixture policy: Python step tool explicitly allowed before native startup", flush=True)
    os.execv("/usr/local/bin/openhuman-core.native", ["openhuman-core", *sys.argv[1:]])
