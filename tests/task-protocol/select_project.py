"""Locate the project using the frozen case policy and downloaded artifacts."""
import hashlib
from pathlib import Path


def select_project(case, nodes, delivered, *, planning_policy=None):
    delivered = Path(delivered)
    if planning_policy == "leader_decides":
        accepted_ids = {node["task_id"] for node in nodes}
        entries = [entry for entry in delivered.glob("*/**/app.py")
                   if entry.relative_to(delivered).parts[0] in accepted_ids]
        if len(entries) != 1:
            raise ValueError("accepted deliverables must identify one project entry")
        primary = entries[0]
        if not primary.resolve().is_relative_to(delivered.resolve()):
            raise ValueError("invalid delivered project entry")
        return primary.parent, {"ownerTaskId": primary.relative_to(delivered).parts[0],
                                "root": str(primary.parent),
                                "selection": "single project entry in accepted delivered artifacts"}

    if case == "simple-csv":
        if len(nodes) != 1:
            raise ValueError("simple task requires one implementation owner")
        owner = nodes[0]["task_id"]
    elif case == "complex-settlement":
        roots = [n for n in nodes if not n.get("depends_on")]
        joins = [n for n in nodes if set(n.get("depends_on", [])) == {r["task_id"] for r in roots}]
        if len(nodes) != 4 or len(roots) != 2 or len(joins) != 1:
            raise ValueError("complex task implementation owner is not established by its required DAG")
        owner = joins[0]["task_id"]
    else:
        raise ValueError("unknown certification case")
    owner_directory = delivered / owner
    if not owner_directory.resolve().is_relative_to(delivered.resolve()):
        raise ValueError("invalid implementation owner")
    primary = list(owner_directory.rglob("app.py"))
    if len(primary) != 1:
        raise ValueError("declared implementation owner must deliver exactly one project entry")
    digest = hashlib.sha256(primary[0].read_bytes()).hexdigest()
    copies = []
    for entry in sorted(delivered.glob("*/**/app.py")):
        if entry == primary[0]:
            continue
        copies.append({"path": str(entry.relative_to(delivered)),
                       "matchesPrimary": hashlib.sha256(entry.read_bytes()).hexdigest() == digest})
    return primary[0].parent, {"ownerTaskId": owner, "root": str(primary[0].parent),
                              "selection": "implementation owner from the required task DAG; no fallback, cross-task assembly or artifact edits",
                              "otherDeliveredAppEntries": copies}
