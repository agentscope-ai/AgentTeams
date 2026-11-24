import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('export_debug_log', ROOT / 'scripts/export-debug-log.py')
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


class SessionExportTests(unittest.TestCase):
    def test_detects_qwenpaw_worker_sessions(self):
        directory = '/root/agentteams-fs/agents/alice/.qwenpaw/workspaces/default/sessions'
        def execute(_container, command):
            if command == 'echo $AGENTTEAMS_WORKER_NAME':
                return 'alice'
            if command.startswith('test -d '):
                return 'yes' if command == f"test -d '{directory}' && echo yes || echo no" else 'no'
            return ''
        with patch.object(exporter, 'docker_exec', side_effect=execute):
            self.assertEqual(exporter.detect_runtime('worker'), ('qwenpaw', directory))

    def test_qwenpaw_state_exports_actual_tool_results_and_filters_old_messages(self):
        result = {'type':'tool_result','name':'teamharness__filesync','state':'success',
            'output':[{'type':'text','text':'{"ok":true,"remotePath":"agentteams/bucket/shared/test/README.md"}'}],
            'created_at':'2026-11-23T21:01:35Z'}
        data = {'agent':{'state':{'summary':'current task', 'context':[
            {'role':'user','timestamp':'2020-01-01T00:00:00Z','content':[]},
            {'role':'assistant','content':[result]},
        ]}}}
        with tempfile.TemporaryDirectory() as directory, patch.object(exporter, 'docker_exec',
                side_effect=['/sessions/current.json', json.dumps(data)]):
            self.assertEqual(exporter.export_copaw_sessions('worker','/sessions',
                exporter.parse_ts('2026-11-23T20:00:00Z'), Path(directory), True,
                runtime='qwenpaw'), (1,1))
            rows = [json.loads(line) for line in (Path(directory)/'current.jsonl').read_text().splitlines()]
        self.assertEqual(rows[0]['runtime'], 'qwenpaw')
        self.assertEqual(rows[0]['compressed_summary'], 'current task')
        self.assertEqual(rows[1]['content'], [result])

    def test_legacy_copaw_memory_format_is_preserved(self):
        data = {'agent':{'name':'legacy','memory':{'content':[[
            {'role':'assistant','timestamp':'2026-11-23T21:01:35Z','content':'legacy reply'}
        ]]}}}
        with tempfile.TemporaryDirectory() as directory, patch.object(exporter, 'docker_exec',
                side_effect=['/sessions/old.json', json.dumps(data)]):
            self.assertEqual(exporter.export_copaw_sessions('worker','/sessions',0,Path(directory),True), (1,1))
            rows = [json.loads(line) for line in (Path(directory)/'old.jsonl').read_text().splitlines()]
        self.assertEqual(rows[0]['runtime'], 'copaw')
        self.assertEqual(rows[1]['content'], 'legacy reply')


if __name__ == '__main__':
    unittest.main()
