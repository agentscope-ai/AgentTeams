"""Shared Matrix markdown → HTML formatting (Phase 12 Q12.5)."""

from __future__ import annotations

import html
import logging

logger = logging.getLogger(__name__)


def render_with_markdown_it(text: str) -> str | None:
    """Render *text* with markdown-it-py, or return ``None`` when unavailable."""
    try:
        from markdown_it import MarkdownIt
    except ImportError:
        return None

    md = MarkdownIt(
        "commonmark",
        {
            "html": False,
            "linkify": True,
            "breaks": True,
            "typographer": False,
        },
    )
    md.enable("strikethrough")
    md.enable("table")

    try:
        from linkify_it import LinkifyIt

        md.linkify = LinkifyIt()
    except ImportError:
        logger.debug(
            "linkify-it-py not installed; bare URLs may not be linkified",
        )

    return md.render(text).rstrip("\n")


def md_to_html_simple(text: str) -> str:
    """Escape plain text and preserve line breaks for Matrix ``formatted_body``."""
    return html.escape(text).replace("\n", "<br>\n")


def md_to_html(text: str) -> str:
    """Convert Markdown text to HTML for Matrix ``formatted_body``."""
    rendered = render_with_markdown_it(text)
    if rendered is not None:
        return rendered
    logger.warning(
        "markdown-it-py not installed; formatted_body will be plain text",
    )
    return md_to_html_simple(text)


def edit_fallback_html(text: str) -> str:
    """Wrap escaped text for Matrix edit fallbacks that avoid nested block HTML."""
    escaped = html.escape(text).replace("\n", "<br>\n")
    return f"<p>* {escaped}</p>"
