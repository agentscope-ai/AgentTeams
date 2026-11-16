"""Validate local skill copies and report failures without changing readiness."""
from __future__ import annotations

import argparse
import json
import logging
import os
from pathlib import Path
import re
import shutil
import urllib.request

log = logging.getLogger(__name__)
_last_warnings: tuple[str, ...] | None = None


def check_skill(directory: Path) -> None:
    import yaml
    for path in directory.rglob("*"):
        if path.is_symlink() and not path.resolve().is_relative_to(directory.resolve()):
            raise ValueError("skill file escapes skill directory")
    content = (directory / "SKILL.md").read_text(encoding="utf-8")
    if not content.strip():
        raise ValueError("empty SKILL.md")
    if content.startswith(("---\n", "---\r\n")):
        match = re.match(r"\A---\s*\n(.*?)\n---(?:\s*\n|\s*$)", content, re.DOTALL)
        if match is None or not isinstance(yaml.safe_load(match.group(1)), dict):
            raise ValueError("invalid SKILL.md frontmatter")


def _request(method: str, suffix: str, payload=None):
    base = os.environ.get("AGENTTEAMS_CONTROLLER_URL", "").rstrip("/")
    name = os.environ.get("AGENTTEAMS_WORKER_CR_NAME") or os.environ.get("AGENTTEAMS_WORKER_NAME", "")
    if not base or not name or os.environ.get("AGENTTEAMS_AGENT_ROLE") == "manager":
        return None
    token = os.environ.get("AGENTTEAMS_AUTH_TOKEN", "")
    if not token:
        path = Path(os.environ.get("AGENTTEAMS_AUTH_TOKEN_FILE", "/var/run/agentteams/cli-token"))
        if path.is_file():
            token = path.read_text().strip()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(f"{base}/api/v1/workers/{name}{suffix}", method=method, headers=headers,
                                     data=None if payload is None else json.dumps(payload).encode())
    with urllib.request.urlopen(request, timeout=3) as response:
        content = response.read()
        return json.loads(content) if content else None


def report_warnings(warnings: list[str]) -> None:
    global _last_warnings
    current = tuple(warnings)
    if current != _last_warnings:
        for warning in warnings:
            log.warning("Skill assignment warning: %s", warning)
        if _last_warnings and not warnings:
            log.info("Skill assignment recovered")
    try:
        _request("POST", "/skill-warnings", {"skillWarnings": warnings})
    except Exception as exc:
        # Old/offline Controllers must not prevent the Agent from starting.
        log.debug("Skill warning report unavailable: %s", type(exc).__name__)
    _last_warnings = current


def inspect_skills(root: Path, assigned: list[str] | None = None) -> list[str]:
    """Exclude malformed copies from SDK discovery; keep remote copies intact."""
    root.mkdir(parents=True, exist_ok=True)
    names = set(assigned or []) | {p.name for p in root.iterdir() if p.is_dir() and not p.name.startswith(".")}
    if assigned is None:
        try:
            worker = _request("GET", "")
            names.update((worker or {}).get("skills") or [])
        except Exception:
            pass  # Local SDK validation still works without the Controller.
    warnings = []
    quarantine = root.parent / ".agentteams-invalid-skills"
    for name in sorted(names):
        if not name or name in {".", ".."} or "/" in name or "\\" in name:
            warnings.append(f"Skill {name}: load failed (ValueError)")
            continue
        path = root / name
        try:
            if not path.resolve().is_relative_to(root.resolve()):
                raise ValueError("skill path escapes workspace")
            check_skill(path)
        except Exception as exc:
            warnings.append(f"Skill {name}: load failed ({type(exc).__name__})")
            # Never follow or move an external symlink, and never remove
            # user files outside this runtime's skill directory.
            if path.is_symlink():
                path.unlink()
            elif path.is_dir() and path.parent == root:
                try:
                    if quarantine.is_symlink():
                        raise OSError("invalid quarantine directory")
                    quarantine.mkdir(parents=True, exist_ok=True)
                    previous = quarantine / name
                    if previous.exists():
                        shutil.rmtree(previous)
                    shutil.move(str(path), previous)
                except OSError:
                    log.warning("Could not quarantine invalid skill %s", name)
    report_warnings(warnings)
    return warnings


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("skills_dir", type=Path)
    args = parser.parse_args()
    inspect_skills(args.skills_dir)
