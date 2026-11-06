# -*- coding: utf-8 -*-
"""Frontend wrapper smoke test (node): the chat wrapper must never throw.

Regression for the `bridgeChatLookup is not defined` ReferenceError that
took the whole QwenPaw console down: every chat render called the
undefined function inside the wrapped core.chat route.
"""

from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest

FRONTEND = Path(__file__).resolve().parents[1] / "frontend" / "index.js"

HARNESS = r"""
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');

let captured = null;
let fetchCalled = 0;
const Inner = function Inner() { return { type: 'Inner' }; };
const ReactMock = {
  useState(initial) { return [initial, function () {}]; },
  useEffect(fn) { try { fn(); } catch (e) { throw e; } },
  useRef(value) { return { current: value }; },
  createElement(type, props) { return { type, props }; },
};
const windowMock = {
  location: { pathname: '/chat/abc', search: '' },
  localStorage: { getItem() { return null; } },
  matchMedia() { return { matches: false, addEventListener() {}, removeEventListener() {} }; },
  addEventListener() {},
  removeEventListener() {},
  clearInterval() {},
  setInterval() { return 1; },
  QwenPaw: {
    host: { React: ReactMock },
    React: ReactMock,
    registerRoutes() {},
    route: { wrap(plugin, target, fn) { captured = fn; } },
  },
};
global.window = windowMock;
global.localStorage = windowMock.localStorage;
global.fetch = function () {
  fetchCalled += 1;
  return Promise.resolve({ ok: false, json: () => Promise.resolve(null) });
};
global.matchMedia = windowMock.matchMedia;
global.setInterval = function () { return 1; };
global.clearInterval = function () {};
vm.runInThisContext(src, { filename: 'frontend-index.js' });

if (typeof captured !== 'function') {
  console.error('wrapper not captured'); process.exit(2);
}
const Component = captured(Inner);
if (typeof Component !== 'function') {
  console.error('wrapper did not return a component'); process.exit(3);
}
let rendered;
try {
  rendered = Component();
} catch (e) {
  console.error('wrapper threw:', e && e.stack ? e.stack : e); process.exit(4);
}
if (!rendered || rendered.type !== Inner) {
  console.error('wrapper did not fall back to Inner:', JSON.stringify(rendered)); process.exit(5);
}
if (fetchCalled === 0) {
  console.error('lookup was never called'); process.exit(6);
}
console.log('WRAPPER-OK');
"""


def test_wrapper_does_not_throw_and_falls_back():
    node = shutil.which("node")
    if node is None:
        pytest.skip("node not available")
    harness = Path(__file__).with_suffix(".harness.js")
    harness.write_text(HARNESS, encoding="utf-8")
    try:
        out = subprocess.run(
            [node, str(harness), str(FRONTEND)],
            capture_output=True,
            text=True,
            timeout=20,
        )
    finally:
        harness.unlink(missing_ok=True)
    assert out.returncode == 0, out.stderr + out.stdout
    assert "WRAPPER-OK" in out.stdout
