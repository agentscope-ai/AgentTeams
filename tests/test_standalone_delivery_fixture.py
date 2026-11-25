"""A standalone assignment is a delivered message, not an acknowledgement."""
import copy
import json
import os
from pathlib import Path
import subprocess


def test_assignment_wait_requires_the_actual_addressed_business_message():
    source = (Path(__file__).parent / 'test-03-assign-task.sh').read_text()
    start = source.index('assignment_received() {')
    function = source[start:source.index('\n}', start) + 2]
    script = 'matrix_read_messages() { cat; }\n' + function + '\nassignment_received token room true'
    env = {**os.environ, 'ALICE_USER': '@alice:test', 'MANAGER_USER': '@manager:test',
           'TASK_ID': 'current-request', 'RESULT_PATH': 'shared/current/README.md'}
    event = {'type': 'm.room.message', 'sender': '@manager:test', 'content': {
        'msgtype': 'm.text', 'body': 'current-request shared/current/README.md',
        'm.mentions': {'user_ids': ['@alice:test']}}}

    def accepted(value):
        result = subprocess.run(['bash', '-c', script], input=json.dumps({'chunk': [value]}),
                                env=env, capture_output=True, text=True, timeout=10)
        return result.returncode == 0

    assert accepted(event)
    for mutate in (
        lambda e: e.update(sender='@other:test'),
        lambda e: e['content'].update(body='old-request shared/current/README.md'),
        lambda e: e['content'].update(body='current-request shared/other/README.md'),
        lambda e: e['content'].update({'m.mentions': {'user_ids': []}}),
        lambda e: e['content'].update({'org.agentteams.progress_only': True}),
        lambda e: e['content'].update(msgtype='m.notice'),
    ):
        invalid = copy.deepcopy(event)
        mutate(invalid)
        assert not accepted(invalid)
