#!/usr/bin/env python3
"""Real Draft 2020-12 schema validation for AgentTeams plugin manifests.

validate-plugin.rb used to only parse the schema JSON file, so its success
proved nothing about schema compatibility. This script runs the actual
validation: it picks the schema with the shared form rule below, checks the
manifest with ``jsonschema`` (Draft 2020-12), and prints every violation
verbatim before exiting non-zero.

Form rule (single implementation, shared by the Ruby validator — which
calls this CLI — and the pytest regressions): a manifest that declares
BOTH ``adapters`` and ``requires`` is bundle form and is checked against
``bundle.schema.json``; anything else is checked against
``plugin.schema.json``.

Usage:
    python3 plugins/scripts/validate_manifest.py <manifest.yaml> <schemas_dir>
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import yaml
from jsonschema import Draft202012Validator
from jsonschema.exceptions import SchemaError

BUNDLE_SCHEMA_FILE = "bundle.schema.json"
PLUGIN_SCHEMA_FILE = "plugin.schema.json"


def is_bundle_form(manifest: object) -> bool:
    """True iff the manifest declares both ``adapters`` and ``requires``."""
    return isinstance(manifest, dict) and "adapters" in manifest and "requires" in manifest


def schema_file_for(manifest: object) -> str:
    """Pick the schema file for a parsed manifest (the shared form rule)."""
    return BUNDLE_SCHEMA_FILE if is_bundle_form(manifest) else PLUGIN_SCHEMA_FILE


def format_error(error) -> str:
    path = ".".join(str(part) for part in error.absolute_path)
    message = error.message
    # A oneOf failure alone ("is not valid under any of the given schemas")
    # hides which requirement each branch wanted; append each branch's own
    # error so the message names the missing keys (e.g. prompts).
    branches = getattr(error, "context", None) or []
    if branches:
        details = []
        for branch in branches:
            sub_path = ".".join(str(part) for part in branch.absolute_path)
            details.append(f"{sub_path}: {branch.message}" if sub_path else branch.message)
        message = f"{message} ({'; '.join(details)})"
    return f"{path}: {message}" if path else message


def validate_manifest(manifest: dict, schemas_dir: Path) -> tuple[str, list[str]]:
    """Validate a parsed manifest; return ``(schema file name, error strings)``."""
    schema_file = schema_file_for(manifest)
    schema_path = schemas_dir / schema_file
    if not schema_path.is_file():
        return schema_file, [f"missing schema file: {schema_path}"]
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    try:
        Draft202012Validator.check_schema(schema)
    except SchemaError as exc:
        return schema_file, [f"invalid {schema_file}: {exc.message}"]
    errors = [format_error(error) for error in Draft202012Validator(schema).iter_errors(manifest)]
    return schema_file, errors


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", help="path to the plugin manifest (YAML)")
    parser.add_argument(
        "schemas_dir",
        help="directory containing plugin.schema.json and bundle.schema.json",
    )
    args = parser.parse_args(argv)

    manifest_path = Path(args.manifest)
    schemas_dir = Path(args.schemas_dir)
    try:
        manifest = yaml.safe_load(manifest_path.read_text(encoding="utf-8"))
    except (OSError, yaml.YAMLError) as exc:
        print(f"ERROR: cannot read manifest {manifest_path}: {exc}")
        return 2
    if not isinstance(manifest, dict):
        print(f"ERROR: manifest {manifest_path} must parse to a mapping")
        return 2

    schema_file, errors = validate_manifest(manifest, schemas_dir)
    if errors:
        print(f"ERROR: schema validation failed for {manifest_path} against {schemas_dir / schema_file}:")
        for error in errors:
            print(f"  {error}")
        return 1

    metadata = manifest.get("metadata") or {}
    name = metadata.get("name") or manifest_path.parent.name
    version = metadata.get("version") or "?"
    print(f"ok: {name} {version} passes {schema_file}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
