"""Room creation accepts team Human invitations without exporting their tokens."""
import io
import json
from pathlib import Path
import sys
import urllib.error
import urllib.request

MCP_DIR = Path(__file__).resolve().parents[3] / 'teamharness' / 'mcp'
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))
import server
from mcp.room.matrix import _auto_join_task_humans


def test_auto_join_uses_scoped_controller_credential(tmp_path, monkeypatch):
    token = tmp_path / 'token'; token.write_text('worker-token')
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://controller:8090')
    monkeypatch.setenv('AGENTTEAMS_AUTH_TOKEN_FILE', str(token))
    monkeypatch.delenv('AGENTTEAMS_AUTH_TOKEN', raising=False)
    def respond(request, **kwargs):
        assert request.full_url == 'http://controller:8090/api/v1/teams/alpha/task-rooms/%21task%3Atest/join-humans'
        assert request.get_header('Authorization') == 'Bearer worker-token'
        assert json.loads(request.data) == {}
        return io.BytesIO(b'{"joined":["@human:test"],"failed":[]}')
    monkeypatch.setattr(urllib.request, 'urlopen', respond)
    assert _auto_join_task_humans('!task:test', 'alpha') == {'joined':['@human:test'], 'failed':[]}


def test_old_controller_retains_invite_compatibility(tmp_path, monkeypatch):
    token = tmp_path / 'token'; token.write_text('worker-token')
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://controller')
    monkeypatch.setenv('AGENTTEAMS_AUTH_TOKEN_FILE', str(token))
    def unavailable(request, **kwargs):
        raise urllib.error.HTTPError(request.full_url, 404, 'old controller', {}, None)
    monkeypatch.setattr(urllib.request, 'urlopen', unavailable)
    assert _auto_join_task_humans('!task:test', 'alpha') == {'skipped':True, 'reason':'controller_upgrade_required'}
