"""Observe accepted public Tasks and real original-room delivery, not legacy prose."""
import copy
import json
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]


class PublicTaskObservationTests(unittest.TestCase):
    def call(self, function, payload):
        script = (ROOT / 'tests/test-26-qwenpaw-teamharness-plugin-mode.sh').read_text()
        start = script.index('_task_check_succeeded()')
        end = script.index('SOURCE_ROOM=', start)
        return subprocess.run(['bash', '-c', script[start:end] + '\n' + function],
            input=json.dumps(payload), capture_output=True, text=True, env={**os.environ,
                'TASK_ID': 'main', 'TEST_TEAM': 'team', 'TEST_WORKER': 'worker',
                'TEST_MATRIX_DOMAIN': 'test', 'MARKER': 'CURRENT',
                'ARTIFACT_ID': 'a1', 'ARTIFACT_VERSION': 'v1'})

    def task(self):
        artifact = {'taskId': 'execution', 'artifactId': 'a1', 'version': 'v1'}
        return {'task': {'id': 'main', 'team': 'team', 'kind': 'main',
            'status': 'completed', 'completionId': 'completion',
            'acceptedResults': [{'assignedTo': 'worker', 'status': 'completed',
                'resultStatus': 'SUCCESS', 'submissionId': 's1',
                'review': {'accepted': True, 'status': 'completed', 'submissionId': 's1'},
                'candidate': {'id': 's1', 'summary': 'CURRENT', 'artifacts': [artifact]}}],
            'artifacts': [artifact]}}

    def test_exact_accepted_candidate_and_final_artifact_required(self):
        task = self.task()
        self.assertEqual(self.call('_task_check_succeeded', task).returncode, 0)
        for label in ('project', 'unaccepted', 'wrong-candidate', 'wrong-worker', 'stale-file', 'pending'):
            changed = copy.deepcopy(task)
            main = changed['task']
            child = main['acceptedResults'][0]
            if label == 'project': main['projectId'] = 'legacy-project'
            if label == 'unaccepted': child['review']['accepted'] = False
            if label == 'wrong-candidate': child['review']['submissionId'] = 's0'
            if label == 'wrong-worker': child['assignedTo'] = 'leader'
            if label == 'stale-file': main['artifacts'] = [{'artifactId': 'a1', 'version': 'v0'}]
            if label == 'pending': main['status'] = 'in_progress'
            with self.subTest(label=label):
                self.assertNotEqual(self.call('_task_check_succeeded', changed).returncode, 0)

    def delivery(self):
        return {'chunk': [
            {'type': 'm.room.message', 'sender': '@manager:test', 'event_id': '$report',
                'content': {'msgtype': 'm.text', 'body': 'Final CURRENT'}},
            {'type': 'm.room.message', 'sender': '@manager:test', 'event_id': '$file',
                'content': {'msgtype': 'm.file', 'url': 'mxc://test/media',
                    'm.relates_to': {'event_id': '$report'},
                    'org.agentteams.task_artifact': {'artifactId': 'a1', 'version': 'v1'}}}]}

    def test_delivery_requires_matching_version_and_related_report(self):
        messages = self.delivery()
        self.assertEqual(json.loads(self.call('_delivered_result', messages).stdout)['file'], 'mxc://test/media')
        for label in ('no-file', 'wrong-version', 'wrong-parent', 'wrong-sender', 'stale-report'):
            changed = copy.deepcopy(messages)
            report, file = changed['chunk']
            if label == 'no-file': changed['chunk'].pop()
            if label == 'wrong-version': file['content']['org.agentteams.task_artifact']['version'] = 'v0'
            if label == 'wrong-parent': file['content']['m.relates_to']['event_id'] = '$old'
            if label == 'wrong-sender': file['sender'] = '@worker:test'
            if label == 'stale-report': report['content']['body'] = 'OLD'
            with self.subTest(label=label):
                self.assertNotEqual(self.call('_delivered_result', changed).returncode, 0)


if __name__ == '__main__':
    unittest.main()
