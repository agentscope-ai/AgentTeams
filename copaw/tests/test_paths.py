"""Tests for copaw_worker.paths."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from copaw_worker.paths import runtime_root


def test_runtime_root_from_copaw_working_dir(tmp_path, monkeypatch):
    worker_root = tmp_path / "alice"
    copaw_dir = worker_root / ".copaw"
    copaw_dir.mkdir(parents=True)
    monkeypatch.setenv("COPAW_WORKING_DIR", str(copaw_dir))
    monkeypatch.chdir(tmp_path)

    assert runtime_root() == worker_root.resolve()


def test_runtime_root_from_workspace_default(tmp_path, monkeypatch):
    worker_root = tmp_path / "alice"
    workspace = worker_root / ".copaw" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    monkeypatch.setenv("COPAW_WORKING_DIR", str(workspace))
    monkeypatch.chdir(tmp_path)

    assert runtime_root() == worker_root.resolve()


def test_runtime_root_from_cwd_workspace_default(tmp_path, monkeypatch):
    worker_root = tmp_path / "alice"
    workspace = worker_root / ".copaw" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)
    monkeypatch.chdir(workspace)

    assert runtime_root() == worker_root.resolve()
