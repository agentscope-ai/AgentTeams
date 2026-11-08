"""Exercise official download fallback and integrity without network access."""
import hashlib
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('install_mc', ROOT / 'hack/install-mc.py')
mc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mc)


class InstallMCTests(unittest.TestCase):
    def install(self, responses, expected=b'verified binary', existing=b'old binary'):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'mc'
            target.write_bytes(existing)
            with patch.object(mc, 'SHA256', hashlib.sha256(expected).hexdigest()), \
                 patch.object(mc.urllib.request, 'urlopen', side_effect=responses) as request:
                try:
                    mc.install(target)
                except Exception as error:
                    return error, target.read_bytes(), request.call_count
                return None, target.read_bytes(), request.call_count

    def test_primary_download_is_verified(self):
        error, content, count = self.install([io.BytesIO(b'verified binary')])
        self.assertIsNone(error)
        self.assertEqual(content, b'verified binary')
        self.assertEqual(count, 1)

    def test_504_uses_official_api_once(self):
        metadata = ('{"assets":[{"name":"' + mc.ASSET + '","id":123}]}').encode()
        error, content, count = self.install([
            HTTPError(mc.URL, 504, 'Gateway Timeout', None, None),
            io.BytesIO(metadata), io.BytesIO(b'verified binary')])
        self.assertIsNone(error)
        self.assertEqual(content, b'verified binary')
        self.assertEqual(count, 3)

    def test_corrupt_download_keeps_existing_tool_and_does_not_retry(self):
        error, content, count = self.install([io.BytesIO(b'corrupt binary')])
        self.assertIn('SHA256', str(error))
        self.assertEqual(content, b'old binary')
        self.assertEqual(count, 1)

    def test_api_failure_is_bounded_and_preserves_existing_tool(self):
        error, content, count = self.install([
            HTTPError(mc.URL, 504, 'Gateway Timeout', None, None),
            HTTPError(mc.URL, 504, 'Gateway Timeout', None, None)])
        self.assertIsNotNone(error)
        self.assertEqual(content, b'old binary')
        self.assertEqual(count, 2)


if __name__ == '__main__':
    unittest.main()
