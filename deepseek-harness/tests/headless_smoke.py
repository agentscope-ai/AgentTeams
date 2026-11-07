#!/usr/bin/env python3
"""Exercise the packaged DSH CLI and adapter with a local model endpoint.

Run inside the Worker image with its entrypoint overridden to python3.
No Matrix, object storage, or real model credentials are required.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Model(BaseHTTPRequestHandler):
    calls = 0
    saw_tool_result = False
    tool_messages = []
    sent_tool = False

    def do_POST(self):
        assert self.path == '/chat/completions', self.path
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert request['model'] == 'deepseek-v4-flash', request['model']
        type(self).calls += 1
        type(self).tool_messages.extend(m for m in request['messages'] if m.get('role') == 'tool')
        if any(m.get('role') == 'tool' and 'tool replay smoke' in json.dumps(m) for m in request['messages']):
            type(self).saw_tool_result = True
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        # Auxiliary model calls (for example session titles) have no tools and
        # may race the agent request. Send the fixture only to the agent.
        if not type(self).sent_tool and any(t.get('function', {}).get('name') == 'read' for t in request.get('tools', [])):
            type(self).sent_tool = True
            arguments = json.dumps({'file_path': str(type(self).output_file)})
            for delta, finish in [
                ({'role': 'assistant', 'tool_calls': [{'index': 0, 'id': 'call-smoke-1', 'type': 'function', 'function': {'name': 'read', 'arguments': arguments}}]}, None),
                ({'tool_calls': [{'index': 0, 'id': '', 'function': {'arguments': ''}}]}, 'tool_calls'),
            ]:
                self.wfile.write(('data: ' + json.dumps({'id': 'smoke-tool', 'object': 'chat.completion.chunk', 'model': request['model'], 'created': 1, 'choices': [{'index': 0, 'delta': delta, 'finish_reason': finish}]}) + '\n\n').encode())
            self.wfile.write(b'data: [DONE]\n\n')
            return
        self.wfile.write(('data: ' + json.dumps({
            'id': 'smoke-completion', 'object': 'chat.completion.chunk',
            'model': request['model'], 'created': 1,
            'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': 'HEADLESS_SMOKE_OK'}, 'finish_reason': 'stop'}],
            'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2},
        }) + '\n\ndata: [DONE]\n\n').encode())

    def log_message(self, *_args):
        pass


with tempfile.TemporaryDirectory() as temp:
    root = Path(temp)
    home = root / 'dsh'
    shutil.copytree('/opt/agentteams/dsh-template', home)
    plugin = Path('/opt/agentteams/plugins/teamharness')
    runtime = root / 'runtime.yaml'
    fixture = (plugin / 'adapters/deepseek-harness/fixtures/runtime.yaml').read_text()
    runtime.write_text(fixture.replace('role: worker', 'role: standalone'))
    workspace = root / 'workspace'
    workspace.mkdir()
    Model.output_file = workspace / 'smoke.txt'
    Model.output_file.write_text('tool replay smoke\n')
    server = ThreadingHTTPServer(('127.0.0.1', 0), Model)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        env = {**os.environ,
            'DSH_HOME': str(home),
            'DEEPSEEK_API_KEY': 'smoke-not-a-real-key',
            'DEEPSEEK_BASE_URL': f'http://127.0.0.1:{server.server_port}',
            'AGENTTEAMS_PLUGIN_DIR': str(plugin),
            'TEAMHARNESS_RUNTIME_CONFIG': str(runtime),
            'TEAMHARNESS_WORKSPACE': str(workspace),
            'TEAMHARNESS_DSH_SKILL_ROOT': str(root / 'skills'),
            'TEAMHARNESS_DSH_MODEL': 'deepseek-v4-flash',
            'TEAMHARNESS_PYTHON': '/usr/bin/python3',
            'TEAMHARNESS_DSH_SESSION_ID': 'session-headless-smoke',
            'TEAMHARNESS_MATRIX_EVENT_ID': '$headless-smoke-first',
        }
        result = subprocess.run(['agentteams-dsh', 'Read smoke.txt, then reply HEADLESS_SMOKE_OK.'], cwd=workspace, env=env, capture_output=True, text=True, timeout=45)
        assert result.returncode == 0, result.stderr
        assert result.stdout.strip() == 'HEADLESS_SMOKE_OK', result.stdout
        assert Model.saw_tool_result, Model.tool_messages
        assert Model.calls >= 1, Model.calls
        env.update({'TEAMHARNESS_DSH_RESUME': 'true', 'TEAMHARNESS_MATRIX_EVENT_ID': '$headless-smoke-second'})
        resumed = subprocess.run(['agentteams-dsh', 'Reply HEADLESS_SMOKE_OK again without tools.'], cwd=workspace, env=env, capture_output=True, text=True, timeout=45)
        assert resumed.returncode == 0, resumed.stderr
        assert resumed.stdout.strip() == 'HEADLESS_SMOKE_OK', resumed.stdout
        print('Packaged DSH boot, tool execution, streamed tool ID, and session resume: PASS')
    finally:
        server.shutdown()
