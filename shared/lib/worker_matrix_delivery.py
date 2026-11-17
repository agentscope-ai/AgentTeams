"""Bind a saved Matrix reply to its first sending device before sending any bytes."""
import hashlib

from task_recovery import RecoveryConflict


async def publish_saved(checkpoint, text, whoami, bind, find, send):
    """Callbacks use the same native account and room for this saved result.

    find must verify sender, room, transaction marker and saved content. Absence
    or unreadable history cannot authorize sending on a different device.
    """
    if checkpoint.get("delivery") != "pending" or not checkpoint.get("result") or not checkpoint.get("reply_txn"):
        raise RecoveryConflict("A pending, verified reply is required")
    body = text.encode()
    result = checkpoint["result"]
    if len(body) != result["size"] or hashlib.sha256(body).hexdigest() != result["sha256"]:
        raise RecoveryConflict("Matrix reply differs from the saved result bytes")
    sender, device = await whoami()
    if not sender or not device:
        raise RecoveryConflict("Matrix delivery requires a known account and device")
    txn = checkpoint["reply_txn"]
    scope = {"state": "sending", "user_id": sender, "device_id": device}
    attempt = checkpoint.get("reply_attempt")
    if attempt == {"state": "ready"}:
        await bind({**checkpoint, "version": checkpoint["version"] + 1, "reply_attempt": scope})
        attempt = scope
    if attempt == scope:
        event_id = await send(txn)
    else:
        if attempt and attempt.get("user_id") != sender:
            raise RecoveryConflict("Saved reply belongs to a different Matrix account")
        event_id = await find(txn, text, sender)
    if not isinstance(event_id, str) or not event_id.startswith("$") or len(event_id) < 2:
        raise RecoveryConflict("Previous Matrix delivery needs verification before retry on this device")
    return event_id
