"""Bind a saved Matrix reply to its first sending device before sending any bytes."""
import hashlib

from task_recovery import RecoveryConflict


def plan_saved(checkpoint, text, sender, device):
    """Return the native action and any checkpoint that must be persisted before sending."""
    if checkpoint.get("delivery") != "pending" or not checkpoint.get("result") or not checkpoint.get("reply_txn"):
        raise RecoveryConflict("A pending, verified reply is required")
    body = text.encode()
    result = checkpoint["result"]
    if len(body) != result["size"] or hashlib.sha256(body).hexdigest() != result["sha256"]:
        raise RecoveryConflict("Matrix reply differs from the saved result bytes")
    if not sender or not device:
        raise RecoveryConflict("Matrix delivery requires a known account and device")
    scope = {"state": "sending", "user_id": sender, "device_id": device}
    attempt = checkpoint.get("reply_attempt")
    if attempt == {"state": "ready"}:
        return "send", {**checkpoint, "version": checkpoint["version"] + 1, "reply_attempt": scope}
    if attempt == scope:
        return "send", None
    if attempt and attempt.get("user_id") != sender:
        raise RecoveryConflict("Saved reply belongs to a different Matrix account")
    return "find", None


async def publish_saved(checkpoint, text, whoami, bind, find, send):
    """Use the same native account and room to verify history or send this saved reply."""
    sender, device = await whoami()
    action, pending = plan_saved(checkpoint, text, sender, device)
    if pending is not None:
        await bind(pending)
    txn = checkpoint["reply_txn"]
    event_id = await send(txn) if action == "send" else await find(txn, text, sender)
    if not isinstance(event_id, str) or not event_id.startswith("$") or len(event_id) < 2:
        raise RecoveryConflict("Previous Matrix delivery needs verification before retry on this device")
    return event_id


def confirm_bound(execution, identity, runtime, checkpoint):
    """Verify device binding after the Controller stamps the committed checkpoint."""
    saved = dict(execution.get("checkpoint") or {})
    submitted = dict(checkpoint)
    saved.pop("saved_at", None)
    submitted.pop("saved_at", None)
    if (execution.get("runtime") != runtime or execution.get("state") != "active" or saved != submitted
            or any(not identity.get(k) or execution.get(k) != identity[k]
                   for k in ("instance_id", "process_id", "generation"))):
        raise RecoveryConflict("Matrix reply device was not bound to the active execution")
