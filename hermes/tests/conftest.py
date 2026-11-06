"""Make ``hermes/src`` importable for the worker tests (bare pytest)."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
