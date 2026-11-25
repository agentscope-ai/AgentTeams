"""The managed nio ingress retains internal Task-room planning notices."""
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

nio = pytest.importorskip('nio')
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from qwenpaw_matrix_sync import ManagedNioSync


@pytest.mark.parametrize('kind,admitted', [('handoff', True), ('assistant_digest', True),
                                           ('unrecognized', False), (None, False)])
def test_only_supported_control_notices_reach_verified_channel_ingress(kind, admitted):
    runner = ManagedNioSync.__new__(ManagedNioSync)
    runner.channel = SimpleNamespace(_on_room_event=AsyncMock())
    content = {'msgtype': 'm.notice', 'body': 'Internal planning event'}
    if kind:
        content['org.agentteams.coordination'] = {'channel_meta': {
            'teamharness_coordination': {'id': 'a' * 32, 'kind': kind, 'main_task_id': 'main'}}}
    event = nio.RoomMessageNotice.from_dict({'type': 'm.room.message', 'sender': '@leader:test',
        'event_id': '$notice', 'origin_server_ts': 1, 'content': content})
    handler = runner.handler(event)
    assert handler == (runner.channel._on_room_event if admitted else None)
