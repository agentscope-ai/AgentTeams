"""Run with python3 tests/test-build-workflows.py (requires PyYAML and jq)."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]


def workflow(name):
    return yaml.safe_load((ROOT / '.github/workflows' / name).read_text())


BUILD = workflow('build.yml')
IMAGE = workflow('build-image.yml')
RELEASE = workflow('release.yml')


class BuildWorkflowTests(unittest.TestCase):
    def test_worker_consumers_checkout_submodules_and_track_gitlink(self):
        for name, job_name in [('test-controller.yml', 'matrix-bundle-core'),
                               ('test-manager.yml', 'manager-tools')]:
            with self.subTest(workflow=name, job=job_name):
                self.assertIn('.github/workflows/' + name,
                              workflow('helm-lint.yml')[True]['pull_request']['paths'])
                config = workflow(name)
                checkout = next(step for step in config['jobs'][job_name]['steps']
                                if step.get('uses', '').startswith('actions/checkout@'))
                self.assertEqual(checkout.get('with', {}).get('submodules'), 'recursive')
                for event in ('pull_request', 'push'):
                    paths = config[True][event]['paths']
                    self.assertIn('worker', paths)
                    self.assertIn('.gitmodules', paths)
                    self.assertIn('.github/workflows/' + name, paths)

    def resolve(self, targets='all', tag=False, version='v1.2.3'):
        script = BUILD['jobs']['prepare']['steps'][0]['run']
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'output'
            result = subprocess.run(['bash', '-e', '-c', script], capture_output=True,
                                    text=True, env={**os.environ,
                                        'GITHUB_OUTPUT': str(output),
                                        'GITHUB_REF': 'refs/tags/v1.2.3' if tag else 'refs/heads/main',
                                        'GITHUB_REF_NAME': 'v1.2.3' if tag else 'main',
                                        'REQUESTED_VERSION': version,
                                        'REQUESTED_TARGETS': targets})
            values = dict(line.split('=', 1) for line in output.read_text().splitlines()) if output.exists() else {}
            return result, values

    def test_tag_builds_every_image(self):
        result, values = self.resolve('worker', tag=True, version='ignored')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(values['version'], 'v1.2.3')
        selected = set(json.loads(values['targets']))
        self.assertEqual(selected, {
            'openclaw-base', 'agentteams-controller', 'embedded',
            'manager', 'manager-qwenpaw', 'worker',
            'hermes-worker', 'qwenpaw-worker',
        })
        self.assertEqual(selected, set(BUILD['jobs']) - {'prepare', 'release'})
        self.assertEqual(set(BUILD['jobs']['release']['needs']), selected | {'prepare'})

    def test_partial_build_includes_only_required_dependencies(self):
        for target in set(BUILD['jobs']) - {'prepare', 'release'}:
            with self.subTest(target=target):
                result, values = self.resolve(target)
                self.assertEqual(result.returncode, 0, result.stderr)
                selected = set(json.loads(values['targets']))
                self.assertEqual(selected, {target} | (set(BUILD['jobs'][target]['needs']) - {'prepare'}))
                # No selected job is blocked by a skipped dependency.
                for selected_target in selected:
                    self.assertTrue(set(BUILD['jobs'][selected_target]['needs']) <= selected | {'prepare'})

    def test_invalid_input_fails_before_building(self):
        for targets, version in [('unknown', 'v1.2.3'), ('all worker', 'latest'), ('all', '$(echo injected)'), ('   ', 'latest')]:
            with self.subTest(targets=targets, version=version):
                result, _ = self.resolve(targets, version=version)
                self.assertNotEqual(result.returncode, 0)

    def test_manual_default_version(self):
        result, values = self.resolve(version='')
        self.assertEqual(result.returncode, 0)
        self.assertEqual(values['version'], 'latest')

    def test_release_only_follows_successful_tag_builds(self):
        # PyYAML's YAML 1.1 loader reads the key "on" as True.
        triggers = RELEASE[True]
        self.assertNotIn('push', triggers)
        self.assertIn('workflow_call', triggers)
        self.assertIn('workflow_dispatch', triggers)
        condition = BUILD['jobs']['release']['if']
        self.assertIn("github.event_name == 'push'", condition)
        self.assertNotIn('always()', condition)
        self.assertNotIn('!cancelled()', condition)

    def test_controller_is_not_rebuilt_by_downstream_jobs(self):
        script = next(s['run'] for s in IMAGE['jobs']['build']['steps'] if s.get('name') == 'Build and push')
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / 'make'
            stub.write_text('#!/bin/bash\nexec /usr/bin/make -n "$@"\n')
            stub.chmod(0o755)
            for target in ['embedded', 'manager']:
                result = subprocess.run(['bash', '-e', '-c', script], cwd=ROOT,
                                        capture_output=True, text=True,
                                        env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                                             'TARGET': target, 'VERSION': 'v1.2.3',
                                             'REGISTRY': 'example.test', 'REPO': 'agentteams'})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn('Building + pushing agentteams-controller', result.stdout)
                self.assertIn('AGENTTEAMS_CONTROLLER_IMAGE=example.test/agentteams/agentteams-controller:v1.2.3', result.stdout)

    def test_hermes_builds_supply_the_trace_context(self):
        for target, podman in [('build-hermes-worker', '0'),
                               ('push-hermes-worker', '0'),
                               ('push-hermes-worker', '1')]:
            with self.subTest(target=target, podman=podman):
                result = subprocess.run(['make', '-n', target, 'IS_PODMAN=' + podman],
                                        cwd=ROOT, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('--build-context teamharness_trace=./plugins/teamharness/adapters/qwenpaw', result.stdout)
                self.assertTrue((ROOT / 'plugins/teamharness/adapters/qwenpaw/task_trace.py').is_file())

    def test_openclaw_base_builds_on_native_runners(self):
        self.assertEqual(BUILD['jobs']['openclaw-base']['uses'], './.github/workflows/build-openclaw-base.yml')
        native = workflow('build-openclaw-base.yml')['jobs']
        build = native['build']
        self.assertEqual(build['runs-on'], '${{ matrix.runner }}')
        self.assertEqual(build['strategy']['matrix']['include'], [
            {'arch': 'amd64', 'runner': 'ubuntu-24.04'},
            {'arch': 'arm64', 'runner': 'ubuntu-24.04-arm'},
        ])
        self.assertEqual(build['if'], "needs.check.outputs.ready != 'true'")
        self.assertEqual(native['merge']['needs'], ['check', 'build'])
        self.assertNotIn('always()', native['merge'].get('if', ''))
        self.assertNotIn('setup-qemu-action', str(native))
        original = next(s['run'] for s in IMAGE['jobs']['build']['steps'] if s.get('id') == 'existing')
        reuse = next(s['run'] for s in native['check']['steps'] if s.get('id') == 'existing')
        self.assertEqual(reuse, original)

    def test_openclaw_base_staging_tags_preserve_version_and_latest_policy(self):
        for arch in ['amd64', 'arm64']:
            result = subprocess.run(['make', '-n', 'push-openclaw-base-platform',
                                     'IS_PODMAN=0', 'VERSION=v1.2.6', 'ARCH=' + arch],
                                    cwd=ROOT, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('--platform linux/' + arch, result.stdout)
            self.assertIn('openclaw-base:v1.2.6-' + arch, result.stdout)
            self.assertNotIn('-t higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/openclaw-base:latest', result.stdout)
        for version in ['v1.2.6', 'v1.2.6-rc.1']:
            result = subprocess.run(['make', '-n', 'push-openclaw-base-manifest',
                                     'IS_PODMAN=0', 'VERSION=' + version],
                                    cwd=ROOT, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('docker buildx imagetools create', result.stdout)
            for arch in ['amd64', 'arm64']:
                self.assertIn('openclaw-base:' + version + '-' + arch, result.stdout)
            self.assertEqual('openclaw-base:latest' in result.stdout, version == 'v1.2.6')

    def test_existing_image_skips_only_when_both_architectures_are_verified(self):
        steps = IMAGE['jobs']['build']['steps']
        check = next(s for s in steps if s.get('id') == 'existing')
        build = next(s for s in steps if s.get('name') == 'Build and push')
        self.assertEqual(build['if'], "steps.existing.outputs.ready != 'true'")
        both = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': arch}} for arch in ['amd64', 'arm64']]})
        single = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': 'amd64'}}]})
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / 'docker'
            stub.write_text('#!/bin/bash\nprintf "%s" "$4" > "$IMAGE_LOG"\nprintf "%s" "$MANIFEST"\nif [[ "$ERROR" == missing ]]; then printf "ERROR: %s: not found\\n" "$4" >&2; else printf "%s" "$ERROR" >&2; fi\nexit "$STATUS"\n')
            stub.chmod(0o755)
            for target in set(BUILD['jobs']) - {'prepare', 'release'}:
                for manifest, status, error, ready in [
                    (both, '0', '', 'true'), (single, '0', '', 'false'),
                    ('', '1', 'missing', 'false'),
                    ('', '1', 'ERROR: timeout', None),
                    ('', '1', 'ERROR: 401 Unauthorized', None),
                    ('', '1', 'ERROR: 429 Too Many Requests', None),
                    ('', '1', 'ERROR: proxy: not found', None),
                    ('not-json', '0', '', None), ('', '0', '', None),
                ]:
                    with self.subTest(target=target, status=status, manifest=manifest):
                        output = Path(directory) / 'output'
                        output.write_text('')
                        log = Path(directory) / 'image'
                        result = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', check['run']], capture_output=True, text=True,
                                                env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                                                     'TARGET': target, 'VERSION': 'v1.2.3', 'REGISTRY': 'example.test', 'REPO': 'agentteams',
                                                     'MANIFEST': manifest, 'STATUS': status, 'ERROR': error, 'GITHUB_OUTPUT': str(output), 'IMAGE_LOG': str(log)})
                        self.assertEqual(result.returncode == 0, ready is not None, result.stderr)
                        self.assertEqual(output.read_text(), 'ready=' + ready + '\n' if ready is not None else '')
                        image = target if target in ['openclaw-base', 'agentteams-controller'] else 'agentteams-' + target
                        self.assertEqual(log.read_text(), f'example.test/agentteams/{image}:v1.2.3')

    def test_release_gate_rejects_missing_or_single_arch_images(self):
        script = next(s['run'] for s in RELEASE['jobs']['release']['steps'] if s.get('name') == 'Verify versioned multi-architecture images')
        both = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': arch}} for arch in ['amd64', 'arm64']]})
        single = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': 'amd64'}}]})
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / 'docker'
            stub.write_text('#!/bin/bash\nprintf "%s\\n" "$4" >> "$IMAGE_LOG"\nif [[ "$4" == *agentteams-hermes-worker:* ]]; then\n  printf "%s" "$LAST_MANIFEST"\n  exit "$LAST_STATUS"\nfi\nprintf "%s" "$MANIFEST"\n')
            stub.chmod(0o755)
            for manifest, status, success in [(both, '0', True), (single, '0', False), ('', '1', False), ('not-json', '0', False), ('', '0', False)]:
                with self.subTest(manifest=manifest, status=status):
                    log = Path(directory) / 'images'
                    log.write_text('')
                    result = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', script],
                                            capture_output=True, text=True,
                                            env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                                                 'REGISTRY': 'example.test', 'REPO': 'agentteams', 'VERSION': 'v1.2.3',
                                                 'MANIFEST': both, 'LAST_MANIFEST': manifest,
                                                 'LAST_STATUS': status, 'IMAGE_LOG': str(log)})
                    self.assertEqual(result.returncode == 0, success, result.stderr)
                    self.assertEqual(len(set(log.read_text().splitlines())), 8)


if __name__ == '__main__':
    unittest.main()
