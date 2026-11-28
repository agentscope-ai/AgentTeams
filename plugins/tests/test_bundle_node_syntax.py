"""The syntax gate checks actual ESM bytes and still rejects malformed payloads."""
import shutil
import pytest

from _bundle_lifecycle import node_check


pytestmark = pytest.mark.skipif(shutil.which('node') is None, reason='Node is not installed')


def test_installed_esm_syntax_is_checked(tmp_path):
    module = tmp_path / 'index.mjs'
    module.write_text('export default { register() {} };\n')
    node_check(module)


def test_malformed_esm_is_rejected(tmp_path):
    module = tmp_path / 'index.mjs'
    module.write_text('export default {\n')
    with pytest.raises(AssertionError, match='SyntaxError'):
        node_check(module)
