"""Logging setup for qwenpaw-worker."""

from __future__ import annotations

import logging
from logging.handlers import RotatingFileHandler
import os
from pathlib import Path
from typing import Optional


LOG_FORMAT = "%(asctime)s [%(levelname)s] %(name)s: %(message)s"
DEFAULT_LOG_MAX_BYTES = 10 * 1024 * 1024
DEFAULT_LOG_BACKUP_COUNT = 20
MAX_LOG_MAX_BYTES = 20 * 1024 * 1024
MAX_LOG_BACKUP_COUNT = 50
DEFAULT_LOG_FILE_NAME = "qwenpaw-worker.log"

_CONSOLE_HANDLER_MARK = "_qwenpaw_worker_console_handler"
_FILE_HANDLER_MARK = "_qwenpaw_worker_file_handler"
_FALSE_VALUES = {"0", "false", "no", "off"}


def configure_worker_logging(working_dir: Optional[Path] = None) -> Optional[Path]:
    """Configure console and rotating file logging for qwenpaw-worker."""

    root = logging.getLogger()
    level = _log_level()
    formatter = logging.Formatter(LOG_FORMAT)
    root.setLevel(level)

    _ensure_console_handler(root, formatter, level)

    if not _file_logging_enabled():
        _remove_marked_handlers(root, _FILE_HANDLER_MARK)
        logging.getLogger(__name__).info(
            "worker logging configured component=worker stage=logging event=disabled file_enabled=False level=%s",
            logging.getLevelName(level),
        )
        return None

    log_file = _log_file_path(working_dir)
    max_bytes = _bounded_int(
        os.environ.get("QWENPAW_WORKER_LOG_MAX_BYTES"),
        DEFAULT_LOG_MAX_BYTES,
        minimum=1,
        maximum=MAX_LOG_MAX_BYTES,
    )
    backup_count = _bounded_int(
        os.environ.get("QWENPAW_WORKER_LOG_BACKUP_COUNT"),
        DEFAULT_LOG_BACKUP_COUNT,
        minimum=0,
        maximum=MAX_LOG_BACKUP_COUNT,
    )
    file_handler = _find_marked_handler(root, _FILE_HANDLER_MARK)
    if file_handler is not None:
        file_handler.setLevel(level)
        file_handler.setFormatter(formatter)
        if isinstance(file_handler, RotatingFileHandler):
            file_handler.maxBytes = max_bytes
            file_handler.backupCount = backup_count
        _log_configured(log_file, level, max_bytes, backup_count)
        return Path(getattr(file_handler, "baseFilename", log_file))

    try:
        log_file.parent.mkdir(parents=True, exist_ok=True)
        file_handler = RotatingFileHandler(
            log_file,
            maxBytes=max_bytes,
            backupCount=backup_count,
            encoding="utf-8",
        )
    except OSError as exc:
        logging.getLogger(__name__).warning(
            "worker log file setup failed component=worker stage=logging event=failed path=%s error_type=%s",
            log_file,
            type(exc).__name__,
        )
        return None

    setattr(file_handler, _FILE_HANDLER_MARK, True)
    file_handler.setLevel(level)
    file_handler.setFormatter(formatter)
    root.addHandler(file_handler)
    _log_configured(log_file, level, max_bytes, backup_count)
    return log_file


def _log_configured(log_file: Path, level: int, max_bytes: int, backup_count: int) -> None:
    logging.getLogger(__name__).info(
        "worker logging configured component=worker stage=logging event=configured file_enabled=True "
        "path=%s max_bytes=%s backup_count=%s level=%s",
        log_file,
        max_bytes,
        backup_count,
        logging.getLevelName(level),
    )


def _ensure_console_handler(root: logging.Logger, formatter: logging.Formatter, level: int) -> None:
    handler = _find_marked_handler(root, _CONSOLE_HANDLER_MARK)
    if handler is None:
        handler = logging.StreamHandler()
        setattr(handler, _CONSOLE_HANDLER_MARK, True)
        root.addHandler(handler)
    handler.setLevel(level)
    handler.setFormatter(formatter)


def _find_marked_handler(root: logging.Logger, mark: str) -> Optional[logging.Handler]:
    for handler in root.handlers:
        if getattr(handler, mark, False):
            return handler
    return None


def _remove_marked_handlers(root: logging.Logger, mark: str) -> None:
    for handler in list(root.handlers):
        if getattr(handler, mark, False):
            root.removeHandler(handler)
            handler.close()


def _file_logging_enabled() -> bool:
    value = os.environ.get("QWENPAW_WORKER_LOG_FILE_ENABLED")
    if value is None:
        return True
    return value.strip().lower() not in _FALSE_VALUES


def _log_file_path(working_dir: Optional[Path]) -> Path:
    log_dir = os.environ.get("QWENPAW_WORKER_LOG_DIR")
    if log_dir:
        return Path(log_dir) / DEFAULT_LOG_FILE_NAME

    env_working_dir = os.environ.get("QWENPAW_WORKING_DIR")
    if env_working_dir:
        return Path(env_working_dir) / "logs" / DEFAULT_LOG_FILE_NAME

    if working_dir is not None:
        return Path(working_dir) / "logs" / DEFAULT_LOG_FILE_NAME

    return Path.cwd() / ".qwenpaw" / "logs" / DEFAULT_LOG_FILE_NAME


def _log_level() -> int:
    value = os.environ.get("QWENPAW_LOG_LEVEL", "INFO").strip()
    if value.isdigit():
        return int(value)

    level = logging.getLevelName(value.upper())
    if isinstance(level, int):
        return level
    return logging.INFO


def _bounded_int(value: Optional[str], default: int, *, minimum: int, maximum: int) -> int:
    try:
        parsed = int(value) if value is not None else default
    except (TypeError, ValueError):
        return default
    if parsed < minimum:
        return default
    return min(parsed, maximum)
