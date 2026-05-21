#!/usr/bin/env python3
"""Patch CoPaw exception classification for streaming disconnects.

HiClaw currently deploys CoPaw from PyPI. Keep this image-local compatibility
patch fail-fast so an upstream exceptions.py shape change cannot silently build
an image that still reports stream-idle disconnects as UnknownAgentException.
"""

from __future__ import annotations

import pathlib
import sys


def patch(site_packages: pathlib.Path) -> None:
    target = site_packages / "copaw" / "exceptions.py"
    if not target.exists():
        raise FileNotFoundError(f"CoPaw exceptions module not found: {target}")

    text = target.read_text(encoding="utf-8")

    model_marker = '"llm",\n    ]'
    model_replacement = '''"llm",
        "remoteprotocolerror",
    ]'''
    if "remoteprotocolerror" not in text:
        if model_marker not in text:
            raise RuntimeError("could not find model-related error marker in copaw/exceptions.py")
        text = text.replace(model_marker, model_replacement, 1)

    timeout_marker = '"deadline exceeded",'
    timeout_replacement = '''"deadline exceeded",
            "peer closed connection",
            "incomplete chunked read",
            "remoteprotocolerror",
            "stream_idle_timeout",'''
    if "incomplete chunked read" not in text:
        if timeout_marker not in text:
            raise RuntimeError("could not find timeout keyword marker in copaw/exceptions.py")
        text = text.replace(timeout_marker, timeout_replacement, 1)

    timeout_call = "return ModelTimeoutException(model, timeout=60, details=details)"
    timeout_call_replacement = "return ModelTimeoutException(model, timeout=900, details=details)"
    if timeout_call_replacement not in text:
        if timeout_call not in text:
            raise RuntimeError("could not find ModelTimeoutException timeout marker in copaw/exceptions.py")
        text = text.replace(timeout_call, timeout_call_replacement, 1)

    required = [
        "remoteprotocolerror",
        "peer closed connection",
        "incomplete chunked read",
        "stream_idle_timeout",
        timeout_call_replacement,
    ]
    missing = [value for value in required if value not in text]
    if missing:
        raise RuntimeError(f"CoPaw stream error patch incomplete; missing: {', '.join(missing)}")

    target.write_text(text, encoding="utf-8")


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: patch_copaw_stream_errors.py <site-packages>", file=sys.stderr)
        return 2
    patch(pathlib.Path(sys.argv[1]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
