"""Bounded previews for Matrix run blocks; full content belongs in Trace/files."""
import json

from runtime_observability import redact


def progress_text(value):
    data = redact(value).encode('utf-8')
    return data.decode('utf-8') if len(data) <= 4096 else data[:4093].decode('utf-8', errors='ignore') + '…'


def progress_value(value):
    text = redact(json.dumps(value, ensure_ascii=False, default=str))
    return json.loads(text) if len(text.encode('utf-8')) <= 4096 else {'preview': progress_text(text), 'truncated': True}
