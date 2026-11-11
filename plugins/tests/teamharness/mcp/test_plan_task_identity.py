"""Planning errors should identify a missing task key rather than suggest bad characters."""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness" / "mcp"))
import server


def test_plan_with_generic_id_explains_the_expected_task_key():
    with pytest.raises(ValueError, match="taskId or task_id"):
        server._normalize_task({"id": "valid-task-01", "title": "Analyze"})


def test_plan_still_rejects_unsafe_task_identity():
    with pytest.raises(ValueError, match="taskId must be a safe id"):
        server._normalize_task({"taskId": "../unsafe"})
