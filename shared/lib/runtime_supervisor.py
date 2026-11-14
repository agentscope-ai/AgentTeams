"""Capture managed agent startup/runtime output without changing its exit status."""
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
import uuid
import json
import argparse
import re
import datetime
import urllib.request

lib_dir = Path(__file__).resolve().parent
sys.path.insert(0, str(lib_dir))
from runtime_observability import event, write_record, redact, log_dir


def main():
    os.environ['AGENTTEAMS_LOG_CAPTURED'] = '1'
    parser = argparse.ArgumentParser()
    parser.add_argument('--role', choices=['manager', 'leader', 'worker', 'standalone', 'remote-member'],
                        default=os.getenv('AGENTTEAMS_AGENT_ROLE', os.getenv('AGENTTEAMS_WORKER_ROLE', 'worker')))
    parser.add_argument('--runtime')
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == '--':
        command = command[1:]
    if not command:
        parser.error('a command is required')
    runtime = args.runtime or os.getenv('AGENTTEAMS_MANAGER_RUNTIME', 'qwenpaw')
    name = os.getenv('AGENTTEAMS_MANAGER_NAME', 'manager') if args.role == 'manager' else os.getenv('AGENTTEAMS_WORKER_CR_NAME', os.getenv('AGENTTEAMS_WORKER_NAME', 'unknown'))
    if not re.fullmatch(r'[A-Za-z0-9_-][A-Za-z0-9_.-]*', name):
        parser.error('invalid agent name for log directory')
    os.environ['AGENTTEAMS_LOG_ROLE'] = args.role
    os.environ['AGENTTEAMS_LOG_RUNTIME'] = runtime
    if runtime in ('qwenpaw', 'hermes'):
        os.environ['AGENTTEAMS_LOG_NATIVE_EVENTS'] = '1'
        os.environ['AGENTTEAMS_OTEL_RUNTIME'] = runtime
    os.environ['AGENTTEAMS_LOG_AGENT_NAME'] = name
    os.environ['AGENTTEAMS_BOOT_ID'] = uuid.uuid4().hex
    os.environ.setdefault('AGENTTEAMS_LOG_DIR', '/var/log/agentteams/manager' if args.role == 'manager' else f'/var/log/agentteams/workers/{name}')
    os.environ['PYTHONUNBUFFERED'] = '1'
    if runtime == 'qwenpaw':
        os.environ.setdefault('QWENPAW_WORKER_LOG_FILE_ENABLED', 'false')
        os.environ.setdefault('QWENPAW_WORKER_LOG_DIR', str(log_dir() / 'native'))
    lib = str(lib_dir)
    os.environ['PYTHONPATH'] = lib + os.pathsep + os.environ.get('PYTHONPATH', '')
    os.umask(0o077)
    try:
        log_dir().mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        print('[runtime-logs] log directory unavailable: ' + redact(str(exc)), file=sys.stderr, flush=True)
    event('startup.begin')
    started = time.monotonic()
    try:
        child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 text=True, errors='replace', start_new_session=True, bufsize=1)
    except OSError as exc:
        event('startup.failed', stage='process.spawn', error_type=type(exc).__name__, error=str(exc))
        event('runtime.exit', exit_code=127)
        return 127
    def monitor():
        runtime = os.environ['AGENTTEAMS_LOG_RUNTIME']
        # Hermes' wrapper endpoint is liveness-only; its live event says so explicitly.
        if runtime == 'hermes':
            event('runtime.live', evidence='entrypoint_process', matrix_verified=False)
        deadline = time.monotonic() + int(os.getenv('AGENTTEAMS_STARTUP_TIMEOUT', '300'))
        while child.poll() is None and time.monotonic() < deadline:
            try:
                if runtime in ('codex', 'deepseek-harness', 'hermes'):
                    status = json.loads((log_dir() / 'status.json').read_text())
                    if status.get('boot_id') == os.environ['AGENTTEAMS_BOOT_ID'] and status.get('state') == 'ready':
                        return
                    time.sleep(2)
                    continue
                if runtime == 'openclaw':
                    probe = subprocess.run(['openclaw', 'gateway', 'health', '--json'], capture_output=True, text=True, timeout=5)
                    ready = probe.returncode == 0 and json.loads(probe.stdout).get('ok') is True
                    evidence = 'native_gateway_health'
                else:
                    port = os.getenv('AGENTTEAMS_CONSOLE_PORT', '8088')
                    url = f'http://127.0.0.1:{port}/healthz'
                    if runtime == 'qwenpaw':
                        url = f'http://127.0.0.1:{port}/api/version'
                    elif runtime == 'openhuman':
                        url = f'http://127.0.0.1:{os.getenv("OPENHUMAN_CORE_PORT", "7788")}/health'
                    with urllib.request.urlopen(url, timeout=3) as response:
                        body = response.read()
                    if runtime in ('qwenpaw', 'openhuman'):
                        ready = True  # Native API HTTP success, not model inference.
                    else:
                        data = json.loads(body)
                        ready = data.get('ok') is True
                    evidence = 'native_api'
                if ready:
                    event('runtime.ready', evidence=evidence, duration_ms=round((time.monotonic()-started)*1000))
                    return
            except (OSError, ValueError, subprocess.SubprocessError):
                pass
            time.sleep(2)
        if child.poll() is None:
            event('startup.timeout', duration_ms=round((time.monotonic()-started)*1000))
    threading.Thread(target=monitor, daemon=True).start()
    def forward(signum, _frame):
        event('shutdown.requested', signal=signum)
        try:
            os.killpg(child.pid, signum)
        except ProcessLookupError:
            pass
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, forward)
    def capture():
        for line in child.stdout:
            clean = redact(line.rstrip('\n'))
            try:
                write_record('runtime.log', f'{datetime.datetime.now(datetime.timezone.utc).isoformat()} role={args.role} agent={name} runtime={runtime} boot={os.environ["AGENTTEAMS_BOOT_ID"]} {clean}')
            except OSError:
                print('[runtime-logs] file write failed; continuing console output', file=sys.stderr, flush=True)
            print(clean, flush=True)
    reader = threading.Thread(target=capture, daemon=True)
    reader.start()
    code = child.wait()
    # Background sync loops can inherit stdout; do not wait for their pipe forever.
    try:
        os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    reader.join(timeout=2)
    event('runtime.exit', exit_code=code, duration_ms=round((time.monotonic()-started)*1000))
    return code if code >= 0 else 128 - code


if __name__ == '__main__':
    raise SystemExit(main())
