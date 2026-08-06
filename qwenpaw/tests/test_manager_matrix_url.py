from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def test_qwenpaw_manager_uses_configured_matrix_url_for_dm_detection() -> None:
    source = (
        ROOT / "manager" / "scripts" / "init" / "start-qwenpaw-manager.sh"
    ).read_text(encoding="utf-8")

    assert 'MATRIX_API="${AGENTTEAMS_MATRIX_URL:-http://127.0.0.1:6167}"' in source
    assert 'MATRIX_API="${MATRIX_API%/}"' in source
