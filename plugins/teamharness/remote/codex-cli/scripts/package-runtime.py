#!/usr/bin/env python3
"""Build a deterministic Codex CLI local-runtime tarball."""

from __future__ import annotations

import argparse
import gzip
from pathlib import Path
import tarfile


VERSION = "0.1.0"
PACKAGE_NAME = f"agentteams-codex-cli-local-runtime-{VERSION}"


def files_to_package(teamharness_root: Path, runtime_root: Path) -> list[tuple[Path, Path]]:
    entries: list[tuple[Path, Path]] = []
    for path in sorted(runtime_root.rglob("*")):
        if not path.is_file() or "__pycache__" in path.parts or path.suffix == ".pyc":
            continue
        entries.append((path, Path("runtime") / path.relative_to(runtime_root)))
    for relative in ("plugin.yaml", "prompts", "skills", "mcp", "adapters/codex-cli"):
        source = teamharness_root / relative
        paths = [source] if source.is_file() else sorted(source.rglob("*"))
        for path in paths:
            if path.is_file() and "__pycache__" not in path.parts and path.suffix != ".pyc":
                entries.append((path, Path("teamharness") / path.relative_to(teamharness_root)))
    return sorted(entries, key=lambda pair: pair[1].as_posix())


def build(output_dir: Path) -> Path:
    runtime_root = Path(__file__).resolve().parents[1]
    teamharness_root = runtime_root.parents[1]
    output_dir.mkdir(parents=True, exist_ok=True)
    output = output_dir / f"{PACKAGE_NAME}.tar.gz"
    with output.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for source, relative in files_to_package(teamharness_root, runtime_root):
                    info = archive.gettarinfo(str(source), arcname=(Path(PACKAGE_NAME) / relative).as_posix())
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    info.mtime = 0
                    info.mode = 0o755 if source.suffix in {".sh", ".py"} else 0o644
                    with source.open("rb") as handle:
                        archive.addfile(info, handle)
    return output


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=Path("dist/remote/codex-cli"))
    args = parser.parse_args()
    print(build(args.output.resolve()))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
