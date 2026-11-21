"""Run business_oracle against a snapshot of downloaded artifacts in Docker.

No candidate code is executed on the host. Use an existing local image with a
Python standard-library interpreter; the exact image ID is recorded in evidence.
"""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("case", choices=("simple-csv", "complex-settlement"))
    parser.add_argument("project", type=Path, help="Downloaded and hash-verified project artifact directory")
    parser.add_argument("--image", required=True, help="Existing local image, preferably pinned by digest")
    parser.add_argument("--python", default="python3", help="Interpreter path inside the image")
    args = parser.parse_args()
    oracle = Path(__file__).with_name("business_oracle.py").resolve()
    project = args.project.resolve()
    name = "at-business-oracle-" + uuid.uuid4().hex[:12]
    record = {"schemaVersion": 1, "caseId": args.case, "passed": False,
              "oracleSHA256": hashlib.sha256(oracle.read_bytes()).hexdigest(),
              "scope": "business behavior only; not Harness certification"}
    started = False
    try:
        image = subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}}", args.image],
                               check=True, capture_output=True, text=True, timeout=20).stdout.strip()
        record["imageID"] = image
        if not project.is_dir() or any(path.is_symlink() for path in project.rglob("*")):
            raise ValueError("artifact directory must exist and contain no symlinks")
        with tempfile.TemporaryDirectory(prefix="at-oracle-snapshot-") as temporary:
            snapshot = Path(temporary) / "project"
            shutil.copytree(project, snapshot)
            manifest = {}
            size = 0
            for file in sorted(snapshot.rglob("*")):
                if file.is_file():
                    size += file.stat().st_size
                    if size > 64 * 1024 * 1024:
                        raise ValueError("artifact tree exceeds 64 MiB")
                    manifest[file.relative_to(snapshot).as_posix()] = hashlib.sha256(file.read_bytes()).hexdigest()
                    file.chmod(0o444)
                elif file.is_dir():
                    file.chmod(0o755)
                else:
                    raise ValueError("artifact tree must contain regular files only")
            # macOS temporary roots are mode 0700; container bind mounting this
            # directory itself exposes only the snapshot, with readable mode.
            snapshot.chmod(0o755)
            record["artifacts"] = manifest
            command = ["docker", "run", "--rm", "--pull=never", "--name", name, "--network", "none",
                       "--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
                       "--pids-limit", "64", "--cpus", "1", "--memory", "512m", "--user", "65534:65534",
                       "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777", "-e", "TASK_ORACLE_ISOLATED=1",
                       "-v", str(snapshot) + ":/candidate:ro", "-v", str(oracle) + ":/business_oracle.py:ro",
                       "--entrypoint", args.python, image, "-S", "-B", "/business_oracle.py", args.case, "/candidate"]
            started = True
            process = subprocess.run(command, capture_output=True, text=True, timeout=180)
            result = json.loads(process.stdout)
            record["verdict"] = result
            record["passed"] = process.returncode == 0 and result.get("passed") is True
            record["exitCode"] = process.returncode
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        record["error"] = type(error).__name__ + ": " + str(error)
    finally:
        if started:
            # A timeout of the Docker client does not prove the container stopped.
            subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=20)
    print(json.dumps(record, ensure_ascii=False, indent=2))
    return 0 if record["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
