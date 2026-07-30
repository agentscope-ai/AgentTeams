from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def test_qwenpaw_2_uses_native_mcp_startup_without_source_patch() -> None:
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")

    assert "patch-qwenpaw-defer-mcp-startup.py" not in dockerfile
