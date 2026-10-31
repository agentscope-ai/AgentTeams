# room/assignee canonicalization shared by project and room fragments (W1-a2: real module)
from __future__ import annotations

from typing import Any

def _canonical_room_id(value: Any) -> str:
    text = str(value or "").strip()
    if text.startswith("room:"):
        text = text[len("room:") :].strip()
    return text


def _canonical_assignee(value: Any) -> str:
    """Comparison key for assignee identifiers.

    Matrix user-id *localparts* are case-insensitive (the spec requires
    reaching ``@user:matrix.org`` as ``@USER:matrix.org``), but *server
    names* are case-sensitive (spec appendices §server-name: ``@user:matrix.org``
    is a different person from ``@user:MATRIX.ORG``). An assignee may be
    supplied as a full MXID or as a leading ``room:``-style alias.
    Normalize only the localpart so a re-delegation to the *same* worker
    (spelled with different localpart casing) compares equal, while a
    different server name remains a genuinely different identity.
    """
    text = str(value or "").strip()
    if text.startswith("room:"):
        text = text[len("room:") :].strip()
    if text.startswith("@") and ":" in text:
        localpart, _, server = text[1:].partition(":")
        return f"@{localpart.casefold()}:{server}"
    return text.casefold()


