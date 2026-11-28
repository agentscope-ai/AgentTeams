"""Private confirmed tool returns, staged before remote receipt publication.

This is evidence, never execution authority. Consumers must still use the
owned finish API, which validates the current operation and admitted calls.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from task_recovery import RecoveryConflict, RecoveryUnavailable


def scope(identity):
    if identity.get('event_key'):
        return ['message', identity['event_key']]
    return ['task', identity['team_id'], identity['task_id']]


def location(identity, operation):
    if not re.fullmatch(r'tools:[0-9a-f]{64}', operation):
        raise RecoveryConflict('Invalid native tool operation')
    config = os.getenv('TEAMHARNESS_RUNTIME_CONFIG')
    if not config:
        return None
    key = hashlib.sha256(json.dumps([scope(identity), operation]).encode()).hexdigest()
    return Path(config).parent / 'native-tool-returns' / (key + '.json')


def load(identity, operation):
    path = location(identity, operation)
    if path is None:
        return None
    if path.exists():
        data = path.read_bytes()
    else:
        key = remote(path)
        if key is None:
            return None
        rows = storage(['ls', '--json', key.rsplit('/', 1)[0] + '/'])
        if not any(json.loads(row).get('key') == 'terminal.json' for row in rows.splitlines()):
            return None
        data = storage(['cat', key])
    record = json.loads(data)
    if record.get('operation') != operation or scope(record['identity']) != scope(identity):
        raise RecoveryConflict('Stored native return belongs to a different operation')
    if not 0 < record['identity'].get('generation', 0) <= identity['generation']:
        raise RecoveryConflict('Stored native return has a different generation')
    cache(path, data)
    return record


def stage(identity, operation, results, **options):
    if (not isinstance(results, list) or not results
            or any(not isinstance(result, dict) or not result.get('id')
                   or 'content' not in result or result.get('effect') == 'unknown'
                   for result in results)):
        raise RecoveryConflict('Only confirmed native tool returns can be staged')
    path = location(identity, operation)
    if path is None:
        return
    record = {'identity': identity, 'operation': operation, 'results': results, **options}
    data = json.dumps(record, ensure_ascii=False).encode()
    cache(path, data)
    key = remote(path)
    if key is not None:
        storage(['pipe', key], data=path.read_bytes())


def not_dispatched(identity, operation, calls):
    """Record a failed admission before the caller can invoke the native body."""
    stage(identity, operation, [
        {'id': call['id'], 'content': [{'type': 'text', 'text':
            'Tool not dispatched: its execution checkpoint could not be confirmed.'}],
         'isError': True, 'status': 'error'} for call in calls])


def remote(path):
    prefix, member = os.getenv('AGENTTEAMS_STORAGE_PREFIX'), os.getenv('AGENTTEAMS_WORKER_NAME')
    if not prefix or not member:
        return None
    return f"{prefix.rstrip('/')}/agents/{member}/runtime/native-tool-returns/{path.stem}/terminal.json"


def storage(arguments, *, data=None):
    try:
        return subprocess.run(['mc', *arguments], input=data, capture_output=True, check=True, timeout=30).stdout
    except (OSError, subprocess.SubprocessError) as error:
        raise RecoveryUnavailable('Native return storage unavailable; preserve the pending operation') from error


def cache(path, data):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
            try:
                os.link(temporary, path)
            except FileExistsError:
                existing, record = json.loads(path.read_bytes()), json.loads(data)
                if {k:v for k,v in existing.items() if k != 'identity'} != {
                        k:v for k,v in record.items() if k != 'identity'}:
                    raise RecoveryConflict('Native operation already has a different return')
            directory = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            temporary.unlink()
