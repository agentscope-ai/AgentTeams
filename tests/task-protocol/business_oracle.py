"""Independent business checks. Run delivered code only in a networkless container.

This verdict proves business behavior, not collaboration or original-room delivery.
Keep this file and its generated inputs outside every Agent workspace.
"""

from __future__ import annotations

import argparse
import csv
import io
import json
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import tempfile


def csv_text(header, rows):
    stream = io.StringIO(newline="")
    writer = csv.writer(stream, lineterminator="\n")
    writer.writerow(header)
    writer.writerows(rows)
    return stream.getvalue()


def money(cents):
    return ("-" if cents < 0 else "") + f"{abs(cents) // 100}.{abs(cents) % 100:02d}"


def expected_summary(rows):
    categories = sorted({category for _, category, _ in rows})
    return {"count": len(rows), "amount": money(sum(value for _, _, value in rows)),
            "categories": [{"category": category, "count": sum(c == category for _, c, _ in rows),
                            "amount": money(sum(v for _, c, v in rows if c == category))}
                           for category in categories]}


def expected_settlement(rows, refunds):
    by_id = {identity: (category, value) for identity, category, value in rows}
    matched = [(identity, transaction, value) for identity, transaction, value in refunds if transaction in by_id]
    categories = []
    for category in sorted({category for _, category, _ in rows}):
        gross = sum(value for _, c, value in rows if c == category)
        returned = sum(value for _, transaction, value in matched if by_id[transaction][0] == category)
        categories.append({"category": category, "transactions": sum(c == category for _, c, _ in rows),
                           "gross": money(gross), "refunds": money(returned), "net": money(gross - returned)})
    gross = sum(value for _, _, value in rows)
    returned = sum(value for _, _, value in matched)
    return {"transactions": len(rows), "gross": money(gross), "matchedRefunds": money(returned),
            "net": money(gross - returned), "categories": categories,
            "unmatchedRefunds": [{"id": identity, "transactionId": transaction, "amount": money(value)}
                                 for identity, transaction, value in sorted(refunds)
                                 if transaction not in by_id]}


def cases(kind):
    header = ["id", "category", "amount"]
    refund_header = ["id", "transaction_id", "amount"]
    rng = random.Random(129)
    rows = [(f"tx-{i}", ["A", "a", "中,文", " spaced ", 'quote"cat'][i % 5],
             rng.randrange(0, 100000)) for i in range(31)]
    # Beyond IEEE-754's exact integer range, with cent cancellation and -0.00.
    if kind == "simple-csv":
        rows += [("large", "precision", 900719925474099301),
                 ("cancel", "precision", -900719925474099300), ("negative", "A", -101)]
    else:
        rows += [("large", "precision", 900719925474099301)]
    refunds = [("refund-z", "tx-0", 1), ("refund-a", "tx-0", 2),
               ("unmatched-z", "missing", 900719925474099301), ("unmatched-a", "other", 0)]
    valid = [("empty", [], []), ("exact-and-unicode", rows, refunds),
             ("permuted", list(reversed(rows)), list(reversed(refunds))),
             ("zero", [("zero", "Z", 0)], [("r", "zero", 0)])]
    if kind == "complex-settlement":
        valid.append(("fully-refunded", [("x", "A", 123)], [("r1", "x", 120), ("r2", "x", 3)]))
    result = []
    for name, transactions, returns in valid:
        files = {"input.csv": csv_text(header, [(i, c, money(v)) for i, c, v in transactions])}
        if kind == "complex-settlement":
            files["refunds.csv"] = csv_text(refund_header, [(i, t, money(v)) for i, t, v in returns])
        expected = expected_summary(transactions) if kind == "simple-csv" else expected_settlement(transactions, returns)
        result.append((name, files, expected))
    if kind == "simple-csv":
        result.append(("negative-zero", {"input.csv": csv_text(header, [("x", "Z", "-0.00")])}, expected_summary([("x", "Z", 0)])))
    invalid = {
        "wrong-header": "category,id,amount\nA,x,1.00\n",
        "missing-column": "id,category,amount\nx,A\n",
        "extra-column": "id,category,amount\nx,A,1.00,extra\n",
        "duplicate-id": "id,category,amount\nx,A,1.00\nx,B,2.00\n",
        "empty-id": "id,category,amount\n,A,1.00\n",
        "empty-category": "id,category,amount\nx,,1.00\n",
    }
    for amount in ("1", "1.0", "1.000", "01.00", "+1.00", " 1.00", "1.00 ", "1e2", "NaN", "", "١.00"):
        invalid["invalid-amount-" + str(len(invalid))] = csv_text(header, [("x", "A", amount)])
    if kind == "complex-settlement":
        invalid["negative"] = "id,category,amount\nx,A,-1.00\n"
    for name, text in invalid.items():
        files = {"input.csv": text}
        if kind == "complex-settlement":
            files["refunds.csv"] = csv_text(refund_header, [])
        result.append((name, files, None))
    if kind == "complex-settlement":
        # Exercise the second parser's entire error domain independently.
        for name, text in invalid.items():
            text = text.replace("category", "transaction_id")
            result.append(("refund-" + name, {"input.csv": csv_text(header, []), "refunds.csv": text}, None))
        result.append(("over-refund", {"input.csv": csv_text(header, [("x", "A", "1.00")]),
                                      "refunds.csv": csv_text(refund_header, [("a", "x", "0.51"), ("b", "x", "0.50")])}, None))
    return result


def run(project, kind):
    results = []
    # Copy verified downloaded files into isolated scratch; reject symlink inputs.
    if any(path.is_symlink() for path in project.rglob("*")):
        raise ValueError("artifact tree must not contain symlinks")
    with tempfile.TemporaryDirectory(prefix="task-oracle-") as temporary:
        root = Path(temporary)
        code = root / "project"
        shutil.copytree(project, code)
        if not (code / "app.py").is_file():
            raise ValueError("missing app.py")
        if not any(path.is_file() and path.name.lower().startswith("readme") for path in code.iterdir()):
            raise ValueError("missing README")
        for index, (name, inputs, expected) in enumerate(cases(kind)):
            work = root / str(index)
            work.mkdir()
            for filename, text in inputs.items():
                (work / filename).write_text(text, encoding="utf-8")
            output = work / "summary.json"
            command = [sys.executable, "-S", "-B", str(code / "app.py")]
            if kind == "simple-csv":
                command += ["--input", str(work / "input.csv")]
            else:
                command += ["--transactions", str(work / "input.csv"), "--refunds", str(work / "refunds.csv")]
            command += ["--output", str(output)]
            try:
                # File-backed output keeps runaway candidate logging out of memory.
                with (work / "stdout").open("wb") as stdout, (work / "stderr").open("wb") as stderr:
                    process = subprocess.run(command, cwd=code, stdout=stdout, stderr=stderr, timeout=10)
                if expected is None:
                    passed = process.returncode == 2 and not output.exists()
                else:
                    actual = json.loads(output.read_text(encoding="utf-8")) if output.is_file() else None
                    # JSON equality alone would treat true == 1. Preserve types.
                    passed = process.returncode == 0 and json.dumps(actual, sort_keys=True) == json.dumps(expected, sort_keys=True)
                results.append({"case": name, "passed": passed, "exitCode": process.returncode})
            except (OSError, ValueError, subprocess.TimeoutExpired) as error:
                results.append({"case": name, "passed": False, "error": type(error).__name__})
        if kind == "complex-settlement":
            results += check_modules(code, root)
            results.append(check_parser_reuse(code, root))
        with (root / "tests.log").open("wb") as log:
            try:
                tests = subprocess.run([sys.executable, "-S", "-B", "-m", "unittest", "discover"], cwd=code,
                                       stdout=log, stderr=log, timeout=30)
                text = (root / "tests.log").read_text(encoding="utf-8", errors="replace")
                import re
                counts = re.findall(r"Ran (\d+) tests? in", text)
                results.append({"case": "delivered-unittest", "passed": tests.returncode == 0 and bool(counts) and int(counts[-1]) > 0})
            except subprocess.TimeoutExpired:
                results.append({"case": "delivered-unittest", "passed": False, "error": "TimeoutExpired"})
    return {"schemaVersion": 1, "caseId": kind, "passed": all(r["passed"] for r in results),
            "checks": results, "scope": "business behavior only; not Harness certification"}


def check_modules(code, root):
    """Verify parser API/error semantics separately from the CLI wrapper."""
    script = """
import importlib, json, sys
module, function, filename = sys.argv[1:]
try:
    result = getattr(importlib.import_module(module), function)(filename)
    print(json.dumps(result, sort_keys=True))
except ValueError:
    sys.exit(2)
"""
    results = []
    for module in ("transactions", "refunds"):
        for index, (name, text, expected) in enumerate(module_cases(module)):
            filename = root / (module + str(index) + ".csv")
            filename.write_text(text, encoding="utf-8")
            try:
                with tempfile.TemporaryFile() as log:
                    process = subprocess.run([sys.executable, "-S", "-B", "-c", script, module, "load_" + module, str(filename)],
                                             cwd=code, stdout=log, stderr=subprocess.DEVNULL, timeout=10)
                    log.seek(0)
                    data = log.read(65536)
                passed = (process.returncode == 2 if expected is None else process.returncode == 0
                          and json.dumps(json.loads(data), sort_keys=True) == json.dumps(expected, sort_keys=True))
                results.append({"case": module + "-api-" + name, "passed": passed})
            except (ValueError, OSError, subprocess.TimeoutExpired):
                results.append({"case": module + "-api-" + name, "passed": False})
    return results


def module_cases(module):
    selected, seen = [], set()
    for name, files, expected in cases("complex-settlement"):
        if name == "over-refund" or (expected is None and name.startswith("refund-") != (module == "refunds")):
            continue
        text = files["input.csv" if module == "transactions" else "refunds.csv"]
        if text in seen:
            continue
        seen.add(text)
        rows = None
        if expected is not None:
            rows = list(csv.DictReader(io.StringIO(text)))
            for row in rows:
                units, fraction = row["amount"].split(".")
                row["amount"] = int(units) * 100 + int(fraction)
        selected.append((name, text, rows))
    return selected


def check_parser_reuse(code, root):
    """Change parser return data to prove the CLI consumes the public functions."""
    script = """
import runpy, sys, transactions, refunds
transactions.load_transactions = lambda path: [{'id':'injected','category':'parser-source','amount':12345}]
refunds.load_refunds = lambda path: [{'id':'refund','transaction_id':'injected','amount':345}]
sys.argv = ['app.py'] + sys.argv[1:]
runpy.run_path('app.py', run_name='__main__')
"""
    transactions = root / "reuse-transactions.csv"
    refunds = root / "reuse-refunds.csv"
    output = root / "reuse.json"
    transactions.write_text("id,category,amount\nx,A,1.00\n")
    refunds.write_text("id,transaction_id,amount\n")
    try:
        with tempfile.TemporaryFile() as log:
            process = subprocess.run([sys.executable, "-S", "-B", "-c", script, "--transactions", str(transactions),
                                      "--refunds", str(refunds), "--output", str(output)], cwd=code,
                                     stdout=log, stderr=log, timeout=10)
        expected = expected_settlement([("injected", "parser-source", 12345)], [("refund", "injected", 345)])
        actual = json.loads(output.read_text()) if output.is_file() else None
        passed = process.returncode == 0 and json.dumps(actual, sort_keys=True) == json.dumps(expected, sort_keys=True)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        passed = False
    return {"case": "cli-consumes-public-parsers", "passed": passed}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("case", choices=("simple-csv", "complex-settlement"))
    parser.add_argument("project", type=Path)
    args = parser.parse_args()
    if os.getenv("TASK_ORACLE_ISOLATED") != "1":
        parser.error("execute inside the documented networkless disposable container")
    try:
        result = run(args.project.resolve(), args.case)
    except (OSError, ValueError) as error:
        result = {"schemaVersion": 1, "caseId": args.case, "passed": False, "error": str(error)}
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
