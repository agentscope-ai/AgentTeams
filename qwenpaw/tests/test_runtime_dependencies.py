from pathlib import Path
import tomllib


ROOT = Path(__file__).resolve().parents[1]


def test_qwenpaw_acp_api_is_compatible() -> None:
    from acp import SetSessionModelResponse

    assert SetSessionModelResponse is not None


def test_worker_and_image_target_qwenpaw_2_2_0() -> None:
    project = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")

    assert project["project"]["requires-python"] == ">=3.11,<3.14"
    assert "qwenpaw==2.2.0" in project["project"]["dependencies"]
    assert "agent-client-protocol>=0.9.0,<0.11.0" in project["project"]["dependencies"]
    assert "ARG QWENPAW_PIP_SPEC=qwenpaw==2.2.0" in dockerfile


def test_runtime_uses_public_qwenpaw_extension_points_only() -> None:
    sources = "\n".join(
        path.read_text(encoding="utf-8")
        for path in (
            ROOT / "src" / "qwenpaw_worker" / "worker.py",
            ROOT / "src" / "qwenpaw_worker" / "update.py",
            ROOT.parent / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "plugin.py",
            ROOT.parent / "plugins" / "workerflow" / "adapters" / "qwenpaw" / "plugin.py",
        )
    )
    for forbidden in (
        "QwenPawAgent._acting",
        "legacy_mcp_client_to_driver",
        "SkillPoolService",
        "save_agent_config",
        "ProviderManager",
        "get_access_control_store",
    ):
        assert forbidden not in sources
