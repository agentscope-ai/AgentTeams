"""Expose the common protocol library to Matrix channel regression tests."""
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "shared/lib"))
