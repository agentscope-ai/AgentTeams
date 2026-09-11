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
            'manager', 'manager-qwenpaw', 'worker', 'copaw-worker',
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

    def test_existing_image_skips_only_when_both_architectures_are_verified(self):
        steps = IMAGE['jobs']['build']['steps']
        check = next(s for s in steps if s.get('id') == 'existing')
        build = next(s for s in steps if s.get('name') == 'Build and push')
        self.assertEqual(build['if'], "steps.existing.outputs.ready != 'true'")
        both = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': arch}} for arch in ['amd64', 'arm64']]})
        single = json.dumps({'manifests': [{'platform': {'os': 'linux', 'architecture': 'amd64'}}]})
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / 'docker'
            stub.write_text('#!/bin/bash\nprintf "%s" "$4" > "$IMAGE_LOG"\nprintf "%s" "$MANIFEST"\nexit "$STATUS"\n')
            stub.chmod(0o755)
            for target in set(BUILD['jobs']) - {'prepare', 'release'}:
                for manifest, status, ready in [(both, '0', 'true'), (single, '0', 'false'), ('', '1', 'false'), ('not-json', '0', 'false'), ('', '0', 'false')]:
                    with self.subTest(target=target, status=status, manifest=manifest):
                        output = Path(directory) / 'output'
                        output.write_text('')
                        log = Path(directory) / 'image'
                        result = subprocess.run(['bash', '-e', '-o', 'pipefail', '-c', check['run']], capture_output=True, text=True,
                                                env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'],
                                                     'TARGET': target, 'VERSION': 'v1.2.3', 'REGISTRY': 'example.test', 'REPO': 'agentteams',
                                                     'MANIFEST': manifest, 'STATUS': status, 'GITHUB_OUTPUT': str(output), 'IMAGE_LOG': str(log)})
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(output.read_text(), 'ready=' + ready + '\n')
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
                    self.assertEqual(len(set(log.read_text().splitlines())), 9)


if __name__ == '__main__':
    unittest.main()
