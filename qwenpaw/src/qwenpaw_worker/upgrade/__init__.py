"""Upgrade support for the QwenPaw runtime (checker / versions / runner).

Importable core of the formerly-standalone ``check_qwenpaw_upgrade.py``
CLI plus the two-phase upgrade job lifecycle used by the
``agentteams-upgrade`` plugin. The CLI script remains a thin wrapper around
``checker.run_preflight`` so ``bash``/``python3`` invocations keep working
unchanged.
"""
from qwenpaw_worker.upgrade import checker, versions  # noqa: F401
from qwenpaw_worker.upgrade.checker import (  # noqa: F401
    Check,
    STATUS_FAIL,
    STATUS_NOT_RUN,
    STATUS_PASS,
    STATUS_SKIP,
    STATUS_WARN,
    download_wheel,
    run_check,
    run_preflight,
)
from qwenpaw_worker.upgrade.runner import (  # noqa: F401
    JobState,
    UpgradeError,
    UpgradeInProgress,
    UpgradeNotFound,
    UpgradeRejected,
    UpgradeUnsupported,
    active_job,
    job_status,
    log_tail,
    start_upgrade,
)
from qwenpaw_worker.upgrade.versions import UpgradeIndexError  # noqa: F401

__all__ = [
    "Check",
    "JobState",
    "STATUS_FAIL",
    "STATUS_NOT_RUN",
    "STATUS_PASS",
    "STATUS_SKIP",
    "STATUS_WARN",
    "UpgradeError",
    "UpgradeIndexError",
    "UpgradeInProgress",
    "UpgradeNotFound",
    "UpgradeRejected",
    "UpgradeUnsupported",
    "active_job",
    "checker",
    "download_wheel",
    "job_status",
    "log_tail",
    "run_check",
    "run_preflight",
    "start_upgrade",
    "versions",
]
