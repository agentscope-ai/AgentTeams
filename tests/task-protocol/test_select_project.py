import tempfile
from pathlib import Path
import unittest

from select_project import select_project


class ProjectSelectionTest(unittest.TestCase):
    nodes = [{"task_id": "A", "depends_on": []}, {"task_id": "B", "depends_on": []},
             {"task_id": "C", "depends_on": ["A", "B"]}, {"task_id": "D", "depends_on": ["C"]}]

    def write(self, root, name, body):
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(body)

    def test_qa_copy_does_not_make_the_primary_ambiguous(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for owner in ["C", "D"]:
                self.write(root, owner + "/app.py", "same implementation")
            project, evidence = select_project("complex-settlement", self.nodes, root)
            self.assertEqual(project, root / "C")
            self.assertTrue(evidence["otherDeliveredAppEntries"][0]["matchesPrimary"])

    def test_favourable_qa_copy_cannot_replace_the_required_implementation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write(root, "C/app.py", "broken implementation")
            self.write(root, "D/app.py", "fixed implementation")
            project, evidence = select_project("complex-settlement", self.nodes, root)
            self.assertEqual((project / "app.py").read_text(), "broken implementation")
            self.assertFalse(evidence["otherDeliveredAppEntries"][0]["matchesPrimary"])

    def test_missing_primary_does_not_fall_back_to_qa(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write(root, "D/app.py", "copy")
            with self.assertRaises(ValueError):
                select_project("complex-settlement", self.nodes, root)

    def test_ambiguity_inside_the_primary_remains_an_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ["C/app.py", "C/other/app.py"]:
                self.write(root, name, "implementation")
            with self.assertRaises(ValueError):
                select_project("complex-settlement", self.nodes, root)

    def test_simple_uses_its_declared_execution_owner(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write(root, "execution/app.py", "implementation")
            project, _ = select_project("simple-csv", [{"task_id": "execution"}], root)
            self.assertEqual(project, root / "execution")

    def test_leader_defined_plan_selects_the_unique_delivered_project(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write(root, "implementation/project/app.py", "implementation")
            self.write(root, "verification/check.py", "verification")
            nodes = [{"task_id": "implementation"}, {"task_id": "verification"}]
            for case in ("simple-csv", "complex-settlement"):
                with self.subTest(case=case):
                    project, evidence = select_project(case, nodes, root, planning_policy="leader_decides")
                    self.assertEqual(project, root / "implementation/project")
                    self.assertEqual(evidence["ownerTaskId"], "implementation")
            self.write(root, "verification/app.py", "different result")
            with self.assertRaises(ValueError):
                select_project("simple-csv", nodes, root, planning_policy="leader_decides")

    def test_leader_defined_plan_selects_integration_over_its_input_projects(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write(root, "draft-a/app.py", "working copy")
            self.write(root, "draft-b/app.py", "alternate working copy")
            self.write(root, "integration/app.py", "final delivery")
            nodes = [
                {"task_id": "draft-a"},
                {"task_id": "draft-b"},
                {"task_id": "prep", "depends_on": ["draft-b"]},
                {"task_id": "integration", "depends_on": ["draft-a", "prep"]},
                {"task_id": "review", "depends_on": ["integration"]},
            ]
            project, evidence = select_project("complex-settlement", nodes, root, planning_policy="leader_decides")
            self.assertEqual(project, root / "integration")
            self.assertEqual(evidence["ownerTaskId"], "integration")


if __name__ == "__main__":
    unittest.main()
