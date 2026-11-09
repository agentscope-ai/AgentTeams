# identifier canonicalization shared by project / room / lifecycle
# fragments (W1-a2: real module)
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


def _canonical_account(value: Any) -> str:
    """Identity comparison key for account identifiers (#1467 round 8).

    The ONE canonicalization shared by the approval write path (the
    server-written ``resolvedActor`` marker) and the r1 self-approval
    comparison, so both paths reduce an identity the same way: a
    Matrix MXID (@localpart:server) reduces to its localpart, and the
    result is casefolded (the localpart is case-insensitive per the
    Matrix spec) — so ``@worker:review.test`` and the runtimeName
    ``worker`` of the same member are the same identity. The server
    part is dropped on purpose: a runtime member identity is a bare
    name, never an MXID, and the localpart is the identity both sides
    can spell. A bare account is compared casefolded as-is. Empty
    input stays empty: no identity is never an identity."""
    text = str(value or "").strip()
    if text.startswith("@") and ":" in text:
        text = text[1:].partition(":")[0]
    return text.casefold()


