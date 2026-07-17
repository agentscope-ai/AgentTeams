#!/usr/bin/env python3
"""Install bundled QwenPaw plugins into an image-local working directory."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import shutil
import sys

from qwenpaw_worker.plugin_install import install_qwenpaw_plugin_package, write_builtin_markers


def _install(zip_path: Path, working_dir: Path) -> None:
    qwenpaw_bin = shutil.which("qwenpaw") or str(Path(sys.executable).with_name("qwenpaw"))
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    install_qwenpaw_plugin_package(
        qwenpaw_bin,
        zip_path,
        temp_prefix="qwenpaw-builtin-plugin-",
        env=env,
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--working-dir", required=True)
    parser.add_argument("packages", nargs="+")
    args = parser.parse_args()

    working_dir = Path(args.working_dir)
    working_dir.mkdir(parents=True, exist_ok=True)
    for package in args.packages:
        _install(Path(package), working_dir)
    write_builtin_markers(working_dir)


if __name__ == "__main__":
    main()
