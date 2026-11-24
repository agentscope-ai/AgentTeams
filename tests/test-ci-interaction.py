"""Exercise CI observation waits without containers, keys, or real sleeps."""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class SessionObservationTests(unittest.TestCase):
    def observe(self, runtime, session, size, worker=False):
        result = subprocess.run(['bash', '-c', r'''
source tests/lib/agent-metrics.sh
_detect_runtime() { echo "$RUNTIME"; }
_detect_session_dir() { echo /sessions; }
get_latest_session() { echo "$SESSION"; }
worker_container_name() { echo test-worker; }
docker() {
    if [ "$1" = ps ]; then echo test-worker; else echo "$SIZE"; fi
}
sleep() { echo SLEEP; }
if [ "$WORKER" = 1 ]; then
    wait_for_worker_session_stable alice 4 8
else
    wait_for_session_stable 4 8
fi
'''], cwd=ROOT, env={**os.environ, 'RUNTIME': runtime, 'SESSION': session,
            'SIZE': str(size), 'WORKER': str(int(worker)),
            'TEST_CONTROLLER_CONTAINER': 'test-controller', 'TEST_AGENT_CONTAINER': 'test-manager'},
            capture_output=True, text=True, check=True)
        return result.stdout, result.stderr

    def test_absent_empty_and_unsupported_observations_do_not_wait(self):
        for worker in (False, True):
            for runtime, session, size in [('unknown', '', 0), ('openclaw', '', 0),
                    ('openclaw', '/sessions/empty.jsonl', 0), ('copaw', '', 0),
                    ('qwenpaw', '', 0), ('hermes', '', 0)]:
                with self.subTest(worker=worker, runtime=runtime, session=session):
                    out, err = self.observe(runtime, session, size, worker)
                    self.assertNotIn('SLEEP', out)
                    self.assertNotIn('stable after', err)

    def test_real_nonempty_session_still_gets_bounded_stabilization(self):
        for worker in (False, True):
            out, err = self.observe('openclaw', '/sessions/live.jsonl', 42, worker)
            self.assertEqual(out.count('SLEEP'), 2)
            self.assertIn('stable after 4s (size=42)', err)


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


class MatrixReplyTests(unittest.TestCase):
    def test_immediate_reply_is_kept_with_explicit_pre_send_baseline(self):
        result = subprocess.run(['bash', '-c', r'''
source tests/lib/matrix-client.sh
matrix_read_messages() { echo '{"chunk":[{"sender":"@alice:test","type":"m.room.message","event_id":"$ready","content":{"body":"CURRENT_READY"}}]}'; }
sleep() { :; }
matrix_wait_for_reply_matching token room @alice: CURRENT_READY 10 "" "" "" 600 ""
'''], cwd=ROOT, capture_output=True, text=True, env={**os.environ,
            'TEST_CONTROLLER_CONTAINER': 'test-controller', 'TEST_AGENT_CONTAINER': 'test-manager'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), 'CURRENT_READY')


class HumanSupplementTests(unittest.TestCase):
    def scenario(self, script, ready=True, delivered=True):
        source = (ROOT / 'tests/test-04-human-intervene.sh').read_text()
        block = source[source.index('RUN_ID='):source.index('log_section "Collect Metrics"')]
        with tempfile.TemporaryDirectory() as directory:
            fixture = Path(directory) / 'fixture.py'
            fixture.write_text(script)
            result = subprocess.run(['bash', '-c', r'''
source tests/lib/test-helpers.sh
get_worker_room_id() { echo '!alice:test'; }
minio_setup() { :; }
matrix_send_message() { echo SEND >> "$TEST_OUTPUT_DIR/trace"; }
matrix_wait_for_reply_matching() { [ "$READY" = 1 ] && echo "${RUN_ID}_READY"; }
minio_wait_for_file() { [ "$DELIVERED" = 1 ]; }
minio_read_file() { cat "$FIXTURE"; }
test_teardown() { :; }
''' + block + '\n[ "$TESTS_FAILED" = 0 ]'], cwd=ROOT,
                capture_output=True, text=True, env={**os.environ,
                    'TEST_CONTROLLER_CONTAINER': 'fixture-controller', 'TEST_AGENT_CONTAINER': 'fixture-manager',
                    'TEST_OUTPUT_DIR': directory, 'FIXTURE': str(fixture),
                    'READY': str(int(ready)), 'DELIVERED': str(int(delivered))})
            sends = (Path(directory) / 'trace').read_text().count('SEND')
            return result, sends

    def test_both_original_and_supplement_must_execute_correctly(self):
        correct = 'import sys\nprint("Hello, " + (sys.argv[1] if len(sys.argv) > 1 else "World") + "!")\n'
        result, sends = self.scenario(correct)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(sends, 2)
        result, _ = self.scenario('print("Hello, World!")\n')
        self.assertNotEqual(result.returncode, 0)

    def test_missing_delivery_and_missing_initial_ack_fail(self):
        result, _ = self.scenario('', delivered=False)
        self.assertNotEqual(result.returncode, 0)
        result, sends = self.scenario('', ready=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(sends, 1)


if __name__ == '__main__':
    unittest.main()
