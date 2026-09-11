#!/usr/bin/env python3
"""Deterministic tests for external-task-trigger.py.

Run: python3 -m pytest tests/test_external_task_trigger.py -v
Or:  python3 -m unittest tests.test_external_task_trigger -v
Or:  python3 tests/test_external_task_trigger.py
"""

import json
import os
import sys
import tempfile
import unittest
from io import StringIO
from pathlib import Path
from unittest.mock import patch


SCRIPT_DIR = Path(__file__).resolve().parent.parent / "scripts"
sys.path.insert(0, str(SCRIPT_DIR))

import importlib.util
spec = importlib.util.spec_from_file_location(
    "external_task_trigger",
    SCRIPT_DIR / "external-task-trigger.py",
)
ext = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ext)


class TestTaskValidation(unittest.TestCase):
    def test_valid_task(self):
        task = {
            "team": "demo-team",
            "skill": "analyze",
            "params": {"request": "test"},
        }
        result = ext.validate_task(task)
        self.assertTrue(result)

    def test_missing_team(self):
        task = {"skill": "analyze", "params": {}}
        with self.assertRaises(ValueError) as ctx:
            ext.validate_task(task)
        self.assertIn("team", str(ctx.exception))

    def test_missing_skill(self):
        task = {"team": "demo", "params": {}}
        with self.assertRaises(ValueError) as ctx:
            ext.validate_task(task)
        self.assertIn("skill", str(ctx.exception))

    def test_missing_params(self):
        task = {"team": "demo", "skill": "analyze"}
        with self.assertRaises(ValueError) as ctx:
            ext.validate_task(task)
        self.assertIn("params", str(ctx.exception))

    def test_params_not_dict(self):
        task = {"team": "demo", "skill": "analyze", "params": "not-a-dict"}
        with self.assertRaises(ValueError) as ctx:
            ext.validate_task(task)
        self.assertIn("params", str(ctx.exception).lower())

    def test_multiple_missing(self):
        task = {}
        with self.assertRaises(ValueError) as ctx:
            ext.validate_task(task)
        msg = str(ctx.exception)
        self.assertIn("team", msg)
        self.assertIn("skill", msg)
        self.assertIn("params", msg)


class TestIDGeneration(unittest.TestCase):
    def test_task_id_format(self):
        tid = ext.generate_task_id()
        self.assertTrue(tid.startswith("task-"))
        self.assertEqual(len(tid), len("task-") + 12)

    def test_trace_id_format(self):
        tid = ext.generate_trace_id()
        self.assertTrue(tid.startswith("trace-"))
        self.assertEqual(len(tid), len("trace-") + 12)

    def test_ids_are_unique(self):
        ids = {ext.generate_task_id() for _ in range(100)}
        self.assertEqual(len(ids), 100)


class TestManagerMessage(unittest.TestCase):
    def test_message_contains_ids(self):
        task = {
            "team": "demo-team",
            "skill": "analyze_request",
            "params": {"request": "test"},
            "metadata": {"external_job_id": "crm-001"},
        }
        msg = ext.build_manager_message(task, "task-abc", "trace-xyz")
        self.assertIn("task-abc", msg)
        self.assertIn("trace-xyz", msg)
        self.assertIn("crm-001", msg)
        self.assertIn("demo-team", msg)
        self.assertIn("analyze_request", msg)
        self.assertIn("EXTERNAL_TASK", msg)

    def test_message_contains_params(self):
        task = {
            "team": "t1",
            "skill": "s1",
            "params": {"key": "value", "num": 42},
            "metadata": {"external_job_id": "ext-1"},
        }
        msg = ext.build_manager_message(task, "task-1", "trace-1")
        self.assertIn("key", msg)
        self.assertIn("value", msg)
        self.assertIn("42", msg)

    def test_message_without_optional_metadata(self):
        task = {"team": "t1", "skill": "s1", "params": {"x": 1}}
        msg = ext.build_manager_message(task, "task-1", "trace-1")
        self.assertIn("external_job_id: unknown", msg)

    def test_message_with_context(self):
        task = {
            "team": "t1",
            "skill": "s1",
            "params": {"x": 1},
            "metadata": {
                "external_job_id": "ext-1",
                "context": "Customer is urgent",
            },
        }
        msg = ext.build_manager_message(task, "task-1", "trace-1")
        self.assertIn("Customer is urgent", msg)


class TestTaskLoading(unittest.TestCase):
    def test_load_from_file(self):
        task_data = {
            "team": "test",
            "skill": "test",
            "params": {"k": "v"},
        }
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task_data, f)
            f.flush()
            path = f.name

        try:
            loaded = ext.load_task(input_file=path)
            self.assertEqual(loaded, task_data)
        finally:
            os.unlink(path)

    def test_load_nonexistent_file(self):
        with self.assertRaises(FileNotFoundError):
            ext.load_task(input_file="/nonexistent/path.json")

    def test_load_empty_stdin_raises(self):
        with patch("sys.stdin", StringIO("")):
            with self.assertRaises(ValueError):
                ext.load_task(use_stdin=True)

    def test_no_input_raises(self):
        with self.assertRaises(ValueError):
            ext.load_task()


class TestDryRun(unittest.TestCase):
    def test_dry_run_returns_completed(self):
        task = {
            "team": "demo-team",
            "skill": "analyze",
            "params": {"request": "test"},
            "metadata": {"external_job_id": "crm-001"},
        }
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertEqual(result["status"], "completed")
            self.assertIn("task_id", result)
            self.assertIn("trace_id", result)
            self.assertEqual(result["external_job_id"], "crm-001")
            self.assertIn("DRY RUN", result["result"])
        finally:
            os.unlink(path)

    def test_dry_run_missing_field_returns_error(self):
        task = {"team": "demo"}
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertEqual(result["status"], "error")
            self.assertIn("error", result)
        finally:
            os.unlink(path)

    def test_dry_run_no_matrix_needed(self):
        task = {
            "team": "t",
            "skill": "s",
            "params": {},
        }
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertEqual(result["status"], "completed")
            self.assertIsNotNone(result["task_id"])
            self.assertIsNotNone(result["trace_id"])
        finally:
            os.unlink(path)


class TestErrorHandling(unittest.TestCase):
    def test_invalid_json(self):
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            f.write("not valid json {{{")
            f.flush()
            path = f.name

        try:
            with self.assertRaises(json.JSONDecodeError):
                ext.load_task(input_file=path)
        finally:
            os.unlink(path)

    def test_missing_required_field_gives_useful_error(self):
        task = {"team": "only-team"}
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertEqual(result["status"], "error")
            self.assertIn("skill", result["error"].lower())
            self.assertIn("params", result["error"].lower())
        finally:
            os.unlink(path)


class TestTraceability(unittest.TestCase):
    def test_manager_message_contains_trace_ids(self):
        task = {
            "team": "t",
            "skill": "s",
            "params": {"x": 1},
            "metadata": {"external_job_id": "ext-42"},
        }
        msg = ext.build_manager_message(task, "task-aaa", "trace-bbb")
        self.assertIn("task_id: task-aaa", msg)
        self.assertIn("trace_id: trace-bbb", msg)

    def test_result_preserves_external_job_id(self):
        task = {
            "team": "t",
            "skill": "s",
            "params": {},
            "metadata": {"external_job_id": "MY-JOB-99"},
        }
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertEqual(result["external_job_id"], "MY-JOB-99")
        finally:
            os.unlink(path)

    def test_result_has_submitted_at_timestamp(self):
        task = {
            "team": "t",
            "skill": "s",
            "params": {},
        }
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            json.dump(task, f)
            f.flush()
            path = f.name

        try:
            result = ext.run_task(task_file=path, dry_run=True)
            self.assertIn("submitted_at", result)
            self.assertIsNotNone(result["submitted_at"])
        finally:
            os.unlink(path)


if __name__ == "__main__":
    unittest.main()