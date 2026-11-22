"""Read registered public identities once for native Task context binding."""
import re
from urllib.parse import quote


def _rows(payload, key):
    if not isinstance(payload, dict) or not isinstance(payload.get(key), list):
        raise ValueError("public " + key + " roster unavailable")
    rows = payload[key]
    if any(not isinstance(row, dict) or not isinstance(row.get("name"), str) or not row["name"] for row in rows):
        raise ValueError("public " + key + " roster has invalid registered identities")
    if len({row["name"] for row in rows}) != len(rows):
        raise ValueError("public " + key + " roster has duplicate registered identities")
    return rows


def _matrix_id(member):
    value = member.get("matrixUserID") or member.get("matrixUserId") or None
    if value is not None and (not isinstance(value, str) or not value.startswith("@")):
        raise ValueError("registered Matrix identity is invalid")
    return value


def roster(role, get_json, team=None):
    """Project public API data; native bindings, not model args, supply scope.

    Missing Matrix identities remain explicit for unready registered resources.
    API/shape errors propagate instead of pretending the roster is empty.
    """
    role = "leader" if role == "team_leader" else role
    if role == "manager":
        if team is not None:
            raise ValueError("Manager roster does not accept a Team override")
        teams = _rows(get_json("teams"), "teams")
        workers = _rows(get_json("workers"), "workers")
        identities = {worker["name"]: _matrix_id(worker) for worker in workers}
        return {"role": role, "teams": [
            {"team": item["name"], "description": item.get("description"),
             "leader": item.get("leaderName") or None,
             "matrix_id": identities.get(item.get("leaderName")), "phase": item.get("phase")}
            for item in teams]}
    if role != "leader" or not isinstance(team, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", team):
        raise ValueError("registered Leader Team scope required")
    record = get_json("teams/" + quote(team, safe=""))
    if not isinstance(record, dict) or record.get("name") != team:
        raise ValueError("Leader roster does not match its registered Team")
    if ("members" not in record and "workerMembers" in record
            and (record["workerMembers"] is None or record["workerMembers"] == [])):
        record = {**record, "members": []}
    members = _rows(record, "members")
    return {"role": role, "team": team, "members": [
        {"name": member["name"], "role": member.get("role"), "matrix_id": _matrix_id(member),
         **{key: member[key] for key in ("phase", "state") if key in member}}
        for member in members]}
