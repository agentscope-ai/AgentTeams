"""Release/CI planning and prerequisite checks without Docker or model calls."""
import gzip
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
import yaml

ROOT = Path(__file__).resolve().parents[1]


class PlanTests(unittest.TestCase):
    def plan(self, path, before='before\n', after='after\n', event='pull_request', ref='refs/heads/test', main_change=None):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=repo, text=True).strip()
            git('init', '-q')
            git('config', 'core.hooksPath', '/dev/null')
            git('config', 'user.name', 'CI fixture')
            git('config', 'user.email', 'ci@example.test')
            file = repo / path
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text(before)
            git('add', '.')
            git('commit', '-qm', 'base')
            base = git('rev-parse', 'HEAD')
            file.write_text(after)
            git('commit', '-qam', 'change')
            head = git('rev-parse', 'HEAD')
            if main_change:
                git('checkout', '-qb', 'advanced-main', base)
                changed = repo / main_change[0]
                changed.parent.mkdir(parents=True, exist_ok=True)
                changed.write_text(main_change[1])
                git('add', '.')
                git('commit', '-qm', 'unrelated main change')
                base = git('rev-parse', 'HEAD')
            result = subprocess.run(['python3', str(ROOT / 'hack/ci-integration-plan.py'),
                '--base', base, '--head', head, '--event', event, '--ref', ref],
                cwd=repo, capture_output=True, text=True, env={**os.environ, 'GITHUB_OUTPUT': ''})
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(result.stdout)

    def test_runtime_and_unknown_changes_keep_full_matrix(self):
        for path in ['manager/scripts/start.sh', 'Makefile', 'hack/versions.env', 'tests/test-03-assign-task.sh']:
            self.assertTrue(self.plan(path)['run_models'], path)

    def test_pipeline_changes_keep_build_and_controller_checks_without_models(self):
        for path in ['.github/workflows/build.yml', 'qwenpaw/tests/test_integration_workflow.py']:
            result = self.plan(path)
            self.assertTrue(result['run_builds'])
            self.assertFalse(result['run_models'])

    def test_version_pins_skip_repeated_runtime_builds(self):
        result = self.plan('install/agentteams-install.sh',
            'VERSION="v1.2.5"\nport=28090\n', 'VERSION="v1.2.6"\nport=28090\n')
        self.assertFalse(result['run_builds'])
        self.assertFalse(result['run_models'])
        changed_code = self.plan('install/agentteams-install.sh',
            'VERSION="v1.2.5"\nport=28090\n', 'VERSION="v1.2.6"\nport=28091\n')
        self.assertTrue(changed_code['run_models'])

    def test_release_tags_and_manual_runs_always_keep_full_matrix(self):
        for event, ref in [('push', 'refs/tags/v1.2.7'), ('workflow_dispatch', 'refs/heads/main')]:
            self.assertTrue(self.plan('.github/workflows/build.yml', event=event, ref=ref)['run_models'])

    def test_main_unchanged_base_is_not_forced_to_recompile(self):
        self.assertFalse(self.plan('worker/start.sh', event='push')['base_changed'])
        self.assertTrue(self.plan('openclaw-base/Dockerfile', event='push')['base_changed'])
        self.assertTrue(self.plan('hack/versions.env')['base_changed'])

    def test_pr_ignores_unrelated_changes_when_main_advances(self):
        result = self.plan('.github/workflows/test-controller.yml',
                           main_change=('openclaw-base/Dockerfile', 'main-only runtime change\n'))
        self.assertTrue(result['run_builds'])
        self.assertFalse(result['run_models'])
        self.assertFalse(result['base_changed'])

    def test_pr_pin_normalization_also_reads_from_the_merge_base(self):
        result = self.plan('install/agentteams-install.sh',
            'VERSION=v1.2.5\n', 'VERSION=v1.2.6\n',
            main_change=('install/agentteams-install.sh', 'VERSION=v1.2.5\nnew_behavior=1\n'))
        self.assertFalse(result['run_builds'])
        self.assertFalse(result['run_models'])

    def test_pr_runtime_changes_still_require_models_when_main_advances(self):
        self.assertTrue(self.plan('worker/start.sh',
            main_change=('README.md', 'main-only docs\n'))['run_models'])

    def test_unavailable_diff_keeps_full_coverage(self):
        scope = runpy.run_path(str(ROOT / 'hack/ci-integration-plan.py'))
        result = scope['plan']('missing-base', 'missing-head', 'pull_request', 'refs/pull/1/merge')
        self.assertTrue(all(result[key] for key in ('run_builds', 'run_models', 'base_changed')))


class BaseProbeTests(unittest.TestCase):
    def probe(self, manifest, status=0, error=''):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / 'docker'
            stub.write_text('#!/bin/sh\nif [ "$5" = "--format" ]; then printf "sha256:%064d" 0; else printf "%s" "$MANIFEST"; fi\nprintf "%s" "$ERROR" >&2\nexit "$STATUS"\n')
            stub.chmod(0o755)
            return subprocess.run(['python3', str(ROOT / 'hack/ci-base-image.py'), 'registry.example/base:pin'],
                capture_output=True, text=True, env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                    'MANIFEST': json.dumps(manifest), 'ERROR': error, 'STATUS': str(status), 'GITHUB_OUTPUT': ''})

    def test_published_multiarch_base_is_used(self):
        result = self.probe({'schemaVersion': 2, 'manifests': [
            {'platform': {'os': 'linux', 'architecture': arch}} for arch in ['amd64', 'arm64']]})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(json.loads(result.stdout)['openclaw_base'])
        self.assertEqual(json.loads(result.stdout)['openclaw_base_ref'], 'registry.example/base@sha256:' + '0' * 64)

    def test_real_absence_selects_one_shared_build(self):
        result = self.probe(None, 1, 'ERROR: registry.example/base:pin: not found')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)['openclaw_base'])

    def test_uncertain_registry_errors_cannot_trigger_source_bootstraps(self):
        for error in ['ERROR: timeout', 'ERROR: 401 Unauthorized', 'ERROR: proxy: not found']:
            result = self.probe(None, 1, error)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('Cannot verify', result.stderr)

    def test_incomplete_published_base_is_not_rebuilt_or_consumed(self):
        result = self.probe({'schemaVersion': 2, 'manifests': [
            {'platform': {'os': 'linux', 'architecture': 'amd64'}}]})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('incomplete', result.stderr)


class WorkflowPlanTests(unittest.TestCase):
    def test_runtime_submodule_updates_trigger_runtime_validation(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())
        triggers = workflow.get('on', workflow.get(True))
        for event in ('push', 'pull_request'):
            self.assertIn('hermes', triggers[event]['paths'])
            self.assertIn('worker', triggers[event]['paths'])
            self.assertIn('.gitmodules', triggers[event]['paths'])
        for name in ('build-image', 'build-integration-images', 'build-rc', 'check-node-pins', 'test-integration'):
            jobs = yaml.safe_load((ROOT / f'.github/workflows/{name}.yml').read_text())['jobs']
            for job in jobs.values():
                for step in job.get('steps', []):
                    if step.get('uses', '').startswith('actions/checkout@'):
                        self.assertEqual(step.get('with', {}).get('submodules'), 'recursive', name)

    def test_runtime_shards_download_consumers_without_the_build_only_base(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())
        steps = workflow['jobs']['integration-tests']['steps']
        downloads = [step for step in steps if step.get('uses') == 'actions/download-artifact@v4'
                     and step.get('with', {}).get('path') == '/tmp/images']
        self.assertEqual(len(downloads), 2)
        for step in downloads:
            self.assertEqual(step['with']['pattern'], 'image-!(openclaw-base)')

    def test_archive_retry_only_rechecks_a_failed_download_or_verification(self):
        steps = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())['jobs']['integration-tests']['steps']
        retry = next(step for step in steps if step.get('name') == 'Retry the image download once')
        verify = next(step for step in steps if step.get('name') == 'Verify image archives after the retry')
        self.assertEqual(verify.get('if'), retry['if'])
        for failed in ('download-images', 'verify-images'):
            self.assertIn(f"steps.{failed}.outcome == 'failure'", verify['if'])

    def test_each_archive_verifier_rejects_empty_or_corrupt_downloads(self):
        steps = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())['jobs']['integration-tests']['steps']
        for step in steps:
            if step.get('name') not in ('Verify image archives', 'Verify image archives after the retry'):
                continue
            for state in ('valid', 'empty', 'corrupt'):
                with self.subTest(step=step['name'], state=state), tempfile.TemporaryDirectory() as directory:
                    archive = Path(directory) / 'image-worker/worker.tar.gz'
                    archive.parent.mkdir()
                    if state == 'valid':
                        archive.write_bytes(gzip.compress(b'image fixture'))
                    elif state == 'corrupt':
                        archive.write_bytes(b'truncated')
                    script = step['run'].replace('/tmp/images', directory)
                    result = subprocess.run(['bash', '-eu', '-c', script], capture_output=True, text=True)
                    self.assertEqual(result.returncode == 0, state == 'valid', result.stderr)

    def test_matrix_channel_cases_keep_separate_processes_with_two_setups(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/test-controller.yml').read_text())['jobs']
        job = jobs['matrix-channel-regressions']
        suites = job['strategy']['matrix']['include']
        self.assertEqual(len(suites), 2)
        expected = {'consume_error', 'show_thinking_gate', 'send_gate_retrigger_placeholder',
                    'mention_localpart', 'send_gate_retrigger_relevance', 'retry_classifier',
                    'sync_timeout_clamp', 'no_reply_final_line'}
        actual = [case for suite in suites for case in suite['cases'].split()]
        self.assertEqual(set(actual), expected)
        self.assertEqual(len(actual), len(expected))
        steps = job['steps']
        guard = next(step for step in steps if step.get('name') == 'Verify matrix-nio is importable (tests must not skip)')
        self.assertIn('import nio', guard['run'])
        run = next(step for step in steps if step.get('name') == 'Run isolated Matrix regressions')
        with tempfile.TemporaryDirectory() as directory:
            calls = Path(directory) / 'calls'
            stub = Path(directory) / 'python'
            stub.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALLS"\ncase "$*" in *"$FAIL_CASE"*) exit 1;; esac\n')
            stub.chmod(0o755)
            for fail in (False, True):
                calls.write_text('')
                for suite in suites:
                    result = subprocess.run(['bash', '-eu', '-c', run['run']], capture_output=True, text=True,
                        env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                             'CASES': suite['cases'], 'CALLS': str(calls),
                             'FAIL_CASE': suite['cases'].split()[0] if fail else 'no-failure'})
                    self.assertEqual(result.returncode == 0, not fail, result.stderr)
                # Every case runs even if an earlier pytest process fails.
                self.assertCountEqual(calls.read_text().splitlines(), [
                    f'-m pytest plugins/tests/agentteams-matrix-channel/test_{case}.py -q -rs'
                    for case in expected])

    def test_qwenpaw_workflow_contract_agrees_with_build_groups(self):
        contract = runpy.run_path(str(ROOT / 'qwenpaw/tests/test_integration_workflow.py'))
        contract['test_integration_workflow_runs_qwenpaw_like_copaw']()

    def test_build_groups_wait_only_for_actual_dockerfile_dependencies(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())['jobs']
        expected = {
            'build-images-independent': ({'qwenpaw-worker', 'deepseek-harness-worker'}, {'detect-changes', 'build-base'}),
            'build-images-controller': ({'embedded', 'manager-qwenpaw', 'hermes-worker'}, {'detect-changes', 'build-base'}),
            'build-images-openclaw': ({'manager', 'worker'}, {'detect-changes', 'resolve-openclaw-base', 'build-base', 'build-openclaw-base'}),
        }
        dockerfiles = {'embedded': 'agentteams-controller/Dockerfile.embedded',
                      'manager-qwenpaw': 'qwenpaw/Dockerfile', 'hermes-worker': 'hermes/Dockerfile',
                      'qwenpaw-worker': 'qwenpaw/Dockerfile', 'deepseek-harness-worker': 'deepseek-harness/Dockerfile',
                      'manager': 'worker/Dockerfile', 'worker': 'worker/Dockerfile'}
        targets = []
        for name, (wanted, dependencies) in expected.items():
            job = jobs[name]
            self.assertEqual(set(job['needs']), dependencies)
            self.assertEqual(job['with'].get('controller', False), 'build-base' in dependencies)
            selected = json.loads(job['with']['targets'])
            self.assertEqual(set(selected), wanted)
            targets.extend(selected)
            for target in selected:
                dockerfile = (ROOT / dockerfiles[target]).read_text()
                self.assertEqual('${AGENTTEAMS_CONTROLLER_IMAGE}' in dockerfile, 'build-base' in dependencies, target)
                self.assertEqual('${OPENCLAW_BASE_IMAGE}' in dockerfile, 'build-openclaw-base' in dependencies, target)
        self.assertEqual(len(targets), len(set(targets)))
        self.assertEqual(set(targets), set(dockerfiles))
        # A failed/stalled registry probe must not become a transitive parent of independent builds.
        self.assertNotIn('resolve-openclaw-base', jobs['detect-changes'].get('needs', []))
        self.assertNotIn('decide', {step.get('id') for step in jobs['detect-changes']['steps']})

    def test_test_shards_require_every_image_group_to_succeed(self):
        jobs = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())['jobs']
        test = jobs['integration-tests']
        for group in ['build-images-independent', 'build-images-controller', 'build-images-openclaw', 'build-images-codex']:
            self.assertIn(group, test['needs'])
            self.assertIn(f"needs.{group}.result == 'success'", test['if'])
        openclaw = jobs['build-images-openclaw']
        self.assertIn("needs.resolve-openclaw-base.result == 'success'", openclaw['if'])
        self.assertIn("needs.build-base.result == 'success'", openclaw['if'])
        self.assertIn("needs.build-openclaw-base.result == 'skipped'", openclaw['if'])

    def test_consumers_build_with_local_shared_or_immutable_published_base(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/build-integration-images.yml').read_text())
        step = next(step for step in workflow['jobs']['build']['steps']
                    if step.get('name') == 'Build image')['run']
        for local in ('true', 'false'):
            for target in ('manager', 'worker'):
                immutable = 'example/base@sha256:' + '0' * 64
                script = step.replace('${{ inputs.local_openclaw_base }}', local)
                script = script.replace('${{ matrix.target }}', target)
                script = script.replace('${{ inputs.published_base_ref }}', immutable)
                script = script.replace('${{ inputs.controller }}', 'true')
                script = script.replace('make "${MAKE_ARGS[@]}"', 'make -n "${MAKE_ARGS[@]}"')
                result = subprocess.run(['bash', '-eu', '-c', script], cwd=ROOT, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                expected = 'agentteams/openclaw-base:latest' if local == 'true' else immutable
                self.assertIn('OPENCLAW_BASE_IMAGE=' + expected, result.stdout)
                self.assertNotIn('==> Building agentteams-controller image:', result.stdout)

    def test_pipeline_matrix_keeps_three_runtime_controller_checks(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/test-integration.yml').read_text())
        step = next(step for step in workflow['jobs']['detect-changes']['steps']
                    if step.get('id') == 'test-matrix')
        with tempfile.NamedTemporaryFile() as output:
            subprocess.run(['bash', '-eu', '-c', step['run']], check=True,
                           env={**os.environ, 'GITHUB_OUTPUT': output.name,
                                'RUN_MODELS': 'false', 'UNTRUSTED_PR': 'false'})
            matrix = json.loads(Path(output.name).read_text().removeprefix('matrix='))['include']
        self.assertEqual({item['worker_runtime'] for item in matrix}, {'openclaw', 'qwenpaw', 'hermes'})
        self.assertEqual(len(matrix), 3)
        self.assertTrue(all(item['shard'] == 'controller-cr' and not item['requires_secret'] for item in matrix))


if __name__ == '__main__':
    unittest.main()
