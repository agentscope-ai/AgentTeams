#!/usr/bin/env python3
"""agentteams — TeamHarness plugin CLI fallback."""

from __future__ import annotations

import argparse
from pathlib import Path

from agentteams_cli import plugin_manager
from agentteams_cli.config_store import ConfigStore


def cmd_plugin_install(args: argparse.Namespace) -> int:
    store = ConfigStore()
    ok = plugin_manager.install(
        store,
        args.name,
        package=Path(args.package) if args.package else None,
        source=Path(args.source) if args.source else None,
    )
    return 0 if ok else 1


def cmd_plugin_list(_args: argparse.Namespace) -> int:
    plugins = plugin_manager.list_plugins(ConfigStore())
    if not plugins:
        print("No plugins installed.")
        return 0
    print(f"{'NAME':<20} {'VERSION':<10} {'INSTALLED'}")
    for plugin in plugins:
        print(f"{plugin.get('name', '?'):<20} {plugin.get('version', '?'):<10} {plugin.get('installed_at', '?')}")
    return 0


def cmd_plugin_update(args: argparse.Namespace) -> int:
    store = ConfigStore()
    ok = plugin_manager.update(
        store,
        args.name,
        package=Path(args.package) if args.package else None,
        source=Path(args.source) if args.source else None,
    )
    return 0 if ok else 1


def cmd_plugin_uninstall(args: argparse.Namespace) -> int:
    ok = plugin_manager.uninstall(ConfigStore(), args.name)
    return 0 if ok else 1


def main() -> int:
    parser = argparse.ArgumentParser(
        prog="agentteams",
        description="AgentTeams plugin manager and TeamHarness fallback installer",
    )
    sub = parser.add_subparsers(dest="command")

    plugin_parser = sub.add_parser("plugin", help="Manage TeamHarness-compatible plugins")
    plugin_sub = plugin_parser.add_subparsers(dest="plugin_action")

    p_install = plugin_sub.add_parser("install", help="Install a plugin package")
    p_install.add_argument("name", help="Plugin name, for example teamharness")
    p_install.add_argument("--package", help="LoongSuite-compatible plugin tarball or directory")
    p_install.add_argument("--source", help="Raw plugin source directory")

    plugin_sub.add_parser("list", help="List installed plugins")

    p_update = plugin_sub.add_parser("update", help="Update an installed plugin")
    p_update.add_argument("name", help="Plugin name")
    p_update.add_argument("--package", help="LoongSuite-compatible plugin tarball or directory")
    p_update.add_argument("--source", help="Raw plugin source directory")

    p_uninstall = plugin_sub.add_parser("uninstall", help="Uninstall a plugin")
    p_uninstall.add_argument("name", help="Plugin name")

    args = parser.parse_args()
    if args.command == "plugin":
        handlers = {
            "install": cmd_plugin_install,
            "list": cmd_plugin_list,
            "update": cmd_plugin_update,
            "uninstall": cmd_plugin_uninstall,
        }
        fn = handlers.get(getattr(args, "plugin_action", None))
        if fn:
            return fn(args)
        plugin_parser.print_help()
        return 1

    parser.print_help()
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
