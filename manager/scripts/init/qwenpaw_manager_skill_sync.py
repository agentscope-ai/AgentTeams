#!/usr/bin/env python3
"""Mirror Manager workspace skills into QwenPaw and refresh them at runtime."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sys
import tempfile
import time
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


SKILL_NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


class ManagerSkillSync:
    def __init__(
        self,
        *,
        source_dir: Path,
        workspace_dir: Path,
        api_url: str,
        state_file: Path,
        request_timeout: float = 5.0,
    ) -> None:
        self.source_dir = source_dir
        self.workspace_dir = workspace_dir
        self.skills_dir = workspace_dir / "skills"
        self.active_skills_dir = workspace_dir / "active_skills"
        self.api_url = api_url.rstrip("/")
        self.state_file = state_file
        self.request_timeout = request_timeout

    def _load_state(self) -> dict[str, str]:
        try:
            payload = json.loads(self.state_file.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError, OSError):
            return {}
        skills = payload.get("skills")
        if not isinstance(skills, dict):
            return {}
        return {
            name: digest
            for name, digest in skills.items()
            if isinstance(name, str)
            and SKILL_NAME_PATTERN.fullmatch(name)
            and isinstance(digest, str)
        }

    def _save_state(self, skills: dict[str, str]) -> None:
        self.state_file.parent.mkdir(parents=True, exist_ok=True)
        temp_file = self.state_file.with_suffix(".tmp")
        temp_file.write_text(
            json.dumps({"skills": skills}, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        os.replace(temp_file, self.state_file)

    def _skill_digest(self, skill_dir: Path) -> str:
        digest = hashlib.sha256()
        for path in sorted(item for item in skill_dir.rglob("*") if item.is_file()):
            relative = path.relative_to(skill_dir).as_posix().encode()
            digest.update(len(relative).to_bytes(8, "big"))
            digest.update(relative)
            with path.open("rb") as handle:
                for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                    digest.update(chunk)
        return digest.hexdigest()

    def _scan_source(self) -> dict[str, tuple[Path, str]]:
        result: dict[str, tuple[Path, str]] = {}
        for skill_dir in sorted(self.source_dir.iterdir()):
            if not skill_dir.is_dir():
                continue
            name = skill_dir.name
            if not SKILL_NAME_PATTERN.fullmatch(name):
                continue
            if not (skill_dir / "SKILL.md").is_file():
                continue
            result[name] = (skill_dir, self._skill_digest(skill_dir))
        return result

    def _replace_skill_dir(self, source: Path, destination: Path) -> None:
        destination.parent.mkdir(parents=True, exist_ok=True)
        stage_root = Path(
            tempfile.mkdtemp(prefix=f".{destination.name}.agentteams-", dir=destination.parent)
        )
        staged = stage_root / destination.name
        backup = stage_root / "previous"
        try:
            shutil.copytree(source, staged)
            if destination.exists():
                os.replace(destination, backup)
            try:
                os.replace(staged, destination)
            except Exception:
                if backup.exists() and not destination.exists():
                    os.replace(backup, destination)
                raise
        finally:
            shutil.rmtree(backup, ignore_errors=True)
            shutil.rmtree(stage_root, ignore_errors=True)

    def _post_json(self, path: str, payload: object) -> Any:
        body = json.dumps(payload).encode()
        request = Request(
            f"{self.api_url}{path}",
            data=body,
            headers={"content-type": "application/json"},
            method="POST",
        )
        with urlopen(request, timeout=self.request_timeout) as response:
            response_body = response.read()
        if not response_body:
            return None
        return json.loads(response_body)

    def _refresh_and_enable(self, changed: list[str]) -> None:
        self._post_json("/api/skills/refresh", {})
        if not changed:
            return
        result = self._post_json("/api/skills/batch-enable", changed)
        if isinstance(result, dict) and isinstance(result.get("results"), dict):
            results = result["results"]
            failed = [
                name
                for name in changed
                if not isinstance(results.get(name), dict)
                or not results[name].get("success")
            ]
        elif isinstance(result, list):
            # Keep compatibility with earlier CoPaw API responses.
            failed = [
                str(item.get("name") or "unknown")
                for item in result
                if not isinstance(item, dict) or not item.get("success")
            ]
        else:
            raise RuntimeError("QwenPaw skill enable returned an invalid response")
        if failed:
            raise RuntimeError(f"QwenPaw skill enable failed: {', '.join(failed)}")

    def sync_once(self) -> bool:
        if not self.source_dir.is_dir():
            print(
                f"[manager-skill-sync] source directory is missing: {self.source_dir}",
                file=sys.stderr,
            )
            return False

        try:
            previous = self._load_state()
            current = self._scan_source()
            changed = sorted(
                name
                for name, (_, digest) in current.items()
                if previous.get(name) != digest
            )
            removed = sorted(set(previous) - set(current))
            if not changed and not removed:
                return True

            for name in changed:
                self._replace_skill_dir(current[name][0], self.skills_dir / name)
            for name in removed:
                shutil.rmtree(self.skills_dir / name, ignore_errors=True)
                shutil.rmtree(self.active_skills_dir / name, ignore_errors=True)
            self._refresh_and_enable(changed)
            self._save_state({name: digest for name, (_, digest) in current.items()})
        except (HTTPError, URLError, OSError, RuntimeError, ValueError) as exc:
            print(f"[manager-skill-sync] sync failed: {exc}", file=sys.stderr)
            return False

        if changed:
            print(
                f"[manager-skill-sync] refreshed and enabled: {', '.join(changed)}",
                file=sys.stderr,
            )
        if removed:
            print(f"[manager-skill-sync] removed: {', '.join(removed)}", file=sys.stderr)
        return True


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-dir", type=Path, required=True)
    parser.add_argument("--workspace-dir", type=Path, required=True)
    parser.add_argument("--api-url", default="http://127.0.0.1:18799")
    parser.add_argument("--state-file", type=Path, required=True)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--once", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    sync = ManagerSkillSync(
        source_dir=args.source_dir,
        workspace_dir=args.workspace_dir,
        api_url=args.api_url,
        state_file=args.state_file,
    )
    if args.once:
        return 0 if sync.sync_once() else 1
    while True:
        sync.sync_once()
        time.sleep(max(args.interval, 0.1))


if __name__ == "__main__":
    raise SystemExit(main())
