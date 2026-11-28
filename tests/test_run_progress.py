"""Matrix previews stay bounded for large multilingual tool payloads."""
import json
import unittest

from run_progress import progress_text, progress_value


class RunProgressTests(unittest.TestCase):
    def test_large_multilingual_tool_updates_fit_a_matrix_event(self):
        block = {'type': 'tool_call', 'tool_name': 'write',
                 'arguments': progress_value({'content': '文件正文😀' * 20000}),
                 'result': progress_value({'output': '执行结果😀' * 20000})}
        content = {'body': 'write · succeeded', 'org.agentteams.run': {'blocks': [block]}}
        update = {**content, 'm.new_content': content, 'm.relates_to': {'event_id': '$original', 'rel_type': 'm.replace'}}
        self.assertLess(len(json.dumps(update).encode()), 65536)
        self.assertLess(len(json.dumps(update, ensure_ascii=False).encode()), 65536)
        self.assertTrue(block['arguments']['truncated'])

    def test_preview_redacts_credentials_and_preserves_small_values(self):
        original = {'file_path': 'report.md', 'api_key': 'private-key'}
        self.assertEqual(progress_value(original), {'file_path': 'report.md', 'api_key': '[REDACTED]'})
        self.assertEqual(original['api_key'], 'private-key')

    def test_thinking_preview_uses_bytes_and_keeps_valid_unicode(self):
        preview = progress_text('分析😀' * 20000)
        self.assertLessEqual(len(preview.encode()), 4096)
        self.assertNotIn('\ufffd', preview)
        self.assertTrue(preview.endswith('…'))
