"""Workflow YAML syntax validation with PyYAML (no act/ruby/node).

Parses every workflow under ``.github/workflows/`` with PyYAML and checks
the structure each job must satisfy. This keeps the CI-syntax gate
reproducible on a plain Python runner: a syntax error in any workflow
file, or a job missing ``runs-on``/``steps``, turns the suite red.
"""
from __future__ import annotations

from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = REPO_ROOT / ".github" / "workflows"


def _workflow_files():
    return sorted(WORKFLOWS.glob("*.yml")) + sorted(
        WORKFLOWS.glob("*.yaml")
    )


WORKFLOW_NAMES = [path.name for path in _workflow_files()]


def _load(name: str) -> dict:
    doc = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    assert isinstance(doc, dict), f"{name}: top level must be a mapping"
    return doc


def test_pyyaml_6_is_available():
    # The gate is defined against the PyYAML 6.0 line; an older major
    # would silently accept or reject different syntax.
    assert tuple(int(p) for p in yaml.__version__.split(".")[:2]) >= (6, 0)


def test_workflow_files_exist():
    assert WORKFLOW_NAMES, "no workflow files found under .github/workflows"


@pytest.mark.parametrize("name", WORKFLOW_NAMES)
def test_workflow_parses(name: str) -> None:
    doc = _load(name)
    assert doc.get("name"), f"{name}: must declare a workflow name"
    jobs = doc.get("jobs")
    assert isinstance(jobs, dict) and jobs, (
        f"{name}: must define a non-empty 'jobs' mapping"
    )


@pytest.mark.parametrize("name", WORKFLOW_NAMES)
def test_workflow_jobs_are_well_formed(name: str) -> None:
    jobs = _load(name)["jobs"]
    for job_id, job in jobs.items():
        assert isinstance(job, dict), f"{name}/{job_id}: job must be a mapping"
        if "uses" in job:
            # A reusable-workflow call has no runs-on by design.
            assert str(job["uses"]).endswith(
                ".yml"
            ), f"{name}/{job_id}: reusable job must use a local workflow"
            continue
        assert job.get("runs-on"), f"{name}/{job_id}: missing runs-on"
        steps = job.get("steps")
        assert isinstance(steps, list) and steps, (
            f"{name}/{job_id}: must define steps"
        )
        for index, step in enumerate(steps):
            assert isinstance(step, dict), (
                f"{name}/{job_id}/step {index}: must be a mapping"
            )
            assert "run" in step or "uses" in step, (
                f"{name}/{job_id}/step {index}: needs a run or uses key"
            )
