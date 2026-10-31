# -*- coding: utf-8 -*-
"""Make the plugin root (``qwen_code_bridge`` package) importable."""

import sys
from pathlib import Path

_PLUGIN_ROOT = str(Path(__file__).resolve().parent.parent)
if _PLUGIN_ROOT not in sys.path:
    sys.path.insert(0, _PLUGIN_ROOT)
