"""Matrix markdown → HTML formatting (MCP-only, Phase 5 P5.6)."""

from __future__ import annotations

import html
import re

from agentteams_matrix_format import render_with_markdown_it


def render_inline_matrix_html(text: str) -> str:
    parts = re.split(r"(`[^`\n]+`)", text)
    rendered: list[str] = []
    for part in parts:
        if len(part) >= 2 and part.startswith("`") and part.endswith("`"):
            rendered.append(f"<code>{html.escape(part[1:-1])}</code>")
            continue
        escaped = html.escape(part)
        escaped = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", escaped)
        rendered.append(escaped)
    return "".join(rendered)


def table_cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def is_table_separator(line: str) -> bool:
    cells = table_cells(line)
    return bool(cells) and all(re.fullmatch(r":?-{3,}:?", cell or "") for cell in cells)


def render_fallback_table(lines: list[str]) -> str:
    header = table_cells(lines[0])
    rows = [table_cells(line) for line in lines[2:]]
    parts = ["<table>", "<thead><tr>"]
    parts.extend(f"<th>{render_inline_matrix_html(cell)}</th>" for cell in header)
    parts.append("</tr></thead>")
    if rows:
        parts.append("<tbody>")
        for row in rows:
            parts.append("<tr>")
            parts.extend(f"<td>{render_inline_matrix_html(cell)}</td>" for cell in row)
            parts.append("</tr>")
        parts.append("</tbody>")
    parts.append("</table>")
    return "".join(parts)


def md_to_html(text: str) -> str:
    rendered = render_with_markdown_it(text)
    if rendered is not None:
        return rendered

    lines = (text or "").splitlines()
    if not lines:
        return ""

    blocks: list[str] = []
    in_code_block = False
    code_lines: list[str] = []
    index = 0

    while index < len(lines):
        line = lines[index]
        if line.strip().startswith("```"):
            if in_code_block:
                code = html.escape("\n".join(code_lines))
                blocks.append(f"<pre><code>{code}</code></pre>")
                code_lines = []
                in_code_block = False
            else:
                in_code_block = True
                code_lines = []
            index += 1
            continue
        if in_code_block:
            code_lines.append(line)
            index += 1
            continue

        if index + 1 < len(lines) and "|" in line and is_table_separator(lines[index + 1]):
            table_lines = [line, lines[index + 1]]
            index += 2
            while index < len(lines) and "|" in lines[index] and lines[index].strip():
                table_lines.append(lines[index])
                index += 1
            blocks.append(render_fallback_table(table_lines))
            continue

        heading = re.match(r"^(#{1,6})\s+(.+?)\s*$", line)
        if heading:
            level = len(heading.group(1))
            blocks.append(f"<h{level}>{render_inline_matrix_html(heading.group(2))}</h{level}>")
            index += 1
            continue

        if re.match(r"^\s*[-*]\s+\S", line):
            items: list[str] = []
            while index < len(lines):
                item = re.match(r"^\s*[-*]\s+(.+?)\s*$", lines[index])
                if not item:
                    break
                items.append(item.group(1))
                index += 1
            blocks.append(
                "<ul>"
                + "".join(f"<li>{render_inline_matrix_html(item)}</li>" for item in items)
                + "</ul>"
            )
            continue

        if line.strip():
            blocks.append(render_inline_matrix_html(line))
        else:
            blocks.append("")
        index += 1

    if in_code_block:
        code = html.escape("\n".join(code_lines))
        blocks.append(f"<pre><code>{code}</code></pre>")

    return "<br>\n".join(blocks)
