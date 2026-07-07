import logging
from contextlib import contextmanager
from logging.handlers import RotatingFileHandler


@contextmanager
def _clear_root_handlers():
    root = logging.getLogger()
    previous_handlers = list(root.handlers)
    previous_level = root.level
    for handler in previous_handlers:
        root.removeHandler(handler)
    try:
        yield root
    finally:
        for handler in list(root.handlers):
            root.removeHandler(handler)
            handler.close()
        for handler in previous_handlers:
            root.addHandler(handler)
        root.setLevel(previous_level)


def test_configure_worker_logging_writes_default_rotating_file(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))
    monkeypatch.delenv("QWENPAW_WORKER_LOG_DIR", raising=False)
    monkeypatch.delenv("QWENPAW_WORKER_LOG_FILE_ENABLED", raising=False)

    with _clear_root_handlers() as root:
        log_file = configure_worker_logging()

        logging.getLogger("qwenpaw_worker.test").info("component=worker stage=start duration_ms=1")
        for handler in root.handlers:
            handler.flush()

        assert log_file == tmp_path / ".qwenpaw" / "logs" / "qwenpaw-worker.log"
        assert any(isinstance(handler, RotatingFileHandler) for handler in root.handlers)
        assert any(
            isinstance(handler, logging.StreamHandler) and not isinstance(handler, RotatingFileHandler)
            for handler in root.handlers
        )
        assert "component=worker stage=start duration_ms=1" in log_file.read_text(encoding="utf-8")
        assert "worker logging configured component=worker stage=logging event=configured" in log_file.read_text(
            encoding="utf-8"
        )
        assert "max_bytes=" in log_file.read_text(encoding="utf-8")
        assert "backup_count=" in log_file.read_text(encoding="utf-8")


def test_configure_worker_logging_rotates_file(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))
    monkeypatch.setenv("QWENPAW_WORKER_LOG_MAX_BYTES", "160")
    monkeypatch.setenv("QWENPAW_WORKER_LOG_BACKUP_COUNT", "1")

    with _clear_root_handlers() as root:
        log_file = configure_worker_logging()

        logger = logging.getLogger("qwenpaw_worker.rotate")
        for index in range(20):
            logger.info("component=worker stage=rotate duration_ms=%d payload=%s", index, "x" * 80)
        for handler in root.handlers:
            handler.flush()

        assert log_file is not None
        assert log_file.exists()
        assert log_file.with_name("qwenpaw-worker.log.1").exists()


def test_configure_worker_logging_can_disable_file_logging(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))

    with _clear_root_handlers() as root:
        configure_worker_logging()
        monkeypatch.setenv("QWENPAW_WORKER_LOG_FILE_ENABLED", "false")

        log_file = configure_worker_logging()

        assert log_file is None
        assert not any(isinstance(handler, RotatingFileHandler) for handler in root.handlers)
        assert any(isinstance(handler, logging.StreamHandler) for handler in root.handlers)


def test_configure_worker_logging_is_idempotent(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))

    with _clear_root_handlers() as root:
        first = configure_worker_logging()
        second = configure_worker_logging()

        assert second == first
        assert sum(isinstance(handler, RotatingFileHandler) for handler in root.handlers) == 1
        assert (
            sum(
                isinstance(handler, logging.StreamHandler) and not isinstance(handler, RotatingFileHandler)
                for handler in root.handlers
            )
            == 1
        )


def test_configure_worker_logging_falls_back_for_invalid_rotation_env(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import DEFAULT_LOG_BACKUP_COUNT, DEFAULT_LOG_MAX_BYTES, configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))
    monkeypatch.setenv("QWENPAW_WORKER_LOG_MAX_BYTES", "not-a-number")
    monkeypatch.setenv("QWENPAW_WORKER_LOG_BACKUP_COUNT", "-1")

    with _clear_root_handlers() as root:
        configure_worker_logging()

        file_handler = next(handler for handler in root.handlers if isinstance(handler, RotatingFileHandler))
        assert file_handler.maxBytes == DEFAULT_LOG_MAX_BYTES
        assert file_handler.backupCount == DEFAULT_LOG_BACKUP_COUNT


def test_configure_worker_logging_clamps_rotation_env_to_upper_limits(tmp_path, monkeypatch) -> None:
    from qwenpaw_worker.log import MAX_LOG_BACKUP_COUNT, MAX_LOG_MAX_BYTES, configure_worker_logging

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))
    monkeypatch.setenv("QWENPAW_WORKER_LOG_MAX_BYTES", str(MAX_LOG_MAX_BYTES * 100))
    monkeypatch.setenv("QWENPAW_WORKER_LOG_BACKUP_COUNT", str(MAX_LOG_BACKUP_COUNT * 100))

    with _clear_root_handlers() as root:
        log_file = configure_worker_logging()

        file_handler = next(handler for handler in root.handlers if isinstance(handler, RotatingFileHandler))
        for handler in root.handlers:
            handler.flush()

        assert file_handler.maxBytes == MAX_LOG_MAX_BYTES
        assert file_handler.backupCount == MAX_LOG_BACKUP_COUNT
        assert f"max_bytes={MAX_LOG_MAX_BYTES}" in log_file.read_text(encoding="utf-8")
        assert f"backup_count={MAX_LOG_BACKUP_COUNT}" in log_file.read_text(encoding="utf-8")
