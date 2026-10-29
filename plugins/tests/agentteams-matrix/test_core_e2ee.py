"""E2EE maintenance core tests (P9, PR1 M4c).

Pins the moved behaviour: the crypto store path layout, the olm
probe, and the post-sync maintenance pass — the call order and
arguments (``keys_claim`` receiving ``get_users_for_key_claiming()``),
the unconditional to-device send, the not-ready guards, and the
warning-swallowing error handling.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import List, Optional, Tuple

import pytest

from core.e2ee import E2EEMaintainer, e2ee_store_path, probe_olm


def _run(coro):
    """Run an async scenario to completion (no pytest-asyncio)."""
    return asyncio.run(coro)


class FakeClient:
    """matrix-nio ``AsyncClient`` stand-in recording the maintenance calls."""

    def __init__(
        self,
        upload: bool = False,
        query: bool = False,
        claim: bool = False,
        users: Tuple[str, ...] = ("u1", "u2"),
        olm: bool = True,
        upload_exc: Optional[Exception] = None,
    ) -> None:
        self.should_upload_keys = upload
        self.should_query_keys = query
        self.should_claim_keys = claim
        self._users = list(users)
        self.olm = object() if olm else None
        self._upload_exc = upload_exc
        self.calls: List[Tuple] = []

    def get_users_for_key_claiming(self):
        return list(self._users)

    async def keys_upload(self):
        self.calls.append(("upload",))
        if self._upload_exc is not None:
            raise self._upload_exc

    async def keys_query(self):
        self.calls.append(("query",))

    async def keys_claim(self, users):
        self.calls.append(("claim", tuple(users)))

    async def send_to_device_messages(self):
        self.calls.append(("to-device",))


def test_e2ee_store_path_layout():
    assert e2ee_store_path(Path("/work")) == Path(
        "/work/matrix_crypto_store"
    )


def test_probe_olm_absent():
    if probe_olm():
        pytest.skip("olm is installed in this environment")
    assert probe_olm() is False


def test_run_once_all_flags_order_and_args():
    client = FakeClient(upload=True, query=True, claim=True)

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == [
        ("upload",),
        ("query",),
        ("claim", ("u1", "u2")),
        ("to-device",),
    ]


def test_run_once_all_flags_false_skips_key_operations():
    """With all ``should_*`` false the three key operations are skipped;
    the to-device send is unconditional in the legacy behaviour."""
    client = FakeClient()

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == [("to-device",)]


def test_run_once_without_olm_zero_calls():
    client = FakeClient(upload=True, query=True, claim=True, olm=False)

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == []


def test_run_once_none_client_zero_calls():
    # Must not raise.
    _run(E2EEMaintainer(None).run_once())


def test_run_once_error_swallowed_with_warning_and_stops(
    caplog,
):
    client = FakeClient(
        upload=True,
        query=True,
        claim=True,
        upload_exc=RuntimeError("boom"),
    )

    with caplog.at_level("WARNING", logger="agentteams.matrix.core.e2ee"):
        # Must not raise.
        _run(E2EEMaintainer(client).run_once())

    # The whole block is one try (legacy behaviour): nothing after the
    # failing keys_upload runs.
    assert client.calls == [("upload",)]
    assert any(
        "MatrixChannel: E2EE maintenance error: boom" in record.getMessage()
        for record in caplog.records
    )
