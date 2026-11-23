"""Test the verifier with canned results, independent of any Agent implementation."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("business_oracle", Path(__file__).with_name("business_oracle.py"))
oracle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oracle)


class BusinessOracleTests(unittest.TestCase):
    def candidate(self, root, kind, mutation=""):
        root.mkdir()
        entries = {json.dumps(files, sort_keys=True): expected for _, files, expected in oracle.cases(kind)}
        (root / "answers.json").write_text(json.dumps(entries), encoding="utf-8")
        # This fixture replays exact generated answers; it does not implement CSV
        # processing and cannot demonstrate real business implementation success.
        script = '''
import argparse, json, pathlib, sys
p=argparse.ArgumentParser()
p.add_argument('--input'); p.add_argument('--transactions'); p.add_argument('--refunds'); p.add_argument('--output')
a=p.parse_args()
files={'input.csv':pathlib.Path(a.input or a.transactions).read_text(encoding='utf-8')}
if a.refunds: files['refunds.csv']=pathlib.Path(a.refunds).read_text(encoding='utf-8')
result=json.loads(pathlib.Path('answers.json').read_text()).get(json.dumps(files,sort_keys=True),{})
if result is None:
    INVALID
    sys.exit(2)
if a.refunds:
    import transactions, refunds
    values=transactions.load_transactions(a.transactions)
    if any(value['id']=='injected' for value in values):
        result=INJECTED
        pathlib.Path(a.output).write_text(json.dumps(result)); sys.exit(0)
MUTATION
pathlib.Path(a.output).write_text(json.dumps(result),encoding='utf-8')
'''.replace("    INVALID", "    pathlib.Path(a.output).write_text('{}')" if mutation == "invalid-output" else "    pass")
        script = script.replace("INJECTED", repr(oracle.expected_settlement(
            [("injected", "parser-source", 12345)], [("refund", "injected", 345)])))
        # The canned fixture's parser is not used for the ordinary answer table.
        script = script.replace("    values=transactions.load_transactions(a.transactions)",
                                "    values=transactions.load_transactions(a.transactions)" if mutation != "bypass-parser" else "    values=[{'id':'not-injected'}]")
        script = script.replace("MUTATION", "result['amount']='0.00'" if mutation == "wrong-money" else "pass")
        (root / "app.py").write_text(script)
        (root / "README.md").write_text("Canned verifier fixture, never a certification candidate.")
        (root / "test_sample.py").write_text("import unittest\nclass Check(unittest.TestCase):\n def test_ok(self): self.assertTrue(True)\n")
        for module, field in (("transactions", "category"), ("refunds", "transaction_id")):
            (root / (module + ".json")).write_text(json.dumps({text: expected for _, text, expected in oracle.module_cases(module)}))
            (root / (module + ".py")).write_text(
                "import json, pathlib\n"
                f"def load_{module}(path):\n"
                f" result=json.loads(pathlib.Path('{module}.json').read_text()).get(pathlib.Path(path).read_text())\n"
                " if result is None: raise ValueError('invalid fixture input')\n"
                " return result\n")
        return root

    def test_canned_results_pass_both_business_suites(self):
        with tempfile.TemporaryDirectory() as temporary:
            for kind in ("simple-csv", "complex-settlement"):
                result = oracle.run(self.candidate(Path(temporary) / kind, kind), kind)
                self.assertTrue(result["passed"], result)
                self.assertGreater(len(result["checks"]), 20)

    def test_wrong_amount_and_error_output_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            for mutation in ("wrong-money", "invalid-output"):
                result = oracle.run(self.candidate(Path(temporary) / mutation, "simple-csv", mutation), "simple-csv")
                self.assertFalse(result["passed"])
                self.assertTrue(any(not check["passed"] for check in result["checks"]))

    def test_symlink_tree_is_rejected_before_execution(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "candidate"
            root.mkdir()
            (root / "app.py").symlink_to("/etc/passwd")
            with self.assertRaisesRegex(ValueError, "symlink"):
                oracle.run(root, "simple-csv")

    def test_cli_that_ignores_public_parser_results_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            result = oracle.run(self.candidate(Path(temporary) / "candidate", "complex-settlement", "bypass-parser"), "complex-settlement")
            self.assertFalse(result["passed"])
            self.assertIn({"case": "cli-consumes-public-parsers", "passed": False}, result["checks"])

    def test_missing_tests_do_not_pass_unittest_discovery(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = self.candidate(Path(temporary) / "candidate", "simple-csv")
            (root / "test_sample.py").unlink()
            result = oracle.run(root, "simple-csv")
            self.assertFalse(result["passed"])
            self.assertEqual(result["checks"][-1], {"case": "delivered-unittest", "passed": False})


if __name__ == "__main__":
    unittest.main()
