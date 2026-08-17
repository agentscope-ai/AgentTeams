#!/usr/bin/env python3
"""Safely extract and validate one Worker Skill from a ZIP archive."""

from __future__ import annotations

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import stat
import sys
import zipfile


MAX_ARCHIVE_ENTRIES = 1000
MAX_UNCOMPRESSED_BYTES = 100 * 1024 * 1024
SKILL_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


class SkillArchiveError(ValueError):
    """An expected validation failure for a Worker Skill archive."""


def _clean_entry_name(raw_name: str) -> PurePosixPath:
    if (
        not raw_name
        or "\x00" in raw_name
        or "\\" in raw_name
        or raw_name.startswith("/")
        or re.match(r"^[A-Za-z]:", raw_name)
    ):
        raise SkillArchiveError(f"unsafe ZIP entry path: {raw_name!r}")

    raw_parts = raw_name.rstrip("/").split("/")
    if any(part in ("", ".", "..") for part in raw_parts):
        raise SkillArchiveError(f"unsafe ZIP entry path: {raw_name!r}")

    path = PurePosixPath(raw_name)
    if path.is_absolute() or any(part in ("", ".", "..") for part in path.parts):
        raise SkillArchiveError(f"unsafe ZIP entry path: {raw_name!r}")
    return path


def _entry_mode(info: zipfile.ZipInfo) -> int:
    return (info.external_attr >> 16) & 0xFFFF


def _validate_entries(archive: zipfile.ZipFile) -> list[tuple[zipfile.ZipInfo, PurePosixPath]]:
    if len(archive.infolist()) > MAX_ARCHIVE_ENTRIES:
        raise SkillArchiveError(
            f"archive contains more than {MAX_ARCHIVE_ENTRIES} entries",
        )

    entries: list[tuple[zipfile.ZipInfo, PurePosixPath]] = []
    seen: set[str] = set()
    total_size = 0

    for info in archive.infolist():
        path = _clean_entry_name(info.filename)
        normalized = path.as_posix().rstrip("/")
        if normalized in seen:
            raise SkillArchiveError(f"duplicate ZIP entry: {info.filename!r}")
        seen.add(normalized)

        if info.flag_bits & 0x1:
            raise SkillArchiveError(f"encrypted ZIP entry is not supported: {info.filename!r}")

        mode = _entry_mode(info)
        file_type = stat.S_IFMT(mode)
        if stat.S_ISLNK(mode):
            raise SkillArchiveError(f"symlinks are not allowed: {info.filename!r}")
        if file_type not in (0, stat.S_IFREG, stat.S_IFDIR):
            raise SkillArchiveError(f"special files are not allowed: {info.filename!r}")

        if not info.is_dir():
            total_size += info.file_size
            if total_size > MAX_UNCOMPRESSED_BYTES:
                raise SkillArchiveError(
                    "archive expands beyond the 100 MiB safety limit",
                )
        entries.append((info, path))

    return entries


def _extract_entries(
    archive: zipfile.ZipFile,
    entries: list[tuple[zipfile.ZipInfo, PurePosixPath]],
    output_dir: Path,
) -> None:
    output_dir.mkdir(parents=True, exist_ok=True)
    output_root = output_dir.resolve()

    for info, relative in entries:
        destination = (output_dir / Path(*relative.parts)).resolve()
        try:
            destination.relative_to(output_root)
        except ValueError as exc:
            raise SkillArchiveError(f"ZIP entry escapes destination: {info.filename!r}") from exc

        if info.is_dir():
            destination.mkdir(parents=True, exist_ok=True)
            continue

        destination.parent.mkdir(parents=True, exist_ok=True)
        written = 0
        with archive.open(info) as source, destination.open("wb") as target:
            while chunk := source.read(1024 * 1024):
                written += len(chunk)
                if written > info.file_size:
                    raise SkillArchiveError(f"ZIP entry exceeds declared size: {info.filename!r}")
                target.write(chunk)
        if written != info.file_size:
            raise SkillArchiveError(f"ZIP entry size mismatch: {info.filename!r}")

        mode = _entry_mode(info) & 0o777
        destination.chmod(mode or 0o644)


def _frontmatter_fields(skill_md: Path) -> dict[str, str]:
    try:
        lines = skill_md.read_text(encoding="utf-8").splitlines()
    except UnicodeDecodeError as exc:
        raise SkillArchiveError("SKILL.md must be UTF-8 text") from exc

    if not lines or lines[0].lstrip("\ufeff") != "---":
        raise SkillArchiveError("SKILL.md must start with YAML frontmatter")

    try:
        end = lines.index("---", 1)
    except ValueError as exc:
        raise SkillArchiveError("SKILL.md frontmatter is not closed") from exc

    fields: dict[str, str] = {}
    index = 1
    while index < end:
        line = lines[index]
        match = re.match(r"^([A-Za-z_][A-Za-z0-9_-]*):\s*(.*)$", line)
        if not match:
            index += 1
            continue
        key, value = match.groups()
        value = value.strip()
        if value in ("|", "|-", "|+", ">", ">-", ">+"):
            block_lines: list[str] = []
            index += 1
            while index < end and (not lines[index] or lines[index][0].isspace()):
                block_lines.append(lines[index].strip())
                index += 1
            value = " ".join(part for part in block_lines if part).strip()
            fields[key] = value
            continue
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
            value = value[1:-1].strip()
        elif value.startswith("#"):
            value = ""
        else:
            value = re.sub(r"\s+#.*$", "", value).strip()
        fields[key] = value
        index += 1
    return fields


def _find_and_validate_skill(output_dir: Path) -> tuple[Path, str, bool]:
    candidates = sorted(output_dir.rglob("SKILL.md"))
    if len(candidates) != 1:
        raise SkillArchiveError(
            f"archive must contain exactly one Skill root; found {len(candidates)} SKILL.md files",
        )

    skill_md = candidates[0]
    skill_root = skill_md.parent.resolve()
    output_root = output_dir.resolve()
    fields = _frontmatter_fields(skill_md)
    skill_name = fields.get("name", "")
    description = fields.get("description", "")

    if not skill_name:
        raise SkillArchiveError("SKILL.md frontmatter field 'name' is required")
    if not SKILL_NAME_RE.fullmatch(skill_name):
        raise SkillArchiveError(
            "Skill name must match ^[A-Za-z0-9][A-Za-z0-9._-]*$",
        )
    if not description:
        raise SkillArchiveError("SKILL.md frontmatter field 'description' is required")
    if skill_root != output_root and skill_root.name != skill_name:
        raise SkillArchiveError(
            f"Skill directory {skill_root.name!r} does not match frontmatter name {skill_name!r}",
        )

    for path in output_dir.rglob("*"):
        if path.is_dir() or path == skill_md:
            continue
        try:
            path.resolve().relative_to(skill_root)
        except ValueError as exc:
            raise SkillArchiveError(
                f"archive contains a file outside the Skill root: {path.relative_to(output_dir)}",
            ) from exc

    return skill_root, skill_name, bool(fields.get("assign_when", ""))


def extract_skill(archive_path: Path, output_dir: Path) -> dict[str, object]:
    if not archive_path.is_file():
        raise SkillArchiveError(f"archive file does not exist: {archive_path}")
    if output_dir.exists() and any(output_dir.iterdir()):
        raise SkillArchiveError(f"output directory must be empty: {output_dir}")

    try:
        with zipfile.ZipFile(archive_path) as archive:
            entries = _validate_entries(archive)
            _extract_entries(archive, entries, output_dir)
    except zipfile.BadZipFile as exc:
        raise SkillArchiveError(f"invalid ZIP archive: {archive_path}") from exc

    skill_root, skill_name, has_assign_when = _find_and_validate_skill(output_dir)
    return {
        "name": skill_name,
        "skillRoot": str(skill_root),
        "hasAssignWhen": has_assign_when,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()

    try:
        result = extract_skill(args.archive, args.output_dir)
    except (OSError, SkillArchiveError) as exc:
        print(f"Worker Skill archive validation failed: {exc}", file=sys.stderr)
        return 1

    print(json.dumps(result, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
