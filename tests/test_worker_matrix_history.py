import asyncio
import copy
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_matrix_history import backfill_round


def room(*ids):
    return {"gaps": ids, "backfill": {key: {"from_token": key, "until": "$known"} for key in ids}}


def fixture(rooms):
    start = {"instance_id": "actual", "runtime": "hermes", "process_id": "boot"}
    state = {"active_start": start, "rooms": rooms}
    snapshot = {"instance_id": "actual", "runtime": "hermes", "state": state, "etag": 1}
    def save(previous, page):
        assert previous["etag"] == snapshot["etag"]
        gap = rooms[page["room_id"]]["backfill"][page["gap_id"]]
        assert gap["from_token"] == page["from_token"]
        gap["from_token"] = page["next_token"]
        snapshot["etag"] += 1
        return copy.deepcopy(snapshot)
    return SimpleNamespace(runtime="hermes", recovery=SimpleNamespace(process="boot"),
        load=Mock(side_effect=lambda: copy.deepcopy(snapshot)), backfill=Mock(side_effect=save)), snapshot


def test_each_gap_advances_one_page_per_round_in_server_order_and_resumes():
    other, main = "!other:test", "!main:test"
    matrix, snapshot = fixture({main: room("first", "second"), other: room("older")})
    cursor_requests = []
    async def fetch(room_id, token):
        cursor_requests.append((room_id, token))
        return ["$z", "$a"], token + "-next"
    prepare = AsyncMock(side_effect=lambda room_id, events, until: [{"event_id": event} for event in events])
    expected = [(main, "first"), (other, "older"), (main, "second")]
    assert asyncio.run(backfill_round(matrix, fetch, prepare)) == {}
    assert cursor_requests == expected
    assert matrix.backfill.call_args_list[0].args[1]["inputs"] == [{"event_id": "$z"}, {"event_id": "$a"}]
    assert asyncio.run(backfill_round(matrix, fetch, prepare)) == {}
    assert cursor_requests[3:] == [(r, t + "-next") for r, t in expected]
    assert snapshot["etag"] == 7


def test_inaccessible_boundary_and_decode_error_do_not_consume_or_starve_other_rooms():
    boundary, blocked, ready = "!boundary:test", "!blocked:test", "!ready:test"
    matrix, snapshot = fixture({boundary: room("end"), blocked: room("encrypted"), ready: room("ok")})
    snapshot["state"]["rooms"][boundary]["backfill"]["end"]["boundary"] = True
    async def prepare(room_id, events, until):
        if room_id == blocked:
            raise RuntimeError("decryption key missing")
        return events
    fetch = AsyncMock(return_value=([], None))
    failures = asyncio.run(backfill_round(matrix, fetch, prepare))
    assert set(failures) == {blocked}
    assert [call.args for call in fetch.await_args_list] == [(blocked, "encrypted"), (ready, "ok")]
    assert snapshot["state"]["rooms"][blocked]["backfill"]["encrypted"]["from_token"] == "encrypted"
    page = matrix.backfill.call_args.args[1]
    assert page["room_id"] == ready and page["next_token"] == "" and page["inputs"] == []


@pytest.mark.parametrize("end", ["same", "first", 7])
def test_invalid_or_cyclic_pages_never_commit(end):
    matrix, snapshot = fixture({"!room:test": room("first")})
    snapshot["state"]["rooms"]["!room:test"]["backfill"]["first"]["from_token"] = "same"
    prepare = AsyncMock()
    failures = asyncio.run(backfill_round(matrix, AsyncMock(return_value=([], end)), prepare))
    assert "!room:test" in failures
    matrix.backfill.assert_not_called()
    prepare.assert_not_awaited()


def test_conflicting_old_page_cannot_be_relabelled_with_the_next_etag():
    matrix, snapshot = fixture({"!conflict:test": room("older"), "!valid:test": room("valid")})
    save = matrix.backfill.side_effect
    def conflict_once(previous, page):
        if page["room_id"] == "!conflict:test":
            snapshot["etag"] += 1
            raise RecoveryConflict("competing page")
        return save(previous, page)
    matrix.backfill.side_effect = conflict_once
    fetch, prepare = AsyncMock(return_value=([], "next")), AsyncMock(return_value=[])
    failures = asyncio.run(backfill_round(matrix, fetch, prepare))
    assert set(failures) == {"!conflict:test"}
    assert matrix.backfill.call_count == 2
    assert matrix.backfill.call_args.args[0]["etag"] == 2
    assert snapshot["state"]["rooms"]["!conflict:test"]["backfill"]["older"]["from_token"] == "older"


def test_cancelled_pagination_or_replaced_worker_cannot_write():
    matrix, snapshot = fixture({"!room:test": room("first")})
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(backfill_round(matrix, AsyncMock(side_effect=asyncio.CancelledError), AsyncMock()))
    matrix.backfill.assert_not_called()
    snapshot["state"]["active_start"]["process_id"] = "other"
    fetch = AsyncMock()
    with pytest.raises(RecoveryConflict):
        asyncio.run(backfill_round(matrix, fetch, AsyncMock()))
    fetch.assert_not_awaited()
