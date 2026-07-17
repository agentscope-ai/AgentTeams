from agentteams_matrix_format import edit_fallback_html, md_to_html


def test_md_to_html_renders_code_fence() -> None:
    html_body = md_to_html('Result:\n\n```json\n{"ok": true}\n```')
    assert "<pre><code" in html_body
    assert "ok" in html_body


def test_edit_fallback_html_wraps_without_pre_blocks() -> None:
    text = 'Result:\n\n```json\n{"ok": true}\n```'
    html_body = edit_fallback_html(text)
    assert html_body.startswith("<p>* ")
    assert "<pre" not in html_body
