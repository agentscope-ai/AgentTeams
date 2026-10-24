"""Regression coverage for saved configuration merge and route provenance."""
import copy
import json
import re
import subprocess
import unittest
from test_minimax_routing import ROOT, LIB, bash, function

STARTUP = (ROOT / 'manager/scripts/init/start-manager-agent.sh').read_text()
KNOWN = json.loads((ROOT / 'manager/configs/known-models.json').read_text())
HELPERS = '\n'.join(function(LIB, n) for n in ['agentteams_is_official_minimax', 'agentteams_minimax_config'])
ENV = {'AGENTTEAMS_LLM_PROVIDER':'openai-compat', 'AGENTTEAMS_OPENAI_BASE_URL':'https://api.minimax.io/v1',
       'AGENTTEAMS_DEFAULT_MODEL':'MiniMax-M3.1-Flash'}


def config(base='http://aigw-local.agentteams.io:8080/v1'):
    return {'models':{'providers':{'agentteams-gateway':{'baseUrl':base,'api':'openai-completions','models':copy.deepcopy(KNOWN)}}},
            'agents':{'defaults':{'model':{'primary':'agentteams-gateway/MiniMax-M3.1-Flash'},'models':{}}},
            'tools':{'media':{'image':{'enabled':True},'audio':{'model':'saved-speech'}}}}


def filtered(cfg, **env):
    return json.loads(bash(HELPERS+'\nagentteams_minimax_config', {**ENV, **env}, json.dumps(cfg)))


def merged(cfg):
    # Execute the actual startup merge before the filter; helper-only checks
    # miss alias/params destruction in this preceding production step.
    program = re.search(r"       '\n(        # Merge known models:.*?)\n       ' \\", STARTUP, re.S).group(1)
    args = ['jq']
    for key, value in {'token':'fixture','key':'fixture','model':'MiniMax-M3.1-Flash','emb_model':'',
                       'aigw_domain':'fixture','matrix_user_id':'@manager:fixture'}.items():
        args += ['--arg', key, value]
    for key, value in {'e2ee':False,'known_models':KNOWN,'ctx':1000000,'max':524288,
                       'reasoning':True,'input':['text','image']}.items():
        args += ['--argjson', key, json.dumps(value)]
    return json.loads(subprocess.run(args+[program], input=json.dumps(cfg), text=True,
                                    capture_output=True, check=True).stdout)


class SavedRoutes(unittest.TestCase):
    def test_real_merge_preserves_custom_legacy_alias_and_params(self):
        cfg = config()
        key = 'agentteams-gateway/MiniMax-M2.7'
        saved = {'alias':'Saved legacy route','params':{'temperature':0.3}}
        cfg['agents']['defaults']['models'][key] = saved
        actual = filtered(merged(cfg))
        self.assertEqual(actual['agents']['defaults']['models'][key], saved)
        self.assertIn('MiniMax-M2.7', [m['id'] for m in actual['models']['providers']['agentteams-gateway']['models']])
        self.assertEqual(actual['tools']['media'], cfg['tools']['media'])

    def test_runtime_provider_overrides_upstream_environment(self):
        custom = {'id':'MiniMax-M3.1-Flash','name':'private-deployment','reasoning':False,
                  'input':['text'],'contextWindow':8192,'maxTokens':512,
                  'compat':{'supportsReasoningEffort':True,'supportsStore':True,'maxTokensField':'max_tokens'}}
        for base in ['https://private.example/v1', 'https://api.minimax.io/v1', '', None]:
            cfg = config(base)
            cfg['models']['providers']['agentteams-gateway']['models'].insert(0, copy.deepcopy(custom))
            before = merged(cfg)
            self.assertEqual(filtered(before), before)
        cfg = config()
        cfg['models']['providers']['agentteams-gateway']['api'] = 'anthropic-messages'
        self.assertEqual(filtered(cfg), cfg)
        cfg = config('https://configured-gateway.example/v1/')
        result = filtered(cfg, AGENTTEAMS_AI_GATEWAY_URL='https://configured-gateway.example/')
        self.assertEqual(result['models']['providers']['agentteams-gateway']['models'][0]['id'], 'MiniMax-M3.1-Flash')

    def test_ordinary_highspeed_removed_saved_highspeed_retained(self):
        for legacy in ['MiniMax-M2', 'MiniMax-M2.1', 'MiniMax-M2.1-highspeed', 'MiniMax-M2.5-highspeed', 'MiniMax-M2.7-highspeed']:
            for saved in ['none', 'primary', 'fallback', 'alias', 'custom-name']:
                cfg = config()
                spec = {'id':legacy,'name':legacy,'reasoning':True,'input':['text'],'contextWindow':200000,'maxTokens':128000}
                cfg['models']['providers']['agentteams-gateway']['models'].append(spec)
                defaults = cfg['agents']['defaults']
                defaults['models']['agentteams-gateway/'+legacy] = {'alias':legacy}
                if saved == 'primary': defaults['model']['primary'] = 'agentteams-gateway/'+legacy
                if saved == 'fallback': defaults['model']['fallbacks'] = ['agentteams-gateway/'+legacy]
                if saved == 'alias': defaults['models']['agentteams-gateway/'+legacy]['params'] = {'temperature':0.3}
                if saved == 'custom-name': spec['name'] = 'My custom deployment'
                result = filtered(cfg)
                ids = [m['id'] for m in result['models']['providers']['agentteams-gateway']['models']]
                self.assertEqual(legacy in ids, saved != 'none', (legacy,saved))
                self.assertEqual(result['agents']['defaults']['model'], defaults['model'])
                self.assertEqual(result['tools'], cfg['tools'])


if __name__ == '__main__':
    unittest.main()
