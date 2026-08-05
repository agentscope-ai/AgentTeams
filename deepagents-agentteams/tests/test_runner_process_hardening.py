import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from deepagents_agentteams import runner
from deepagents_agentteams.runner_core import RunnerService


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_consumes_runner_token_before_hardening_and_keeps_it_usable_for_app() -> None:
    library = MagicMock()
    library.prctl.return_value = 0

    with (
        patch.dict(os.environ, {"AGENTTEAMS_RUNNER_TOKEN": "test-runner-token"}, clear=False),
        patch.object(runner.resource, "setrlimit") as setrlimit,
        patch.object(runner.ctypes, "CDLL", return_value=library),
        tempfile.TemporaryDirectory() as workspace,
        tempfile.TemporaryDirectory() as state,
    ):
        token = runner.consume_and_harden_runner_token()
        app = runner.create_app(
            service=RunnerService(workspace=Path(workspace), state_dir=Path(state)),
            bearer_token=token,
        )

        assert app is not None
        assert "AGENTTEAMS_RUNNER_TOKEN" not in os.environ
        setrlimit.assert_called_once_with(runner.resource.RLIMIT_CORE, (0, 0))
        library.prctl.assert_called_once_with(runner._PR_SET_DUMPABLE, 0, 0, 0, 0)


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_rejects_missing_runner_token_before_hardening() -> None:
    with patch.dict(os.environ, {}, clear=True), pytest.raises(RuntimeError):
        runner.consume_and_harden_runner_token()


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_main_does_not_construct_app_or_start_uvicorn_after_hardening_failure() -> None:
    with (
        patch.dict(os.environ, {"AGENTTEAMS_RUNNER_TOKEN": "test-runner-token"}, clear=True),
        patch.object(runner.resource, "setrlimit", side_effect=OSError("hardening failed")),
        patch.object(runner, "create_app") as create_app,
        patch.object(runner.uvicorn, "run") as uvicorn_run,
        pytest.raises(OSError),
    ):
        runner.main()

    create_app.assert_not_called()
    uvicorn_run.assert_not_called()


def test_rejects_unsupported_platform_before_startup() -> None:
    with (
        patch.dict(os.environ, {"AGENTTEAMS_RUNNER_TOKEN": "test-runner-token"}, clear=True),
        patch.object(runner.sys, "platform", "darwin"),
        pytest.raises(RuntimeError),
    ):
        runner.consume_and_harden_runner_token()


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_propagates_process_library_failure() -> None:
    with (
        patch.dict(os.environ, {"AGENTTEAMS_RUNNER_TOKEN": "test-runner-token"}, clear=True),
        patch.object(runner.resource, "setrlimit"),
        patch.object(runner.ctypes, "CDLL", side_effect=OSError("library unavailable")),
        pytest.raises(OSError),
    ):
        runner.consume_and_harden_runner_token()


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_raises_oserror_with_errno_when_prctl_fails() -> None:
    class FailingPrctl:
        def __call__(self, *_args: object) -> int:
            return -1

    class Library:
        prctl = FailingPrctl()

    with (
        patch.dict(os.environ, {"AGENTTEAMS_RUNNER_TOKEN": "test-runner-token"}, clear=True),
        patch.object(runner.resource, "setrlimit"),
        patch.object(runner.ctypes, "CDLL", return_value=Library()),
        patch.object(runner.ctypes, "get_errno", return_value=1),
        pytest.raises(OSError) as error,
    ):
        runner.consume_and_harden_runner_token()

    assert error.value.errno == 1


@pytest.mark.skipif(sys.platform != "linux", reason="runner process hardening is Linux-only")
def test_commands_cannot_read_hardened_parent_environment_or_runner_token() -> None:
    helper = """
import json
import tempfile
from pathlib import Path

from deepagents_agentteams.runner import consume_and_harden_runner_token
from deepagents_agentteams.runner_core import RunnerService

consume_and_harden_runner_token()
with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
    result = RunnerService(workspace=Path(workspace), state_dir=Path(state)).execute(
        request_id="proc-environ",
        command="cat /proc/$PPID/environ >/dev/null",
        timeout_seconds=5,
    )
    print(json.dumps({
        "parent_environment_blocked": result.exit_code != 0,
        "sentinel_absent": "runner-sentinel-not-a-secret" not in result.output,
        "runner_token_absent": "AGENTTEAMS_RUNNER_TOKEN" not in result.output,
    }))
"""
    environment = os.environ.copy()
    environment["AGENTTEAMS_RUNNER_TOKEN"] = "runner-sentinel-not-a-secret"  # noqa: S105 - non-secret sentinel.
    completed = subprocess.run(  # noqa: S603 - helper and interpreter are fixed test inputs.
        [sys.executable, "-c", helper],
        check=True,
        capture_output=True,
        cwd=Path(__file__).resolve().parents[1],
        env=environment,
        text=True,
    )

    assert json.loads(completed.stdout) == {
        "parent_environment_blocked": True,
        "sentinel_absent": True,
        "runner_token_absent": True,
    }
