"""Exercise the image's shell/linker flags and the supported Make entry points."""
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ControllerVersionBuildTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("go"), "Go is required for the linker probe")
    def test_docker_stamp_expands_requested_version(self):
        instruction = next(
            line.removeprefix("RUN ")
            for line in (ROOT / "agentteams-controller/Dockerfile").read_text().splitlines()
            if line.startswith("RUN CGO_ENABLED=1 go build")
        )
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "main.go"
            source.write_text('package main\nimport "fmt"\nvar version="dev"\nfunc main(){fmt.Print(version)}\n')
            for version in ("v1.2.9-test", "v9.8.7-test"):
                with self.subTest(version=version):
                    binary = Path(directory) / f"controller-{version}"
                    command = instruction.replace(
                        "-o /agentteams-controller ./cmd/controller/",
                        f"-o {shlex.quote(str(binary))} {shlex.quote(str(source))}",
                    )
                    self.assertNotEqual(command, instruction)
                    subprocess.run(
                        ["sh", "-c", command], check=True,
                        env=dict(os.environ, CONTROLLER_VERSION=version),
                        capture_output=True, text=True,
                    )
                    actual = subprocess.check_output([str(binary)], text=True)
                    self.assertEqual(actual, version)

    def test_image_make_targets_pass_release_version(self):
        for version in ("v1.2.9-test", "v9.8.7-test", "latest"):
            expected = "dev" if version == "latest" else version
            for target, engine in (
                ("build-agentteams-controller", "0"),
                ("push-agentteams-controller", "0"),
                ("push-agentteams-controller", "1"),
            ):
                with self.subTest(version=version, target=target, podman=engine):
                    result = subprocess.run(
                        ["make", "-n", target, f"VERSION={version}", f"IS_PODMAN={engine}"],
                        cwd=ROOT, check=True, capture_output=True, text=True,
                    )
                    self.assertIn(f"--build-arg CONTROLLER_VERSION={expected}", result.stdout)


if __name__ == "__main__":
    unittest.main()
