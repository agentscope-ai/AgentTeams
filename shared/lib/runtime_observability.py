"""Bounded, redacted Manager and Worker runtime logs. Payloads and tool arguments are excluded."""
import datetime
import fcntl
import json
import logging
from logging.handlers import RotatingFileHandler
import os
from pathlib import Path
import re
import threading

_lock = threading.Lock()


def redact(text):
    text = str(text)
    try:
        parsed = json.loads(text)
    except (ValueError, TypeError):
        parsed = None
    if isinstance(parsed, (dict, list)):
        def scrub(value):
            if isinstance(value, dict):
                return {k: '[REDACTED]' if re.search(r'access[_-]?token|api[_-]?key|password|secret|authorization|gateway[_-]?key|credential', k, re.I) else scrub(v) for k, v in value.items()}
            if isinstance(value, list):
                return [scrub(v) for v in value]
            return redact(value) if isinstance(value, str) else value
        return json.dumps(scrub(parsed), ensure_ascii=False)
    for name, value in os.environ.items():
        if name == 'AGENTTEAMS_FS_ACCESS_KEY' and value == os.getenv('AGENTTEAMS_WORKER_NAME'):
            continue  # Controller-issued storage usernames are the public agent identity.
        if len(value) >= 8 and re.search(r'KEY|TOKEN|PASSWORD|SECRET|AUTH', name, re.I):
            text = text.replace(value, '[REDACTED]')
    text = re.sub(r'(?i)(Bearer\s+)[^\s"\',]+', r'\1[REDACTED]', text)
    text = re.sub(r'(?i)(["\']?(?:access_token|api_key|apiKey|password|secret|authorization)["\']?\s*[:=]\s*)("[^"]*"|\'[^\']*\'|[^\s,}]+)', r'\1"[REDACTED]"', text)
    text = re.sub(r'(https?://)[^/\s@]+@', r'\1[REDACTED]@', text)
    return text


def log_dir():
    return Path(os.getenv('AGENTTEAMS_LOG_DIR', '/var/log/agentteams/manager'))


def write_record(filename, text):
    directory = log_dir()
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    with _lock, (directory / '.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        handler = RotatingFileHandler(directory / filename,
            maxBytes=int(os.getenv('AGENTTEAMS_LOG_MAX_BYTES', str(10 * 1024 * 1024))),
            backupCount=int(os.getenv('AGENTTEAMS_LOG_BACKUPS', '3')), encoding='utf-8')
        os.chmod(directory / filename, 0o600)
        try:
            handler.emit(logging.LogRecord('runtime', logging.INFO, '', 0, redact(text), (), None))
        finally:
            handler.close()


def event(name, **fields):
    # Only callers' explicit metadata is serialized; never dump requests/responses.
    record = {'timestamp': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'runtime': os.getenv('AGENTTEAMS_LOG_RUNTIME', os.getenv('AGENTTEAMS_MANAGER_RUNTIME', 'unknown')),
              'role': os.getenv('AGENTTEAMS_LOG_ROLE', 'manager'),
              'agent_name': os.getenv('AGENTTEAMS_LOG_AGENT_NAME', os.getenv('AGENTTEAMS_WORKER_NAME', 'manager')), 'boot_id': os.getenv('AGENTTEAMS_BOOT_ID', ''),
              'event': name, **fields}
    try:
        serialized = redact(json.dumps(record, ensure_ascii=False))
        print('[agentteams-event] ' + serialized, flush=True)
        write_record('events.jsonl', serialized)
        states = {'startup.begin': 'starting', 'startup.stage': 'starting', 'runtime.launch': 'starting',
                  'runtime.live': 'live', 'runtime.ready': 'ready', 'startup.timeout': 'timeout', 'startup.failed': 'failed',
                  'runtime.exit': 'exited' if fields.get('exit_code') == 0 else 'failed'}
        if name in states:
            with _lock, (log_dir() / '.lock').open('a') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                try:
                    previous = json.loads((log_dir() / 'status.json').read_text())
                except (OSError, ValueError):
                    previous = {}
                if name != 'startup.begin' and previous.get('boot_id') not in (None, record['boot_id']):
                    return
                if name == 'startup.begin':
                    previous = {}
                if previous.get('state') == 'ready' and name in ('runtime.live', 'startup.stage', 'runtime.launch'):
                    return
                state = {**previous, **record, 'state': states[name]}
                if previous.get('state') == 'failed' and name == 'runtime.exit':
                    state['state'] = 'failed'
                temporary = log_dir() / 'status.json.tmp'
                temporary.write_text(redact(json.dumps(state, ensure_ascii=False)) + '\n')
                temporary.chmod(0o600)
                temporary.replace(log_dir() / 'status.json')
    except OSError:
        pass  # Observability must not interrupt message delivery.


if __name__ == '__main__':
    import sys
    event('startup.stage', stage=sys.argv[1])
