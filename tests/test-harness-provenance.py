"""Validate published image provenance for Docker lists and OCI indexes."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts/release/verify-harness-image.sh'

class HarnessProvenanceTest(unittest.TestCase):
    def fixture(self):
        manifest = {'schemaVersion': 2, 'mediaType': 'application/vnd.docker.distribution.manifest.list.v2+json',
                    'manifests': [{'platform': {'os': 'linux', 'architecture': arch}} for arch in ['amd64', 'arm64']]}
        images = {'linux/' + arch: {'os': 'linux', 'architecture': arch, 'config': {'Labels': {
            'org.opencontainers.image.revision': 'tested-sha', 'org.opencontainers.image.version': 'v1.2.7'}}}
                  for arch in ['amd64', 'arm64']}
        return manifest, images

    def verify(self, manifest, images):
        with tempfile.TemporaryDirectory() as directory:
            tool = Path(directory) / 'docker'
            tool.write_text('#!' + sys.executable + '\nimport os\nprint(os.environ["IMAGES"])\n')
            tool.chmod(0o755)
            return subprocess.run(['bash', str(SCRIPT), 'registry.test/image:v1.2.7', 'tested-sha', 'v1.2.7'],
                                  input=json.dumps(manifest), capture_output=True, text=True,
                                  env={**os.environ, 'PATH': directory + ':' + os.environ['PATH'], 'IMAGES': json.dumps(images)})

    def test_docker_list_without_oci_annotations(self):
        self.assertEqual(self.verify(*self.fixture()).returncode, 0)

    def test_oci_index_with_attestation(self):
        manifest, images = self.fixture()
        manifest['mediaType'] = 'application/vnd.oci.image.index.v1+json'
        manifest['manifests'].append({'platform': {'os': 'unknown', 'architecture': 'unknown'}})
        self.assertEqual(self.verify(manifest, images).returncode, 0)

    def test_missing_or_duplicate_architecture_is_rejected(self):
        for mutate in [lambda m: m['manifests'].pop(), lambda m: m['manifests'].append(copy.deepcopy(m['manifests'][0]))]:
            manifest, images = self.fixture()
            mutate(manifest)
            self.assertNotEqual(self.verify(manifest, images).returncode, 0)

    def test_both_architectures_require_matching_source_and_version(self):
        for arch in ['amd64', 'arm64']:
            for field in ['revision', 'version']:
                manifest, images = self.fixture()
                images['linux/' + arch]['config']['Labels']['org.opencontainers.image.' + field] = 'unexpected'
                self.assertNotEqual(self.verify(manifest, images).returncode, 0)

    def test_declared_platform_must_match_config(self):
        manifest, images = self.fixture()
        images['linux/arm64']['architecture'] = 'amd64'
        self.assertNotEqual(self.verify(manifest, images).returncode, 0)

if __name__ == '__main__':
    unittest.main()
