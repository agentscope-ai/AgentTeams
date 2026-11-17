"""Recover one page of each outstanding Matrix history gap per sync round."""
import asyncio
from itertools import zip_longest

from task_recovery import RecoveryConflict


async def backfill_round(matrix, fetch_page, prepare_inputs):
    """Use native decoding and policy with the Controller's pagination bounds.

    fetch_page(room, from_token) returns (events, next_token) in reverse
    chronological server order. prepare_inputs(room, events, until) classifies
    those events for persistence without executing them. Cancellation propagates.
    """
    snapshot = await asyncio.to_thread(matrix.load)
    start = snapshot["state"]["active_start"]
    if (start["instance_id"] != snapshot["instance_id"] or start["runtime"] != matrix.runtime
            or snapshot["runtime"] != matrix.runtime or start["process_id"] != matrix.recovery.process):
        raise RecoveryConflict("Matrix history requires the registered Worker start")
    batches = [[(room_id, gap_id) for gap_id in room.get("gaps", ())]
               for room_id, room in snapshot["state"]["rooms"].items() if room.get("gaps")]
    failures = {}
    for group in zip_longest(*batches):
        for candidate in group:
            if candidate is None:
                continue
            room_id, gap_id = candidate
            try:
                room = snapshot["state"]["rooms"].get(room_id, {})
                if gap_id not in room.get("gaps", ()):
                    continue
                gap = room.get("backfill", {}).get(gap_id)
                if not gap:
                    raise RecoveryConflict("Legacy Matrix gap has no verified pagination bounds")
                if gap.get("boundary"):
                    continue
                token = gap["from_token"]
                events, next_token = await fetch_page(room_id, token)
                if next_token is not None and not isinstance(next_token, str):
                    raise ValueError("Invalid Matrix history end token")
                if next_token and next_token in {token, gap_id}:
                    raise RecoveryConflict("Matrix history pagination repeated its token")
                inputs = await prepare_inputs(room_id, events, gap["until"])
                page = {"room_id": room_id, "gap_id": gap_id, "from_token": token,
                        "next_token": next_token or "", "inputs": inputs}
                snapshot = await asyncio.to_thread(matrix.backfill, snapshot, page)
            except Exception as error:
                failures.setdefault(room_id, error)
                # A stale ETag in one room must not stop subsequent rooms.
                # Recheck actual identity before making more native requests.
                snapshot = await asyncio.to_thread(matrix.load)
                if (snapshot["state"]["active_start"] != start or snapshot["instance_id"] != start["instance_id"]
                        or snapshot["runtime"] != matrix.runtime):
                    raise RecoveryConflict("Worker changed during Matrix history recovery") from error
    return failures
