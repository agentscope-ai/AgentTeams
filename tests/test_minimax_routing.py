"""Offline checks for the direct MiniMax install/config/gateway boundary.

Run: python3 tests/test_minimax_routing.py
Only extracted production functions/blocks execute; no installation or inference.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

ROOT = Path(__file__).resolve().parents[1]
INSTALL = (ROOT / 'install/agentteams-install.sh').read_text()
LIB = (ROOT / 'shared/lib/agentteams-env.sh').read_text()
HIGRESS = (ROOT / 'manager/scripts/init/setup-higress.sh').read_text()


def function(source, name):
    return re.search(r'^' + re.escape(name) + r'\(\) \{\n.*?^\}', source, re.M | re.S).group()


def bash(script, env=None, data=None):
    allowed = {'PATH': os.environ['PATH'], 'LANG': 'C.UTF-8'}
    allowed.update(env or {})
    return subprocess.run(['bash', '-e', '-c', script], input=data, text=True,
                          capture_output=True, check=True, env=allowed).stdout


class MiniMaxRouting(unittest.TestCase):
    def test_installer_default_and_request_boundary(self):
        requests = []
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                requests.append((self.path, json.loads(self.rfile.read(int(self.headers['Content-Length'])))))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"success":true}')
            def log_message(self, *args):
                pass
        server = HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            for endpoint in ['https://api.minimax.io/v1', 'https://api.minimax.cn/v1/']:
                for selected in ['', 'MiniMax-M3', 'MiniMax-M2.7', 'saved-custom']:
                    for language in ['en', 'zh']:
                        helpers = '\n'.join(function(INSTALL, name) for name in [
                            'is_official_minimax', 'openai_default_model', 'step_llm', 'test_llm_connectivity'])
                        # Only reroute the transport: retain production payload generation.
                        script = '''log() { :; }; msg() { :; }; prompt() { :; }
                        curl() {
                            local args=() arg
                            for arg in "$@"; do
                                case "$arg" in https://api.minimax.*/v1/chat/completions)
                                    arg="$FIXTURE_URL/v1/chat/completions";; esac
                                args+=("$arg")
                            done
                            command curl "${args[@]}"
                        }
                        ''' + helpers + '''
                        step_llm
                        test_llm_connectivity "$AGENTTEAMS_OPENAI_BASE_URL" fixture "$AGENTTEAMS_DEFAULT_MODEL"
                        '''
                        bash(script, {'AGENTTEAMS_NON_INTERACTIVE':'1','AGENTTEAMS_LANGUAGE':language,
                                      'AGENTTEAMS_LLM_PROVIDER':'openai-compat', 'AGENTTEAMS_OPENAI_BASE_URL':endpoint,
                                      'AGENTTEAMS_DEFAULT_MODEL':selected, 'FIXTURE_URL':f'http://127.0.0.1:{server.server_port}'})
                        self.assertEqual(requests[-1], ('/v1/chat/completions', {
                            'model':selected or 'MiniMax-M3.1-Flash','messages':[{'role':'user','content':'hi'}]}))
            # Exercise production Higress provider JSON and its actual console HTTP helper.
            block = HIGRESS[HIGRESS.index('    case "${LLM_PROVIDER}" in', HIGRESS.index('# NON-IDEMPOTENT SECTION')):]
            block = block[:block.index('\n    esac') + len('\n    esac')]
            for endpoint in ['https://api.minimax.io/v1', 'https://api.minimax.cn/v1']:
                bash('log() { :; }; higress_get() { :; };\n' + function(HIGRESS,'higress_api') + '\n' + block,
                     {'LLM_PROVIDER':'openai-compat', 'AGENTTEAMS_OPENAI_BASE_URL':endpoint,
                      'AGENTTEAMS_LLM_API_KEY':'fixture', 'HIGRESS_COOKIE_FILE':'',
                      'CONSOLE_URL':f'http://127.0.0.1:{server.server_port}'})
                path, provider = requests[-1]
                self.assertEqual(path, '/v1/ai/providers')
                self.assertEqual(provider['rawConfigs']['openaiCustomUrl'], endpoint)
                self.assertEqual(provider['protocol'], 'openai/v1')
                self.assertEqual(provider['tokens'], ['fixture'])
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_saved_fallback_and_customized_legacy_alias(self):
        helper = function(LIB,'agentteams_is_official_minimax') + '\n' + function(LIB,'agentteams_minimax_config')
        for saved in [
            {'model':{'primary':'agentteams-gateway/MiniMax-M3','fallbacks':['agentteams-gateway/MiniMax-M2.7']},'models':{}},
            {'model':{'primary':'agentteams-gateway/MiniMax-M3'},'models':{'agentteams-gateway/MiniMax-M2.7':{'alias':'My saved model','params':{'temperature':0.5}}}},
        ]:
            config = {'models':{'providers':{'agentteams-gateway':{'models':json.loads((ROOT/'manager/configs/known-models.json').read_text())}}},'agents':{'defaults':saved}}
            result = json.loads(bash(helper+'\nagentteams_minimax_config', {'AGENTTEAMS_LLM_PROVIDER':'openai-compat','AGENTTEAMS_OPENAI_BASE_URL':'https://api.minimax.io/v1','AGENTTEAMS_DEFAULT_MODEL':'MiniMax-M3'}, json.dumps(config)))
            self.assertIn('MiniMax-M2.7',[m['id'] for m in result['models']['providers']['agentteams-gateway']['models']])
            self.assertEqual(result['agents']['defaults']['model'], saved['model'])
            if saved['models']:
                self.assertEqual(result['agents']['defaults']['models']['agentteams-gateway/MiniMax-M2.7'],saved['models']['agentteams-gateway/MiniMax-M2.7'])

    def test_catalog_preserves_saved_custom_and_media(self):
        helper = function(LIB,'agentteams_is_official_minimax') + '\n' + function(LIB,'agentteams_minimax_config')
        models = json.loads((ROOT / 'manager/configs/known-models.json').read_text())
        for selected in ['MiniMax-M3.1-Flash', 'MiniMax-M2.7', 'custom-model']:
            config = {'models':{'providers':{'agentteams-gateway':{'models':models + [{'id':'custom-model','input':['text']} ]}}},
                      'agents':{'defaults':{'model':{'primary':'agentteams-gateway/'+selected},
                                            'models':{'agentteams-gateway/MiniMax-M2.7':{'alias':'MiniMax-M2.7'}}}},
                      'tools':{'media':{'image':{'enabled':True},'audio':{'model':'speech-fixture'}}}}
            for endpoint in ['https://api.minimax.io/v1', 'https://api.minimax.cn/v1/']:
                result = json.loads(bash(helper+'\nagentteams_minimax_config', {
                    'AGENTTEAMS_LLM_PROVIDER':'openai-compat','AGENTTEAMS_OPENAI_BASE_URL':endpoint,
                    'AGENTTEAMS_DEFAULT_MODEL':selected},json.dumps(config)))
                specs = result['models']['providers']['agentteams-gateway']['models']
                ids = [m['id'] for m in specs]
                self.assertEqual(ids[:2], ['MiniMax-M3.1-Flash','MiniMax-M3'])
                self.assertEqual('MiniMax-M2.7' in ids, selected == 'MiniMax-M2.7')
                self.assertIn('custom-model',ids)
                self.assertEqual(specs[0]['maxTokens'],524288)
                self.assertFalse(specs[0]['compat']['supportsReasoningEffort'])
                self.assertEqual(result['agents']['defaults']['model'],config['agents']['defaults']['model'])
                self.assertEqual(result['tools'],config['tools'])
            for endpoint in ['https://coding.dashscope.aliyuncs.com/v1', 'https://custom.example/v1',
                             'https://api.minimax.io.evil.example/v1', 'https://api.minimax.io/anthropic']:
                raw = json.dumps(config)
                self.assertEqual(bash(helper+'\nagentteams_minimax_config', {
                    'AGENTTEAMS_LLM_PROVIDER':'openai-compat','AGENTTEAMS_OPENAI_BASE_URL':endpoint},raw),raw)


if __name__ == '__main__':
    unittest.main()
