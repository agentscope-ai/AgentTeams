# -*- coding: utf-8 -*-
"""Regression for the verify-bridge.sh gate (real exit codes, honest pass).

The gate used to capture only the *last line* of pytest output piped
through ``tail`` — the pipeline's exit status is tail's (always 0), and
the evidence JSON's ``pass`` field was computed from the live checks
alone. A collection error (pytest exit 2) therefore produced
``pass: true``. These tests run the *real* script against three
controlled one-file suites (via the ``TESTS_DIR`` override) with
``LIVE_CHECKS=skip`` and assert that the process exit code and the
evidence JSON's ``pass`` field agree with the suite's outcome:

  1. healthy suite      -> exit 0,  pass: true
  2. collection error   -> exit != 0, pass: false (pytest exit code 2)
  3. assertion failure  -> exit != 0, pass: false (pytest exit code 1)
"""
from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
import os
import subprocess
import sys
from pathlib import Path

SCRIPT = (
    Path(__file__).resolve().parent.parent / "scripts" / "verify-bridge.sh"
)

SUITE_OK = "def test_ok():\n    assert True\n"
SUITE_COLLECTION_ERROR = "def test_broken(:\n    pass\n"  # SyntaxError at import
SUITE_ASSERTION_FAILURE = "def test_fail():\n    assert False\n"


def _run_gate(
    tmp_path: Path, suite_source: str, name: str, app_url: str | None = None
) -> tuple[int, dict]:
    """Run the real gate script against a controlled suite; return
    (process exit code, parsed evidence JSON)."""
    suite = tmp_path / f"suite-{name}"
    suite.mkdir()
    (suite / f"test_{name}.py").write_text(suite_source, encoding="utf-8")
    out_json = tmp_path / f"evidence-{name}.json"
    env = dict(os.environ)
    env["PY"] = sys.executable
    env["TESTS_DIR"] = str(suite)
    env["LIVE_CHECKS"] = "run" if app_url else "skip"
    if app_url:
        env["QWENPAW_APP_URL"] = app_url
    env.pop("QWEN_CODE_BRIDGE_7565_SRC", None)
    proc = subprocess.run(
        ["sh", str(SCRIPT), str(out_json)],
        env=env,
        capture_output=True,
        text=True,
        timeout=300,
    )
    evidence = json.loads(out_json.read_text(encoding="utf-8"))
    return proc.returncode, evidence


def test_gate_healthy_suite_passes(tmp_path):
    """1. healthy suite: exit 0, evidence pass=true, suite object
    carries the real (zero) exit code, live section marked skipped."""
    rc, evidence = _run_gate(tmp_path, SUITE_OK, "ok")
    assert rc == 0
    assert evidence["pass"] is True
    suite = evidence["tests_installed"]
    assert suite["exit_code"] == 0
    assert suite["pass"] is True
    assert "passed" in suite["summary"]
    assert evidence["tests_7565"] == {
        "skipped": True,
        "reason": "skipped (QWEN_CODE_BRIDGE_7565_SRC unset)",
    }
    assert evidence["live"] == {"skipped": True}


def test_gate_collection_error_fails(tmp_path):
    """2. collection error (pytest exit 2): previously masked as
    pass=true; now exit != 0 and the evidence says so, with the real
    pytest exit code preserved."""
    rc, evidence = _run_gate(tmp_path, SUITE_COLLECTION_ERROR, "collecterr")
    assert rc != 0
    assert evidence["pass"] is False
    suite = evidence["tests_installed"]
    assert suite["exit_code"] == 2
    assert suite["pass"] is False
    assert "error" in suite["summary"]


def test_gate_assertion_failure_fails(tmp_path):
    """3. assertion failure (pytest exit 1): exit != 0 and
    pass=false, with the real pytest exit code preserved."""
    rc, evidence = _run_gate(tmp_path, SUITE_ASSERTION_FAILURE, "assertfail")
    assert rc != 0
    assert evidence["pass"] is False
    suite = evidence["tests_installed"]
    assert suite["exit_code"] == 1
    assert suite["pass"] is False
    assert "failed" in suite["summary"]


@pytest.mark.parametrize("html, valid", [
    ('<head><script>var B="/api/qwen-code-bridge/web";</script></head>', True),
    ('<head><script>window.__QWEN_DAEMON_BASE__="/api/qwen-code-bridge/web";</script></head>', True),
    ('<head>Unrewritten shell</head>', False),
])
def test_gate_accepts_supported_shell_routing_only(tmp_path, html, valid):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == "/api/plugins":
                body = b'[{"id":"qwen-code-bridge","loaded":true}]'
            elif self.path == "/api/qwen-code-bridge/web/":
                body = html.encode()
            else:
                body = b"[]"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        rc, evidence = _run_gate(tmp_path, SUITE_OK, "routing", f"http://127.0.0.1:{server.server_address[1]}")
        assert evidence["tests_installed"]["pass"] is True
        assert (rc == 0) is valid
        assert evidence["pass"] is valid
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
