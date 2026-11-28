"""Confirmed return staging never re-executes native tools or grants ownership."""
import json
import os
from pathlib import Path
import sys
import tempfile
import subprocess
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from task_recovery import RecoveryConflict
from native_tool_returns import stage, load


class NativeReturnsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.enterContext(patch.dict(os.environ, TEAMHARNESS_RUNTIME_CONFIG=self.directory.name + '/runtime.yaml'))
        self.identity = {'task_id':'task', 'team_id':'team', 'generation':2,
                         'process_id':'owner', 'instance_id':'instance'}
        self.operation = 'tools:' + 'a' * 64
        self.results = [{'id':'call', 'content':[{'type':'text', 'text':'original return'}]}]

    def test_committed_bytes_survive_reopen_and_cannot_be_replaced(self):
        stage(self.identity, self.operation, self.results, artifacts=[])
        record = load(self.identity, self.operation)
        self.assertEqual(record['results'], self.results)
        stage(self.identity, self.operation, self.results, artifacts=[])
        with self.assertRaisesRegex(RecoveryConflict, 'different'):
            stage(self.identity, self.operation, [{'id':'call','content':'changed'}], artifacts=[])
        self.assertEqual(load(self.identity, self.operation), record)
        path = next(Path(self.directory.name).rglob('*.json'))
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_current_owner_can_use_prior_return_only_for_the_same_scope_operation(self):
        stage(self.identity, self.operation, self.results, artifacts=[])
        new_owner = {**self.identity, 'generation':3, 'process_id':'replacement'}
        self.assertEqual(load(new_owner, self.operation)['results'], self.results)
        for change in [{'team_id':'foreign'}, {'task_id':'foreign'}, {'event_key':'foreign'}]:
            self.assertIsNone(load({**self.identity, **change}, self.operation))
        self.assertIsNone(load(self.identity, 'tools:' + 'b' * 64))
        with self.assertRaisesRegex(RecoveryConflict, 'generation'):
            load({**self.identity, 'generation':1}, self.operation)

    def test_message_scope_and_delivery_flag_are_retained(self):
        identity = {'event_key':'event', 'generation':1, 'instance_id':'i', 'process_id':'p'}
        stage(identity, self.operation, self.results, deliver_result=True)
        self.assertIs(load(identity, self.operation)['deliver_result'], True)
        self.assertIsNone(load({**identity,'event_key':'other'}, self.operation))

    def test_unknown_results_are_not_staged(self):
        with self.assertRaises(RecoveryConflict):
            stage(self.identity, self.operation, [{'id':'call','content':'lost','effect':'unknown'}])
        self.assertIsNone(load(self.identity, self.operation))

    def test_corrupt_receipt_cannot_be_used(self):
        stage(self.identity, self.operation, self.results)
        path = next(Path(self.directory.name).rglob('*.json'))
        record = json.loads(path.read_text())
        record['operation'] = 'tools:' + 'c' * 64
        path.write_text(json.dumps(record))
        with self.assertRaises(RecoveryConflict):
            load(self.identity, self.operation)

    def test_remote_return_restores_after_local_container_files_are_lost(self):
        from types import SimpleNamespace
        objects = {}
        def run(command, **kwargs):
            action, key = command[1], command[-1]
            if action == 'pipe':
                objects[key] = kwargs['input']
                return SimpleNamespace(stdout=b'')
            if action == 'ls':
                return SimpleNamespace(stdout=b'{"key":"terminal.json"}\n' if key + 'terminal.json' in objects else b'')
            return SimpleNamespace(stdout=objects[key])
        with patch.dict(os.environ, AGENTTEAMS_STORAGE_PREFIX='private/store', AGENTTEAMS_WORKER_NAME='worker'), \
                patch('native_tool_returns.subprocess.run', side_effect=run):
            stage(self.identity, self.operation, self.results, artifacts=[])
            original = next(iter(objects.values()))
            next(Path(self.directory.name).rglob('*.json')).unlink()
            current = {**self.identity, 'generation':3, 'process_id':'replacement'}
            self.assertEqual(load(current,self.operation)['results'],self.results)
            stage(current,self.operation,self.results,artifacts=[])
            self.assertEqual(next(iter(objects.values())),original, 'retain the original return provenance')

    def test_upload_failure_keeps_the_private_local_return(self):
        from task_recovery import RecoveryUnavailable
        with patch.dict(os.environ, AGENTTEAMS_STORAGE_PREFIX='private/store', AGENTTEAMS_WORKER_NAME='worker'), \
                patch('native_tool_returns.subprocess.run', side_effect=subprocess.TimeoutExpired('mc',30)):
            with self.assertRaises(RecoveryUnavailable):
                stage(self.identity,self.operation,self.results)
            self.assertEqual(load(self.identity,self.operation)['results'],self.results)
